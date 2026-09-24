package conf

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestDefaultsAndExplicitResourceConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	for _, tc := range []struct {
		body   string
		buffer int32
	}{{`{"Nodes":[{"ApiHost":"https://panel.example","NodeID":1}]}`, 128}, {`{"ConnectionConfig":{"BufferSize":32},"Resource":{"GOGC":80,"MemoryLimitMB":256},"Nodes":[{"NodeID":1}]}`, 32}} {
		if err := os.WriteFile(p, []byte(tc.body), 0600); err != nil {
			t.Fatal(err)
		}
		c := New()
		if err := c.LoadFromPath(p); err != nil {
			t.Fatal(err)
		}
		if c.ConnectionConfig.BufferSize != tc.buffer || c.ConnectionConfig.Handshake != 4 || c.ConnectionConfig.ConnIdle != 120 {
			t.Fatalf("bad defaults: %+v", c.ConnectionConfig)
		}
	}
}

func TestExamplesLoad(t *testing.T) {
	for _, name := range []string{"config.example.json", "config.low-memory.example.json"} {
		c := New()
		if err := c.LoadFromPath(filepath.Join("..", name)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
func TestWatcherCoalescesAtomicReplacementAndStops(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	os.WriteFile(p, []byte("old"), 0600)
	c := New()
	var calls atomic.Int32
	if err := c.Watch(p, func() { calls.Add(1) }); err != nil {
		t.Fatal(err)
	}
	defer c.CloseWatch()
	next := filepath.Join(dir, "new.json")
	os.WriteFile(next, []byte("new"), 0600)
	os.Remove(p)
	os.Rename(next, p)
	deadline := time.Now().Add(3 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if calls.Load() != 1 {
		t.Fatalf("reload calls=%d", calls.Load())
	}
	c.CloseWatch()
	os.WriteFile(p, []byte("after close"), 0600)
	time.Sleep(50 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatal("closed watcher still signalled")
	}
}
