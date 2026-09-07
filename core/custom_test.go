package core

import (
	"encoding/json"
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/xtls/xray-core/app/dns"
	coreConf "github.com/xtls/xray-core/infra/conf"
	"github.com/xtls/xray-core/proxy/freedom"
	"github.com/xtls/xray-core/transport/internet"
)

func TestDefaultEgressKeepsDNSAndFreedomOnIPv4(t *testing.T) {
	dnsConfig, outbounds, _, _, _, err := GetCustomConfig([]*panel.NodeInfo{{
		Id: 1, Tag: "node", Common: &panel.CommonNode{},
	}})
	if err != nil {
		t.Fatalf("build custom config: %v", err)
	}
	if dnsConfig.GetQueryStrategy() != dns.QueryStrategy_USE_IP4 {
		t.Fatalf("DNS query strategy = %s, want IPv4", dnsConfig.GetQueryStrategy())
	}
	if len(outbounds) == 0 || outbounds[0].ProxySettings == nil {
		t.Fatal("default freedom outbound is missing")
	}
	instance, err := outbounds[0].ProxySettings.GetInstance()
	if err != nil {
		t.Fatalf("decode freedom outbound: %v", err)
	}
	settings, ok := instance.(*freedom.Config)
	if !ok {
		t.Fatalf("default outbound settings type = %T", instance)
	}
	if settings.GetDomainStrategy() != internet.DomainStrategy_USE_IP4 {
		t.Fatalf("freedom domain strategy = %s, want IPv4", settings.GetDomainStrategy())
	}
}

func TestCustomRoutingRejectsMalformedOrUnsupportedRules(t *testing.T) {
	malformed := "{"
	for name, infos := range map[string][]*panel.NodeInfo{
		"nil node":       {nil},
		"missing common": {{Id: 1}},
		"malformed outbound": {{
			Id: 1, Tag: "node", Common: &panel.CommonNode{Routes: []panel.Route{{
				Id: 2, Action: "route", Match: []string{"example.com"}, ActionValue: &malformed,
			}}},
		}},
		"unknown action": {{
			Id: 1, Tag: "node", Common: &panel.CommonNode{Routes: []panel.Route{{
				Id: 3, Action: "future_fail_open_action",
			}}},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, _, _, _, err := GetCustomConfig(infos); err == nil {
				t.Fatal("unsafe routing configuration was silently ignored")
			}
		})
	}
}

func TestEveryFreedomOutboundBlocksPrivateDestinationsFirst(t *testing.T) {
	raw := json.RawMessage(`{"domainStrategy":"UseIPv4","finalRules":[{"action":"allow"}]}`)
	outbound := &coreConf.OutboundDetourConfig{
		Protocol: "freedom",
		Tag:      "custom-direct",
		Settings: &raw,
	}
	if err := hardenFreedomOutbound(outbound); err != nil {
		t.Fatalf("harden freedom outbound: %v", err)
	}

	var settings struct {
		FinalRules []struct {
			Action string   `json:"action"`
			IP     []string `json:"ip"`
		} `json:"finalRules"`
	}
	if err := json.Unmarshal(*outbound.Settings, &settings); err != nil {
		t.Fatalf("decode hardened settings: %v", err)
	}
	if len(settings.FinalRules) < 2 || settings.FinalRules[0].Action != "block" {
		t.Fatalf("private block is not the first final rule: %#v", settings.FinalRules)
	}
	want := map[string]bool{
		"10.0.0.0/8":     false,
		"127.0.0.0/8":    false,
		"169.254.0.0/16": false,
		"192.168.0.0/16": false,
		"::/127":         false,
		"fc00::/7":       false,
		"fe80::/10":      false,
	}
	for _, cidr := range settings.FinalRules[0].IP {
		if _, ok := want[cidr]; ok {
			want[cidr] = true
		}
	}
	for cidr, found := range want {
		if !found {
			t.Fatalf("private block is missing %s: %#v", cidr, settings.FinalRules[0].IP)
		}
	}
}

func TestMultipleDefaultOutboundsCreateBalancerAndObservatory(t *testing.T) {
	wg1 := `{
		"tag": "38454722",
		"protocol": "wireguard",
		"settings": {
			"secretKey": "COYrxmQRV27b/5XUMrhxa70XhkT5JFAkYqLARDNKSW4=",
			"address": ["10.2.0.2/32"],
			"peers": [{
				"publicKey": "NfKOMtk2fuDycbQXv36yk5mfdgDA8/8SN6amCdFrKxQ=",
				"endpoint": "188.214.152.226:51820",
				"allowedIPs": ["0.0.0.0/0"],
				"keepAlive": 25
			}],
			"mtu": 1280
		}
	}`
	wg2 := `{
		"tag": "38454723",
		"protocol": "wireguard",
		"settings": {
			"secretKey": "COYrxmQRV27b/5XUMrhxa70XhkT5JFAkYqLARDNKSW4=",
			"address": ["10.2.0.3/32"],
			"peers": [{
				"publicKey": "NfKOMtk2fuDycbQXv36yk5mfdgDA8/8SN6amCdFrKxQ=",
				"endpoint": "188.214.152.227:51820",
				"allowedIPs": ["0.0.0.0/0"],
				"keepAlive": 25
			}],
			"mtu": 1280
		}
	}`

	infos := []*panel.NodeInfo{{
		Id: 1, Tag: "node1", Common: &panel.CommonNode{
			Routes: []panel.Route{
				{Id: 10, Action: "default_out", ActionValue: &wg1},
				{Id: 11, Action: "default_out", ActionValue: &wg2},
			},
		},
	}}

	dnsConfig, outbounds, routerConfig, obsConfig, defaultTags, err := GetCustomConfig(infos)
	if err != nil {
		t.Fatalf("GetCustomConfig failed: %v", err)
	}
	if dnsConfig == nil {
		t.Fatal("dnsConfig is nil")
	}
	if len(defaultTags) != 2 {
		t.Fatalf("expected 2 defaultTags, got: %v", defaultTags)
	}
	if defaultTags[0] != "38454722" || defaultTags[1] != "38454723" {
		t.Fatalf("unexpected defaultTags: %v", defaultTags)
	}
	if obsConfig == nil {
		t.Fatal("obsConfig is nil")
	}
	if len(routerConfig.BalancingRule) == 0 {
		t.Fatal("expected BalancingRule in routerConfig")
	}
	if routerConfig.BalancingRule[0].Tag != "default_balancer" {
		t.Fatalf("expected balancer tag default_balancer, got: %s", routerConfig.BalancingRule[0].Tag)
	}

	foundRule := false
	for _, r := range routerConfig.Rule {
		if r.GetBalancingTag() == "default_balancer" {
			foundRule = true
			break
		}
	}
	if !foundRule {
		t.Fatal("expected router rule with balancerTag default_balancer")
	}

	found1, found2 := false, false
	for _, o := range outbounds {
		if o.Tag == "38454722" {
			found1 = true
		}
		if o.Tag == "38454723" {
			found2 = true
		}
	}
	if !found1 || !found2 {
		t.Fatalf("outbounds missing wg1 or wg2: found1=%v, found2=%v", found1, found2)
	}
}

func TestDefaultOutboundArrayParsesMultipleOutbounds(t *testing.T) {
	arrayJson := `[
		{
			"tag": "wg_arr_1",
			"protocol": "wireguard",
			"settings": {
				"secretKey": "COYrxmQRV27b/5XUMrhxa70XhkT5JFAkYqLARDNKSW4=",
				"address": ["10.2.0.2/32"],
				"peers": [{
					"publicKey": "NfKOMtk2fuDycbQXv36yk5mfdgDA8/8SN6amCdFrKxQ=",
					"endpoint": "188.214.152.226:51820",
					"allowedIPs": ["0.0.0.0/0"]
				}]
			}
		},
		{
			"tag": "wg_arr_2",
			"protocol": "wireguard",
			"settings": {
				"secretKey": "COYrxmQRV27b/5XUMrhxa70XhkT5JFAkYqLARDNKSW4=",
				"address": ["10.2.0.3/32"],
				"peers": [{
					"publicKey": "NfKOMtk2fuDycbQXv36yk5mfdgDA8/8SN6amCdFrKxQ=",
					"endpoint": "188.214.152.227:51820",
					"allowedIPs": ["0.0.0.0/0"]
				}]
			}
		}
	]`

	infos := []*panel.NodeInfo{{
		Id: 1, Tag: "node1", Common: &panel.CommonNode{
			Routes: []panel.Route{
				{Id: 20, Action: "default_out", ActionValue: &arrayJson},
			},
		},
	}}

	_, _, _, obsConfig, defaultTags, err := GetCustomConfig(infos)
	if err != nil {
		t.Fatalf("GetCustomConfig with array failed: %v", err)
	}
	if len(defaultTags) != 2 || defaultTags[0] != "wg_arr_1" || defaultTags[1] != "wg_arr_2" {
		t.Fatalf("expected [wg_arr_1, wg_arr_2], got: %v", defaultTags)
	}
	if obsConfig == nil {
		t.Fatal("obsConfig is nil")
	}
}
