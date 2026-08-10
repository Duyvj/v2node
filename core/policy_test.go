package core

import (
	"testing"

	"github.com/wyx2685/v2node/conf"
)

func TestRAMPolicyDisablesDuplicateStatsAndUses64KiBBuffer(t *testing.T) {
	policy := buildLevelPolicy(conf.RuntimeConfig{})
	if policy.StatsUserUplink || policy.StatsUserDownlink {
		t.Fatal("duplicate Xray per-user stats are enabled")
	}
	if policy.BufferSize == nil || *policy.BufferSize != 64 {
		t.Fatalf("buffer size = %v, want 64 KiB", policy.BufferSize)
	}
}
