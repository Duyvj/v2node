package core

import (
	"context"
	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
	"github.com/xtls/xray-core/app/proxyman"
	"os"
	"testing"
)

func TestTransportSelectionWithoutSettings(t *testing.T) {
	for _, protocol := range []string{"vless", "vmess", "trojan", "anytls"} {
		for _, network := range []string{"tcp", "ws", "grpc", "httpupgrade", "xhttp"} {
			t.Run(protocol+"/"+network, func(t *testing.T) {
				config, err := buildInbound(&panel.NodeInfo{Type: protocol, Common: &panel.CommonNode{ListenIP: "127.0.0.1", ServerPort: 8080, Network: network}}, "empty-settings")
				if err != nil {
					t.Fatal(err)
				}
				receiver, err := config.ReceiverSettings.GetInstance()
				if err != nil {
					t.Fatal(err)
				}
				want := network
				if network == "ws" {
					want = "websocket"
				}
				if network == "xhttp" {
					want = "splithttp"
				}
				if receiver.(*proxyman.ReceiverConfig).StreamSettings.ProtocolName != want {
					t.Fatal("transport silently reverted to TCP")
				}
			})
		}
	}
}

func TestRejectedUserBatchRollsBack(t *testing.T) {
	cfg := conf.New()
	cfg.LogConfig.Level = "error"
	vc := New(cfg)
	if err := vc.Start(nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { vc.Close() })
	info := &panel.NodeInfo{Type: "vless", Common: &panel.CommonNode{ListenIP: "127.0.0.1", ServerPort: protocolFreePort(t), Network: "tcp"}}
	if err := vc.AddNode("rollback-users", info); err != nil {
		t.Fatal(err)
	}
	users := []panel.UserInfo{{Id: 1, Uuid: protocolTestUUID}, {Id: 2, Uuid: ""}}
	if n, err := vc.AddUsers(&AddUsersParams{Tag: "rollback-users", Users: users, NodeInfo: info}); err == nil || n != 0 {
		t.Fatal("invalid user batch accepted")
	}
	if len(vc.users.uidMap) != 0 {
		t.Fatal("failed batch retained UID mapping")
	}
	manager, _ := vc.GetUserManager("rollback-users")
	if manager.GetUser(context.Background(), "rollback-users|"+protocolTestUUID) != nil {
		t.Fatal("failed batch retained authentication")
	}
	if n, err := vc.AddUsers(&AddUsersParams{Tag: "rollback-users", Users: users[:1], NodeInfo: info}); err != nil || n != 1 {
		t.Fatal("valid retry failed", err)
	}
}

func TestCertificateChangeAndInvalidReplacement(t *testing.T) {
	cert := protocolTestCertificate(t)
	vc := New(conf.New())
	vc.ReloadCh = make(chan struct{}, 1)
	if err := vc.Start(nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { vc.Close() })
	info := &panel.NodeInfo{Type: "vless", Security: panel.Tls, Common: &panel.CommonNode{ListenIP: "127.0.0.1", ServerPort: protocolFreePort(t), Network: "tcp", CertInfo: cert}}
	if err := vc.AddNode("certificate-test", info); err != nil {
		t.Fatal(err)
	}
	if err := vc.checkCertificateUpdates(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(vc.ReloadCh) != 0 {
		t.Fatal("unchanged certificate requested reload")
	}
	if err := os.WriteFile(cert.KeyFile, []byte("incomplete key"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := vc.checkCertificateUpdates(context.Background()); err == nil || len(vc.ReloadCh) != 0 {
		t.Fatal("invalid replacement requested reload")
	}
	replacement := protocolTestCertificate(t)
	for dest, src := range map[string]string{cert.CertFile: replacement.CertFile, cert.KeyFile: replacement.KeyFile} {
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dest, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := vc.checkCertificateUpdates(context.Background()); err != nil || len(vc.ReloadCh) != 1 {
		t.Fatal("new certificate did not request reload", err)
	}
	if err := vc.Close(); err != nil {
		t.Fatal(err)
	}
	if vc.certificates.worker != nil || vc.certificates.files != nil {
		t.Fatal("closed core retained certificate worker")
	}
}
