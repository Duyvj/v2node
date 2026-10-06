package core

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/limiter"
)

func TestSS2022ConcurrentUserUpdates(t *testing.T) {
	tcp, udp := protocolEchoServers(t)
	for _, size := range []int{16, 32} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			info := &panel.NodeInfo{Tag: fmt.Sprint("ss-update-", size), Type: "shadowsocks", Common: &panel.CommonNode{
				ListenIP: "127.0.0.1", ServerPort: protocolFreePort(t), Network: "tcp",
				Cipher: fmt.Sprintf("2022-blake3-aes-%d-gcm", size*8), ServerKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, size)),
			}}
			out := `{"tag":"echo","protocol":"freedom","settings":{"finalRules":[{"action":"allow","ip":["127.0.0.1/32"]}]}}`
			info.Common.Routes = []panel.Route{{Action: "default_out", ActionValue: &out}}
			cfg := conf.New()
			cfg.LogConfig.Level = "error"
			server := New(cfg)
			if err := server.Start([]*panel.NodeInfo{info}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { server.Close(); limiter.DeleteLimiter(info.Tag) })
			users := []panel.UserInfo{{Id: 1, Uuid: protocolTestUUID, DeviceLimit: 2}}
			limiter.AddLimiter(info.Type, info.Tag, users, nil)
			if err := server.AddNode(info.Tag, info); err != nil {
				t.Fatal(err)
			}
			if _, err := server.AddUsers(&AddUsersParams{Tag: info.Tag, Users: users, NodeInfo: info}); err != nil {
				t.Fatal(err)
			}
			client := protocolClient(t, info)
			extra := []panel.UserInfo{{Id: 2, Uuid: "22222222-2222-4222-8222-222222222222"}}
			stop := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				for {
					select {
					case <-stop:
						done <- nil
						return
					default:
					}
					if _, err := server.AddUsers(&AddUsersParams{Tag: info.Tag, Users: extra, NodeInfo: info}); err != nil {
						done <- err
						return
					}
					if err := server.DelUsers(extra, info.Tag, info); err != nil {
						done <- err
						return
					}
				}
			}()
			defer func() {
				close(stop)
				if err := <-done; err != nil {
					t.Error(err)
				}
			}()
			for i := 0; i < 12; i++ {
				protocolExchange(t, client, "tcp", tcp, []byte("update-tcp"))
				protocolExchange(t, client, "udp", udp, []byte("update-udp"))
			}
			traffic, _ := server.SnapshotUserTraffic(info.Tag, 0)
			for _, sample := range traffic {
				if sample.UID != 1 {
					t.Fatalf("traffic attributed to a different user: %+v", sample)
				}
			}
		})
	}
}
