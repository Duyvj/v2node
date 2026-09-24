package cmd

import (
	"encoding/json"
	"fmt"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/core"
	"github.com/wyx2685/v2node/limiter"
	"github.com/wyx2685/v2node/node"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestFailedCandidateRestoresPreviousCore(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listenPort := probe.Addr().(*net.TCPAddr).Port
	probe.Close()
	var candidate atomic.Bool
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/server/config":
			port := listenPort
			if candidate.Load() {
				port = -1
			}
			fmt.Fprintf(w, `{"protocol":"vless","listen_ip":"127.0.0.1","server_port":%d,"network":"tcp","tls":0,"base_config":{"push_interval":60,"pull_interval":60}}`, port)
		case "/api/v1/server/UniProxy/user":
			fmt.Fprint(w, `{"users":[{"id":1,"uuid":"11111111-1111-4111-8111-111111111111","device_limit":2}]}`)
		case "/api/v1/server/UniProxy/alivelist":
			fmt.Fprint(w, `{"alive":{}}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer s.Close()
	cfg := conf.New()
	cfg.NodeConfigs = []conf.NodeConfig{{APIHost: s.URL, NodeID: 1, Key: "test"}}
	limiter.Init()
	n, err := node.New(cfg.NodeConfigs)
	if err != nil {
		t.Fatal(err)
	}
	c := core.New(cfg)
	c.ReloadCh = make(chan struct{}, 1)
	if err = c.Start(n.NodeInfos); err != nil {
		t.Fatal(err)
	}
	defer func() { n.Close(); c.Close() }()
	if err = n.Start(cfg.NodeConfigs, c); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "config.json")
	data, _ := json.Marshal(map[string]any{"Nodes": []map[string]any{{"ApiHost": s.URL, "NodeID": 1, "ApiKey": "test"}}})
	os.WriteFile(p, data, 0600)
	previous := c
	candidate.Store(true)
	if err = reload(p, &n, &c); err == nil {
		t.Fatal("invalid candidate accepted")
	}
	if c == previous || c.Server == nil {
		t.Fatal("previous runtime not restored")
	}
	if _, err = c.GetUserManager(n.NodeInfos[0].Tag); err != nil {
		t.Fatal("restored inbound missing", err)
	}
}

func TestInvalidReloadDoesNotTouchCurrentRuntime(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(p, []byte("{"), 0600)
	n := &node.Node{}
	c := &core.V2Core{}
	beforeN, beforeC := n, c
	if err := reload(p, &n, &c); err == nil {
		t.Fatal("invalid reload succeeded")
	}
	if n != beforeN || c != beforeC {
		t.Fatal("invalid candidate replaced runtime")
	}
}
