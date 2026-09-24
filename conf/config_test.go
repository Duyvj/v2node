package conf

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestExistingNodeConfigNeedsNoExtraSettings(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	for _, body := range []string{`{"Nodes":[{"ApiHost":"https://panel.example","NodeID":1}]}`, `{"Nodes":[{"NodeID":1}],"Log":{"Level":"warning"}}`} {
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		c := New()
		if err := c.LoadFromPath(p); err != nil {
			t.Fatal(err)
		}
		if len(c.NodeConfigs) != 1 || c.NodeConfigs[0].RetryCount == nil || *c.NodeConfigs[0].RetryCount != DefaultNodeRetryCount {
			t.Fatal("existing node config did not load defaults")
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
