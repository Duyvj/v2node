package conf

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRuntimeConfigNormalizeAndClamp(t *testing.T) {
	r := RuntimeConfig{}
	r.Normalize()
	if r.MinPollIntervalSeconds != 30 || r.MaxPollIntervalSeconds != 3600 || r.BufferSizeKB != 64 ||
		r.MaxTrackedIPsPerUser != 256 || r.MaxTrackedIPsPerNode != 32768 ||
		r.MaxPanelResponseBytes != 16*1024*1024 || r.MaxUsers != 100000 {
		t.Fatalf("unexpected defaults: %+v", r)
	}
	if got := r.ClampPollInterval(0); got != 30*time.Second {
		t.Fatalf("zero interval = %s, want 30s", got)
	}
	if got := r.ClampPollInterval(2 * time.Hour); got != time.Hour {
		t.Fatalf("large interval = %s, want 1h", got)
	}
}

func TestLoadConfigKeepsRuntimeDefaultsWhenOmitted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"Log":{"Level":"warning"},"Nodes":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	c := New()
	if err := c.LoadFromPath(path); err != nil {
		t.Fatal(err)
	}
	if c.Runtime.BufferSizeKB != 64 || c.Runtime.MinPollIntervalSeconds != 30 ||
		c.Runtime.MaxTrackedIPsPerUser != 256 || c.Runtime.MaxTrackedIPsPerNode != 32768 ||
		c.Runtime.MaxPanelResponseBytes != 16*1024*1024 || c.Runtime.MaxUsers != 100000 {
		t.Fatalf("runtime defaults lost during unmarshal: %+v", c.Runtime)
	}
}

func TestParseMemoryLimit(t *testing.T) {
	tests := map[string]int64{
		"448MiB": 448 * 1024 * 1024,
		"1GB":    1000 * 1000 * 1000,
		"65536":  65536,
		"off":    0,
	}
	for input, want := range tests {
		got, err := ParseMemoryLimit(input)
		if err != nil || got != want {
			t.Errorf("ParseMemoryLimit(%q) = %d, %v; want %d", input, got, err, want)
		}
	}
	if _, err := ParseMemoryLimit("not-a-limit"); err == nil {
		t.Fatal("invalid memory limit accepted")
	}
}
