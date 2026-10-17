package node

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/core"
	"github.com/wyx2685/v2node/limiter"
)

type mockPanel struct {
	*httptest.Server
	mu     sync.Mutex
	port   int
	routes string
	users  string
}

func newMockPanel(t *testing.T) *mockPanel {
	p := &mockPanel{port: 1000, routes: "[]", users: `[{"id":1,"uuid":"00000000-0000-4000-8000-000000000001"}]`}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/server/config":
			fmt.Fprintf(w, `{"protocol":"vmess","listen_ip":"0.0.0.0","server_port":%d,"tls":0,"network":"tcp","network_settings":{},"routes":%s,"base_config":{"push_interval":60,"pull_interval":60}}`, p.port, p.routes)
		case "/api/v1/server/UniProxy/user":
			fmt.Fprintf(w, `{"users":%s}`, p.users)
		case "/api/v1/server/UniProxy/alivelist":
			fmt.Fprint(w, `{"alive":{}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *mockPanel) set(f func(p *mockPanel)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f(p)
}

type fakeServer struct {
	mu         sync.Mutex
	calls      []string
	addNodeErr error
}

func (f *fakeServer) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeServer) AddNode(_ string, info *panel.NodeInfo) error {
	f.record(fmt.Sprintf("AddNode:%d", info.Common.ServerPort))
	return f.addNodeErr
}

func (f *fakeServer) DelNode(string) error {
	f.record("DelNode")
	return nil
}

func (f *fakeServer) AddUsers(p *core.AddUsersParams) (int, error) {
	f.record(fmt.Sprintf("AddUsers:%d", len(p.Users)))
	return len(p.Users), nil
}

func (f *fakeServer) DelUsers(users []panel.UserInfo, _ string, _ *panel.NodeInfo) error {
	f.record(fmt.Sprintf("DelUsers:%d", len(users)))
	return nil
}

func (f *fakeServer) GetUserTrafficSlice(string, int) ([]panel.UserTraffic, error) {
	return nil, nil
}

func newTestController(t *testing.T, p *mockPanel, srv *fakeServer) *Controller {
	t.Helper()
	ctx := context.Background()
	api, err := panel.New(&conf.NodeConfig{APIHost: p.URL, NodeID: 1, Key: "k"})
	if err != nil {
		t.Fatal(err)
	}
	info, err := api.GetNodeInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	users, err := api.GetUserList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	limiter.Init()
	c := NewController(api, &conf.NodeConfig{}, info)
	c.server = srv
	c.reloadCh = make(chan struct{}, 1)
	c.tag = info.Tag
	c.userList = users
	c.limiter = limiter.AddLimiter(info.Type, c.tag, users, map[int]int{})
	t.Cleanup(func() { limiter.DeleteLimiter(c.tag) })
	return c
}

func reloadRequested(c *Controller) bool {
	select {
	case <-c.reloadCh:
		return true
	default:
		return false
	}
}

func TestInboundChangeRebuildsOnlyThisNode(t *testing.T) {
	p := newMockPanel(t)
	srv := &fakeServer{}
	c := newTestController(t, p, srv)

	p.set(func(p *mockPanel) { p.port = 2000 })
	if err := c.nodeInfoMonitor(context.Background()); err != nil {
		t.Fatal(err)
	}

	if reloadRequested(c) {
		t.Fatal("a port change reloaded the whole core")
	}
	if want := []string{"DelNode", "AddNode:2000", "AddUsers:1"}; !slices.Equal(srv.calls, want) {
		t.Fatalf("calls = %v, want %v", srv.calls, want)
	}
	if c.info.Common.ServerPort != 2000 {
		t.Fatalf("controller kept port %d", c.info.Common.ServerPort)
	}
}

func TestRouteChangeReloadsWithoutTouchingTheOldCore(t *testing.T) {
	p := newMockPanel(t)
	srv := &fakeServer{}
	c := newTestController(t, p, srv)

	p.set(func(p *mockPanel) {
		p.routes = `[{"id":1,"match":["example.com"],"action":"block"}]`
		p.users = `[{"id":2,"uuid":"00000000-0000-4000-8000-000000000002"}]`
	})
	if err := c.nodeInfoMonitor(context.Background()); err != nil {
		t.Fatal(err)
	}

	if !reloadRequested(c) {
		t.Fatal("a route change did not request a full reload")
	}
	if len(srv.calls) != 0 {
		t.Fatalf("controller kept using the core it asked to reload: %v", srv.calls)
	}
}

func TestFailedRebuildFallsBackToFullReload(t *testing.T) {
	p := newMockPanel(t)
	srv := &fakeServer{addNodeErr: errors.New("bind: address already in use")}
	c := newTestController(t, p, srv)

	p.set(func(p *mockPanel) { p.port = 2000 })
	if err := c.nodeInfoMonitor(context.Background()); err != nil {
		t.Fatal(err)
	}

	if !reloadRequested(c) {
		t.Fatal("a failed inbound rebuild did not fall back to a full reload")
	}
	if want := []string{"DelNode", "AddNode:2000"}; !slices.Equal(srv.calls, want) {
		t.Fatalf("calls = %v, want %v", srv.calls, want)
	}
}
