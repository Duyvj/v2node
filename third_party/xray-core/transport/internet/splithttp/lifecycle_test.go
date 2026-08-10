package splithttp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	stdnet "net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	xbuf "github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/signal/done"
)

type blockingReadCloser struct {
	done      chan struct{}
	closeOnce sync.Once
	closes    atomic.Int32
}

func newBlockingReadCloser() *blockingReadCloser {
	return &blockingReadCloser{done: make(chan struct{})}
}

func (r *blockingReadCloser) Read([]byte) (int, error) {
	<-r.done
	return 0, io.EOF
}

func (r *blockingReadCloser) Close() error {
	r.closeOnce.Do(func() {
		r.closes.Add(1)
		close(r.done)
	})
	return nil
}

type discardWriteCloser struct {
	closes atomic.Int32
}

type deadlineWriteResponse struct {
	header    http.Header
	entered   chan struct{}
	released  chan struct{}
	enterOnce sync.Once
	closeOnce sync.Once
}

func newDeadlineWriteResponse() *deadlineWriteResponse {
	return &deadlineWriteResponse{header: make(http.Header), entered: make(chan struct{}), released: make(chan struct{})}
}

func (w *deadlineWriteResponse) Header() http.Header { return w.header }
func (w *deadlineWriteResponse) WriteHeader(int)     {}
func (w *deadlineWriteResponse) Flush()              {}
func (w *deadlineWriteResponse) Write([]byte) (int, error) {
	w.enterOnce.Do(func() { close(w.entered) })
	<-w.released
	return 0, os.ErrDeadlineExceeded
}
func (w *deadlineWriteResponse) SetWriteDeadline(time.Time) error {
	w.closeOnce.Do(func() { close(w.released) })
	return nil
}

func (w *discardWriteCloser) Write(b []byte) (int, error) { return len(b), nil }
func (w *discardWriteCloser) Close() error {
	w.closes.Add(1)
	return nil
}

func TestSplitConnReadDeadlineInterruptsBlockedRead(t *testing.T) {
	reader := newBlockingReadCloser()
	writer := new(discardWriteCloser)
	conn := &splitConn{reader: reader, writer: writer}

	if err := conn.SetReadDeadline(time.Now().Add(25 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	_, err := conn.Read(make([]byte, 1))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Read error = %v, want deadline exceeded", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if got := reader.closes.Load(); got != 1 {
		t.Fatalf("reader closed %d times, want 1", got)
	}
	if got := writer.closes.Load(); got != 1 {
		t.Fatalf("writer closed %d times, want 1", got)
	}
}

type blockingWriteCloser struct {
	done      chan struct{}
	closeOnce sync.Once
	closes    atomic.Int32
}

func newBlockingWriteCloser() *blockingWriteCloser {
	return &blockingWriteCloser{done: make(chan struct{})}
}

func (w *blockingWriteCloser) Write([]byte) (int, error) {
	<-w.done
	return 0, io.ErrClosedPipe
}

func (w *blockingWriteCloser) Close() error {
	w.closeOnce.Do(func() {
		w.closes.Add(1)
		close(w.done)
	})
	return nil
}

func TestSplitConnTerminalDeadlineUnblocksPeerDirection(t *testing.T) {
	reader := newBlockingReadCloser()
	writer := newBlockingWriteCloser()
	conn := &splitConn{reader: reader, writer: writer}

	writeDone := make(chan error, 1)
	go func() {
		_, err := conn.Write([]byte("x"))
		writeDone <- err
	}()
	if err := conn.SetReadDeadline(time.Now().Add(25 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	_, err := conn.Read(make([]byte, 1))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Read error = %v, want deadline exceeded", err)
	}
	select {
	case err := <-writeDone:
		if err == nil {
			t.Fatal("peer write unexpectedly succeeded after terminal deadline")
		}
	case <-time.After(time.Second):
		t.Fatal("peer write deadlocked after read deadline")
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSplitConnSetDeadlineMarksBothDirections(t *testing.T) {
	conn := &splitConn{reader: newBlockingReadCloser(), writer: newBlockingWriteCloser()}
	if err := conn.SetDeadline(time.Now().Add(-time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Read after SetDeadline = %v, want deadline exceeded", err)
	}
	if _, err := conn.Write([]byte("x")); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Write after SetDeadline = %v, want deadline exceeded", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSplitConnUsesNativeDirectionalDeadline(t *testing.T) {
	reader, peer := stdnet.Pipe()
	defer peer.Close()
	writer := new(discardWriteCloser)
	conn := &splitConn{reader: reader, writer: writer}
	defer conn.Close()

	if err := conn.SetReadDeadline(time.Now().Add(25 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	_, err := conn.Read(make([]byte, 1))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("native Read error = %v, want deadline exceeded", err)
	}
	if conn.closed.Load() {
		t.Fatal("native read deadline terminally closed the split connection")
	}
	if got := writer.closes.Load(); got != 0 {
		t.Fatalf("native read deadline closed peer direction %d times", got)
	}

	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = peer.Write([]byte("x")) }()
	b := make([]byte, 1)
	if n, err := conn.Read(b); err != nil || n != 1 || b[0] != 'x' {
		t.Fatalf("read after clearing native deadline = (%d, %q, %v)", n, b, err)
	}
}

func TestSplitConnClearedFallbackDeadlineIsStale(t *testing.T) {
	reader := newBlockingReadCloser()
	conn := &splitConn{reader: reader, writer: new(discardWriteCloser)}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	select {
	case <-reader.done:
		t.Fatal("stale fallback deadline closed the reader")
	default:
	}
}

func TestSessionStoreBoundsUniqueIDChurn(t *testing.T) {
	const maxPending = 32
	store := newSessionStore(maxPending, time.Minute)
	defer store.Close()

	for i := 0; i < 100_000; i++ {
		_, err := store.upsert(fmt.Sprintf("session-%d", i), 1)
		if i < maxPending && err != nil {
			t.Fatalf("upsert %d failed: %v", i, err)
		}
		if i >= maxPending && !errors.Is(err, errTooManyPending) {
			t.Fatalf("upsert %d error = %v, want pending limit", i, err)
		}
	}

	sessions, pending := store.counts()
	if sessions != maxPending || pending != maxPending {
		t.Fatalf("store counts = (%d, %d), want (%d, %d)", sessions, pending, maxPending, maxPending)
	}
}

func TestSessionStoreCapsActiveSessionsAndRejectsSecondDownlink(t *testing.T) {
	store := newSessionStore(8, time.Minute)
	defer store.Close()
	store.maxSessions = 3

	var first *httpSession
	for i := 0; i < store.maxSessions; i++ {
		id := fmt.Sprintf("active-%d", i)
		session, err := store.upsert(id, 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.markConnected(id, session); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = session
		}
	}
	if _, err := store.upsert("overflow", 1); !errors.Is(err, errTooManySessions) {
		t.Fatalf("active overflow error = %v, want session cap", err)
	}
	if err := store.markConnected("active-0", first); !errors.Is(err, errSessionConnected) {
		t.Fatalf("second downlink error = %v, want already connected", err)
	}
	sessions, pending := store.counts()
	if sessions != 3 || pending != 0 {
		t.Fatalf("counts after duplicate downlink = (%d, %d), want (3, 0)", sessions, pending)
	}
	store.delete("active-0", first)
	retry, err := store.upsert("active-0", 1)
	if err != nil {
		t.Fatalf("retry after original downlink closed: %v", err)
	}
	if retry == first {
		t.Fatal("retry reused the deleted downlink session")
	}
	if err := store.markConnected("active-0", retry); err != nil {
		t.Fatalf("retry could not claim a fresh downlink: %v", err)
	}
}

func TestSlowSameIDExpiresBodyAndReleasesReservations(t *testing.T) {
	store := newSessionStore(4, 20*time.Millisecond)
	defer store.Close()
	session, err := store.upsert("slow", 4)
	if err != nil {
		t.Fatal(err)
	}
	body := newBlockingReadCloser()
	if _, err := store.registerBody("slow", session, body); err != nil {
		t.Fatal(err)
	}
	bytes, err := store.reserveBytes("slow", session, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := session.uploadQueue.reserve(7, bytes)
	if err != nil {
		t.Fatal(err)
	}

	duplicateBytes, err := store.reserveBytes("slow", session, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.uploadQueue.reserve(7, duplicateBytes); !errors.Is(err, errDuplicatePacket) {
		duplicateBytes.release()
		t.Fatalf("same-ID duplicate reservation error = %v", err)
	}
	duplicateBytes.release()

	deadline := time.Now().Add(time.Second)
	for {
		if store.retained() == 0 && session.uploadQueue.reservationCount() == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expiry retained bytes=%d reservations=%d", store.retained(), session.uploadQueue.reservationCount())
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-body.done:
	case <-time.After(time.Second):
		t.Fatal("pending expiry did not close the in-progress body")
	}
	if got := body.closes.Load(); got != 1 {
		t.Fatalf("body closed %d times, want 1", got)
	}
	reservation.release()
}

func TestConnectedSessionBodyReadDeadlineIsSwept(t *testing.T) {
	store := newSessionStore(4, time.Minute)
	defer store.Close()
	store.bodyReadTimeout = 20 * time.Millisecond
	session, err := store.upsert("connected", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.markConnected("connected", session); err != nil {
		t.Fatal(err)
	}
	body := newBlockingReadCloser()
	if _, err := store.registerBody("connected", session, body); err != nil {
		t.Fatal(err)
	}
	store.expire(time.Now().Add(time.Second))
	select {
	case <-body.done:
	case <-time.After(time.Second):
		t.Fatal("body-read deadline did not close a slow connected request body")
	}
	sessions, pending := store.counts()
	if sessions != 1 || pending != 0 {
		t.Fatalf("body deadline removed connected session: (%d, %d)", sessions, pending)
	}
}

func TestListenerRetainedByteBudgetIsGlobal(t *testing.T) {
	store := newSessionStore(8, time.Minute)
	defer store.Close()
	store.maxSessionRetainedBytes = 100
	store.maxListenerRetainedBytes = 150

	first, _ := store.upsert("first", 2)
	second, _ := store.upsert("second", 2)
	firstBytes, err := store.reserveBytes("first", first, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.reserveBytes("second", second, 51); !errors.Is(err, errListenerByteBudget) {
		t.Fatalf("listener overflow error = %v, want global byte budget", err)
	}
	secondBytes, err := store.reserveBytes("second", second, 50)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.retained(); got != 150 {
		t.Fatalf("listener retained %d bytes, want 150", got)
	}
	firstBytes.release()
	secondBytes.release()
	if got := store.retained(); got != 0 {
		t.Fatalf("listener retained %d bytes after release", got)
	}
}

func TestSessionStoreExpiresAndClosesPendingSession(t *testing.T) {
	store := newSessionStore(4, 20*time.Millisecond)
	defer store.Close()
	session, err := store.upsert("pending", 1)
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	for {
		sessions, pending := store.counts()
		if sessions == 0 && pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pending session was not swept: sessions=%d pending=%d", sessions, pending)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := session.uploadQueue.Push(Packet{Payload: []byte("late")}); err == nil {
		t.Fatal("expired session queue remained open")
	}
}

func TestSessionStoreCloseReleasesAllSessions(t *testing.T) {
	store := newSessionStore(8, time.Minute)
	session, err := store.upsert("pending", 1)
	if err != nil {
		t.Fatal(err)
	}
	packet, err := session.uploadQueue.reserve(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.reservePacketBytes("pending", session, packet, 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	sessions, pending := store.counts()
	if sessions != 0 || pending != 0 {
		t.Fatalf("closed store counts = (%d, %d), want zero", sessions, pending)
	}
	if retained := store.retained(); retained != 0 {
		t.Fatalf("closed store retained %d packet bytes", retained)
	}
	if err := session.uploadQueue.Push(Packet{Payload: []byte("late")}); err == nil {
		t.Fatal("listener-close path left a session queue open")
	}
}

func TestSessionIDLengthIsBounded(t *testing.T) {
	if validSessionID(strings.Repeat("x", maxSessionIDBytes+1)) {
		t.Fatal("oversized inbound session ID was accepted")
	}
	cfg := &Config{
		SessionIDLength: &RangeConfig{From: maxSessionIDBytes + 100, To: maxSessionIDBytes + 100},
		SessionIDTable:  "a",
	}
	if id := cfg.GenerateSessionID(); len(id) != maxSessionIDBytes {
		t.Fatalf("generated session ID length = %d, want %d", len(id), maxSessionIDBytes)
	}
}

func TestUploadQueueFullDoesNotBlockHandler(t *testing.T) {
	queue := NewUploadQueue(1)
	defer queue.Close()
	if err := queue.Push(Packet{Payload: []byte("first"), Seq: 0}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := queue.Push(Packet{Payload: []byte("second"), Seq: 1}); err == nil {
		t.Fatal("full queue accepted another payload")
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("full queue blocked for %v", elapsed)
	}
}

func TestUploadQueueCombinedBudgetAndCloseReleasesHighWaterForGC(t *testing.T) {
	queue := NewUploadQueue(4)
	for seq := uint64(1); seq <= 4; seq++ {
		if err := queue.Push(Packet{Payload: make([]byte, 1<<20), Seq: seq}); err != nil {
			t.Fatal(err)
		}
	}
	readDone := make(chan error, 1)
	go func() {
		_, err := queue.Read(make([]byte, 1))
		readDone <- err
	}()
	deadline := time.Now().Add(time.Second)
	for queue.reservationCount() != 4 {
		if time.Now().After(deadline) {
			t.Fatal("queue did not retain the expected combined budget")
		}
		time.Sleep(time.Millisecond)
	}
	if err := queue.Push(Packet{Payload: make([]byte, 1), Seq: 5}); !errors.Is(err, errPacketQueueFull) {
		t.Fatalf("packet beyond combined channel+heap budget error = %v", err)
	}
	if err := queue.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-readDone:
		if err != io.EOF {
			t.Fatalf("blocked read after close = %v, want EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queue close did not unblock the missing-sequence read")
	}
	if queue.heap != nil || queue.pushedPackets != nil || queue.reservations != nil || queue.sequences != nil {
		t.Fatal("queue close retained high-water backing storage")
	}
	if got := queue.reservationCount(); got != 0 {
		t.Fatalf("queue retained %d reservations after close", got)
	}
	runtime.GC()
}

func TestUploadHeapPopZerosReleasedSlot(t *testing.T) {
	heapStorage := uploadHeap{
		{Payload: []byte("first"), Seq: 0},
		{Payload: []byte("second"), Seq: 1},
	}
	backing := heapStorage
	_ = heapStorage.Pop()
	if backing[1].Payload != nil || backing[1].Reader != nil || backing[1].reservation != nil {
		t.Fatal("heap pop retained references in the released slot")
	}
}

func TestUploadQueueConfigurationIsBounded(t *testing.T) {
	if got := (&Config{ScMaxBufferedPosts: -1}).GetNormalizedScMaxBufferedPosts(); got != 30 {
		t.Fatalf("negative queue size normalized to %d, want 30", got)
	}
	if got := (&Config{ScMaxBufferedPosts: 1 << 30}).GetNormalizedScMaxBufferedPosts(); got != maxUploadQueuePackets {
		t.Fatalf("huge queue size normalized to %d, want %d", got, maxUploadQueuePackets)
	}
}

func TestUploadQueueReaderPushWakesBlockedRead(t *testing.T) {
	queue := NewUploadQueue(1)
	defer queue.Close()
	result := make(chan error, 1)
	go func() {
		b := make([]byte, 1)
		n, err := queue.Read(b)
		if err == nil && (n != 1 || b[0] != 'x') {
			err = fmt.Errorf("read = (%d, %q), want (1, x)", n, b[:n])
		}
		result <- err
	}()

	time.Sleep(10 * time.Millisecond)
	reader := &httpServerConn{
		Instance: done.New(),
		Reader:   io.NopCloser(strings.NewReader("x")),
	}
	if err := queue.Push(Packet{Reader: reader}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("reader push did not wake blocked queue read")
	}
}

func TestHTTPServerConnCloseClosesRequestBody(t *testing.T) {
	body := newBlockingReadCloser()
	conn := &httpServerConn{Instance: done.New(), Reader: body}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if got := body.closes.Load(); got != 1 {
		t.Fatalf("request body closed %d times, want 1", got)
	}
}

func TestHTTPServerConnCloseJoinsBlockedResponseWrite(t *testing.T) {
	response := newDeadlineWriteResponse()
	conn := &httpServerConn{
		Instance:       done.New(),
		Reader:         io.NopCloser(strings.NewReader("")),
		ResponseWriter: response,
	}
	writeDone := make(chan error, 1)
	go func() {
		_, err := conn.Write([]byte("x"))
		writeDone <- err
	}()
	select {
	case <-response.entered:
	case <-time.After(time.Second):
		t.Fatal("response write did not block")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- conn.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("httpServerConn Close deadlocked behind a response write")
	}
	select {
	case err := <-writeDone:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("blocked response write error = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("response write outlived httpServerConn Close")
	}
}

func TestStreamUpKeepaliveStopsImmediatelyOnClose(t *testing.T) {
	httpSC := &httpServerConn{
		Instance:       done.New(),
		Reader:         io.NopCloser(strings.NewReader("")),
		ResponseWriter: httptest.NewRecorder(),
	}
	returned := make(chan struct{})
	go func() {
		runStreamUpKeepalive(httpSC, &Config{}, &RangeConfig{From: 3600, To: 3600})
		close(returned)
	}()
	time.Sleep(10 * time.Millisecond)
	if err := httpSC.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("stream-up keepalive retained a sleeping goroutine after close")
	}
}

func TestWaitReadCloserConcurrentSetReadClose(t *testing.T) {
	w := &WaitReadCloser{Wait: make(chan struct{})}
	closers := make([]*blockingReadCloser, 32)
	var wg sync.WaitGroup
	for i := range closers {
		closers[i] = newBlockingReadCloser()
		wg.Add(1)
		go func(rc *blockingReadCloser) {
			defer wg.Done()
			w.Set(rc)
		}(closers[i])
	}
	readDone := make(chan error, 1)
	go func() {
		_, err := w.Read(make([]byte, 1))
		readDone <- err
	}()
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = w.Close()
		}()
	}
	wg.Wait()
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("concurrent WaitReadCloser read did not exit")
	}
	for i, rc := range closers {
		if got := rc.closes.Load(); got != 1 {
			t.Fatalf("candidate closer %d closed %d times, want 1", i, got)
		}
	}
}

func TestPacketBodyConcurrentReadCloseReleasesOwnership(t *testing.T) {
	body := newPacketBody(xbuf.MultiBuffer{xbuf.FromBytes(make([]byte, 1<<20))})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = body.Read(make([]byte, 1024))
		}()
		go func() {
			defer wg.Done()
			_ = body.Close()
		}()
	}
	wg.Wait()
	body.mu.Lock()
	container := body.container
	body.mu.Unlock()
	if container != nil {
		t.Fatal("packet body retained its MultiBuffer container after Close")
	}
}

func TestOutboundSplitCloseCancelsHungResponseHeaders(t *testing.T) {
	started := make(chan struct{})
	handlerDone := make(chan struct{})
	var startOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		startOnce.Do(func() { close(started) })
		<-request.Context().Done()
		close(handlerDone)
	}))
	defer server.Close()

	client := &DefaultDialerClient{
		transportConfig: &Config{},
		client:          server.Client(),
		uploadAll:       make(map[*H1Conn]struct{}),
	}
	lifecycle, cancel := context.WithCancel(context.Background())
	reader, _, _, err := client.OpenStream(lifecycle, server.URL, "session", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("hung request never reached the server")
	}
	conn := &splitConn{reader: reader, writer: new(discardWriteCloser), onClose: cancel}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("split close did not cancel the hung HTTP request")
	}
}

func TestDefaultDialerCloseClosesTrackedRawUploads(t *testing.T) {
	clientSide, serverSide := stdnet.Pipe()
	defer serverSide.Close()
	raw := NewH1Conn(clientSide)
	client := &DefaultDialerClient{
		client:     &http.Client{Transport: &http.Transport{}},
		uploadAll:  map[*H1Conn]struct{}{raw: {}},
		uploadIdle: []*H1Conn{raw},
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	_ = serverSide.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := serverSide.Read(make([]byte, 1)); err == nil {
		t.Fatal("tracked raw upload connection remained open")
	}
}

func TestRawUploadRetriesStalePooledConnectionAndTracksReplacement(t *testing.T) {
	staleClient, staleServer := stdnet.Pipe()
	stale := NewH1Conn(staleClient)
	_ = staleClient.Close()
	_ = staleServer.Close()

	serverDone := make(chan error, 1)
	var dials atomic.Int32
	client := &DefaultDialerClient{
		transportConfig: &Config{},
		client:          &http.Client{Transport: &http.Transport{}},
		httpVersion:     "1.1",
		uploadAll:       map[*H1Conn]struct{}{stale: {}},
		uploadIdle:      []*H1Conn{stale},
		dialUploadConn: func(context.Context) (stdnet.Conn, error) {
			dials.Add(1)
			clientConn, serverConn := stdnet.Pipe()
			go func() {
				defer serverConn.Close()
				request, err := http.ReadRequest(bufio.NewReader(serverConn))
				if err != nil {
					serverDone <- err
					return
				}
				_, _ = io.Copy(io.Discard, request.Body)
				_ = request.Body.Close()
				_, err = io.WriteString(serverConn, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
				serverDone <- err
			}()
			return clientConn, nil
		},
	}
	defer client.Close()

	payload := xbuf.MultiBuffer{xbuf.FromBytes([]byte("payload"))}
	if err := client.PostPacket(context.Background(), "http://example.invalid/upload", "session", "0", payload); err != nil {
		t.Fatal(err)
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("fresh raw upload dials = %d, want 1 after stale pooled write", got)
	}
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement raw upload did not complete")
	}
	client.uploadMu.Lock()
	idle := len(client.uploadIdle)
	tracked := len(client.uploadAll)
	client.uploadMu.Unlock()
	if idle != 1 || tracked != 1 {
		t.Fatalf("replacement raw pool = idle %d tracked %d, want 1/1", idle, tracked)
	}
}

func TestH1ResponseHeaderReaderIsBounded(t *testing.T) {
	clientConn, serverConn := stdnet.Pipe()
	h1 := NewH1Conn(clientConn)
	defer h1.Close()
	defer serverConn.Close()
	writeDone := make(chan error, 1)
	go func() {
		_, err := serverConn.Write(make([]byte, maxH1ResponseHeaderBytes+1))
		writeDone <- err
	}()
	h1.startResponseHeader()
	_, err := io.Copy(io.Discard, h1.RespBufReader)
	h1.finishResponseHeader()
	if !errors.Is(err, errH1ResponseHeaderTooLarge) {
		t.Fatalf("oversized response header error = %v, want bounded-header error", err)
	}
	_ = h1.Close()
	select {
	case <-writeDone:
	case <-time.After(time.Second):
		t.Fatal("bounded header read did not release the server writer")
	}
}
