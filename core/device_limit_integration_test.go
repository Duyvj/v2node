package core

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/limiter"
	xnet "github.com/xtls/xray-core/common/net"
	xcore "github.com/xtls/xray-core/core"
)

// Use six real source addresses through the inbound and dispatcher, rather
// than calling the limiter directly. Reporting must not release occupied IPs.
func TestDeviceLimitThroughRealConnections(t *testing.T) {
	cert := protocolTestCertificate(t)
	tcp, udp := protocolEchoServers(t)
	info := &panel.NodeInfo{Type: "vless", Security: panel.Tls, Tag: "device-limit", Common: &panel.CommonNode{
		ListenIP: "127.0.0.1", ServerPort: protocolFreePort(t), Network: "tcp", CertInfo: cert,
	}}
	out := `{"tag":"test-echo","protocol":"freedom","settings":{"finalRules":[{"action":"allow","ip":["127.0.0.1/32"]}]}}`
	info.Common.Routes = []panel.Route{{Action: "default_out", ActionValue: &out}}
	cfg := conf.New()
	cfg.LogConfig.Level = "error"
	server := New(cfg)
	if err := server.Start([]*panel.NodeInfo{info}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close(); limiter.DeleteLimiter(info.Tag) })
	users := []panel.UserInfo{{Id: 1, Uuid: protocolTestUUID, DeviceLimit: 2}}
	l := limiter.AddLimiter(info.Type, info.Tag, users, nil)
	if err := server.AddNode(info.Tag, info); err != nil {
		t.Fatal(err)
	}
	if _, err := server.AddUsers(&AddUsersParams{Tag: info.Tag, Users: users, NodeInfo: info}); err != nil {
		t.Fatal(err)
	}
	clients := make([]*xcore.Instance, 6)
	for i := range clients {
		clients[i] = protocolClientFrom(t, info, fmt.Sprintf("127.0.0.%d", i+2))
	}
	for _, network := range []string{"tcp", "udp", "tcp"} {
		t.Run(network, func(t *testing.T) {
			address := tcp
			if network == "udp" {
				address = udp
			}
			dest, err := xnet.ParseDestination(network + ":" + address)
			if err != nil {
				t.Fatal(err)
			}
			accepted := make(chan int, len(clients))
			var wg sync.WaitGroup
			for i, client := range clients {
				wg.Add(1)
				go func(i int, client *xcore.Instance) {
					defer wg.Done()
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					conn, err := xcore.Dial(ctx, client, dest)
					if err != nil {
						return
					}
					defer conn.Close()
					payload := []byte("device-limit-data")
					done := make(chan bool, 1)
					go func() {
						if _, err := conn.Write(payload); err != nil {
							done <- false
							return
						}
						response := make([]byte, len(payload))
						_, err := io.ReadFull(conn, response)
						done <- err == nil && bytes.Equal(payload, response)
					}()
					select {
					case ok := <-done:
						if ok {
							accepted <- i
						}
					case <-ctx.Done():
					}
				}(i, client)
			}
			wg.Wait()
			if len(accepted) != 2 {
				t.Fatalf("%s: %d of 6 source IPs transferred data, want 2", network, len(accepted))
			}
			for i := 0; i < 3; i++ {
				online, err := l.GetOnlineDevice()
				if err != nil || len(*online) != 2 {
					t.Fatalf("online report: count=%d error=%v", len(*online), err)
				}
			}
			t.Logf("%s: 2 accepted, 4 blocked; online report remains 2", network)
		})
	}
}
