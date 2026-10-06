package core

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/limiter"
)

func TestVLESSVisionAndRealityRoundTrip(t *testing.T) {
	cert := protocolTestCertificate(t)
	tcp, udp := protocolEchoServers(t)
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	target := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("reality-target")) }))
	target.EnableHTTP2 = true
	target.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	target.StartTLS()
	t.Cleanup(target.Close)
	host, port, _ := net.SplitHostPort(target.Listener.Addr().String())
	for _, security := range []int{panel.Tls, panel.Reality} {
		t.Run(fmt.Sprintf("security-%d", security), func(t *testing.T) {
			info := &panel.NodeInfo{Tag: fmt.Sprintf("vision-%d", security), Type: "vless", Security: security, Common: &panel.CommonNode{
				ListenIP: "127.0.0.1", ServerPort: protocolFreePort(t), Network: "tcp", Flow: "xtls-rprx-vision", CertInfo: cert,
				TlsSettings: panel.TlsSettings{ServerName: "localhost", Dest: host, ServerPort: port, ShortId: "0123456789abcdef", PrivateKey: base64.RawURLEncoding.EncodeToString(key.Bytes())},
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
			limiter.AddLimiter("vless", info.Tag, users, nil)
			if err := server.AddNode(info.Tag, info); err != nil {
				t.Fatal(err)
			}
			if _, err := server.AddUsers(&AddUsersParams{Tag: info.Tag, Users: users, NodeInfo: info}); err != nil {
				t.Fatal(err)
			}
			client := protocolClient(t, info)
			protocolExchange(t, client, "tcp", tcp, bytes.Repeat([]byte("vision-test"), 1024))
			protocolExchange(t, client, "udp", udp, []byte("vision-udp-test"))
		})
	}
}
