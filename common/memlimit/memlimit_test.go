package memlimit

import (
	"os"
	"runtime/debug"
	"testing"
)

const meminfo8G = "MemTotal:        8000000 kB\nMemFree:         1000000 kB\n"

func TestAvailableUsesMemTotal(t *testing.T) {
	if got := available(meminfo8G, "", ""); got != 8000000*1024 {
		t.Fatalf("available = %d, want %d", got, 8000000*1024)
	}
}

func TestAvailableUsesSmallerCgroupLimit(t *testing.T) {
	if got := available(meminfo8G, "2147483648\n", ""); got != 2147483648 {
		t.Fatalf("cgroup v2 limit: available = %d", got)
	}
	if got := available(meminfo8G, "", "1073741824\n"); got != 1073741824 {
		t.Fatalf("cgroup v1 limit: available = %d", got)
	}
}

func TestAvailableIgnoresUnlimitedCgroup(t *testing.T) {
	if got := available(meminfo8G, "max\n", "9223372036854771712\n"); got != 8000000*1024 {
		t.Fatalf("available = %d, want MemTotal", got)
	}
}

func TestApplyLeavesExplicitGOMEMLIMIT(t *testing.T) {
	before := debug.SetMemoryLimit(-1)
	t.Setenv("GOMEMLIMIT", "512MiB")
	if got := Apply(); got != 0 {
		t.Fatalf("Apply = %d with GOMEMLIMIT set, want 0", got)
	}
	if now := debug.SetMemoryLimit(-1); now != before {
		t.Fatalf("memory limit changed to %d although GOMEMLIMIT is set", now)
	}
}

func TestApplySetsThreeQuartersOfMachineMemory(t *testing.T) {
	before := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(before) })
	t.Setenv("GOMEMLIMIT", "")
	os.Unsetenv("GOMEMLIMIT")

	got := Apply()
	if got <= 0 {
		t.Fatalf("Apply = %d, want a positive limit", got)
	}
	if now := debug.SetMemoryLimit(-1); now != got {
		t.Fatalf("runtime limit = %d, Apply returned %d", now, got)
	}
}
