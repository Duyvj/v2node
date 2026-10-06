package splithttp

import (
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type auditReadCloser struct {
	io.Reader
	closed atomic.Int32
}

func (r *auditReadCloser) Close() error { r.closed.Add(1); return nil }

func TestAuditWaitReadCloserCloseBeforeResponse(t *testing.T) {
	for i := 0; i < 100; i++ {
		w := &WaitReadCloser{Wait: make(chan struct{})}
		r := &auditReadCloser{Reader: strings.NewReader("response")}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); w.Set(r) }()
		go func() { defer wg.Done(); w.Close() }()
		wg.Wait()
		if r.closed.Load() != 1 {
			t.Fatal("late response was retained or closed twice")
		}
		if _, err := w.Read(make([]byte, 1)); err != io.ErrClosedPipe {
			t.Fatal("closed response remained readable", err)
		}
	}
}
