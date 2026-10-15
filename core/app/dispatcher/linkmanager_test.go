package dispatcher

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/xtls/xray-core/common/buf"
)

type linkManagerTestWriter struct {
	closed atomic.Int32
}

func (*linkManagerTestWriter) WriteMultiBuffer(buf.MultiBuffer) error { return nil }

func (w *linkManagerTestWriter) Close() error {
	w.closed.Add(1)
	return nil
}

type linkManagerTestReader struct {
	interrupted atomic.Int32
}

func (*linkManagerTestReader) ReadMultiBuffer() (buf.MultiBuffer, error) { return nil, nil }

func (r *linkManagerTestReader) Interrupt() {
	r.interrupted.Add(1)
}

func TestLinkManagerShrinksAfterConnectionBurst(t *testing.T) {
	manager := newLinkManager()
	writers := make([]*ManagedWriter, 4096)
	for i := range writers {
		writers[i] = &ManagedWriter{writer: &linkManagerTestWriter{}, manager: manager}
		if !manager.AddLink(writers[i], nil) {
			t.Fatal("AddLink rejected a link on an open manager")
		}
	}

	for _, writer := range writers[1:] {
		if err := writer.Close(); err != nil {
			t.Fatalf("closing writer: %v", err)
		}
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	if got := len(manager.links); got != 1 {
		t.Fatalf("active links = %d, want 1", got)
	}
	if manager.highWater > minLinkMapShrinkSize/4 {
		t.Fatalf("retained link map high-water mark = %d, want at most %d", manager.highWater, minLinkMapShrinkSize/4)
	}
}

func TestLinkManagerCloseAllReleasesLinksAndRejectsAdds(t *testing.T) {
	manager := newLinkManager()
	writer := &linkManagerTestWriter{}
	reader := &linkManagerTestReader{}
	managedWriter := &ManagedWriter{writer: writer, manager: manager}
	if !manager.AddLink(managedWriter, reader) {
		t.Fatal("AddLink rejected a link on an open manager")
	}

	manager.CloseAll()
	manager.CloseAll()

	if got := writer.closed.Load(); got != 1 {
		t.Fatalf("writer close count = %d, want 1", got)
	}
	if got := reader.interrupted.Load(); got != 1 {
		t.Fatalf("reader interrupt count = %d, want 1", got)
	}
	if manager.links != nil {
		t.Fatal("CloseAll retained the link map")
	}
	if manager.AddLink(&ManagedWriter{writer: &linkManagerTestWriter{}, manager: manager}, nil) {
		t.Fatal("AddLink accepted a link after CloseAll")
	}
}

func TestLinkManagerConcurrentAddRemove(t *testing.T) {
	manager := newLinkManager()
	const workers = 8
	const linksPerWorker = 512
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < linksPerWorker; i++ {
				writer := &ManagedWriter{writer: &linkManagerTestWriter{}, manager: manager}
				if manager.AddLink(writer, nil) {
					manager.RemoveWriter(writer)
				}
			}
		}()
	}
	wg.Wait()

	manager.mu.Lock()
	defer manager.mu.Unlock()
	if got := len(manager.links); got != 0 {
		t.Fatalf("active links after all removals = %d, want 0", got)
	}
	if manager.links != nil {
		t.Fatal("empty link manager retained its map")
	}
}

func TestDefaultDispatcherCloseReleasesTrackedState(t *testing.T) {
	dispatcher := new(DefaultDispatcher)
	manager := newLinkManager()
	writer := &linkManagerTestWriter{}
	reader := &linkManagerTestReader{}
	managedWriter := &ManagedWriter{writer: writer, manager: manager}
	if !manager.AddLink(managedWriter, reader) {
		t.Fatal("AddLink rejected a link on an open manager")
	}
	dispatcher.LinkManagers.Store("user", manager)
	dispatcher.Counter.Store("inbound", new(int))

	if err := dispatcher.Close(); err != nil {
		t.Fatalf("Close returned an error: %v", err)
	}

	if got := writer.closed.Load(); got != 1 {
		t.Fatalf("writer close count = %d, want 1", got)
	}
	if got := reader.interrupted.Load(); got != 1 {
		t.Fatalf("reader interrupt count = %d, want 1", got)
	}
	if _, ok := dispatcher.getLinkManager("new-user"); ok {
		t.Fatal("dispatcher created a link manager after Close")
	}
	if found := syncMapHasEntries(&dispatcher.LinkManagers); found {
		t.Fatal("dispatcher retained link managers after Close")
	}
	if found := syncMapHasEntries(&dispatcher.Counter); found {
		t.Fatal("dispatcher retained traffic counters after Close")
	}
}

func syncMapHasEntries(values *sync.Map) bool {
	found := false
	values.Range(func(_, _ interface{}) bool {
		found = true
		return false
	})
	return found
}
