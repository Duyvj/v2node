package dispatcher

import (
	"errors"
	"sync/atomic"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
)

var errDeviceLimit = errors.New("device IP lease denied")

// One gate is shared by both directions of a session. Once its lease is lost,
// the session must not resume when a slot later becomes available.
type deviceSessionGate struct {
	check  func() bool
	denied atomic.Bool
}

func (g *deviceSessionGate) allow() bool {
	if g.denied.Load() {
		return false
	}
	if !g.check() {
		g.denied.Store(true)
		return false
	}
	return !g.denied.Load()
}

type deviceTouchWriter struct {
	writer buf.Writer
	touch  func() bool
}

func (w *deviceTouchWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	if !mb.IsEmpty() && w.touch != nil {
		if !w.touch() {
			buf.ReleaseMulti(mb)
			return errDeviceLimit
		}
	}
	return w.writer.WriteMultiBuffer(mb)
}

func (w *deviceTouchWriter) Close() error {
	return common.Close(w.writer)
}

func (w *deviceTouchWriter) Interrupt() { common.Interrupt(w.writer) }

// DispatchLink receives upload bytes through a Reader, including upload-only
// UDP sessions. Guard that direction as well as the download Writer.
type deviceTouchReader struct {
	reader buf.Reader
	touch  func() bool
}

func (r *deviceTouchReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	mb, err := r.reader.ReadMultiBuffer()
	if !mb.IsEmpty() && r.touch != nil && !r.touch() {
		buf.ReleaseMulti(mb)
		return nil, errDeviceLimit
	}
	return mb, err
}

func (r *deviceTouchReader) Close() error { return common.Close(r.reader) }
func (r *deviceTouchReader) Interrupt()   { common.Interrupt(r.reader) }
