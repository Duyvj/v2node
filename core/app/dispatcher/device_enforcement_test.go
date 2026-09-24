package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/format"
	"github.com/wyx2685/v2node/limiter"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/transport"
)

func TestEstablishedStreamsStopWhenLimitDropsFromSixToTwo(t *testing.T) {
	limiter.Init()
	users := []panel.UserInfo{{Id: 1, Uuid: "credential", DeviceLimit: 6}}
	l := limiter.AddLimiter("vless", "node", users, nil, nil, "https://panel.example")
	t.Cleanup(func() { limiter.DeleteLimiter("node") })
	d := &DefaultDispatcher{maxConnectionsPerUser: 32, maxConnections: 128}
	var links []*transport.Link
	for i := 1; i <= 6; i++ {
		inbound := &session.Inbound{
			Tag: "node", User: &protocol.MemoryUser{Email: format.UserTag("node", "credential")},
			Source: net.TCPDestination(net.ParseAddress(fmt.Sprintf("192.0.2.%d", i)), 12345),
		}
		ctx := session.ContextWithInbound(context.Background(), inbound)
		in, out, _, err := d.getLink(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if inbound.CanSpliceCopy != 3 {
			t.Fatal("splice could bypass device checks")
		}
		links = append(links, in)
		t.Cleanup(func() {
			common.Close(in.Writer)
			common.Interrupt(in.Reader)
			common.Close(out.Writer)
			common.Interrupt(out.Reader)
		})
	}
	l.UpdateUser("node", nil, nil, []panel.UserInfo{{Id: 1, Uuid: "credential", DeviceLimit: 2}})
	allowed := 0
	for _, link := range links {
		err := link.Writer.WriteMultiBuffer(buf.MultiBuffer{buf.FromBytes([]byte("payload"))})
		if err == nil {
			allowed++
		} else if !errors.Is(err, errDeviceLimit) {
			t.Fatal(err)
		}
	}
	if allowed != 2 {
		t.Fatalf("%d established streams transmitted after limit=2", allowed)
	}
}

func TestUploadReaderDropsDeniedPayloadAndDoesNotCountIt(t *testing.T) {
	gate := &deviceSessionGate{check: func() bool { return false }}
	reader := &deviceTouchReader{reader: &finalPayloadReader{}, touch: gate.allow}
	var counter atomic.Int64
	wrapped := newManagedTimeoutReader(reader, &counter, nil)
	mb, err := wrapped.ReadMultiBufferTimeout(time.Second)
	defer buf.ReleaseMulti(mb)
	if !errors.Is(err, errDeviceLimit) || !mb.IsEmpty() || counter.Load() != 0 {
		t.Fatalf("upload passed denial: bytes=%d counter=%d err=%v", mb.Len(), counter.Load(), err)
	}
	// A free slot later must not resurrect the rejected session.
	gate.check = func() bool { return true }
	if gate.allow() {
		t.Fatal("denied session was resurrected")
	}
}

func TestUploadReaderPreservesAllowedFinalPayload(t *testing.T) {
	r := &deviceTouchReader{reader: &finalPayloadReader{}, touch: func() bool { return true }}
	mb, err := r.ReadMultiBuffer()
	defer buf.ReleaseMulti(mb)
	if mb.Len() != 5 || !errors.Is(err, io.EOF) {
		t.Fatalf("lost final payload: %d %v", mb.Len(), err)
	}
}
