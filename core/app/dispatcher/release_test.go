package dispatcher

import (
	"context"
	"testing"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/format"
	"github.com/wyx2685/v2node/limiter"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/transport"
)

const (
	testInboundTag = "test-inbound"
	testUUID       = "00000000-0000-4000-8000-000000000001"
)

var testEmail = format.UserTag(testInboundTag, testUUID)

type testOutbound struct {
	block bool
	done  chan struct{}
}

func (*testOutbound) Start() error                         { return nil }
func (*testOutbound) Close() error                         { return nil }
func (*testOutbound) Tag() string                          { return "direct" }
func (*testOutbound) SenderSettings() *serial.TypedMessage { return nil }
func (*testOutbound) ProxySettings() *serial.TypedMessage  { return nil }

func (h *testOutbound) Dispatch(_ context.Context, link *transport.Link) {
	defer close(h.done)
	if h.block {
		for {
			if _, err := link.Reader.ReadMultiBuffer(); err != nil {
				break
			}
		}
	}
	common.Close(&buf.EndpointOverrideWriter{Writer: link.Writer})
	common.Interrupt(link.Reader)
}

type testOutboundManager struct{ h outbound.Handler }

func (*testOutboundManager) Type() interface{}                                  { return outbound.ManagerType() }
func (*testOutboundManager) Start() error                                       { return nil }
func (*testOutboundManager) Close() error                                       { return nil }
func (m *testOutboundManager) GetHandler(string) outbound.Handler               { return m.h }
func (m *testOutboundManager) GetDefaultHandler() outbound.Handler              { return m.h }
func (*testOutboundManager) AddHandler(context.Context, outbound.Handler) error { return nil }
func (*testOutboundManager) RemoveHandler(context.Context, string) error        { return nil }
func (m *testOutboundManager) ListHandlers(context.Context) []outbound.Handler {
	return []outbound.Handler{m.h}
}

func newTestDispatcher(t *testing.T, h *testOutbound) (*DefaultDispatcher, context.Context) {
	t.Helper()
	limiter.Init()
	limiter.AddLimiter("vless", testInboundTag, []panel.UserInfo{{Id: 1, Uuid: testUUID}}, map[int]int{})
	t.Cleanup(func() { limiter.DeleteLimiter(testInboundTag) })
	d := &DefaultDispatcher{ohm: &testOutboundManager{h: h}}
	ctx := session.ContextWithInbound(context.Background(), &session.Inbound{
		Tag:    testInboundTag,
		Source: net.TCPDestination(net.ParseAddress("1.2.3.4"), 40000),
		User:   &protocol.MemoryUser{Email: testEmail},
	})
	return d, ctx
}

func trackedState(d *DefaultDispatcher) (managers, links int) {
	d.LinkManagers.Range(func(_, value interface{}) bool {
		managers++
		lm := value.(*LinkManager)
		lm.mu.Lock()
		links += len(lm.links)
		lm.mu.Unlock()
		return true
	})
	return managers, links
}

func waitUntracked(t *testing.T, d *DefaultDispatcher) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		managers, links := trackedState(d)
		if managers == 0 && links == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("dispatcher still tracks %d link managers with %d links after the outbound returned", managers, links)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitDone(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("outbound did not return")
	}
}

var testTarget = net.TCPDestination(net.ParseAddress("9.9.9.9"), 80)

func TestDispatchLinkReleasesLinkWhenOutboundSwallowsClose(t *testing.T) {
	h := &testOutbound{done: make(chan struct{})}
	d, ctx := newTestDispatcher(t, h)

	link := &transport.Link{Reader: &linkManagerTestReader{}, Writer: &linkManagerTestWriter{}}
	if err := d.DispatchLink(ctx, testTarget, link); err != nil {
		t.Fatalf("DispatchLink: %v", err)
	}

	waitDone(t, h.done)
	waitUntracked(t, d)
}

func TestDispatchReleasesLinkAfterOutboundReturns(t *testing.T) {
	h := &testOutbound{done: make(chan struct{})}
	d, ctx := newTestDispatcher(t, h)

	if _, err := d.Dispatch(ctx, testTarget); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	waitDone(t, h.done)
	waitUntracked(t, d)
}

func TestRemoveUserLinksStillCutsActiveDispatch(t *testing.T) {
	h := &testOutbound{block: true, done: make(chan struct{})}
	d, ctx := newTestDispatcher(t, h)

	if _, err := d.Dispatch(ctx, testTarget); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if _, links := trackedState(d); links != 1 {
		t.Fatalf("tracked links while the outbound runs = %d, want 1", links)
	}

	d.RemoveUserLinks(testEmail)

	waitDone(t, h.done)
	waitUntracked(t, d)
}
