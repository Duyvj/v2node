package cnc

import (
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/buf"
)

type blockingReader struct {
	done      chan struct{}
	closeOnce sync.Once
}

func newBlockingReader() *blockingReader {
	return &blockingReader{done: make(chan struct{})}
}

func (r *blockingReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	<-r.done
	return nil, io.EOF
}

func (r *blockingReader) Close() error {
	r.closeOnce.Do(func() { close(r.done) })
	return nil
}

type countingCloser struct {
	calls atomic.Int32
}

type blockingWriter struct {
	done      chan struct{}
	entered   chan struct{}
	closeOnce sync.Once
}

func newBlockingWriter() *blockingWriter {
	return &blockingWriter{done: make(chan struct{}), entered: make(chan struct{})}
}

func (w *blockingWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	buf.ReleaseMulti(mb)
	select {
	case <-w.entered:
	default:
		close(w.entered)
	}
	<-w.done
	return io.ErrClosedPipe
}

func (w *blockingWriter) Close() error {
	w.closeOnce.Do(func() { close(w.done) })
	return nil
}

func (c *countingCloser) Close() error {
	c.calls.Add(1)
	return nil
}

func TestReadDeadlineInterruptsBlockedReader(t *testing.T) {
	reader := newBlockingReader()
	onClose := new(countingCloser)
	conn := NewConnection(
		ConnectionOutputMulti(reader),
		ConnectionInputMulti(buf.Discard),
		ConnectionOnClose(onClose),
	)

	if err := conn.SetReadDeadline(time.Now().Add(25 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err := conn.Read(make([]byte, 1))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Read error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("deadline took too long: %v", elapsed)
	}

	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if got := onClose.calls.Load(); got != 1 {
		t.Fatalf("onClose called %d times, want 1", got)
	}
}

func TestTerminalReadDeadlineUnblocksPeerWriterAndClose(t *testing.T) {
	reader := newBlockingReader()
	writer := newBlockingWriter()
	conn := NewConnection(
		ConnectionOutputMulti(reader),
		ConnectionInputMulti(writer),
	)
	writeDone := make(chan error, 1)
	go func() {
		_, err := conn.Write([]byte("blocked"))
		writeDone <- err
	}()
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("write did not block in the peer direction")
	}
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
			t.Fatal("peer writer unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("peer writer deadlocked after terminal deadline")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- conn.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close deadlocked after terminal deadline")
	}
}

func TestSetDeadlineMarksBothDirections(t *testing.T) {
	conn := NewConnection(
		ConnectionOutputMulti(newBlockingReader()),
		ConnectionInputMulti(newBlockingWriter()),
	)
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

func TestCloseReleasesBufferedReadData(t *testing.T) {
	conn := NewConnection(
		ConnectionOutputMulti(newBlockingReader()),
		ConnectionInputMulti(buf.Discard),
	).(*Connection)
	conn.reader.Buffer = buf.MultiBuffer{buf.FromBytes(make([]byte, 1<<20))}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if conn.reader.Buffer != nil {
		t.Fatal("Close retained BufferedReader data")
	}
	if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Read after Close = %v, want closed pipe", err)
	}
}

func TestClearedReadDeadlineDoesNotCloseConnection(t *testing.T) {
	reader := newBlockingReader()
	conn := NewConnection(
		ConnectionOutputMulti(reader),
		ConnectionInputMulti(buf.Discard),
	)
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
		t.Fatal("cleared deadline closed the reader")
	default:
	}
}
