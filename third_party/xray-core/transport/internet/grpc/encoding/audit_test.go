package encoding

import (
	"context"
	"io"
	"testing"
	"time"
)

type auditBlockingHunk struct {
	ctx     context.Context
	sending chan struct{}
}

func (h *auditBlockingHunk) Context() context.Context { return h.ctx }
func (h *auditBlockingHunk) Send(*Hunk) error         { close(h.sending); <-h.ctx.Done(); return h.ctx.Err() }
func (h *auditBlockingHunk) Recv() (*Hunk, error)     { return nil, io.EOF }
func (h *auditBlockingHunk) SendMsg(any) error        { return nil }
func (h *auditBlockingHunk) RecvMsg(any) error        { return io.EOF }
func (h *auditBlockingHunk) CloseSend() error         { return nil }

func TestAuditCloseCancelsBlockedSend(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h := &auditBlockingHunk{ctx: ctx, sending: make(chan struct{})}
	w := NewHunkReadWriter(h, cancel)
	done := make(chan struct{})
	go func() { w.Write([]byte("payload")); close(done) }()
	<-h.sending
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("gRPC send retained after close")
	}
	if _, err := w.Write([]byte("late")); err != io.ErrClosedPipe {
		t.Fatal("closed stream accepted a write", err)
	}
}
