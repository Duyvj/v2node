package core

import (
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
	"google.golang.org/protobuf/proto"
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
	for _, value := range []string{`{`, `{"protocol":"freedom"}`, `{"tag":"invalid","protocol":"not-supported"}`, `null`, `[]`, `[null]`, `[{}]`, `[{"tag":"shared","protocol":"freedom"},null]`, `["invalid"]`} {
		nodes[0].Common.Routes[0].ActionValue = &value
		if _, _, _, err := GetCustomConfig(nodes[:1]); err == nil {
			t.Fatal("invalid outbound accepted", value)
		}
	}
}

func TestCustomOutboundArrayCompatibility(t *testing.T) {
	object := `{"tag":"shared","protocol":"freedom","settings":{}}`
	nodes := []*panel.NodeInfo{{Tag: "one", Common: &panel.CommonNode{Routes: []panel.Route{{Id: 6, Action: "default_out", ActionValue: &object}}}}}
	_, objectOut, objectRoutes, err := GetCustomConfig(nodes)
	if err != nil {
		t.Fatal(err)
	}
	array := " [" + object + "] "
	nodes[0].Common.Routes[0].ActionValue = &array
	_, arrayOut, arrayRoutes, err := GetCustomConfig(nodes)
	if err != nil {
		t.Fatal(err)
	}
	if len(arrayOut) != len(objectOut) || !proto.Equal(arrayRoutes, objectRoutes) {
		t.Fatal("single outbound array changed routing")
	}
	for i := range objectOut {
		if !proto.Equal(objectOut[i], arrayOut[i]) {
			t.Fatal("single outbound array changed outbound", i)
		}
	}
	array = `[{"tag":"shared","protocol":"freedom","proxySettings":{"tag":"helper"}},{"tag":"helper","protocol":"freedom"}]`
	_, arrayOut, arrayRoutes, err = GetCustomConfig(nodes)
	if err != nil {
		t.Fatal(err)
	}
	if len(arrayOut) != 5 || len(arrayRoutes.Rule) != 2 || arrayRoutes.Rule[1].GetTag() != "shared" {
		t.Fatal("array must register helper outbounds and route only to its first entry")
	}
	array = `[{"tag":"shared","protocol":"freedom"},{"tag":"shared","protocol":"blackhole"}]`
	if _, _, _, err := GetCustomConfig(nodes); err == nil {
		t.Fatal("conflicting tags within array accepted")
	}
}
