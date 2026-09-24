package core

import (
	"github.com/wyx2685/v2node/common/counter"
	"github.com/wyx2685/v2node/core/app/dispatcher"
	"sync"
	"testing"
)

func TestTrafficAcknowledgementPreservesConcurrentBytes(t *testing.T) {
	vc := &V2Core{users: &UserMap{uidMap: map[string]int{"a": 1, "b": 1}}, dispatcher: &dispatcher.DefaultDispatcher{}}
	c := counter.NewTrafficCounter()
	vc.dispatcher.Counter.Store("node", c)
	c.GetCounter("a").UpCounter.Add(100)
	c.GetCounter("b").UpCounter.Add(200)
	traffic, commit := vc.SnapshotUserTraffic("node", 0)
	if len(traffic) != 2 {
		t.Fatal("missing credential counter")
	}
	// No acknowledgement on failed reports: an immediate snapshot is identical.
	again, _ := vc.SnapshotUserTraffic("node", 0)
	if len(again) != 2 {
		t.Fatal("failed report lost bytes")
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				c.GetCounter("a").UpCounter.Add(1)
			}
		}()
	}
	commit()
	commit()
	wg.Wait()
	if c.GetUpCount("a") != 4000 || c.GetUpCount("b") != 0 {
		t.Fatalf("wrong remaining counters: %d %d", c.GetUpCount("a"), c.GetUpCount("b"))
	}
}
