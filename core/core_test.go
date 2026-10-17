package core

import "testing"

func TestPolicyReleasesConnectionStateQuickly(t *testing.T) {
	cfg, err := buildPolicyConfig()
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Level[0]
	if p.Buffer == nil || p.Buffer.Connection != 0 {
		t.Fatalf("per-connection buffer = %v, want 0", p.Buffer)
	}
	to := p.Timeout
	if to.UplinkOnly.Value != 1 || to.DownlinkOnly.Value != 1 {
		t.Fatalf("uplinkOnly/downlinkOnly = %d/%d, want 1/1", to.UplinkOnly.Value, to.DownlinkOnly.Value)
	}
	if to.Handshake.Value != 4 || to.ConnectionIdle.Value != 120 {
		t.Fatalf("handshake/connIdle = %d/%d, want 4/120", to.Handshake.Value, to.ConnectionIdle.Value)
	}
	if !p.Stats.UserUplink || !p.Stats.UserDownlink {
		t.Fatal("user traffic stats must stay enabled")
	}
}
