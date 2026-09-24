package dispatcher

import (
	"io"
	"sync/atomic"

	"github.com/wyx2685/v2node/common/counter"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
)

func (d *DefaultDispatcher) trafficCounter(tag string) *counter.TrafficCounter {
	if v, ok := d.Counter.Load(tag); ok {
		return v.(*counter.TrafficCounter)
	}
	v, _ := d.Counter.LoadOrStore(tag, counter.NewTrafficCounter())
	return v.(*counter.TrafficCounter)
}
func (d *DefaultDispatcher) linkManager(user string) *LinkManager {
	if v, ok := d.LinkManagers.Load(user); ok {
		return v.(*LinkManager)
	}
	v, _ := d.LinkManagers.LoadOrStore(user, &LinkManager{links: make(map[*ManagedWriter]buf.Reader)})
	return v.(*LinkManager)
}

type activityGate struct {
	check  func() bool
	denied atomic.Bool
}

func (g *activityGate) allow() bool {
	if g.denied.Load() {
		return false
	}
	if !g.check() {
		g.denied.Store(true)
		return false
	}
	return !g.denied.Load()
}

type activityWriter struct {
	writer  buf.Writer
	counter *atomic.Int64
	gate    *activityGate
}

func (w *activityWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	if !mb.IsEmpty() && !w.gate.allow() {
		buf.ReleaseMulti(mb)
		return io.ErrClosedPipe
	}
	n := int64(mb.Len())
	err := w.writer.WriteMultiBuffer(mb)
	if err == nil {
		w.counter.Add(n)
	}
	return err
}
func (w *activityWriter) Close() error { return common.Close(w.writer) }
func (w *activityWriter) Interrupt()   { common.Interrupt(w.writer) }

type activityReader struct {
	reader buf.Reader
	gate   *activityGate
}

func (r *activityReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	mb, err := r.reader.ReadMultiBuffer()
	if !mb.IsEmpty() && !r.gate.allow() {
		buf.ReleaseMulti(mb)
		return nil, io.ErrClosedPipe
	}
	return mb, err
}
func (r *activityReader) Interrupt()   { common.Interrupt(r.reader) }
func (r *activityReader) Close() error { return common.Close(r.reader) }
