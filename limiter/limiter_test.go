package limiter

import (
	"fmt"
	"runtime"
	"sync"
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/format"
)

func TestSetAliveListAffectsDeviceLimit(t *testing.T) {
	Init()
	const (
		tag  = "node-a"
		uuid = "user-a"
	)
	l := AddLimiter("vmess", tag, []panel.UserInfo{{Id: 7, Uuid: uuid, DeviceLimit: 1}}, map[int]int{7: 0})
	key := format.UserTag(tag, uuid)

	if _, rejected := l.CheckLimit(key, "192.0.2.1", true); rejected {
		t.Fatal("first IP was rejected below the device limit")
	}
	l.SetAliveList(map[int]int{7: 1})
	if _, rejected := l.CheckLimit(key, "192.0.2.2", true); !rejected {
		t.Fatal("new IP was accepted after the device limit was reached")
	}
}

func TestLocalIPsCountAgainstDeviceLimit(t *testing.T) {
	Init()
	const (
		tag  = "node-local"
		uuid = "limited-user"
	)
	l := AddLimiter("vmess", tag, []panel.UserInfo{{Id: 9, Uuid: uuid, DeviceLimit: 2}}, nil, 32, 64)
	key := format.UserTag(tag, uuid)
	for _, ip := range []string{"192.0.2.1", "192.0.2.2"} {
		if _, rejected := l.CheckLimit(key, ip, true); rejected {
			t.Fatalf("IP %s was rejected below the local device limit", ip)
		}
	}
	if _, rejected := l.CheckLimit(key, "192.0.2.3", true); !rejected {
		t.Fatal("third distinct local IP was accepted for a two-device user")
	}
	if total, _ := l.TrackedIPCount(); total != 2 {
		t.Fatalf("tracked IPs = %d, want 2", total)
	}
}

func TestPreviouslyReportedIPCountsWhenPanelAliveSnapshotLags(t *testing.T) {
	Init()
	const (
		tag  = "node-lag"
		uuid = "one-device"
	)
	l := AddLimiter("vmess", tag, []panel.UserInfo{{Id: 10, Uuid: uuid, DeviceLimit: 1}}, nil, 16, 32)
	key := format.UserTag(tag, uuid)
	if _, rejected := l.CheckLimit(key, "192.0.2.1", true); rejected {
		t.Fatal("first IP was rejected")
	}
	_, _ = l.GetOnlineDevice()
	l.SetAliveList(map[int]int{10: 0})
	if _, rejected := l.CheckLimit(key, "192.0.2.1", true); rejected {
		t.Fatal("previously reported IP was rejected")
	}
	if _, rejected := l.CheckLimit(key, "192.0.2.2", true); !rejected {
		t.Fatal("new IP bypassed the device limit while panel alive data lagged")
	}
}

func TestNewIPIsRejectedBeforePreviousIPReconnects(t *testing.T) {
	Init()
	const (
		tag  = "node-lag-new-first"
		uuid = "one-device"
	)
	l := AddLimiter("vmess", tag, []panel.UserInfo{{Id: 10, Uuid: uuid, DeviceLimit: 1}}, nil, 16, 32)
	key := format.UserTag(tag, uuid)
	if _, rejected := l.CheckLimit(key, "192.0.2.1", true); rejected {
		t.Fatal("first IP was rejected")
	}
	_, _ = l.GetOnlineDevice()
	l.SetAliveList(map[int]int{10: 0})
	if _, rejected := l.CheckLimit(key, "192.0.2.2", true); !rejected {
		t.Fatal("new IP bypassed the device limit before the previous IP reconnected")
	}
}

func TestUniqueIPChurnIsHardBounded(t *testing.T) {
	Init()
	const (
		tag  = "node-churn"
		uuid = "unlimited-user"
	)
	l := AddLimiter("vmess", tag, []panel.UserInfo{{Id: 11, Uuid: uuid}}, nil, 64, 1000)
	key := format.UserTag(tag, uuid)
	for i := 0; i < 100_000; i++ {
		if _, rejected := l.CheckLimit(key, fmt.Sprintf("198.51.%d.%d", (i/256)%256, i%256), true); rejected {
			t.Fatalf("unlimited user was rejected at IP %d", i)
		}
	}
	total, users := l.TrackedIPCount()
	if total != 64 || users != 1 {
		t.Fatalf("bounded tracker = (%d IPs, %d users), want (64, 1)", total, users)
	}
	online, err := l.GetOnlineDevice()
	if err != nil {
		t.Fatal(err)
	}
	if len(*online) != 64 {
		t.Fatalf("snapshot has %d IPs, want 64", len(*online))
	}
	if total, _ := l.TrackedIPCount(); total != 0 {
		t.Fatalf("tracker retained %d current IPs after rotation", total)
	}
}

func TestUniqueIPFloodHasBoundedLiveHeap(t *testing.T) {
	Init()
	const (
		tag  = "node-heap"
		uuid = "heap-user"
	)
	l := AddLimiter("vmess", tag, []panel.UserInfo{{Id: 12, Uuid: uuid}}, nil, 64, 128)
	key := format.UserTag(tag, uuid)
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i < 500_000; i++ {
		_, _ = l.CheckLimit(key, fmt.Sprintf("198.%d.%d.%d", (i/65536)%256, (i/256)%256, i%256), true)
	}
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	const maxGrowth = 4 * 1024 * 1024
	if after.HeapAlloc > before.HeapAlloc+maxGrowth {
		t.Fatalf("live heap grew by %d bytes after unique-IP flood; cap is %d", after.HeapAlloc-before.HeapAlloc, maxGrowth)
	}
	if total, _ := l.TrackedIPCount(); total != 64 {
		t.Fatalf("tracker retained %d IPs, want 64", total)
	}
}

func TestNodeWideIPCap(t *testing.T) {
	Init()
	const tag = "node-cap"
	users := make([]panel.UserInfo, 100)
	for i := range users {
		users[i] = panel.UserInfo{Id: i + 1, Uuid: fmt.Sprintf("user-%d", i)}
	}
	l := AddLimiter("vmess", tag, users, nil, 64, 1000)
	for i, user := range users {
		key := format.UserTag(tag, user.Uuid)
		for j := 0; j < 100; j++ {
			if _, rejected := l.CheckLimit(key, fmt.Sprintf("203.%d.%d.%d", i%256, j/256, j%256), true); rejected {
				t.Fatalf("unlimited user %d was rejected", i)
			}
		}
	}
	if total, _ := l.TrackedIPCount(); total != 1000 {
		t.Fatalf("node tracker retained %d IPs, want hard cap 1000", total)
	}
}

func TestDeleteUserReleasesCurrentAndPreviousIPMaps(t *testing.T) {
	Init()
	const (
		tag  = "node-delete"
		uuid = "gone"
	)
	user := panel.UserInfo{Id: 13, Uuid: uuid}
	l := AddLimiter("vmess", tag, []panel.UserInfo{user}, nil, 16, 32)
	key := format.UserTag(tag, uuid)
	_, _ = l.CheckLimit(key, "192.0.2.1", true)
	_, _ = l.GetOnlineDevice()
	_, _ = l.CheckLimit(key, "192.0.2.2", true)
	l.UpdateUser(tag, nil, []panel.UserInfo{user}, nil)
	l.onlineMu.Lock()
	currentUsers := len(l.online)
	previousUsers := len(l.previous)
	l.onlineMu.Unlock()
	if currentUsers != 0 || previousUsers != 0 {
		t.Fatalf("deleted user retained state: current=%d previous=%d", currentUsers, previousUsers)
	}
}

func TestConcurrentTrackAndRotateStaysBounded(t *testing.T) {
	Init()
	const (
		tag  = "node-race"
		uuid = "race-user"
	)
	l := AddLimiter("vmess", tag, []panel.UserInfo{{Id: 21, Uuid: uuid}}, nil, 32, 64)
	key := format.UserTag(tag, uuid)
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 5000; i++ {
				_, _ = l.CheckLimit(key, fmt.Sprintf("10.%d.%d.%d", worker, (i/256)%256, i%256), true)
			}
		}(worker)
	}
	for i := 0; i < 200; i++ {
		_, _ = l.GetOnlineDevice()
	}
	wg.Wait()
	if total, _ := l.TrackedIPCount(); total > 32 {
		t.Fatalf("concurrent tracker exceeded per-user cap: %d", total)
	}
}

func TestExistingIPCheckDoesNotAllocate(t *testing.T) {
	Init()
	const (
		tag  = "node-alloc"
		uuid = "alloc-user"
	)
	l := AddLimiter("vmess", tag, []panel.UserInfo{{Id: 31, Uuid: uuid}}, nil, 16, 32)
	key := format.UserTag(tag, uuid)
	_, _ = l.CheckLimit(key, "192.0.2.10", true)
	allocs := testing.AllocsPerRun(1000, func() {
		_, _ = l.CheckLimit(key, "192.0.2.10", true)
	})
	if allocs != 0 {
		t.Fatalf("existing-IP CheckLimit allocations = %f, want 0", allocs)
	}
}

func TestConcurrentDeleteCannotRecreateUserState(t *testing.T) {
	Init()
	const (
		tag  = "node-delete-race"
		uuid = "deleted-user"
	)
	user := panel.UserInfo{Id: 44, Uuid: uuid, SpeedLimit: 10}
	l := AddLimiter("vmess", tag, []panel.UserInfo{user}, nil, 32, 64)
	key := format.UserTag(tag, uuid)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for worker := 0; worker < 16; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-start
			for i := 0; i < 2000; i++ {
				_, _ = l.CheckLimit(key, fmt.Sprintf("192.0.%d.%d", worker, i%256), true)
			}
		}(worker)
	}
	close(start)
	l.UpdateUser(tag, nil, []panel.UserInfo{user}, nil)
	wg.Wait()

	if _, ok := l.userLimitInfo.Load(key); ok {
		t.Fatal("deleted user limit was recreated")
	}
	if _, ok := l.speedLimiter.Load(key); ok {
		t.Fatal("deleted user's speed bucket was recreated")
	}
	l.onlineMu.Lock()
	_, online := l.online[key]
	_, previous := l.previous[user.Id]
	l.onlineMu.Unlock()
	if online || previous {
		t.Fatal("deleted user's online state was recreated")
	}
	if _, rejected := l.CheckLimit(key, "192.0.2.1", true); !rejected {
		t.Fatal("deleted user was accepted")
	}
}
