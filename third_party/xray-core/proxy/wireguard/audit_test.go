package wireguard

import (
	"fmt"
	"net"
	"testing"
)

func TestAuditDNSCacheRemainsBounded(t *testing.T) {
	h := &Handler{cache: make(map[string]entry)}
	lookup := func(string) ([]net.IP, uint32, error) { return []net.IP{net.ParseIP("192.0.2.1")}, 3600, nil }
	for i := 0; i < 10000; i++ {
		if _, err := h.resolveDomain(fmt.Sprintf("host-%d.example", i), DeviceConfig_FORCE_IP4, lookup); err != nil {
			t.Fatal(err)
		}
	}
	if len(h.cache) > 4096 {
		t.Fatal("DNS cache grew without bound", len(h.cache))
	}
}
