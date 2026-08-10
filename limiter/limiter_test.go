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

func TestEvictedPriorPeriodIPCannotBypassFullPerUserCap(t *testing.T) {
	Init()
	const (
		tag  = "node-prior-cap"
		uuid = "limited-user"
	)
	l := AddLimiter("vmess", tag, []panel.UserInfo{{Id: 14, Uuid: uuid, DeviceLimit: 10}}, nil, 2, 8)
	key := format.UserTag(tag, uuid)
	oldIPs := []string{"192.0.2.1", "192.0.2.2"}
	for _, ip := range oldIPs {
		if _, rejected := l.CheckLimit(key, ip, true); rejected {
			t.Fatalf("initial IP %s was rejected", ip)
		}
	}
	_, _ = l.GetOnlineDevice()

	// A disjoint current set replaces stale prior-period entries while keeping
	// the combined per-user state at the cap.
	for _, ip := range []string{"198.51.100.1", "198.51.100.2"} {
		if _, rejected := l.CheckLimit(key, ip, true); rejected {
			t.Fatalf("current IP %s was rejected even though a stale slot was available", ip)
		}
		assertStoredIPCaps(t, l)
	}
	// The prior implementation still considered these IPs privileged and
	// accepted them without tracking once the current map was full.
	for _, ip := range oldIPs {
		if _, rejected := l.CheckLimit(key, ip, true); !rejected {
			t.Fatalf("prior-period IP %s bypassed the full tracker", ip)
		}
	}
}

func TestPriorPeriodIPMovesIntoCurrentAtNodeCap(t *testing.T) {
	Init()
	const (
		tag      = "node-prior-node-cap"
		priorIP  = "192.0.2.1"
		overflow = "198.51.100.3"
	)
	users := []panel.UserInfo{
		{Id: 16, Uuid: "limited-user", DeviceLimit: 10},
		{Id: 17, Uuid: "unlimited-user"},
	}
	l := AddLimiter("vmess", tag, users, nil, 4, 6)
	limitedKey := format.UserTag(tag, users[0].Uuid)
	unlimitedKey := format.UserTag(tag, users[1].Uuid)
	if _, rejected := l.CheckLimit(limitedKey, priorIP, true); rejected {
		t.Fatal("initial prior-period IP was rejected")
	}
	_, _ = l.GetOnlineDevice()

	for _, ip := range []string{"192.0.2.2", "192.0.2.3", "192.0.2.4"} {
		if _, rejected := l.CheckLimit(limitedKey, ip, true); rejected {
			t.Fatalf("limited user's current IP %s was rejected", ip)
		}
	}
	for _, ip := range []string{"198.51.100.1", "198.51.100.2", overflow} {
		if _, rejected := l.CheckLimit(unlimitedKey, ip, true); rejected {
			t.Fatalf("unlimited user's current IP %s was rejected", ip)
		}
		assertStoredIPCaps(t, l)
	}

	// The node is at its retained-state cap. Re-observing a stored prior IP
	// must transfer its slot into the current report instead of bypassing the
	// full current tracker or consuming a seventh slot.
	if _, rejected := l.CheckLimit(limitedKey, priorIP, true); rejected {
		t.Fatal("tracked prior-period IP was rejected at the node cap")
	}
	assertStoredIPCaps(t, l)
	online, err := l.GetOnlineDevice()
	if err != nil {
		t.Fatal(err)
	}
	seenPrior := false
	seenOverflow := false
	for _, got := range *online {
		seenPrior = seenPrior || got.IP == priorIP
		seenOverflow = seenOverflow || got.IP == overflow
	}
	if !seenPrior {
		t.Fatal("prior-period IP bypassed tracking and was omitted from the current report")
	}
	if seenOverflow {
		t.Fatal("untracked node-cap overflow IP entered the current report")
	}
}

func TestDisjointIPRotationsStayWithinPerUserCap(t *testing.T) {
	Init()
	const (
		tag       = "node-disjoint-user"
		uuid      = "rotating-user"
		uid       = 15
		rotations = 128
		perUser   = 8
	)
	l := AddLimiter("vmess", tag, []panel.UserInfo{{Id: uid, Uuid: uuid}}, nil, perUser, 32)
	key := format.UserTag(tag, uuid)

	for rotation := 0; rotation < rotations; rotation++ {
		want := make(map[string]struct{}, perUser)
		for slot := 0; slot < perUser; slot++ {
			ip := stressIP(198, rotation*perUser+slot)
			want[ip] = struct{}{}
			if _, rejected := l.CheckLimit(key, ip, true); rejected {
				t.Fatalf("rotation %d IP %s was rejected for an unlimited user", rotation, ip)
			}
			assertStoredIPCaps(t, l)
		}

		// Unlimited users remain allowed after the tracker is full, but the
		// excess address must not enlarge retained state or enter the report.
		excess := stressIP(203, rotation)
		if _, rejected := l.CheckLimit(key, excess, true); rejected {
			t.Fatalf("rotation %d excess IP was rejected for an unlimited user", rotation)
		}
		assertStoredIPCaps(t, l)

		online, err := l.GetOnlineDevice()
		if err != nil {
			t.Fatal(err)
		}
		if len(*online) != perUser {
			t.Fatalf("rotation %d reported %d IPs, want %d", rotation, len(*online), perUser)
		}
		for _, got := range *online {
			if got.UID != uid {
				t.Fatalf("rotation %d reported UID %d, want %d", rotation, got.UID, uid)
			}
			if _, ok := want[got.IP]; !ok {
				t.Fatalf("rotation %d reported stale or excess IP %s", rotation, got.IP)
			}
			delete(want, got.IP)
		}
		if len(want) != 0 {
			t.Fatalf("rotation %d omitted %d current IPs", rotation, len(want))
		}
		assertStoredIPCaps(t, l)
	}
}

func TestDisjointIPRotationsStayWithinNodeCap(t *testing.T) {
	Init()
	const (
		tag       = "node-disjoint-node"
		rotations = 128
		perUser   = 3
		nodeCap   = 7
	)
	users := []panel.UserInfo{
		{Id: 101, Uuid: "user-1"},
		{Id: 102, Uuid: "user-2"},
		{Id: 103, Uuid: "user-3"},
		{Id: 104, Uuid: "overflow-user"},
	}
	l := AddLimiter("vmess", tag, users, nil, perUser, nodeCap)

	for rotation := 0; rotation < rotations; rotation++ {
		want := make(map[string]int, nodeCap)
		for slot := 0; slot < nodeCap; slot++ {
			user := users[slot/perUser]
			ip := stressIP(10, rotation*nodeCap+slot)
			want[ip] = user.Id
			if _, rejected := l.CheckLimit(format.UserTag(tag, user.Uuid), ip, true); rejected {
				t.Fatalf("rotation %d node slot %d was rejected", rotation, slot)
			}
			assertStoredIPCaps(t, l)
		}
		if _, rejected := l.CheckLimit(format.UserTag(tag, users[3].Uuid), stressIP(172, rotation), true); rejected {
			t.Fatalf("rotation %d overflow IP was rejected for an unlimited user", rotation)
		}
		assertStoredIPCaps(t, l)

		online, err := l.GetOnlineDevice()
		if err != nil {
			t.Fatal(err)
		}
		if len(*online) != nodeCap {
			t.Fatalf("rotation %d reported %d IPs, want node cap %d", rotation, len(*online), nodeCap)
		}
		for _, got := range *online {
			if uid, ok := want[got.IP]; !ok || uid != got.UID {
				t.Fatalf("rotation %d reported unexpected entry UID=%d IP=%s", rotation, got.UID, got.IP)
			}
			delete(want, got.IP)
		}
		if len(want) != 0 {
			t.Fatalf("rotation %d omitted %d current node IPs", rotation, len(want))
		}
		assertStoredIPCaps(t, l)
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

func assertStoredIPCaps(t *testing.T, l *Limiter) {
	t.Helper()
	l.onlineMu.Lock()
	defer l.onlineMu.Unlock()

	perUser := make(map[int]int, len(l.online)+len(l.previous))
	current := 0
	for _, state := range l.online {
		current += len(state.ips)
		perUser[state.uid] += len(state.ips)
	}
	prior := 0
	for uid, ips := range l.previous {
		prior += len(ips)
		perUser[uid] += len(ips)
	}
	if current != l.trackedIPs {
		t.Fatalf("current IP accounting mismatch: maps=%d counter=%d", current, l.trackedIPs)
	}
	if prior != l.previousIPs {
		t.Fatalf("prior IP accounting mismatch: maps=%d counter=%d", prior, l.previousIPs)
	}
	if current+prior > l.maxPerNode {
		t.Fatalf("stored node IPs=%d (current=%d prior=%d), cap=%d", current+prior, current, prior, l.maxPerNode)
	}
	for uid, count := range perUser {
		if count > l.maxPerUser {
			t.Fatalf("stored IPs for UID %d=%d, per-user cap=%d", uid, count, l.maxPerUser)
		}
	}
}

func stressIP(firstOctet, value int) string {
	return fmt.Sprintf("%d.%d.%d.%d", firstOctet, (value>>16)&255, (value>>8)&255, value&255)
}
