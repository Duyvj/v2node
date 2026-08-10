package browser_dialer

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestDialGetContextCancelsWhileWaitingForBrowser(t *testing.T) {
	mu.Lock()
	previous := conns
	conns = make(chan *websocket.Conn)
	mu.Unlock()
	defer func() {
		mu.Lock()
		conns = previous
		mu.Unlock()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := make(chan error, 1)
	go func() {
		_, err := DialGetContext(ctx, "https://example.invalid/", http.Header{}, nil)
		result <- err
	}()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("DialGetContext error = %v, want canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled browser dial remained blocked waiting for a connection")
	}
}
