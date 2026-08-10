package hysteria

import (
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/transport/internet/stat"
)

func fullUDPSessionManager() *udpSessionManager {
	m := &udpSessionManager{m: make(map[uint32]*InterConn)}
	for i := 0; i < maxUDPSessionsPerConn; i++ {
		id := uint32(i)
		m.m[id] = &InterConn{id: id, queue: newUDPDatagramQueue()}
	}
	return m
}

func TestUDPSessionManagerHardLimit(t *testing.T) {
	m := fullUDPSessionManager()
	var added atomic.Int32
	m.addConn = func(_ stat.Connection) {
		added.Add(1)
	}

	m.feed(uint32(maxUDPSessionsPerConn+1), []byte{0, 0, 0, 1})
	if got := len(m.m); got != maxUDPSessionsPerConn {
		t.Fatalf("session count = %d, want %d", got, maxUDPSessionsPerConn)
	}
	if got := added.Load(); got != 0 {
		t.Fatalf("inbound callback count = %d, want 0", got)
	}
	if conn, err := m.udp(); err == nil || conn != nil {
		t.Fatalf("full manager returned conn=%v err=%v", conn, err)
	}
}

func TestUDPSessionManagerShutdownDropsQueues(t *testing.T) {
	const sessions = 8
	if maxQueuedUDPDatagrams != 4096 {
		t.Fatalf("aggregate queue budget = %d, want 4096", maxQueuedUDPDatagrams)
	}
	m := &udpSessionManager{m: make(map[uint32]*InterConn)}
	conns := make([]*InterConn, 0, sessions)
	m.Lock()
	for i := 0; i < sessions; i++ {
		id := uint32(i)
		conn := m.newUDPConnLocked(id)
		m.m[id] = conn
		for j := 0; j < udpMessageChanSize; j++ {
			m.enqueueLocked(conn, make([]byte, MaxDatagramFrameSize))
		}
		conns = append(conns, conn)
	}
	m.Unlock()
	if got := m.queued.Load(); got != maxQueuedUDPDatagrams {
		t.Fatalf("aggregate queued datagrams = %d, want %d", got, maxQueuedUDPDatagrams)
	}
	totalQueued := 0
	for _, conn := range conns {
		totalQueued += conn.queue.len()
	}
	if totalQueued != maxQueuedUDPDatagrams {
		t.Fatalf("total channel occupancy = %d, want %d", totalQueued, maxQueuedUDPDatagrams)
	}

	m.shutdown()
	if m.m != nil {
		t.Fatalf("session map retained after shutdown: %d", len(m.m))
	}
	for _, conn := range conns {
		if !conn.closed.Load() {
			t.Fatalf("session %d was not marked closed", conn.id)
		}
		if _, ok := conn.queue.pop(); ok {
			t.Fatalf("session %d retained a queued datagram", conn.id)
		}
	}
	if got := m.queued.Load(); got != 0 {
		t.Fatalf("queue permits retained after shutdown: %d", got)
	}
}

func TestUDPSessionManagerFeedAndShutdownAreConcurrentSafe(t *testing.T) {
	m := &udpSessionManager{m: make(map[uint32]*InterConn)}
	conn := m.newUDPConnLocked(1)
	m.m[1] = conn

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				m.feed(1, []byte{0, 0, 0, 1})
			}
		}()
	}
	m.shutdown()
	wg.Wait()
	if m.m != nil {
		t.Fatal("session map was repopulated after shutdown")
	}
	if got := m.queued.Load(); got != 0 {
		t.Fatalf("queue permits retained after concurrent shutdown: %d", got)
	}
}

func TestClosingOldUDPSessionDoesNotRemoveReusedID(t *testing.T) {
	old := &InterConn{id: 7, queue: newUDPDatagramQueue()}
	replacement := &InterConn{id: 7, queue: newUDPDatagramQueue()}
	m := &udpSessionManager{m: map[uint32]*InterConn{7: replacement}}

	m.Lock()
	m.closeConnLocked(old)
	m.Unlock()

	if got := m.m[7]; got != replacement {
		t.Fatalf("closing old session removed replacement: got %p, want %p", got, replacement)
	}
	if !old.closed.Load() {
		t.Fatal("old session was not closed")
	}
	if replacement.closed.Load() {
		t.Fatal("replacement session was closed")
	}
}

func TestUDPSessionAllowsUpstreamBurstAndReleasesPermitsOnRead(t *testing.T) {
	const upstreamBurst = 1024
	if udpMessageChanSize != upstreamBurst {
		t.Fatalf("per-session queue capacity = %d, want upstream capacity %d", udpMessageChanSize, upstreamBurst)
	}
	m := &udpSessionManager{m: make(map[uint32]*InterConn)}
	conn := m.newUDPConnLocked(1)
	m.m[1] = conn
	if got := conn.queue.capacity(); got != udpMessageQueueInitialSize {
		t.Fatalf("initial per-session queue allocation = %d, want %d", got, udpMessageQueueInitialSize)
	}
	datagram := []byte{0, 0, 0, 1}

	for i := 0; i < upstreamBurst; i++ {
		m.feed(1, datagram)
	}
	if got := conn.queue.len(); got != upstreamBurst {
		t.Fatalf("single-session burst queued %d datagrams, want %d", got, upstreamBurst)
	}
	if got := m.queued.Load(); got != upstreamBurst {
		t.Fatalf("queue permits after burst = %d, want %d", got, upstreamBurst)
	}

	// A failed enqueue into the full per-session channel must return its permit.
	m.feed(1, datagram)
	if got := m.queued.Load(); got != upstreamBurst {
		t.Fatalf("failed enqueue leaked a queue permit: %d", got)
	}

	readBuffer := make([]byte, len(datagram))
	for i := 0; i < upstreamBurst; i++ {
		if _, err := conn.Read(readBuffer); err != nil {
			t.Fatalf("read queued datagram %d: %v", i, err)
		}
	}
	if got := m.queued.Load(); got != 0 {
		t.Fatalf("queue permits retained after dequeue: %d", got)
	}
	if got := conn.queue.capacity(); got != udpMessageQueueInitialSize {
		t.Fatalf("empty queue retained burst capacity %d, want %d", got, udpMessageQueueInitialSize)
	}
}

func TestUDPSessionHandlerMayCloseImmediately(t *testing.T) {
	m := &udpSessionManager{m: make(map[uint32]*InterConn)}
	callbackDone := make(chan struct{})
	m.addConn = func(conn stat.Connection) {
		_ = conn.Close()
		close(callbackDone)
	}
	feedDone := make(chan struct{})
	go func() {
		m.feed(9, []byte{0, 0, 0, 9})
		close(feedDone)
	}()

	select {
	case <-callbackDone:
	case <-time.After(time.Second):
		t.Fatal("immediate Close handler deadlocked on manager lock")
	}
	select {
	case <-feedDone:
	case <-time.After(time.Second):
		t.Fatal("feed did not return after immediate Close handler")
	}
	if got := len(m.m); got != 0 {
		t.Fatalf("closed callback session retained in map: %d", got)
	}
	if got := m.queued.Load(); got != 0 {
		t.Fatalf("immediate session close retained queue permits: %d", got)
	}
}

func TestUDPSessionShortReadStillReleasesPermit(t *testing.T) {
	m := &udpSessionManager{m: make(map[uint32]*InterConn)}
	conn := m.newUDPConnLocked(1)
	m.m[1] = conn
	m.feed(1, []byte{0, 0, 0, 1})

	if _, err := conn.Read(make([]byte, 1)); err != io.ErrShortBuffer {
		t.Fatalf("short read error = %v, want %v", err, io.ErrShortBuffer)
	}
	if got := m.queued.Load(); got != 0 {
		t.Fatalf("short read retained queue permit: %d", got)
	}
}
