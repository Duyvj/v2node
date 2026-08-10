package inbound

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/buf"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/signal/done"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/transport/internet/stat"
	"github.com/xtls/xray-core/transport/pipe"
)

type blockingUDPInbound struct {
	started  chan struct{}
	readDone chan struct{}
	release  chan struct{}
	done     chan struct{}
	closed   atomic.Bool
}

func (p *blockingUDPInbound) Network() []xnet.Network { return []xnet.Network{xnet.Network_UDP} }

func (p *blockingUDPInbound) Process(_ context.Context, _ xnet.Network, conn stat.Connection, _ routing.Dispatcher) error {
	close(p.started)
	_, err := conn.(*udpConn).ReadMultiBuffer()
	close(p.readDone)
	<-p.release
	close(p.done)
	return err
}

func (p *blockingUDPInbound) Close() error {
	p.closed.Store(true)
	return nil
}

func TestUDPWorkerConnectionMapIsBounded(t *testing.T) {
	if maxUDPWorkerConnections != 1024 {
		t.Fatalf("UDP worker connection cap = %d, want 1024", maxUDPWorkerConnections)
	}
	w := &udpWorker{activeConn: make(map[connID]*udpConn)}
	for i := 0; i < maxUDPWorkerConnections; i++ {
		id := connID{src: xnet.UDPDestination(xnet.LocalHostIP, xnet.Port(i+1))}
		w.activeConn[id] = &udpConn{done: done.New()}
	}

	id := connID{src: xnet.UDPDestination(xnet.LocalHostIP, xnet.Port(maxUDPWorkerConnections+1))}
	conn, _ := w.getConnection(id)
	if conn != nil {
		t.Fatal("worker admitted a UDP flow after reaching its hard limit")
	}
	if got := len(w.activeConn); got != maxUDPWorkerConnections {
		t.Fatalf("active connection count = %d, want %d", got, maxUDPWorkerConnections)
	}
}

func TestUDPWorkerCloseStopsFlowsAndReleasesQueuedPackets(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	proxy := &blockingUDPInbound{
		started:  make(chan struct{}),
		readDone: make(chan struct{}),
		release:  make(chan struct{}),
		done:     make(chan struct{}),
	}
	w := &udpWorker{
		proxy:      proxy,
		activeConn: make(map[connID]*udpConn),
		address:    xnet.LocalHostIP,
	}
	trackedID := connID{src: xnet.UDPDestination(xnet.LocalHostIP, 10001)}
	blockedConn, existing := w.getConnection(trackedID)
	if blockedConn == nil || existing {
		t.Fatalf("failed to create tracked UDP flow: conn=%v existing=%v", blockedConn, existing)
	}
	blockedConn.setCancel(cancel)

	queuedReader, queuedWriter := pipe.New(pipe.DiscardOverflow(), pipe.WithSizeLimit(16*1024))
	queuedConn := &udpConn{
		reader: queuedReader,
		writer: queuedWriter,
		done:   done.New(),
	}
	packet := buf.New()
	_, _ = packet.Write([]byte("queued packet"))
	if err := queuedWriter.WriteMultiBuffer(buf.MultiBuffer{packet}); err != nil {
		t.Fatalf("queue packet: %v", err)
	}

	w.activeConn[connID{src: xnet.UDPDestination(xnet.LocalHostIP, 12345)}] = queuedConn

	go func() {
		defer w.flowWG.Done()
		_ = proxy.Process(ctx, xnet.Network_UDP, blockedConn, nil)
	}()
	select {
	case <-proxy.started:
	case <-time.After(time.Second):
		t.Fatal("UDP process did not start")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- w.Close() }()

	select {
	case <-proxy.readDone:
	case <-time.After(time.Second):
		t.Fatal("worker close did not unblock UDP process")
	}
	select {
	case err := <-closeDone:
		t.Fatalf("worker close returned before tracked flow exited: %v", err)
	default:
	}
	close(proxy.release)
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("close worker: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker close did not join tracked UDP flow")
	}
	select {
	case <-proxy.done:
	default:
		t.Fatal("tracked UDP flow did not exit before worker close returned")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("worker close did not cancel UDP context")
	}
	if !blockedConn.done.Done() || !queuedConn.done.Done() {
		t.Fatal("UDP connection was not marked done")
	}
	if got := queuedWriter.Len(); got != 0 {
		t.Fatalf("queued packet bytes retained after close: %d", got)
	}
	if w.activeConn != nil {
		t.Fatalf("active connection map retained after close: %d", len(w.activeConn))
	}
	if !proxy.closed.Load() {
		t.Fatal("inbound proxy was not closed")
	}
}
