package encoding

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

type blockingHunkStream struct {
	ctx        context.Context
	closeCalls atomic.Int32
	entered    chan struct{}
	enterOnce  atomic.Bool
}

func (s *blockingHunkStream) Context() context.Context  { return s.ctx }
func (s *blockingHunkStream) Send(*Hunk) error          { return nil }
func (s *blockingHunkStream) SendMsg(interface{}) error { return nil }
func (s *blockingHunkStream) RecvMsg(interface{}) error { return nil }
func (s *blockingHunkStream) Recv() (*Hunk, error) {
	if s.entered != nil && s.enterOnce.CompareAndSwap(false, true) {
		close(s.entered)
	}
	<-s.ctx.Done()
	return nil, s.ctx.Err()
}

func TestHunkConnectionCloseCancelsBlockedRecv(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stream := &blockingHunkStream{ctx: ctx, entered: make(chan struct{})}
	conn := NewHunkConn(stream, cancel, nil)
	readDone := make(chan error, 1)
	go func() {
		_, err := conn.Read(make([]byte, 1))
		readDone <- err
	}()
	select {
	case <-stream.entered:
	case <-time.After(time.Second):
		t.Fatal("read did not enter blocked Recv")
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel blocked Recv")
	}
	if got := stream.closeCalls.Load(); got != 1 {
		t.Fatalf("CloseSend called %d times, want 1", got)
	}
}
func (s *blockingHunkStream) CloseSend() error {
	s.closeCalls.Add(1)
	return nil
}

type blockingMultiHunkStream struct {
	ctx        context.Context
	closeCalls atomic.Int32
	entered    chan struct{}
	enterOnce  atomic.Bool
}

func (s *blockingMultiHunkStream) Context() context.Context  { return s.ctx }
func (s *blockingMultiHunkStream) Send(*MultiHunk) error     { return nil }
func (s *blockingMultiHunkStream) SendMsg(interface{}) error { return nil }
func (s *blockingMultiHunkStream) RecvMsg(interface{}) error { return nil }
func (s *blockingMultiHunkStream) Recv() (*MultiHunk, error) {
	if s.entered != nil && s.enterOnce.CompareAndSwap(false, true) {
		close(s.entered)
	}
	<-s.ctx.Done()
	return nil, s.ctx.Err()
}

func TestMultiHunkConnectionCloseCancelsBlockedRecv(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stream := &blockingMultiHunkStream{ctx: ctx, entered: make(chan struct{})}
	conn := NewMultiHunkConn(stream, cancel, nil)
	readDone := make(chan error, 1)
	go func() {
		_, err := conn.Read(make([]byte, 1))
		readDone <- err
	}()
	select {
	case <-stream.entered:
	case <-time.After(time.Second):
		t.Fatal("read did not enter blocked multi Recv")
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel blocked multi Recv")
	}
	if got := stream.closeCalls.Load(); got != 1 {
		t.Fatalf("CloseSend called %d times, want 1", got)
	}
}
func (s *blockingMultiHunkStream) CloseSend() error {
	s.closeCalls.Add(1)
	return nil
}

func TestHunkConnectionReadDeadlineCancelsStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stream := &blockingHunkStream{ctx: ctx}
	conn := NewHunkConn(stream, cancel, nil)

	if err := conn.SetReadDeadline(time.Now().Add(25 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	_, err := conn.Read(make([]byte, 1))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Read error = %v, want deadline exceeded", err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("deadline did not cancel the gRPC stream")
	}

	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if got := stream.closeCalls.Load(); got != 1 {
		t.Fatalf("CloseSend called %d times, want 1", got)
	}
}

func TestMultiHunkCloseIsIdempotent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stream := &blockingMultiHunkStream{ctx: ctx}
	rw := NewMultiHunkReadWriter(stream, cancel)
	rw.buf = [][]byte{make([]byte, 1<<20)}

	if err := rw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rw.Close(); err != nil {
		t.Fatal(err)
	}
	if got := stream.closeCalls.Load(); got != 1 {
		t.Fatalf("CloseSend called %d times, want 1", got)
	}
	if rw.buf != nil {
		t.Fatal("Close retained multi-hunk receive buffers")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel the multi-hunk stream")
	}
}
