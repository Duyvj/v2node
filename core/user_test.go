package core

import (
	"testing"

	"github.com/wyx2685/v2node/common/counter"
	"github.com/wyx2685/v2node/core/app/dispatcher"
)

func TestGetUserTrafficSliceDropsCountersOfRemovedUsers(t *testing.T) {
	tc := counter.NewTrafficCounter()
	tc.GetCounter("tag|gone")
	tc.GetCounter("tag|live").UpCounter.Store(5000)
	d := new(dispatcher.DefaultDispatcher)
	d.Counter.Store("tag", tc)
	vc := &V2Core{users: &UserMap{uidMap: map[string]int{"tag|live": 7}}, dispatcher: d}

	traffic, err := vc.GetUserTrafficSlice("tag", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(traffic) != 1 || traffic[0].UID != 7 || traffic[0].Upload != 5000 {
		t.Fatalf("traffic = %+v, want only uid 7 with 5000 up", traffic)
	}
	if _, ok := tc.Counters.Load("tag|gone"); ok {
		t.Fatal("counter of a removed user with no traffic is kept forever")
	}
	if _, ok := tc.Counters.Load("tag|live"); !ok {
		t.Fatal("counter of a live user was dropped")
	}
}
