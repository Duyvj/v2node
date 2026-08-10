package dispatcher

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/wyx2685/v2node/common/counter"
	"github.com/xtls/xray-core/common/buf"
)

type testWriter struct {
	closed atomic.Bool
}

func (*testWriter) WriteMultiBuffer(buf.MultiBuffer) error { return nil }
func (w *testWriter) Close() error {
	w.closed.Store(true)
	return nil
}

type testReader struct {
	interrupted atomic.Bool
}

func (*testReader) ReadMultiBuffer() (buf.MultiBuffer, error) { return nil, nil }
func (r *testReader) Interrupt()                              { r.interrupted.Store(true) }

func TestEmptyLinkManagerRetiresItself(t *testing.T) {
	d := &DefaultDispatcher{}
	d.EnableUser("user")
	writer := &testWriter{}
	managed, _, ok := d.registerUserLink("tag", "user", writer, &testReader{})
	if !ok {
		t.Fatal("active user was rejected")
	}
	if err := managed.Close(); err != nil {
		t.Fatal(err)
	}
	if _, retained := d.LinkManagers.Load("user"); retained {
		t.Fatal("empty LinkManager remained in the dispatcher map")
	}
	if !writer.closed.Load() {
		t.Fatal("underlying writer was not closed")
	}
}

func TestUserConnectionChurnLeavesNoHistoricalManagers(t *testing.T) {
	d := &DefaultDispatcher{}
	for i := 0; i < 10_000; i++ {
		email := fmt.Sprintf("user-%d", i)
		d.EnableUser(email)
		managed, _, ok := d.registerUserLink("tag", email, &testWriter{}, &testReader{})
		if !ok {
			t.Fatalf("registration %d rejected", i)
		}
		if err := managed.Close(); err != nil {
			t.Fatal(err)
		}
		d.DisableUser("tag", email)
	}
	managerCount := 0
	d.LinkManagers.Range(func(_, _ interface{}) bool {
		managerCount++
		return true
	})
	if managerCount != 0 {
		t.Fatalf("connection churn retained %d managers", managerCount)
	}
	d.userMu.RLock()
	activeCount := len(d.activeUsers)
	d.userMu.RUnlock()
	if activeCount != 0 {
		t.Fatalf("user churn retained %d active-user entries", activeCount)
	}
}

func TestConcurrentFirstLinksShareOneManager(t *testing.T) {
	d := &DefaultDispatcher{}
	d.EnableUser("user")
	const links = 256
	writers := make([]*ManagedWriter, links)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			managed, _, ok := d.registerUserLink("tag", "user", &testWriter{}, &testReader{})
			if !ok {
				t.Errorf("registration %d rejected", i)
				return
			}
			writers[i] = managed
		}(i)
	}
	wg.Wait()
	value, ok := d.LinkManagers.Load("user")
	if !ok {
		t.Fatal("shared manager missing")
	}
	if got := value.(*LinkManager).Len(); got != links {
		t.Fatalf("manager owns %d links, want %d", got, links)
	}
	for _, writer := range writers {
		if writer != nil {
			_ = writer.Close()
		}
	}
	if _, retained := d.LinkManagers.Load("user"); retained {
		t.Fatal("manager remained after every concurrent link closed")
	}
}

func TestDisableUserPreventsManagerAndCounterReinsertion(t *testing.T) {
	d := &DefaultDispatcher{}
	d.EnableUser("user")
	const attempts = 1000
	var wg sync.WaitGroup
	registered := make(chan *ManagedWriter, attempts)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < attempts/8; j++ {
				managed, _, ok := d.registerUserLink("tag", "user", &testWriter{}, &testReader{})
				if ok {
					registered <- managed
				}
			}
		}()
	}
	d.DisableUser("tag", "user")
	wg.Wait()
	close(registered)
	for writer := range registered {
		_ = writer.Close()
	}
	if _, ok := d.LinkManagers.Load("user"); ok {
		t.Fatal("disabled user reinserted a manager")
	}
	if value, ok := d.Counter.Load("tag"); ok {
		if _, retained := value.(*counter.TrafficCounter).Counters.Load("user"); retained {
			t.Fatal("disabled user reinserted a traffic counter")
		}
	}
}

func TestDispatcherCloseReleasesEveryLink(t *testing.T) {
	d := &DefaultDispatcher{}
	d.EnableUser("user")
	writers := make([]*testWriter, 32)
	readers := make([]*testReader, 32)
	for i := range writers {
		writers[i] = &testWriter{}
		readers[i] = &testReader{}
		if _, _, ok := d.registerUserLink("tag", "user", writers[i], readers[i]); !ok {
			t.Fatal("registration rejected")
		}
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	for i := range writers {
		if !writers[i].closed.Load() || !readers[i].interrupted.Load() {
			t.Fatalf("link %d was not fully released", i)
		}
	}
	if _, ok := d.LinkManagers.Load("user"); ok {
		t.Fatal("dispatcher retained a manager after Close")
	}
}
