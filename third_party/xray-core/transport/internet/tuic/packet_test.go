package tuic

import (
	"bytes"
	"context"
	stderrors "errors"
	"io"
	stdnet "net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apernet/quic-go"

	xnet "github.com/xtls/xray-core/common/net"
)

type blockingSendStream struct {
	started     chan struct{}
	startedOnce sync.Once
	released    chan struct{}
	releaseOnce sync.Once
	access      sync.Mutex
	err         error
}

type scriptedUDPPacketTransport struct {
	send  func(context.Context, []byte) error
	open  func(context.Context) (udpSendStream, error)
	close func() error
}

func (t *scriptedUDPPacketTransport) SendDatagram(ctx context.Context, data []byte) error {
	if t.send == nil {
		return io.ErrClosedPipe
	}
	return t.send(ctx, data)
}

func (t *scriptedUDPPacketTransport) OpenUniStream(ctx context.Context) (udpSendStream, error) {
	if t.open == nil {
		return nil, io.ErrClosedPipe
	}
	return t.open(ctx)
}

func (t *scriptedUDPPacketTransport) Close() error {
	if t.close == nil {
		return nil
	}
	return t.close()
}

func (*scriptedUDPPacketTransport) LocalAddr() stdnet.Addr  { return nil }
func (*scriptedUDPPacketTransport) RemoteAddr() stdnet.Addr { return nil }

func newBlockingSendStream() *blockingSendStream {
	return &blockingSendStream{
		started:  make(chan struct{}),
		released: make(chan struct{}),
	}
}

func (s *blockingSendStream) Write(data []byte) (int, error) {
	s.startedOnce.Do(func() { close(s.started) })
	<-s.released
	s.access.Lock()
	err := s.err
	s.access.Unlock()
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

func (*blockingSendStream) Close() error { return nil }

func (s *blockingSendStream) CancelWrite(quic.StreamErrorCode) {
	s.finish(stdnet.ErrClosed)
}

func (s *blockingSendStream) SetWriteDeadline(deadline time.Time) error {
	if !deadline.IsZero() {
		time.AfterFunc(time.Until(deadline), func() {
			s.finish(os.ErrDeadlineExceeded)
		})
	}
	return nil
}

func (s *blockingSendStream) finish(err error) {
	s.releaseOnce.Do(func() {
		s.access.Lock()
		s.err = err
		s.access.Unlock()
		close(s.released)
	})
}

func TestUDPDefraggerPreservesCompletePackets(t *testing.T) {
	defragger := newUDPDefragger()
	defer defragger.close()
	destination := xnet.UDPDestination(xnet.DomainAddress("example.com"), 53)

	if got := defragger.feed(&udpMessage{
		packetID:      7,
		fragmentTotal: 3,
		fragmentID:    2,
		data:          []byte("three"),
	}); got != nil {
		t.Fatal("incomplete packet was returned")
	}
	if got := defragger.feed(&udpMessage{
		packetID:      7,
		fragmentTotal: 3,
		fragmentID:    0,
		destination:   destination,
		data:          []byte("one"),
	}); got != nil {
		t.Fatal("incomplete packet was returned")
	}
	got := defragger.feed(&udpMessage{
		packetID:      7,
		fragmentTotal: 3,
		fragmentID:    1,
		data:          []byte("two"),
	})
	if got == nil {
		t.Fatal("complete packet was not returned")
	}
	if !bytes.Equal(got.data, []byte("onetwothree")) {
		t.Fatalf("reassembled data = %q", got.data)
	}
	if got.destination != destination {
		t.Fatalf("reassembled destination = %v, want %v", got.destination, destination)
	}
	if packets, retainedBytes := defragger.retained(); packets != 0 || retainedBytes != 0 {
		t.Fatalf("completed packet retained: packets=%d bytes=%d", packets, retainedBytes)
	}
}

func TestUDPDefraggerBoundsHundredThousandIncompletePackets(t *testing.T) {
	const (
		packetLimit = 8
		byteLimit   = 64
	)
	defragger := newUDPDefraggerWithLimits(time.Hour, packetLimit, byteLimit)
	defer defragger.close()

	for index := 0; index < 100_000; index++ {
		defragger.feed(&udpMessage{
			packetID:      uint16(index),
			fragmentTotal: 2,
			fragmentID:    0,
			data:          []byte{byte(index)},
		})
	}
	packets, retainedBytes := defragger.retained()
	if packets > packetLimit {
		t.Fatalf("retained packets = %d, limit %d", packets, packetLimit)
	}
	if retainedBytes > byteLimit {
		t.Fatalf("retained bytes = %d, limit %d", retainedBytes, byteLimit)
	}
}

func TestUDPDefraggerRejectsOversizedReassembledPacket(t *testing.T) {
	defragger := newUDPDefragger()
	defer defragger.close()
	if got := defragger.feed(&udpMessage{
		packetID:      1,
		fragmentTotal: 2,
		fragmentID:    0,
		data:          make([]byte, 40_000),
	}); got != nil {
		t.Fatal("incomplete packet was returned")
	}
	if got := defragger.feed(&udpMessage{
		packetID:      1,
		fragmentTotal: 2,
		fragmentID:    1,
		data:          make([]byte, 40_000),
	}); got != nil {
		t.Fatal("oversized reassembled packet was returned")
	}
	if packets, retainedBytes := defragger.retained(); packets != 0 || retainedBytes != 0 {
		t.Fatalf("oversized packet retained: packets=%d bytes=%d", packets, retainedBytes)
	}
}

func TestUDPDefraggerAcceptsFullUint8FragmentRange(t *testing.T) {
	defragger := newUDPDefragger()
	defer defragger.close()
	var assembled *udpMessage
	for fragmentID := 0; fragmentID < maxUDPFragments; fragmentID++ {
		assembled = defragger.feed(&udpMessage{
			packetID:      1,
			fragmentTotal: maxUDPFragments,
			fragmentID:    uint8(fragmentID),
			data:          []byte{byte(fragmentID)},
		})
	}
	if assembled == nil {
		t.Fatal("255-fragment TUIC packet was not reassembled")
	}
	if got := len(assembled.data); got != maxUDPFragments {
		t.Fatalf("reassembled payload size = %d, want %d", got, maxUDPFragments)
	}
	if packets, retainedBytes := defragger.retained(); packets != 0 || retainedBytes != 0 {
		t.Fatalf("completed fragment table retained: packets=%d bytes=%d", packets, retainedBytes)
	}
}

func TestUDPDefraggerExpiresIncompletePackets(t *testing.T) {
	defragger := newUDPDefraggerWithLimits(15*time.Millisecond, 8, 1024)
	defer defragger.close()
	defragger.feed(&udpMessage{
		packetID:      1,
		fragmentTotal: 2,
		fragmentID:    0,
		data:          []byte("payload"),
	})

	deadline := time.Now().Add(time.Second)
	for {
		packets, retainedBytes := defragger.retained()
		if packets == 0 && retainedBytes == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("incomplete packet did not expire: packets=%d bytes=%d", packets, retainedBytes)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestUDPPacketConnIdleTimeoutAndCloseReleaseState(t *testing.T) {
	var destroyed atomic.Int32
	conn := newUDPPacketConn(context.Background(), nil, false, true, nil, 15*time.Millisecond, func() {
		destroyed.Add(1)
	})
	conn.defragger.feed(&udpMessage{
		packetID:      1,
		fragmentTotal: 2,
		fragmentID:    0,
		data:          []byte("payload"),
	})

	deadline := time.Now().Add(time.Second)
	for !conn.done() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !conn.done() {
		t.Fatal("UDP session did not observe idle timeout")
	}
	for destroyed.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := destroyed.Load(); got != 1 {
		t.Fatalf("destroy callback count = %d, want 1", got)
	}
	if packets, retainedBytes := conn.defragger.retained(); packets != 0 || retainedBytes != 0 {
		t.Fatalf("closed UDP session retained defrag state: packets=%d bytes=%d", packets, retainedBytes)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	conn.readDeadline.access.Lock()
	deadlineTimer := conn.readDeadline.timer
	conn.readDeadline.access.Unlock()
	if deadlineTimer != nil {
		t.Fatal("closed UDP session recreated a deadline timer")
	}
	if got := destroyed.Load(); got != 1 {
		t.Fatalf("destroy callback count after repeated close = %d, want 1", got)
	}
}

func TestUDPPacketConnActivityExtendsIdleTimeout(t *testing.T) {
	const timeout = 80 * time.Millisecond
	conn := newUDPPacketConn(context.Background(), nil, false, true, nil, timeout, nil)
	defer conn.Close()

	time.Sleep(50 * time.Millisecond)
	conn.touch()
	time.Sleep(50 * time.Millisecond)
	if conn.done() {
		t.Fatal("active UDP session expired before the refreshed timeout")
	}

	deadline := time.Now().Add(time.Second)
	for !conn.done() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !conn.done() {
		t.Fatal("UDP session did not expire after activity stopped")
	}
}

func TestUDPPacketConnConcurrentCloseIsIdempotent(t *testing.T) {
	for round := 0; round < 100; round++ {
		var destroyed atomic.Int32
		conn := newUDPPacketConn(context.Background(), nil, false, true, nil, time.Hour, func() {
			destroyed.Add(1)
		})
		conn.defragger.feed(&udpMessage{
			packetID:      1,
			fragmentTotal: 2,
			fragmentID:    0,
			data:          []byte("payload"),
		})

		var workers sync.WaitGroup
		for index := 0; index < 32; index++ {
			workers.Add(1)
			go func(index int) {
				defer workers.Done()
				if index%2 == 0 {
					_ = conn.Close()
					return
				}
				conn.closeWithError(io.ErrClosedPipe)
			}(index)
		}
		workers.Wait()

		if got := destroyed.Load(); got != 1 {
			t.Fatalf("round %d: destroy callback count = %d, want 1", round, got)
		}
		if packets, retainedBytes := conn.defragger.retained(); packets != 0 || retainedBytes != 0 {
			t.Fatalf("round %d: closed connection retained packets=%d bytes=%d", round, packets, retainedBytes)
		}
	}
}

func TestUDPPacketConnQueuedByteReservationsReleaseOnReadAndClose(t *testing.T) {
	budget := newUDPQueueBudget(128)
	conn := newUDPPacketConn(context.Background(), nil, false, true, nil, time.Hour, nil)

	for index := 0; index < cap(conn.data); index++ {
		message := &udpMessage{data: []byte{byte(index)}, queueBudget: budget, queuedBytes: 1}
		if !budget.reserve(1) || !conn.inputPacket(message) {
			t.Fatalf("failed to queue packet %d", index)
		}
	}
	activity := conn.lastActivity.Load()
	time.Sleep(time.Millisecond)
	rejected := &udpMessage{data: []byte("x"), queueBudget: budget, queuedBytes: 1}
	if !budget.reserve(1) {
		t.Fatal("failed to reserve rejected packet")
	}
	if conn.inputPacket(rejected) {
		t.Fatal("packet entered a full per-session queue")
	}
	if got := conn.lastActivity.Load(); got != activity {
		t.Fatal("rejected packet refreshed UDP idle lifetime")
	}
	if got := budget.used.Load(); got != int64(cap(conn.data)) {
		t.Fatalf("queued bytes = %d, want %d", got, cap(conn.data))
	}
	if _, _, err := conn.ReadPacket(); err != nil {
		t.Fatal(err)
	}
	if got := budget.used.Load(); got != int64(cap(conn.data)-1) {
		t.Fatalf("queued bytes after read = %d", got)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if got := budget.used.Load(); got != 0 {
		t.Fatalf("queued bytes after close = %d", got)
	}
}

func TestUDPDefraggerCloseReleasesQueueReservations(t *testing.T) {
	budget := newUDPQueueBudget(32)
	if !budget.reserve(8) {
		t.Fatal("failed to reserve fragment")
	}
	defragger := newUDPDefragger()
	defragger.feed(&udpMessage{
		packetID:      1,
		fragmentTotal: 2,
		data:          make([]byte, 8),
		queueBudget:   budget,
		queuedBytes:   8,
	})
	defragger.close()
	if got := budget.used.Load(); got != 0 {
		t.Fatalf("defragmenter retained %d reserved bytes after close", got)
	}
}

func TestUDPPacketConnConcurrentMTUUpdates(t *testing.T) {
	transport := &scriptedUDPPacketTransport{
		send: func(context.Context, []byte) error {
			return &quic.DatagramTooLargeError{MaxDatagramPayloadSize: 1000}
		},
	}
	conn := newUDPPacketConnWithTransport(context.Background(), transport, false, true, nil, time.Hour, nil)
	defer conn.Close()

	var workers sync.WaitGroup
	for range 32 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			_ = conn.WritePacket(make([]byte, 1400), testUDPDestination())
		}()
	}
	workers.Wait()
	if got := conn.udpMTU.Load(); got != 997 {
		t.Fatalf("UDP MTU = %d, want 997", got)
	}
}

func TestUDPPacketConnBlockedDatagramWriteStopsWithoutClosingSharedTransport(t *testing.T) {
	started := make(chan struct{})
	var aborted atomic.Int32
	transport := &scriptedUDPPacketTransport{
		send: func(ctx context.Context, _ []byte) error {
			close(started)
			<-ctx.Done()
			return context.Cause(ctx)
		},
		close: func() error {
			aborted.Add(1)
			return nil
		},
	}
	conn := newUDPPacketConnWithTransport(context.Background(), transport, false, true, nil, time.Hour, nil)

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- conn.WritePacket([]byte("payload"), testUDPDestination())
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("datagram send did not block")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- conn.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not join the blocked datagram write")
	}
	select {
	case err := <-writeDone:
		if err == nil {
			t.Fatal("blocked datagram write unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("datagram writer survived Close")
	}
	if got := aborted.Load(); got != 0 {
		t.Fatalf("logical session close aborted shared transport %d time(s)", got)
	}
	if conn.activeStreams != nil {
		t.Fatal("closed datagram connection retained write state")
	}
}

func TestUDPPacketConnBlockedDatagramWriteHonorsDeadline(t *testing.T) {
	started := make(chan struct{})
	transport := &scriptedUDPPacketTransport{
		send: func(ctx context.Context, _ []byte) error {
			close(started)
			<-ctx.Done()
			return context.Cause(ctx)
		},
	}
	conn := newUDPPacketConnWithTransport(context.Background(), transport, false, true, nil, time.Hour, nil)

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- conn.WritePacket([]byte("payload"), testUDPDestination())
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("datagram send did not block")
	}
	if err := conn.SetWriteDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-writeDone:
		if !stderrors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("write error = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked datagram write ignored its deadline")
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestUDPPacketConnBlockedUniStreamWriteStopsAndJoinsOnClose(t *testing.T) {
	stream := newBlockingSendStream()
	transport := &scriptedUDPPacketTransport{
		open: func(context.Context) (udpSendStream, error) {
			return stream, nil
		},
	}
	conn := newUDPPacketConnWithTransport(context.Background(), transport, true, true, nil, time.Hour, nil)

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- conn.WritePacket([]byte("payload"), testUDPDestination())
	}()
	select {
	case <-stream.started:
	case <-time.After(time.Second):
		t.Fatal("uni-stream send did not block")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- conn.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not join the blocked uni-stream write")
	}
	select {
	case err := <-writeDone:
		if err == nil {
			t.Fatal("blocked uni-stream write unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("uni-stream writer survived Close")
	}
	if conn.activeStreams != nil {
		t.Fatal("closed uni-stream connection retained write state")
	}
}

func TestUDPPacketConnBlockedUniStreamWriteHonorsUpdatedDeadline(t *testing.T) {
	stream := newBlockingSendStream()
	transport := &scriptedUDPPacketTransport{
		open: func(context.Context) (udpSendStream, error) {
			return stream, nil
		},
	}
	conn := newUDPPacketConnWithTransport(context.Background(), transport, true, true, nil, time.Hour, nil)

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- conn.WritePacket([]byte("payload"), testUDPDestination())
	}()
	select {
	case <-stream.started:
	case <-time.After(time.Second):
		t.Fatal("uni-stream send did not block")
	}
	if err := conn.SetWriteDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-writeDone:
		if !stderrors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("write error = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked uni-stream write ignored the updated deadline")
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
}

func testUDPDestination() xnet.Destination {
	return xnet.UDPDestination(xnet.DomainAddress("example.com"), 53)
}
