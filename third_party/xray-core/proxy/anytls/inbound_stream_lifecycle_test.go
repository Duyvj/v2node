package anytls

import (
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/transport"
	"github.com/xtls/xray-core/transport/pipe"
)

func TestClientStreamAdmissionLosesRaceWithClose(t *testing.T) {
	s := &session{streams: make(map[uint32]*stream)}
	s.streamsMu.Lock()

	started := make(chan struct{})
	admitted := make(chan bool, 1)
	go func() {
		close(started)
		admitted <- s.addClientStream(newStream(1, nil))
	}()
	<-started

	// Model close's ordering while the admission goroutine is blocked on the
	// same mutex: close publishes closed before releasing the stream map.
	s.closed.Store(true)
	s.streams = nil
	s.streamsMu.Unlock()

	if <-admitted {
		t.Fatal("stream was admitted after session close")
	}
	if s.streams != nil {
		t.Fatal("stream map was recreated after session close")
	}
	if got := s.activeStreams.Load(); got != 0 {
		t.Fatalf("active stream count = %d, want 0", got)
	}
}

func TestInboundStreamMapIsBounded(t *testing.T) {
	s := &session{streams: make(map[uint32]*stream)}
	for i := 0; i < maxInboundStreamsPerSession; i++ {
		if !s.addInboundStream(uint32(i)) {
			t.Fatalf("stream %d was rejected below the limit", i)
		}
	}
	for i := 0; i < 128; i++ {
		if s.addInboundStream(uint32(maxInboundStreamsPerSession + i)) {
			t.Fatalf("stream %d was admitted above the limit", maxInboundStreamsPerSession+i)
		}
	}
	if !s.addInboundStream(0) {
		t.Fatal("duplicate active stream should remain accepted")
	}
	if got := len(s.streams); got != maxInboundStreamsPerSession {
		t.Fatalf("stream count = %d, want %d", got, maxInboundStreamsPerSession)
	}

	for i := 0; i < maxInboundStreamsPerSession; i++ {
		s.finishStream(uint32(i), nil)
	}
	if len(s.streams) != 0 {
		t.Fatalf("completed streams retained in map: %d", len(s.streams))
	}
	if !s.addInboundStream(maxInboundStreamsPerSession + 1) {
		t.Fatal("session did not accept a stream after completed streams were removed")
	}
}

func TestSessionCloseReleasesInboundStreamMap(t *testing.T) {
	conn, peer := net.Pipe()
	defer peer.Close()
	streams := make([]*stream, 16)
	s := &session{
		conn:    conn,
		errCh:   make(chan error, 1),
		streams: make(map[uint32]*stream),
	}
	for i := range streams {
		streams[i] = newStream(uint32(i), nil)
		s.streams[uint32(i)] = streams[i]
	}

	s.close(nil)
	if s.streams != nil {
		t.Fatalf("stream map retained after session close: %d", len(s.streams))
	}
	for i, st := range streams {
		select {
		case <-st.done:
		default:
			t.Fatalf("stream %d was not closed", i)
		}
	}
}

func TestNormalDownlinkFinishesStreamAndReleasesMapBuckets(t *testing.T) {
	reader, writer := pipe.New()
	if err := writer.Close(); err != nil {
		t.Fatalf("close downlink writer: %v", err)
	}
	link := &transport.Link{Reader: reader, Writer: writer}
	st := newStream(7, link)
	s := &session{streams: map[uint32]*stream{st.sid: st}}
	s.fw = newFrameWriter(buf.NewBufferedWriter(buf.NewWriter(io.Discard)))

	oldMap := s.streams
	oldMapID := fmt.Sprintf("%p", oldMap)
	s.pumpDownlink(st.sid, link)

	select {
	case <-st.done:
	default:
		t.Fatal("normal downlink completion did not close the stream")
	}
	if len(s.streams) != 0 {
		t.Fatalf("completed stream retained in map: %d", len(s.streams))
	}
	if newMapID := fmt.Sprintf("%p", s.streams); newMapID == oldMapID {
		t.Fatal("empty stream map retained its old backing buckets")
	}
}
