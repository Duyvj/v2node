package core

import (
	panel "github.com/wyx2685/v2node/api/v2board"
	"testing"
)

func TestCustomOutboundDeduplicationAndValidation(t *testing.T) {
	first := `{"tag":"shared","protocol":"freedom","settings":{}}`
	nodes := []*panel.NodeInfo{
		{Tag: "one", Common: &panel.CommonNode{Routes: []panel.Route{{Action: "default_out", ActionValue: &first}}}},
		{Tag: "two", Common: &panel.CommonNode{Routes: []panel.Route{{Action: "route_ip", Match: []string{"192.0.2.1/32"}, ActionValue: &first}}}},
	}
	_, out, routes, err := GetCustomConfig(nodes)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 4 || len(routes.Rule) != 3 {
		t.Fatal("duplicate outbound or missing per-node route")
	}
	conflict := `{"tag":"shared","protocol":"blackhole"}`
	nodes[1].Common.Routes[0].ActionValue = &conflict
	if _, _, _, err := GetCustomConfig(nodes); err == nil {
		t.Fatal("conflicting outbound reused silently")
	}
	for _, value := range []string{`{`, `{"protocol":"freedom"}`, `{"tag":"invalid","protocol":"not-supported"}`} {
		nodes[0].Common.Routes[0].ActionValue = &value
		if _, _, _, err := GetCustomConfig(nodes[:1]); err == nil {
			t.Fatal("invalid outbound accepted", value)
		}
	}
}
