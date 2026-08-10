package core

import (
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
