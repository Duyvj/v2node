package grpc

import (
	"context"
	stdnet "net"
	"testing"
	"time"

	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/transport/internet/stat"
	googlegrpc "google.golang.org/grpc"
)

func TestListenerCloseReleasesActiveTunnel(t *testing.T) {
	listener := &Listener{
		ctx:     context.Background(),
		handler: func(stat.Connection) {},
		s:       googlegrpc.NewServer(),
		active:  make(map[xnet.Conn]struct{}),
		closed:  make(chan struct{}),
	}
	serverConn, clientConn := stdnet.Pipe()
	defer clientConn.Close()

	returned := make(chan struct{})
	go func() {
		_ = listener.serveTunnel(context.Background(), serverConn)
		close(returned)
	}()

	deadline := time.Now().Add(time.Second)
	for {
		listener.mu.Lock()
		active := len(listener.active)
		listener.mu.Unlock()
		if active == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("tunnel was not tracked")
		}
		time.Sleep(time.Millisecond)
	}

	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("active tunnel did not return after listener close")
	}
}

func TestListenerAlreadyCanceledContextClosesAndDoesNotInstallStaleStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	listener := &Listener{
		ctx:    ctx,
		s:      googlegrpc.NewServer(),
		active: make(map[xnet.Conn]struct{}),
		closed: make(chan struct{}),
	}
	listener.installStopContext(ctx)
	select {
	case <-listener.closed:
	case <-time.After(time.Second):
		t.Fatal("already-canceled listener context did not close listener")
	}
	listener.mu.Lock()
	stopContext := listener.stopContext
	isClosed := listener.isClosed
	listener.mu.Unlock()
	if !isClosed {
		t.Fatal("listener did not record closed state")
	}
	if stopContext != nil {
		t.Fatal("listener retained a stale stopContext after concurrent close")
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
}
