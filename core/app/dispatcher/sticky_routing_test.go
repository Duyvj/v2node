package dispatcher

import (
	"context"
	"fmt"
	"testing"

	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/transport"
)

type stickyTestRoute struct {
	routing.Context
	group, tag string
}

func (r stickyTestRoute) GetOutboundGroupTags() []string { return []string{r.group} }
func (r stickyTestRoute) GetOutboundTag() string         { return r.tag }
func (r stickyTestRoute) GetRuleTag() string             { return r.group }

type stickyTestRouter struct {
	routing.DefaultRouter
	groups map[string][]string
}

func (r stickyTestRouter) PickRoute(ctx routing.Context) (routing.Route, error) {
	group := ctx.GetInboundTag()
	return stickyTestRoute{Context: ctx, group: group, tag: r.groups[group][0]}, nil
}

type stickyTestHandler struct {
	outbound.Handler
	tag      string
	selected *string
}

func (h stickyTestHandler) Tag() string                                   { return h.tag }
func (h stickyTestHandler) Dispatch(_ context.Context, _ *transport.Link) { *h.selected = h.tag }

type stickyTestManager struct {
	outbound.Manager
	handlers map[string]outbound.Handler
}

func (m stickyTestManager) GetHandler(tag string) outbound.Handler { return m.handlers[tag] }
func (m stickyTestManager) GetDefaultHandler() outbound.Handler    { panic("unexpected direct fallback") }

func TestRoutedDispatchUsesScopedStickyChoiceForTCPAndUDP(t *testing.T) {
	groups := map[string][]string{"node81": {"WG-A", "WG-B"}, "node82": {"WG-C", "WG-D"}}
	selected := ""
	m := stickyTestManager{handlers: make(map[string]outbound.Handler)}
	for _, tags := range groups {
		for _, tag := range tags {
			m.handlers[tag] = stickyTestHandler{tag: tag, selected: &selected}
		}
	}
	d := &DefaultDispatcher{ohm: m, router: stickyTestRouter{groups: groups}}
	d.ConfigureStickyBalancerGroups(groups)
	defer d.Close()
	for group, tags := range groups {
		for user := 0; user < 2; user++ {
			for _, network := range []xnet.Network{xnet.Network_TCP, xnet.Network_UDP} {
				destination := xnet.Destination{Network: network, Address: xnet.ParseAddress("example.com"), Port: 443}
				ctx := session.ContextWithInbound(context.Background(), &session.Inbound{Tag: group, User: &protocol.MemoryUser{Email: fmt.Sprintf("user-%d", user)}})
				ctx = session.ContextWithOutbounds(ctx, []*session.Outbound{{Target: destination}})
				d.routedDispatch(ctx, &transport.Link{}, destination)
				if selected != tags[user] {
					t.Fatalf("%s user %d %s routed to %s, want %s", group, user, network, selected, tags[user])
				}
			}
		}
	}
}

func TestDispatcherStickyGroupsAreNotProcessGlobal(t *testing.T) {
	first, second := &DefaultDispatcher{}, &DefaultDispatcher{}
	defer first.Close()
	defer second.Close()
	first.ConfigureStickyBalancerGroups(map[string][]string{"node": {"old-WG"}})
	second.ConfigureStickyBalancerGroups(map[string][]string{"node": {"new-WG"}})
	if got := first.stickyBalancer.PickOutboundForGroup("node", "user"); got != "old-WG" {
		t.Fatalf("replacement core changed live core: %s", got)
	}
}
