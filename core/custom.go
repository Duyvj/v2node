package core

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/xtls/xray-core/app/dns"
	"github.com/xtls/xray-core/app/observatory"
	"github.com/xtls/xray-core/app/router"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/core"
	coreConf "github.com/xtls/xray-core/infra/conf"
	"google.golang.org/protobuf/proto"
)

func hasOutboundWithTag(list []*core.OutboundHandlerConfig, tag string) bool {
	for _, o := range list {
		if o != nil && o.Tag == tag {
			return true
		}
	}
	return false
}

func defaultBalancerTag(inboundTag string) string {
	digest := sha256.Sum256([]byte(inboundTag))
	return fmt.Sprintf("default_balancer_%x", digest[:])
}

func appendUniqueTag(tags []string, tag string) []string {
	for _, existing := range tags {
		if existing == tag {
			return tags
		}
	}
	return append(tags, tag)
}

func resolveRouteOutbounds(value *string, existing []*core.OutboundHandlerConfig) ([]string, []*core.OutboundHandlerConfig, error) {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil, nil, fmt.Errorf("route outbound is missing")
	}
	trimmed := strings.TrimSpace(*value)
	var outbounds []*coreConf.OutboundDetourConfig
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal([]byte(trimmed), &outbounds); err != nil {
			return nil, nil, fmt.Errorf("decode route outbounds array: %w", err)
		}
	} else {
		outbound := &coreConf.OutboundDetourConfig{}
		if err := json.Unmarshal([]byte(trimmed), outbound); err != nil {
			return nil, nil, fmt.Errorf("decode route outbound: %w", err)
		}
		outbounds = append(outbounds, outbound)
	}

	var tags []string
	var builtList []*core.OutboundHandlerConfig

	for _, outbound := range outbounds {
		if outbound == nil {
			return nil, nil, fmt.Errorf("route outbound must not contain null")
		}
		if strings.TrimSpace(outbound.Tag) == "" {
			return nil, nil, fmt.Errorf("route outbound tag is missing")
		}
		if err := hardenFreedomOutbound(outbound); err != nil {
			return nil, nil, fmt.Errorf("secure route outbound: %w", err)
		}
		if err := hardenWireGuardOutbound(outbound); err != nil {
			return nil, nil, fmt.Errorf("secure wireguard outbound: %w", err)
		}
		if err := applyXHTTPStreamDefaults(outbound.StreamSetting); err != nil {
			return nil, nil, fmt.Errorf("apply xhttp outbound defaults: %w", err)
		}
		built, err := outbound.Build()
		if err != nil {
			return nil, nil, fmt.Errorf("build route outbound %q: %w", outbound.Tag, err)
		}
		duplicate := false
		for _, list := range [][]*core.OutboundHandlerConfig{existing, builtList} {
			for _, previous := range list {
				if previous != nil && previous.Tag == built.Tag {
					if !proto.Equal(previous, built) {
						return nil, nil, fmt.Errorf("outbound tag %q has conflicting configurations; use distinct tags", built.Tag)
					}
					duplicate = true
				}
			}
		}
		tags = appendUniqueTag(tags, built.Tag)
		if duplicate {
			continue
		}
		builtList = append(builtList, built)
	}

	if len(tags) == 0 {
		return nil, nil, fmt.Errorf("route outbound must include at least one outbound")
	}
	return tags, builtList, nil
}

func resolveRouteOutbound(value *string, existing []*core.OutboundHandlerConfig) (string, *core.OutboundHandlerConfig, error) {
	tags, built, err := resolveRouteOutbounds(value, existing)
	if err != nil {
		return "", nil, err
	}
	if len(tags) != 1 {
		return "", nil, fmt.Errorf("domain/IP route requires exactly one outbound; use default_out for a pool")
	}
	var b *core.OutboundHandlerConfig
	if len(built) > 0 {
		b = built[0]
	}
	return tags[0], b, nil
}

func GetCustomConfig(infos []*panel.NodeInfo) (*dns.Config, []*core.OutboundHandlerConfig, *router.Config, proto.Message, map[string][]string, error) {
	// Prefer the stable IPv4 egress used by the panel's advertised VPS
	// address. Merely having a public IPv6 address on an interface does not
	// prove that the VPS has a working IPv6 route; broken/black-holed IPv6 is a
	// common cause of intermittent QUIC failures in TikTok and Meta apps.
	queryStrategy := "UseIPv4"
	coreDnsConfig := &coreConf.DNSConfig{
		Servers: []*coreConf.NameServerConfig{
			{
				Address: &coreConf.Address{
					Address: xnet.ParseAddress("localhost"),
				},
			},
		},
		QueryStrategy: queryStrategy,
	}
	//outbound
	defaultoutbound, err := buildDefaultOutbound()
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("build default outbound: %w", err)
	}
	coreOutboundConfig := append([]*core.OutboundHandlerConfig{}, defaultoutbound)
	block, err := buildBlockOutbound()
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("build block outbound: %w", err)
	}
	coreOutboundConfig = append(coreOutboundConfig, block)
	dnsOutbound, err := buildDnsOutbound()
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("build DNS outbound: %w", err)
	}
	coreOutboundConfig = append(coreOutboundConfig, dnsOutbound)

	//route
	domainStrategy := "AsIs"
	dnsRule, _ := json.Marshal(map[string]interface{}{
		"port":        "53",
		"network":     "udp",
		"outboundTag": "dns_out",
	})
	coreRouterConfig := &coreConf.RouterConfig{
		RuleList:       []json.RawMessage{dnsRule},
		DomainStrategy: &domainStrategy,
	}

	defaultOutboundGroups := make(map[string][]string)
	seenInboundTags := make(map[string]bool)
	var defaultOutboundTags []string
	var defaultOutboundInbounds []struct {
		inboundTag  string
		balancerTag string
	}

	for _, info := range infos {
		if info == nil || info.Common == nil {
			return nil, nil, nil, nil, nil, fmt.Errorf("custom routing received an empty node configuration")
		}
		if len(info.Common.Routes) == 0 {
			continue
		}
		if strings.TrimSpace(info.Tag) == "" {
			return nil, nil, nil, nil, nil, fmt.Errorf("node %d has an empty inbound tag", info.Id)
		}
		if seenInboundTags[info.Tag] {
			return nil, nil, nil, nil, nil, fmt.Errorf("duplicate inbound tag %q", info.Tag)
		}
		seenInboundTags[info.Tag] = true
		balancerTag := ""
		for _, route := range info.Common.Routes {
			switch route.Action {
			case "dns":
				if route.ActionValue == nil {
					return nil, nil, nil, nil, nil, fmt.Errorf("node %d route %d: DNS server is missing", info.Id, route.Id)
				}
				server := &coreConf.NameServerConfig{
					Address: &coreConf.Address{
						Address: xnet.ParseAddress(*route.ActionValue),
					},
				}
				if len(route.Match) != 0 {
					server.Domains = route.Match
					server.SkipFallback = true
				}
				coreDnsConfig.Servers = append(coreDnsConfig.Servers, server)
			case "block":
				rule := map[string]interface{}{
					"inboundTag":  info.Tag,
					"domain":      route.Match,
					"outboundTag": "block",
				}
				rawRule, err := json.Marshal(rule)
				if err != nil {
					continue
				}
				coreRouterConfig.RuleList = append(coreRouterConfig.RuleList, rawRule)
			case "block_ip":
				rule := map[string]interface{}{
					"inboundTag":  info.Tag,
					"ip":          route.Match,
					"outboundTag": "block",
				}
				rawRule, err := json.Marshal(rule)
				if err != nil {
					continue
				}
				coreRouterConfig.RuleList = append(coreRouterConfig.RuleList, rawRule)
			case "block_port":
				rule := map[string]interface{}{
					"inboundTag":  info.Tag,
					"port":        strings.Join(route.Match, ","),
					"outboundTag": "block",
				}
				rawRule, err := json.Marshal(rule)
				if err != nil {
					continue
				}
				coreRouterConfig.RuleList = append(coreRouterConfig.RuleList, rawRule)
			case "protocol":
				rule := map[string]interface{}{
					"inboundTag":  info.Tag,
					"protocol":    route.Match,
					"outboundTag": "block",
				}
				rawRule, err := json.Marshal(rule)
				if err != nil {
					continue
				}
				coreRouterConfig.RuleList = append(coreRouterConfig.RuleList, rawRule)
			case "route":
				outboundTag, customOutbound, err := resolveRouteOutbound(route.ActionValue, coreOutboundConfig)
				if err != nil {
					return nil, nil, nil, nil, nil, fmt.Errorf("node %d route %d: %w", info.Id, route.Id, err)
				}
				rule := map[string]interface{}{
					"inboundTag":  info.Tag,
					"domain":      route.Match,
					"outboundTag": outboundTag,
				}
				rawRule, err := json.Marshal(rule)
				if err != nil {
					return nil, nil, nil, nil, nil, fmt.Errorf("node %d route %d: marshal domain route: %w", info.Id, route.Id, err)
				}
				coreRouterConfig.RuleList = append(coreRouterConfig.RuleList, rawRule)
				if customOutbound != nil {
					coreOutboundConfig = append(coreOutboundConfig, customOutbound)
				}
			case "route_ip":
				outboundTag, customOutbound, err := resolveRouteOutbound(route.ActionValue, coreOutboundConfig)
				if err != nil {
					return nil, nil, nil, nil, nil, fmt.Errorf("node %d route %d: %w", info.Id, route.Id, err)
				}
				rule := map[string]interface{}{
					"inboundTag":  info.Tag,
					"ip":          route.Match,
					"outboundTag": outboundTag,
				}
				rawRule, err := json.Marshal(rule)
				if err != nil {
					return nil, nil, nil, nil, nil, fmt.Errorf("node %d route %d: marshal IP route: %w", info.Id, route.Id, err)
				}
				coreRouterConfig.RuleList = append(coreRouterConfig.RuleList, rawRule)
				if customOutbound != nil {
					coreOutboundConfig = append(coreOutboundConfig, customOutbound)
				}
			case "default_out":
				tags, customOutbounds, err := resolveRouteOutbounds(route.ActionValue, coreOutboundConfig)
				if err != nil {
					return nil, nil, nil, nil, nil, fmt.Errorf("node %d route %d: %w", info.Id, route.Id, err)
				}
				if balancerTag == "" {
					balancerTag = defaultBalancerTag(info.Tag)
					defaultOutboundInbounds = append(defaultOutboundInbounds, struct {
						inboundTag  string
						balancerTag string
					}{inboundTag: info.Tag, balancerTag: balancerTag})
				}
				for _, b := range customOutbounds {
					if b != nil && !hasOutboundWithTag(coreOutboundConfig, b.Tag) {
						coreOutboundConfig = append(coreOutboundConfig, b)
					}
				}
				for _, tag := range tags {
					// Identical shared outbounds may appear in several groups; only
					// the group's own candidate list is eligible for sticky routing.
					defaultOutboundGroups[balancerTag] = appendUniqueTag(defaultOutboundGroups[balancerTag], tag)
					defaultOutboundTags = appendUniqueTag(defaultOutboundTags, tag)
				}
			default:
				return nil, nil, nil, nil, nil, fmt.Errorf("node %d route %d: unsupported action %q", info.Id, route.Id, route.Action)
			}
		}
	}

	var obsConfig proto.Message
	for _, group := range defaultOutboundInbounds {
		selectors := defaultOutboundGroups[group.balancerTag]
		if len(selectors) == 0 {
			return nil, nil, nil, nil, nil, fmt.Errorf("inbound %q has no default outbound", group.inboundTag)
		}
		coreRouterConfig.Balancers = append(coreRouterConfig.Balancers, &coreConf.BalancingRule{
			Tag:         group.balancerTag,
			Selectors:   coreConf.StringList(selectors),
			Strategy:    coreConf.StrategyConfig{Type: "roundrobin"},
			FallbackTag: selectors[0],
		})
		rule := map[string]interface{}{
			"inboundTag":  group.inboundTag,
			"network":     "tcp,udp",
			"balancerTag": group.balancerTag,
			"ruleTag":     group.balancerTag,
		}
		rawRule, err := json.Marshal(rule)
		if err != nil {
			return nil, nil, nil, nil, nil, fmt.Errorf("marshal default balancer route for %s: %w", group.inboundTag, err)
		}
		coreRouterConfig.RuleList = append(coreRouterConfig.RuleList, rawRule)
	}
	if len(defaultOutboundTags) > 0 {
		obsConfig = &observatory.Config{
			SubjectSelector:   defaultOutboundTags,
			ProbeUrl:          "http://cp.cloudflare.com/generate_204",
			ProbeInterval:     int64(10 * time.Second),
			EnableConcurrency: true,
		}
	}

	DnsConfig, err := coreDnsConfig.Build()
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	RouterConfig, err := coreRouterConfig.Build()
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	return DnsConfig, coreOutboundConfig, RouterConfig, obsConfig, defaultOutboundGroups, nil
}
