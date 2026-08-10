package anytls

import (
	"bytes"
	"context"
	"encoding/binary"
	stderrors "errors"
	"io"
	"net/netip"
	"testing"

	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/common/uot"
	"github.com/xtls/xray-core/common/buf"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/transport"
	"github.com/xtls/xray-core/transport/pipe"
)

type failingDispatcher struct {
	err error
}

func (d *failingDispatcher) Dispatch(context.Context, xnet.Destination) (*transport.Link, error) {
	return nil, d.err
}

func (*failingDispatcher) DispatchLink(context.Context, xnet.Destination, *transport.Link) error {
	return nil
}

func (*failingDispatcher) Start() error { return nil }
func (*failingDispatcher) Close() error { return nil }
func (*failingDispatcher) Type() interface{} {
	return routing.DispatcherType()
}

type linkDispatcher struct {
	link  *transport.Link
	calls int
}

func (d *linkDispatcher) Dispatch(context.Context, xnet.Destination) (*transport.Link, error) {
	d.calls++
	return d.link, nil
}

func (*linkDispatcher) DispatchLink(context.Context, xnet.Destination, *transport.Link) error {
	return nil
}

func (*linkDispatcher) Start() error { return nil }
func (*linkDispatcher) Close() error { return nil }
func (*linkDispatcher) Type() interface{} {
	return routing.DispatcherType()
}

type failingIOWriter struct {
	err error
}

func (w *failingIOWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func newInboundTestSession(t *testing.T, sid uint32, dispatcher routing.Dispatcher, wire io.Writer) (*session, *stream) {
	t.Helper()
	s := &session{
		dispatcher: dispatcher,
		streams:    make(map[uint32]*stream),
		fw:         newFrameWriter(buf.NewBufferedWriter(buf.NewWriter(wire))),
	}
	if !s.addInboundStream(sid) {
		t.Fatal("failed to add inbound stream")
	}
	st := s.streams[sid]
	if st.done != nil {
		t.Fatal("inbound stream unexpectedly has a completion channel")
	}
	return s, st
}

func newTestBuffer(t *testing.T, payload []byte) *buf.Buffer {
	t.Helper()
	b := buf.New()
	if _, err := b.Write(payload); err != nil {
		b.Release()
		t.Fatalf("write buffer: %v", err)
	}
	return b
}

func newSocksaddrBuffer(t *testing.T, destination M.Socksaddr) *buf.Buffer {
	t.Helper()
	b := buf.New()
	if err := M.SocksaddrSerializer.WriteAddrPort(b, destination); err != nil {
		b.Release()
		t.Fatalf("write destination: %v", err)
	}
	return b
}

func newUoTRequestBuffer(t *testing.T, request uot.Request) *buf.Buffer {
	t.Helper()
	b := buf.New()
	if err := uot.WriteRequest(b, request); err != nil {
		b.Release()
		t.Fatalf("write UoT request: %v", err)
	}
	return b
}

func newUoTDestinationBuffer(t *testing.T, destination M.Socksaddr) *buf.Buffer {
	t.Helper()
	b := buf.New()
	if err := uot.AddrParser.WriteAddrPort(b, destination); err != nil {
		b.Release()
		t.Fatalf("write UoT destination: %v", err)
	}
	return b
}

func newTestLink() *transport.Link {
	reader, writer := pipe.New()
	return &transport.Link{Reader: reader, Writer: writer}
}

func newClosedTestLink(t *testing.T) *transport.Link {
	t.Helper()
	reader, writer := pipe.New()
	if err := writer.Close(); err != nil {
		t.Fatalf("close test link: %v", err)
	}
	return &transport.Link{Reader: reader, Writer: writer}
}

func assertBuffersReleased(t *testing.T, buffers ...*buf.Buffer) {
	t.Helper()
	for i, b := range buffers {
		if b.Bytes() != nil {
			t.Fatalf("buffer %d was retained", i)
		}
	}
}

func assertStreamFinished(t *testing.T, s *session, sid uint32) {
	t.Helper()
	if _, found := s.streams[sid]; found {
		t.Fatal("stream was retained")
	}
}

func assertFrameCommands(t *testing.T, wire []byte, want ...byte) {
	t.Helper()
	var got []byte
	for len(wire) > 0 {
		if len(wire) < 7 {
			t.Fatalf("short frame header: %d byte(s)", len(wire))
		}
		got = append(got, wire[0])
		length := int(binary.BigEndian.Uint16(wire[5:7]))
		if len(wire) < 7+length {
			t.Fatalf("short frame body: got %d byte(s), want %d", len(wire)-7, length)
		}
		wire = wire[7+length:]
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("frame commands = %v, want %v", got, want)
	}
}

func TestHandleNewStreamDispatchFailureReleasesAndFinishesStream(t *testing.T) {
	const sid = uint32(41)
	dispatchErr := stderrors.New("dispatch failed")
	var wire bytes.Buffer
	s, st := newInboundTestSession(t, sid, &failingDispatcher{err: dispatchErr}, &wire)

	destination := newSocksaddrBuffer(t, M.Socksaddr{
		Addr: netip.MustParseAddr("192.0.2.1"),
		Port: 443,
	})
	tail1 := newTestBuffer(t, []byte("must be released"))
	tail2 := newTestBuffer(t, []byte("too"))

	if err := s.handleNewStream(context.Background(), st, buf.MultiBuffer{destination, tail1, tail2}); err != nil {
		t.Fatalf("handle new stream: %v", err)
	}
	assertBuffersReleased(t, destination, tail1, tail2)
	assertStreamFinished(t, s, sid)
	assertFrameCommands(t, wire.Bytes(), cmdFIN)
}

func TestHandleNewStreamRejectionsReleaseCompleteFrame(t *testing.T) {
	t.Run("malformed destination is fatal", func(t *testing.T) {
		const sid = uint32(42)
		var wire bytes.Buffer
		s, st := newInboundTestSession(t, sid, nil, &wire)
		first := newTestBuffer(t, []byte{0xff})
		tail1 := newTestBuffer(t, []byte("tail one"))
		tail2 := newTestBuffer(t, []byte("tail two"))

		if err := s.handleNewStream(context.Background(), st, buf.MultiBuffer{first, tail1, tail2}); err == nil {
			t.Fatal("malformed destination unexpectedly succeeded")
		}
		assertBuffersReleased(t, first, tail1, tail2)
		if _, found := s.streams[sid]; !found {
			t.Fatal("fatal parse error finished the stream before session shutdown")
		}
		assertFrameCommands(t, wire.Bytes())
		s.finishStream(sid, nil)
	})

	t.Run("unsupported UoT is stream local", func(t *testing.T) {
		const sid = uint32(43)
		var wire bytes.Buffer
		s, st := newInboundTestSession(t, sid, nil, &wire)
		first := newSocksaddrBuffer(t, M.Socksaddr{Fqdn: "sp.v1.udp-over-tcp.arpa"})
		tail1 := newTestBuffer(t, []byte("tail one"))
		tail2 := newTestBuffer(t, []byte("tail two"))

		if err := s.handleNewStream(context.Background(), st, buf.MultiBuffer{first, tail1, tail2}); err != nil {
			t.Fatalf("handle unsupported UoT stream: %v", err)
		}
		assertBuffersReleased(t, first, tail1, tail2)
		assertStreamFinished(t, s, sid)
		assertFrameCommands(t, wire.Bytes(), cmdFIN)
	})
}

func TestHandleNewStreamUoTDispatchFailureReleasesCompleteFrame(t *testing.T) {
	const sid = uint32(44)
	var wire bytes.Buffer
	s, st := newInboundTestSession(t, sid, &failingDispatcher{err: stderrors.New("dispatch failed")}, &wire)
	outer := newSocksaddrBuffer(t, uot.RequestDestination(uot.Version))
	request := newUoTRequestBuffer(t, uot.Request{
		IsConnect: true,
		Destination: M.Socksaddr{
			Addr: netip.MustParseAddr("192.0.2.10"),
			Port: 53,
		},
	})
	tail1 := newTestBuffer(t, []byte("tail one"))
	tail2 := newTestBuffer(t, []byte("tail two"))

	if err := s.handleNewStream(context.Background(), st, buf.MultiBuffer{outer, request, tail1, tail2}); err != nil {
		t.Fatalf("handle UoT stream: %v", err)
	}
	assertBuffersReleased(t, outer, request, tail1, tail2)
	assertStreamFinished(t, s, sid)
	assertFrameCommands(t, wire.Bytes(), cmdSYNACK, cmdFIN)
}

func TestHandleUDPFrameDispatchFailureReleasesCompleteFrame(t *testing.T) {
	const sid = uint32(45)
	var wire bytes.Buffer
	s, st := newInboundTestSession(t, sid, &failingDispatcher{err: stderrors.New("dispatch failed")}, &wire)
	st.isUDP = true
	st.link = newTestLink()
	first := newUoTDestinationBuffer(t, M.Socksaddr{
		Addr: netip.MustParseAddr("192.0.2.20"),
		Port: 53,
	})
	tail1 := newTestBuffer(t, []byte("tail one"))
	tail2 := newTestBuffer(t, []byte("tail two"))

	if err := s.handleUDPFrame(context.Background(), st, buf.MultiBuffer{first, tail1, tail2}); err != nil {
		t.Fatalf("handle UDP frame: %v", err)
	}
	assertBuffersReleased(t, first, tail1, tail2)
	assertStreamFinished(t, s, sid)
	assertFrameCommands(t, wire.Bytes(), cmdFIN)
}

func TestUDPParseFailuresReleaseCompleteFrame(t *testing.T) {
	t.Run("initial request", func(t *testing.T) {
		const sid = uint32(46)
		var wire bytes.Buffer
		s, st := newInboundTestSession(t, sid, nil, &wire)
		st.isUDP = true
		first := newTestBuffer(t, []byte{1, 0xff})
		tail1 := newTestBuffer(t, []byte("tail one"))
		tail2 := newTestBuffer(t, []byte("tail two"))

		if err := s.handleFirstUDPFrame(context.Background(), st, buf.MultiBuffer{first, tail1, tail2}); err != nil {
			t.Fatalf("handle first UDP frame: %v", err)
		}
		assertBuffersReleased(t, first, tail1, tail2)
		assertStreamFinished(t, s, sid)
		assertFrameCommands(t, wire.Bytes(), cmdFIN)
	})

	t.Run("packet after dispatch", func(t *testing.T) {
		const sid = uint32(47)
		var wire bytes.Buffer
		link := newTestLink()
		dispatcher := &linkDispatcher{link: link}
		s, st := newInboundTestSession(t, sid, dispatcher, &wire)
		outer := newSocksaddrBuffer(t, uot.RequestDestination(uot.Version))
		request := newUoTRequestBuffer(t, uot.Request{
			IsConnect: true,
			Destination: M.Socksaddr{
				Addr: netip.MustParseAddr("192.0.2.30"),
				Port: 53,
			},
		})
		if _, err := request.Write([]byte{0}); err != nil {
			t.Fatalf("write short packet length: %v", err)
		}
		tail1 := newTestBuffer(t, []byte("tail one"))
		tail2 := newTestBuffer(t, []byte("tail two"))

		if err := s.handleNewStream(context.Background(), st, buf.MultiBuffer{outer, request, tail1, tail2}); err != nil {
			t.Fatalf("handle UoT stream: %v", err)
		}
		assertBuffersReleased(t, outer, request, tail1, tail2)
		assertStreamFinished(t, s, sid)
		assertFrameCommands(t, wire.Bytes(), cmdSYNACK, cmdFIN)
		if dispatcher.calls != 1 {
			t.Fatalf("dispatch calls = %d, want 1", dispatcher.calls)
		}
	})
}

func TestSYNACKWriteFailuresReleaseCurrentFrame(t *testing.T) {
	sendErr := stderrors.New("send failed")

	t.Run("TCP", func(t *testing.T) {
		const sid = uint32(48)
		dispatcher := &linkDispatcher{link: newTestLink()}
		s, st := newInboundTestSession(t, sid, dispatcher, &failingIOWriter{err: sendErr})
		first := newSocksaddrBuffer(t, M.Socksaddr{
			Addr: netip.MustParseAddr("192.0.2.40"),
			Port: 443,
		})
		tail1 := newTestBuffer(t, []byte("tail one"))
		tail2 := newTestBuffer(t, []byte("tail two"))

		if err := s.handleNewStream(context.Background(), st, buf.MultiBuffer{first, tail1, tail2}); !stderrors.Is(err, sendErr) {
			t.Fatalf("handle TCP stream error = %v, want %v", err, sendErr)
		}
		assertBuffersReleased(t, first, tail1, tail2)
		if _, found := s.streams[sid]; !found {
			t.Fatal("fatal send error finished the stream before session shutdown")
		}
		s.finishStream(sid, sendErr)
	})

	t.Run("UoT", func(t *testing.T) {
		const sid = uint32(49)
		s, st := newInboundTestSession(t, sid, nil, &failingIOWriter{err: sendErr})
		outer := newSocksaddrBuffer(t, uot.RequestDestination(uot.Version))
		request := newUoTRequestBuffer(t, uot.Request{
			IsConnect: true,
			Destination: M.Socksaddr{
				Addr: netip.MustParseAddr("192.0.2.50"),
				Port: 53,
			},
		})
		tail := newTestBuffer(t, []byte("tail"))

		if err := s.handleNewStream(context.Background(), st, buf.MultiBuffer{outer, request, tail}); !stderrors.Is(err, sendErr) {
			t.Fatalf("handle UoT stream error = %v, want %v", err, sendErr)
		}
		assertBuffersReleased(t, outer, request, tail)
		if _, found := s.streams[sid]; !found {
			t.Fatal("fatal send error finished the stream before session shutdown")
		}
		s.finishStream(sid, sendErr)
	})
}

func TestLinkWriteFailuresConsumeCurrentFrame(t *testing.T) {
	t.Run("TCP", func(t *testing.T) {
		const sid = uint32(50)
		var wire bytes.Buffer
		dispatcher := &linkDispatcher{link: newClosedTestLink(t)}
		s, st := newInboundTestSession(t, sid, dispatcher, &wire)
		first := newSocksaddrBuffer(t, M.Socksaddr{
			Addr: netip.MustParseAddr("192.0.2.60"),
			Port: 443,
		})
		tail1 := newTestBuffer(t, []byte("tail one"))
		tail2 := newTestBuffer(t, []byte("tail two"))

		if err := s.handleNewStream(context.Background(), st, buf.MultiBuffer{first, tail1, tail2}); err == nil {
			t.Fatal("write to closed TCP link unexpectedly succeeded")
		}
		assertBuffersReleased(t, first, tail1, tail2)
		assertFrameCommands(t, wire.Bytes(), cmdSYNACK)
		s.finishStream(sid, nil)
	})

	t.Run("UoT", func(t *testing.T) {
		const sid = uint32(51)
		var wire bytes.Buffer
		dispatcher := &linkDispatcher{link: newClosedTestLink(t)}
		s, st := newInboundTestSession(t, sid, dispatcher, &wire)
		outer := newSocksaddrBuffer(t, uot.RequestDestination(uot.Version))
		request := newUoTRequestBuffer(t, uot.Request{
			IsConnect: true,
			Destination: M.Socksaddr{
				Addr: netip.MustParseAddr("192.0.2.70"),
				Port: 53,
			},
		})
		tail1 := newTestBuffer(t, []byte("tail one"))
		tail2 := newTestBuffer(t, []byte("tail two"))

		if err := s.handleNewStream(context.Background(), st, buf.MultiBuffer{outer, request, tail1, tail2}); err == nil {
			t.Fatal("write to closed UDP link unexpectedly succeeded")
		}
		assertBuffersReleased(t, outer, request, tail1, tail2)
		assertFrameCommands(t, wire.Bytes(), cmdSYNACK)
		s.finishStream(sid, nil)
	})
}
