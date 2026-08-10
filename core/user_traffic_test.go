package core

import (
	"runtime"
	"sync"
	"testing"

	"github.com/wyx2685/v2node/common/counter"
	"github.com/wyx2685/v2node/core/app/dispatcher"
)

func TestGetUserTrafficSliceResetsKnownAndDropsUnknownCounters(t *testing.T) {
	const (
		tag   = "node-a"
		email = "node-a|known-user"
	)
	tc := counter.NewTrafficCounter()
	known := tc.GetCounter(email)
	known.UpCounter.Add(1_500)
	known.DownCounter.Add(2_500)
	tc.GetCounter("node-a|deleted-user")

	d := &dispatcher.DefaultDispatcher{}
	d.Counter.Store(tag, tc)
	vc := &V2Core{
		users:      &UserMap{uidMap: map[string]int{email: 42}},
		dispatcher: d,
	}

	traffic, err := vc.GetUserTrafficSlice(tag, 1)
	if err != nil {
		t.Fatalf("GetUserTrafficSlice() error = %v", err)
	}
	if len(traffic) != 1 || traffic[0].UID != 42 || traffic[0].Upload != 1_500 || traffic[0].Download != 2_500 {
		t.Fatalf("unexpected traffic: %#v", traffic)
	}
	if known.UpCounter.Load() != 0 || known.DownCounter.Load() != 0 {
		t.Fatal("reported counters were not reset")
	}
	if _, ok := tc.Counters.Load("node-a|deleted-user"); ok {
		t.Fatal("unknown-user counter was retained")
	}
}

func TestTrafficSwapDoesNotLoseConcurrentBytes(t *testing.T) {
	const (
		tag        = "node-concurrent"
		email      = "node-concurrent|known-user"
		increments = 200_000
	)
	tc := counter.NewTrafficCounter()
	storage := tc.GetCounter(email)
	d := &dispatcher.DefaultDispatcher{}
	d.Counter.Store(tag, tc)
	vc := &V2Core{
		users:      &UserMap{uidMap: map[string]int{email: 7}},
		dispatcher: d,
	}

	var wg sync.WaitGroup
	wg.Add(1)
	done := make(chan struct{})
	go func() {
		defer wg.Done()
		defer close(done)
		for i := 0; i < increments; i++ {
			storage.UpCounter.Add(1)
			if i%100 == 0 {
				runtime.Gosched()
			}
		}
	}()

	var reported int64
	for {
		traffic, err := vc.GetUserTrafficSlice(tag, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range traffic {
			reported += item.Upload
		}
		select {
		case <-done:
			wg.Wait()
			reported += storage.UpCounter.Swap(0)
			if reported != increments {
				t.Fatalf("reported + remaining bytes = %d, want %d", reported, increments)
			}
			return
		default:
			runtime.Gosched()
		}
	}
}
