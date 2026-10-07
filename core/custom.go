package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"strings"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/xtls/xray-core/app/dns"
	"github.com/xtls/xray-core/app/router"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/core"
	coreConf "github.com/xtls/xray-core/infra/conf"
	"google.golang.org/protobuf/proto"
)

func parseRouteOutbounds(value string) ([]*coreConf.OutboundDetourConfig, error) {
	raw := bytes.TrimSpace([]byte(value))
	var outbounds []*coreConf.OutboundDetourConfig
	if len(raw) > 0 && raw[0] == '[' {
		if err := json.Unmarshal(raw, &outbounds); err != nil {
			return nil, err
		}
	} else {
		var outbound *coreConf.OutboundDetourConfig
		if err := json.Unmarshal(raw, &outbound); err != nil {
			return nil, err
		}
		outbounds = []*coreConf.OutboundDetourConfig{outbound}
	}
	if len(outbounds) == 0 {
		return nil, fmt.Errorf("outbound array is empty")
	}
	for i, outbound := range outbounds {
		if outbound == nil || outbound.Tag == "" {
			return nil, fmt.Errorf("outbound %d: tag is required", i)
		}
	}
	return outbounds, nil
}

// hasPublicIPv6 checks if the machine has a public IPv6 address
func hasPublicIPv6() bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipNet.IP
		// Check if it's IPv6, not loopback, not link-local, not private/ULA
		if ip.To4() == nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsPrivate() {
			return true
		}
	}
	return false
}

func GetCustomConfig(infos []*panel.NodeInfo) (*dns.Config, []*core.OutboundHandlerConfig, *router.Config, error) {
	//dns
	queryStrategy := "UseIPv4v6"
	if !hasPublicIPv6() {
		queryStrategy = "UseIPv4"
	}
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
		return nil, nil, nil, err
	}
	coreOutboundConfig := append([]*core.OutboundHandlerConfig{}, defaultoutbound)
	block, err := buildBlockOutbound()
	if err != nil {
		return nil, nil, nil, err
	}
	coreOutboundConfig = append(coreOutboundConfig, block)
	dns, err := buildDnsOutbound()
	if err != nil {
		return nil, nil, nil, err
	}
	coreOutboundConfig = append(coreOutboundConfig, dns)

	outboundByTag := map[string]*core.OutboundHandlerConfig{defaultoutbound.Tag: defaultoutbound, block.Tag: block, dns.Tag: dns}
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

	for _, info := range infos {
		if info == nil || info.Common == nil {
			return nil, nil, nil, fmt.Errorf("missing route node configuration")
		}
		if len(info.Common.Routes) == 0 {
			continue
		}
		for _, route := range info.Common.Routes {
			switch route.Action {
			case "dns":
				if route.ActionValue == nil {
					continue
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
			case "route", "route_ip", "default_out":
				if route.ActionValue == nil {
					return nil, nil, nil, fmt.Errorf("route %d: missing outbound configuration", route.Id)
				}
				outbounds, err := parseRouteOutbounds(*route.ActionValue)
				if err != nil {
					return nil, nil, nil, fmt.Errorf("route %d: invalid outbound JSON: %w", route.Id, err)
				}
				for _, outbound := range outbounds {
					built, err := outbound.Build()
					if err != nil {
						return nil, nil, nil, fmt.Errorf("route %d: build outbound %s: %w", route.Id, outbound.Tag, err)
					}
					if existing := outboundByTag[outbound.Tag]; existing != nil {
						if !proto.Equal(existing, built) {
							return nil, nil, nil, fmt.Errorf("route %d: conflicting outbound tag %s", route.Id, outbound.Tag)
						}
					} else {
						outboundByTag[outbound.Tag] = built
						coreOutboundConfig = append(coreOutboundConfig, built)
					}
				}
				// An array can include helper outbounds referenced by the first one.
				// Route traffic to its first entry, as with Xray's default outbound.
				rule := map[string]any{"inboundTag": info.Tag, "outboundTag": outbounds[0].Tag}
				switch route.Action {
				case "route":
					rule["domain"] = route.Match
				case "route_ip":
					rule["ip"] = route.Match
				case "default_out":
					rule["network"] = "tcp,udp"
				}
				rawRule, err := json.Marshal(rule)
				if err != nil {
					return nil, nil, nil, err
				}
				coreRouterConfig.RuleList = append(coreRouterConfig.RuleList, rawRule)
			default:
				continue
			}
		}
	}
	DnsConfig, err := coreDnsConfig.Build()
	if err != nil {
		return nil, nil, nil, err
	}
	RouterConfig, err := coreRouterConfig.Build()
	if err != nil {
		return nil, nil, nil, err
	}
	return DnsConfig, coreOutboundConfig, RouterConfig, nil
}
