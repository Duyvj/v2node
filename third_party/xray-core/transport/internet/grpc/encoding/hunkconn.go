package encoding

import (
	"context"
	"io"
	"sync"

	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/net/cnc"
	"github.com/xtls/xray-core/common/signal/done"
)

type HunkConn interface {
	Context() context.Context
	Send(*Hunk) error
	Recv() (*Hunk, error)
	SendMsg(m interface{}) error
	RecvMsg(m interface{}) error
}

type StreamCloser interface {
	CloseSend() error
}

type HunkReaderWriter struct {
	hc     HunkConn
	cancel context.CancelFunc
	done   *done.Instance

	closeOnce sync.Once
	closeErr  error

	readMu sync.Mutex
	buf    []byte
	index  int
}

func NewHunkReadWriter(hc HunkConn, cancel context.CancelFunc) *HunkReaderWriter {
	return &HunkReaderWriter{
		hc:     hc,
		cancel: cancel,
		done:   done.New(),
	}
}

func NewHunkConn(hc HunkConn, cancel context.CancelFunc, trustedXForwardedFor []string) net.Conn {
	rAddr := remoteAddrFromContext(hc.Context(), trustedXForwardedFor)
	wrc := NewHunkReadWriter(hc, cancel)
	return cnc.NewConnection(
		cnc.ConnectionInput(wrc),
		cnc.ConnectionOutput(wrc),
		cnc.ConnectionOnClose(wrc),
		cnc.ConnectionRemoteAddr(rAddr),
	)
}

func (h *HunkReaderWriter) forceFetch() error {
	for {
		if h.done.Done() {
			return io.EOF
		}
		hunk, err := h.hc.Recv()
		if err != nil {
			if err == io.EOF {
				return err
			}

			return errors.New("failed to fetch hunk from gRPC tunnel").Base(err)
		}
		if hunk == nil || len(hunk.Data) == 0 {
			continue
		}

		h.buf = hunk.Data
		h.index = 0

		return nil
	}
}

func (h *HunkReaderWriter) Read(buf []byte) (int, error) {
	h.readMu.Lock()
	defer h.readMu.Unlock()
	if h.done.Done() {
		return 0, io.EOF
	}

	if h.index >= len(h.buf) {
		if err := h.forceFetch(); err != nil {
			return 0, err
		}
	}
	n := copy(buf, h.buf[h.index:])
	h.index += n
	if h.index == len(h.buf) {
		h.buf = nil
		h.index = 0
	}

	return n, nil
}

func (h *HunkReaderWriter) ReadMultiBuffer() (buf.MultiBuffer, error) {
	h.readMu.Lock()
	defer h.readMu.Unlock()
	if h.done.Done() {
		return nil, io.EOF
	}
	if h.index >= len(h.buf) {
		if err := h.forceFetch(); err != nil {
			return nil, err
		}
	}

	b := h.buf[h.index:]
	h.buf = nil
	h.index = 0
	if cap(b) >= buf.Size {
		return buf.MultiBuffer{buf.NewExisted(b)}, nil
	}

	nb := buf.New()
	nb.Extend(int32(len(b)))
	copy(nb.Bytes(), b)
	return buf.MultiBuffer{nb}, nil
}

func (h *HunkReaderWriter) Write(buf []byte) (int, error) {
	if h.done.Done() {
		return 0, io.ErrClosedPipe
	}

	err := h.hc.Send(&Hunk{Data: buf[:]})
	if err != nil {
		return 0, errors.New("failed to send data over gRPC tunnel").Base(err)
	}
	return len(buf), nil
}

func (h *HunkReaderWriter) Close() error {
	h.closeOnce.Do(func() {
		_ = h.done.Close()
		if h.cancel != nil {
			h.cancel()
		}
		if sc, match := h.hc.(StreamCloser); match {
			h.closeErr = sc.CloseSend()
		}
		h.readMu.Lock()
		h.buf = nil
		h.index = 0
		h.readMu.Unlock()
	})
	return h.closeErr
}
