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
			"secretKey": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
			"address": ["10.2.0.2/32"],
			"peers": [{
				"publicKey": "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=",
				"endpoint": "192.0.2.10:51820",
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
			"secretKey": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
			"address": ["10.2.0.3/32"],
			"peers": [{
				"publicKey": "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=",
				"endpoint": "192.0.2.11:51820",
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

	dnsConfig, outbounds, routerConfig, obsConfig, defaultGroups, err := GetCustomConfig(infos)
	if err != nil {
		t.Fatalf("GetCustomConfig failed: %v", err)
	}
	if dnsConfig == nil {
		t.Fatal("dnsConfig is nil")
	}
	balancerTag := defaultBalancerTag("node1")
	defaultTags := defaultGroups[balancerTag]
	if len(defaultGroups) != 1 || len(defaultTags) != 2 {
		t.Fatalf("expected one group with 2 default tags, got: %v", defaultGroups)
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
	if routerConfig.BalancingRule[0].Tag != balancerTag {
		t.Fatalf("expected balancer tag %s, got: %s", balancerTag, routerConfig.BalancingRule[0].Tag)
	}

	foundRule := false
	for _, r := range routerConfig.Rule {
		if r.GetBalancingTag() == balancerTag {
			foundRule = true
			break
		}
	}
	if !foundRule {
		t.Fatalf("expected router rule with balancerTag %s", balancerTag)
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
				"secretKey": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
				"address": ["10.2.0.2/32"],
				"peers": [{
					"publicKey": "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=",
					"endpoint": "192.0.2.10:51820",
					"allowedIPs": ["0.0.0.0/0"]
				}]
			}
		},
		{
			"tag": "wg_arr_2",
			"protocol": "wireguard",
			"settings": {
				"secretKey": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
				"address": ["10.2.0.3/32"],
				"peers": [{
					"publicKey": "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=",
					"endpoint": "192.0.2.11:51820",
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

	_, _, _, obsConfig, defaultGroups, err := GetCustomConfig(infos)
	if err != nil {
		t.Fatalf("GetCustomConfig with array failed: %v", err)
	}
	defaultTags := defaultGroups[defaultBalancerTag("node1")]
	if len(defaultGroups) != 1 || len(defaultTags) != 2 || defaultTags[0] != "wg_arr_1" || defaultTags[1] != "wg_arr_2" {
		t.Fatalf("expected [wg_arr_1, wg_arr_2], got: %v", defaultGroups)
	}
	if obsConfig == nil {
		t.Fatal("obsConfig is nil")
	}
}

func TestDefaultOutboundsAreScopedToTheirInbound(t *testing.T) {
	first := `{"tag":"wg-node-81","protocol":"freedom","settings":{}}`
	second := `{"tag":"wg-node-82","protocol":"freedom","settings":{}}`
	infos := []*panel.NodeInfo{
		{Id: 81, Tag: "node-81", Common: &panel.CommonNode{Routes: []panel.Route{{Id: 1, Action: "default_out", ActionValue: &first}}}},
		{Id: 82, Tag: "node-82", Common: &panel.CommonNode{Routes: []panel.Route{{Id: 2, Action: "default_out", ActionValue: &second}}}},
	}
	_, _, routerConfig, _, groups, err := GetCustomConfig(infos)
	if err != nil {
		t.Fatal(err)
	}
	for inboundTag, want := range map[string]string{"node-81": "wg-node-81", "node-82": "wg-node-82"} {
		group := defaultBalancerTag(inboundTag)
		if got := groups[group]; len(got) != 1 || got[0] != want {
			t.Fatalf("%s candidates = %v, want only %s", inboundTag, got, want)
		}
		found := false
		for _, rule := range routerConfig.Rule {
			if len(rule.InboundTag) == 1 && rule.InboundTag[0] == inboundTag && rule.GetBalancingTag() == group {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing isolated routing rule for %s", inboundTag)
		}
	}
}

func TestDefaultOutboundRejectsEmptyArray(t *testing.T) {
	for _, invalid := range []string{`[]`, `[null]`, `null`, `[null,{"tag":"a","protocol":"freedom"}]`} {
		t.Run(invalid, func(t *testing.T) {
			_, _, _, _, _, err := GetCustomConfig([]*panel.NodeInfo{{
				Id: 81, Tag: "node-81", Common: &panel.CommonNode{Routes: []panel.Route{{Id: 1, Action: "default_out", ActionValue: &invalid}}},
			}})
			if err == nil {
				t.Fatal("invalid default_out must not fall back to the VPS direct outbound")
			}
		})
	}
}

func TestSharedOutboundIsAllowedButConflictingTagsAreRejected(t *testing.T) {
	first := `{"tag":"shared-wg","protocol":"freedom","settings":{}}`
	for _, second := range []string{first, `{"tag":"shared-wg","protocol":"blackhole","settings":{}}`} {
		_, _, _, _, groups, err := GetCustomConfig([]*panel.NodeInfo{
			{Id: 1, Tag: "node1", Common: &panel.CommonNode{Routes: []panel.Route{{Action: "default_out", ActionValue: &first}}}},
			{Id: 2, Tag: "node2", Common: &panel.CommonNode{Routes: []panel.Route{{Action: "default_out", ActionValue: &second}}}},
		})
		if second == first {
			if err != nil || len(groups) != 2 {
				t.Fatalf("same shared config must work in separate groups: %v", err)
			}
		} else if err == nil {
			t.Fatal("conflicting outbound definitions were silently deduplicated")
		}
	}
}
