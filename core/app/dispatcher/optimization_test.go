package dispatcher

import (
	"github.com/xtls/xray-core/common/buf"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type finalReader struct{ timeout time.Duration }

func (r *finalReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	return buf.MultiBuffer{buf.FromBytes([]byte("hello"))}, io.EOF
}
func (r *finalReader) ReadMultiBufferTimeout(d time.Duration) (buf.MultiBuffer, error) {
	r.timeout = d
	return r.ReadMultiBuffer()
}
func TestCountReaderKeepsFinalPayloadAndTimeout(t *testing.T) {
	r := &finalReader{}
	var n atomic.Int64
	c := &CounterReader{Reader: r, Counter: &n}
	mb, err := c.ReadMultiBufferTimeout(37 * time.Millisecond)
	defer buf.ReleaseMulti(mb)
	if err != io.EOF || mb.Len() != 5 || n.Load() != 5 || r.timeout != 37*time.Millisecond {
		t.Fatal("lost payload, count or timeout")
	}
}
func TestActivityReaderRejectsOldSession(t *testing.T) {
	g := &activityGate{check: func() bool { return false }}
	r := &activityReader{reader: &finalReader{}, gate: g}
	mb, err := r.ReadMultiBuffer()
	defer buf.ReleaseMulti(mb)
	if !mb.IsEmpty() || err != io.ErrClosedPipe {
		t.Fatal("denied upload leaked")
	}
	g.check = func() bool { return true }
	if g.allow() {
		t.Fatal("denied session resurrected")
	}
}
func TestConcurrentInitializationUsesSharedState(t *testing.T) {
	d := &DefaultDispatcher{}
	var wg sync.WaitGroup
	first := d.trafficCounter("node")
	manager := d.linkManager("user")
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d.trafficCounter("node") != first || d.linkManager("user") != manager {
				t.Error("state detached")
			}
		}()
	}
	wg.Wait()
}
