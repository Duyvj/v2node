package singbridge

import (
	"context"
	stdnet "net"
	"sync"
	"time"

	B "github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/bufio"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/signal"
	"github.com/xtls/xray-core/transport"
)

func CopyPacketConn(ctx context.Context, inboundConn net.Conn, link *transport.Link, destination net.Destination, serverConn net.PacketConn) error {
	cancel := func() {
		common.Interrupt(link.Reader)
		common.Interrupt(serverConn)
	}
	conn := &PacketConnWrapper{
		Reader: link.Reader,
		Writer: link.Writer,
		Dest:   destination,
		Conn:   inboundConn,
		T:      signal.CancelAfterInactivity(ctx, cancel, 300*time.Second),
	}
	return ReturnError(bufio.CopyPacketConn(ctx, conn, bufio.NewPacketConn(serverConn)))
}

type PacketConnWrapper struct {
	buf.Reader
	buf.Writer
	net.Conn
	Dest   net.Destination
	cached buf.MultiBuffer
	readMu sync.Mutex
	closed bool

	// A simple patch to avoid goroutine leak since sing infra cannot awake read block by write err
	T *signal.ActivityTimer
}

func (w *PacketConnWrapper) ReadPacket(buffer *B.Buffer) (addr M.Socksaddr, err error) {
	w.readMu.Lock()
	defer w.readMu.Unlock()
	if w.closed {
		return M.Socksaddr{}, stdnet.ErrClosed
	}
	w.T.Update()
	defer func() {
		if err != nil {
			// uplinkonly
			w.T.SetTimeout(2 * time.Second)
		}
	}()
	if w.cached != nil {
		mb, bb := buf.SplitFirst(w.cached)
		if bb == nil {
			w.cached = nil
		} else {
			buffer.Write(bb.Bytes())
			w.cached = mb
			var destination net.Destination
			if bb.UDP != nil {
				destination = *bb.UDP
			} else {
				destination = w.Dest
			}
			bb.Release()
			return ToSocksaddr(destination), nil
		}
	}
	mb, err := w.ReadMultiBuffer()
	nb, bb := buf.SplitFirst(mb)
	if bb == nil {
		return M.Socksaddr{}, err
	} else {
		buffer.Write(bb.Bytes())
		w.cached = nb
		var destination net.Destination
		if bb.UDP != nil {
			destination = *bb.UDP
		} else {
			destination = w.Dest
		}
		bb.Release()
		return ToSocksaddr(destination), nil
	}
}

func (w *PacketConnWrapper) WritePacket(buffer *B.Buffer, destination M.Socksaddr) (err error) {
	w.T.Update()
	defer func() {
		if err != nil {
			// downlinkonly
			w.T.SetTimeout(5 * time.Second)
		}
	}()
	endpoint, err := ToDestination(destination, net.Network_UDP)
	if err != nil {
		return err
	}
	vBuf := buf.New()
	vBuf.Write(buffer.Bytes())
	vBuf.UDP = &endpoint
	return w.WriteMultiBuffer(buf.MultiBuffer{vBuf})
}

func (w *PacketConnWrapper) Close() error {
	// Wake a blocked read before waiting for its cache lock.
	common.Interrupt(w.Reader)
	common.Interrupt(w.Writer)
	w.readMu.Lock()
	defer w.readMu.Unlock()
	w.closed = true
	buf.ReleaseMulti(w.cached)
	w.cached = nil
	return nil
}
