package cmd

import (
	"os"
	"runtime/debug"
	"testing"
)

func TestRuntimeDefaultsNeedNoConfigAndRespectEnvironment(t *testing.T) {
	t.Setenv("GOGC", "")
	if err := os.Unsetenv("GOGC"); err != nil {
		t.Fatal(err)
	}
	previous := debug.SetGCPercent(100)
	defer debug.SetGCPercent(previous)
	applyRuntimeDefaults()
	if got := debug.SetGCPercent(100); got != 80 {
		t.Fatalf("default GOGC=%d", got)
	}
	t.Setenv("GOGC", "120")
	debug.SetGCPercent(120)
	applyRuntimeDefaults()
	if got := debug.SetGCPercent(100); got != 120 {
		t.Fatalf("explicit GOGC overridden: %d", got)
	}
}
