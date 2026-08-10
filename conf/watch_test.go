package conf

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatchAddFailureLeavesNoLifecycleState(t *testing.T) {
	c := New()
	missing := filepath.Join(t.TempDir(), "missing", "config.json")
	if err := c.Watch(missing, func() {}); err == nil {
		t.Fatal("watching a missing path succeeded")
	}
	c.watchMu.Lock()
	hasCancel := c.watchCancel != nil
	hasDone := c.watchDone != nil
	c.watchMu.Unlock()
	if hasCancel || hasDone {
		t.Fatal("failed watcher retained cancellation state")
	}
}

func TestWatchCloseJoinsWatcher(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeTestConfig(path); err != nil {
		t.Fatal(err)
	}
	c := New()
	if err := c.Watch(path, func() {}); err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() {
		c.CloseWatch()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("CloseWatch did not join the watcher goroutine")
	}
}

func writeTestConfig(path string) error {
	return os.WriteFile(path, []byte(`{"Nodes":[]}`), 0600)
}
