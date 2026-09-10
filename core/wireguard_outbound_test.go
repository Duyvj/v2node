package core

import (
	"encoding/json"
	"reflect"
	"testing"

	coreConf "github.com/xtls/xray-core/infra/conf"
)

func TestWireGuardPreservesExplicitAndLegacyDNS(t *testing.T) {
	for _, raw := range []string{
		`{"remoteDNS":["10.8.0.1","1.1.1.1"]}`,
		`{"dns":["10.8.0.1","1.1.1.1"]}`,
		`{"remoteDNS":["10.8.0.1","1.1.1.1"],"dns":["10.2.0.1"]}`,
	} {
		settings := json.RawMessage(raw)
		out := &coreConf.OutboundDetourConfig{Protocol: "wireguard", Settings: &settings}
		if err := hardenWireGuardOutbound(out); err != nil {
			t.Fatal(err)
		}
		var parsed struct {
			DNS []string `json:"remoteDNS"`
		}
		if err := json.Unmarshal(*out.Settings, &parsed); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(parsed.DNS, []string{"10.8.0.1", "1.1.1.1"}) {
			t.Fatalf("explicit DNS changed: %v", parsed.DNS)
		}
	}
}

func TestWireGuardOutboundIsRegistered(t *testing.T) {
	raw := []byte(`{
		"tag":"wg_out",
		"protocol":"wireguard",
		"settings":{
			"secretKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
			"address":["10.0.0.2/32"],
			"peers":[{
				"endpoint":"127.0.0.1:51820",
				"publicKey":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=",
				"allowedIPs":["0.0.0.0/0"]
			}],
			"noKernelTun":true,
			"mtu":1280,
			"reserved":[0,0,0],
			"domainStrategy":"ForceIP"
		}
	}`)

	config := &coreConf.OutboundDetourConfig{}
	if err := json.Unmarshal(raw, config); err != nil {
		t.Fatalf("decode WireGuard outbound: %v", err)
	}
	if _, err := config.Build(); err != nil {
		t.Fatalf("build WireGuard outbound: %v", err)
	}
}

func TestHardenWireGuardOutboundSetsNoKernelTun(t *testing.T) {
	raw := []byte(`{
		"tag":"wg_test",
		"protocol":"wireguard",
		"settings":{
			"secretKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
			"address":["10.8.0.2/32"],
			"peers":[{
				"endpoint":"192.0.2.10:51820",
				"publicKey":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=",
				"allowedIPs":["0.0.0.0/0"],
				"keepAlive":25
			}],
			"mtu":1280
		}
	}`)
	config := &coreConf.OutboundDetourConfig{}
	if err := json.Unmarshal(raw, config); err != nil {
		t.Fatal(err)
	}
	if err := hardenWireGuardOutbound(config); err != nil {
		t.Fatal(err)
	}
	var settings map[string]interface{}
	if err := json.Unmarshal(*config.Settings, &settings); err != nil {
		t.Fatal(err)
	}
	if noTun, ok := settings["noKernelTun"].(bool); !ok || !noTun {
		t.Fatalf("expected noKernelTun=true, got: %v", settings["noKernelTun"])
	}
	if _, exists := settings["remoteDNS"]; exists {
		t.Fatalf("expected Xray's generic DNS defaults, got forced remoteDNS: %v", settings["remoteDNS"])
	}
	// Verify it still builds cleanly into an Xray OutboundHandlerConfig
	if _, err := config.Build(); err != nil {
		t.Fatalf("build hardened WireGuard outbound failed: %v", err)
	}
}
