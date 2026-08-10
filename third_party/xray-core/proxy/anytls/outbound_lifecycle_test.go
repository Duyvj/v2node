package anytls

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	xnet "github.com/xtls/xray-core/common/net"
)

type rejectingWriter struct {
	err error
}

func (w rejectingWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func newLifecycleTestClient() *Client {
	c := &Client{
		idleSessionCheckInterval: time.Hour,
		idleSessionTimeout:       time.Hour,
		sessions:                 make(map[uint64]*session),
		stopCh:                   make(chan struct{}),
	}
	c.cleanupWG.Add(1)
	go func() {
		defer c.cleanupWG.Done()
		c.cleanupIdleSessions()
	}()
	return c
}

func TestStreamDieHookInstalledAfterCompletionFiresExactlyOnce(t *testing.T) {
	c := &Client{}
	sess := &session{
		client:  c,
		seq:     17,
		streams: make(map[uint32]*stream),
	}
	st := newStream(7, nil)
	if !sess.addClientStream(st) {
		t.Fatal("failed to admit test stream")
	}

	// Model a peer sending FIN after openStream publishes the stream but
	// before Process installs its completion callback.
	sess.finishStream(st.sid, nil)

	var calls atomic.Int32
	st.installDieHook(func() {
		calls.Add(1)
		if sess.isClosed() || sess.activeStreams.Load() != 0 {
			return
		}
		c.markSessionIdle(sess)
	})
	// A duplicate installation must not run a second completion callback.
	st.installDieHook(func() {
		calls.Add(1)
	})

	if got := calls.Load(); got != 1 {
		t.Fatalf("completion hook calls = %d, want 1", got)
	}
	if !sess.inIdlePool.Load() {
		t.Fatal("completed session was not returned to the idle pool")
	}
	if len(c.idleSessions) != 1 || c.idleSessions[0] != sess.seq {
		t.Fatalf("idle sessions = %v, want [%d]", c.idleSessions, sess.seq)
	}
}

func TestStreamDieHookConcurrentWithCompletion(t *testing.T) {
	for i := 0; i < 256; i++ {
		st := newStream(uint32(i), nil)
		start := make(chan struct{})
		var calls atomic.Int32
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			st.installDieHook(func() {
				calls.Add(1)
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			st.close(nil)
		}()
		close(start)
		wg.Wait()

		if got := calls.Load(); got != 1 {
			t.Fatalf("iteration %d: completion hook calls = %d, want 1", i, got)
		}
	}
}

func TestOpenStreamFailureDoesNotReturnSessionToIdlePool(t *testing.T) {
	writeErr := errors.New("write failed")
	c := &Client{}
	sess := &session{
		client:        c,
		isClient:      true,
		paddingScheme: getDefaultPaddingScheme(),
		streams:       make(map[uint32]*stream),
		synAckCh:      make(map[uint32]chan error),
		errCh:         make(chan error, 1),
		settingsSent:  true,
	}
	sess.nextSID.Store(1)
	sess.fw = newFrameWriter(buf.NewBufferedWriter(buf.NewWriter(rejectingWriter{err: writeErr})))

	_, err := sess.openStream(
		context.Background(),
		xnet.TCPDestination(xnet.ParseAddress("example.com"), 443),
		nil,
	)
	if err == nil {
		t.Fatal("openStream unexpectedly succeeded")
	}
	if got := sess.activeStreams.Load(); got != 0 {
		t.Fatalf("active stream count = %d, want 0", got)
	}
	if len(sess.streams) != 0 {
		t.Fatalf("failed stream retained in map: %d", len(sess.streams))
	}
	if sess.inIdlePool.Load() || len(c.idleSessions) != 0 {
		t.Fatalf("failed stream returned session to idle pool: %v", c.idleSessions)
	}
}

func TestClientCloseStopsCleanupAndReleasesSessions(t *testing.T) {
	c := newLifecycleTestClient()
	conn, peer := net.Pipe()
	defer peer.Close()
	activeStream := newStream(7, nil)

	sess := &session{
		client:   c,
		isClient: true,
		conn:     conn,
		streams:  map[uint32]*stream{activeStream.sid: activeStream},
		synAckCh: make(map[uint32]chan error),
		errCh:    make(chan error, 1),
		seq:      1,
	}
	sess.dieHook = func() {
		c.sessionsMu.Lock()
		delete(c.sessions, sess.seq)
		c.sessionsMu.Unlock()
	}
	c.sessions[sess.seq] = sess
	c.idleSessions = make([]uint64, 1, 4096)
	c.idleSessions[0] = sess.seq

	// Outbound Handler.Close uses this same common.Close dispatch path.
	if err := common.Close(c); err != nil {
		t.Fatalf("close client: %v", err)
	}

	if !c.closed.Load() {
		t.Fatal("client was not marked closed")
	}
	if !sess.isClosed() {
		t.Fatal("active session was not closed")
	}
	select {
	case <-activeStream.done:
	default:
		t.Fatal("active stream was not closed")
	}
	if c.sessions != nil {
		t.Fatalf("session map was retained after close: len=%d", len(c.sessions))
	}
	if c.idleSessions != nil {
		t.Fatalf("idle-session backing slice was retained after close: len=%d cap=%d", len(c.idleSessions), cap(c.idleSessions))
	}
	select {
	case err := <-sess.errCh:
		if err == nil || err.Error() != errClientClosed.Error() {
			t.Fatalf("session close error = %v, want %v", err, errClientClosed)
		}
	default:
		t.Fatal("active session was not notified of client shutdown")
	}
}

func TestClientCloseIsConcurrentAndIdempotent(t *testing.T) {
	c := newLifecycleTestClient()

	const closers = 32
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(closers)
	for range closers {
		go func() {
			defer wg.Done()
			<-start
			if err := c.Close(); err != nil {
				t.Errorf("close client: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	// A late stream completion must not repopulate the released idle pool.
	orphan := &session{seq: 99, streams: make(map[uint32]*stream)}
	c.markSessionIdle(orphan)
	if c.idleSessions != nil {
		t.Fatalf("closed client accepted an idle session: %v", c.idleSessions)
	}
}
