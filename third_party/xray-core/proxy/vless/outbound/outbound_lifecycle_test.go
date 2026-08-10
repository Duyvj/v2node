package outbound

import (
	"context"
	stdnet "net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appreverse "github.com/xtls/xray-core/app/reverse"
	"github.com/xtls/xray-core/common/mux"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/common/task"
	"github.com/xtls/xray-core/transport"
	"github.com/xtls/xray-core/transport/internet/stat"
	"github.com/xtls/xray-core/transport/pipe"
)

type lifecycleDialer struct {
	peers chan stdnet.Conn
	dials atomic.Int32
}

func (d *lifecycleDialer) Dial(context.Context, xnet.Destination) (stat.Connection, error) {
	client, peer := stdnet.Pipe()
	d.dials.Add(1)
	d.peers <- peer
	return client, nil
}

func (*lifecycleDialer) DestIpAddress() xnet.IP { return nil }

func (*lifecycleDialer) SetOutboundGateway(context.Context, *session.Outbound) {}

func TestHandlerClosePreventsDelayedReverseStart(t *testing.T) {
	var calls atomic.Int32
	handler := &Handler{
		reverse: &Reverse{
			monitorTask: &task.Periodic{
				Interval: time.Hour,
				Execute: func() error {
					calls.Add(1)
					return nil
				},
			},
		},
	}

	handler.scheduleReverseStart(time.Hour)
	if err := handler.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate a timer callback that was already queued when Close stopped it.
	handler.startReverse()
	if got := calls.Load(); got != 0 {
		t.Fatalf("reverse monitor executed %d times after Handler.Close, want 0", got)
	}

	handler.reverseStartAccess.Lock()
	defer handler.reverseStartAccess.Unlock()
	if !handler.closed {
		t.Fatal("handler was not marked closed")
	}
	if handler.reverseStartTimer != nil {
		t.Fatal("delayed reverse start timer was retained after close")
	}
}

func TestHandlerCloseCancelsPreconnectWorkers(t *testing.T) {
	dialer := &lifecycleDialer{peers: make(chan stdnet.Conn, 4)}
	handler := &Handler{testpre: 1}
	handler.startPreconnections(dialer, xnet.TCPDestination(xnet.LocalHostIP, 443))

	var peer stdnet.Conn
	select {
	case peer = <-dialer.peers:
	case <-time.After(time.Second):
		t.Fatal("preconnect worker did not dial")
	}
	defer peer.Close()

	const closers = 8
	var wg sync.WaitGroup
	wg.Add(closers)
	errs := make(chan error, closers)
	for range closers {
		go func() {
			defer wg.Done()
			errs <- handler.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Handler.Close failed: %v", err)
		}
	}

	select {
	case <-handler.preDone:
	default:
		t.Fatal("Handler.Close did not cancel the preconnect context")
	}
	if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err == nil {
		if _, err := peer.Read(make([]byte, 1)); err == nil {
			t.Fatal("preconnect connection remained open after Handler.Close")
		}
	}

	handler.startPreconnections(dialer, xnet.TCPDestination(xnet.LocalHostIP, 443))
	time.Sleep(20 * time.Millisecond)
	if got := dialer.dials.Load(); got != 1 {
		t.Fatalf("closed handler started more preconnect workers: dials=%d", got)
	}
}

func TestReverseCloseReleasesWorkers(t *testing.T) {
	serverReader, peerWriter := pipe.New(pipe.WithSizeLimit(1024))
	peerReader, serverWriter := pipe.New(pipe.WithSizeLimit(1024))
	worker, err := mux.NewServerWorker(context.Background(), nil, &transport.Link{
		Reader: serverReader,
		Writer: serverWriter,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer peerWriter.Interrupt()
	defer peerReader.Interrupt()

	reverse := &Reverse{
		workers: []*appreverse.BridgeWorker{{Worker: worker}},
		monitorTask: &task.Periodic{
			Interval: time.Hour,
			Execute:  func() error { return nil },
		},
	}
	if err := reverse.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reverse.Close(); err != nil {
		t.Fatalf("second Reverse.Close failed: %v", err)
	}
	if !reverse.closed || reverse.workers != nil {
		t.Fatalf("closed reverse retained workers: closed=%v workers=%d", reverse.closed, len(reverse.workers))
	}
	select {
	case <-worker.WaitClosed():
	case <-time.After(time.Second):
		t.Fatal("reverse mux worker did not close")
	}
	if err := reverse.Start(); err == nil {
		t.Fatal("closed reverse monitor restarted")
	}
}
