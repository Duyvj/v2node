package core

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
)

// Generate disposable credentials per test run. Never copy panel responses or
// user-provided VPN credentials into a source fixture.
func testRealityPrivateKey(t *testing.T) string {
	t.Helper()
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(key.Bytes())
}

func TestSyntheticPanelRealityAndWireGuardPool(t *testing.T) {
	// Preserve the null/legacy fields covered by the former live-panel fixture,
	// but use local test data only and generate the private key at runtime.
	raw := `{"listen_ip":"127.0.0.1","server_port":10830,"protocol":"vless","network":"tcp","network_settings":null,"tls":2,"tls_settings":{"server_name":"example.com","server_port":"443","short_id":"01234567"},"encryption":null,"encryption_settings":null,"base_config":{"push_interval":60,"pull_interval":60}}`
	var common panel.CommonNode
	if err := json.Unmarshal([]byte(raw), &common); err != nil {
		t.Fatal(err)
	}
	common.TlsSettings.PrivateKey = testRealityPrivateKey(t)
	pool := `[
	  {"tag":"test-wg-a","protocol":"wireguard","settings":{"secretKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","address":["10.2.0.2/32"],"peers":[{"publicKey":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=","endpoint":"192.0.2.10:51820","allowedIPs":["0.0.0.0/0"]}]}},
	  {"tag":"test-wg-b","protocol":"wireguard","settings":{"secretKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","address":["10.2.0.2/32"],"peers":[{"publicKey":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=","endpoint":"192.0.2.11:51820","allowedIPs":["0.0.0.0/0"]}]}}
	]`
	common.Routes = []panel.Route{{Action: "default_out", ActionValue: &pool}}
	info := &panel.NodeInfo{Id: 1, Tag: "synthetic-node", Type: "vless", Security: panel.Reality, Common: &common}
	if _, err := buildInbound(info, info.Tag); err != nil {
		t.Fatal(err)
	}
	_, outbounds, routing, observation, groups, err := GetCustomConfig([]*panel.NodeInfo{info})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups[defaultBalancerTag(info.Tag)]) != 2 || len(outbounds) != 5 || len(routing.BalancingRule) != 1 || observation == nil {
		t.Fatal("synthetic panel config did not produce both WG outbounds and its scoped balancer")
	}
}
