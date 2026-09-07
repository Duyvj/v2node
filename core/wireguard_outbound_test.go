package core

import (
	"encoding/json"
	"testing"

	coreConf "github.com/xtls/xray-core/infra/conf"
)

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
		"tag":"wg_proton",
		"protocol":"wireguard",
		"settings":{
			"secretKey":"COYrxmQRV27b/5XUMrhxa70XhkT5JFakYqLARDNkSW4=",
			"address":["10.2.0.2/32"],
			"peers":[{
				"endpoint":"188.214.152.226:51820",
				"publicKey":"NfKOMtk2fuDycbQXv36yk5mfdgDA8/8SN6amCdFrKxQ=",
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
	if dns, ok := settings["remoteDNS"].([]interface{}); !ok || len(dns) == 0 {
		t.Fatalf("expected remoteDNS fallback, got: %v", settings["remoteDNS"])
	}
	// Verify it still builds cleanly into an Xray OutboundHandlerConfig
	if _, err := config.Build(); err != nil {
		t.Fatalf("build hardened WireGuard outbound failed: %v", err)
	}
}
