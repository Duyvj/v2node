package limiter

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/format"
	"github.com/wyx2685/v2node/conf"
)

func TestLimiterWithoutRedisAdmitsOnlyTwoOfSixIPs(t *testing.T) {
	for _, config := range []*conf.GlobalDeviceLimitConfig{nil, {Enable: false}, {Enable: true, RedisNetwork: "invalid"}} {
		Init()
		l := AddLimiter("vless", "local", []panel.UserInfo{{Id: 1, Uuid: "credential", DeviceLimit: 2}}, nil, config, "https://panel.example")
		key := format.UserTag("local", "credential")
		for i := 1; i <= 6; i++ {
			_, rejected := l.CheckLimit(context.Background(), key, fmt.Sprintf("192.0.2.%d", i))
			if rejected != (i > 2) {
				t.Fatalf("config=%+v ip=%d rejected=%v", config, i, rejected)
			}
		}
		if _, rejected := l.CheckLimit(context.Background(), key, "::ffff:192.0.2.1"); rejected {
			t.Fatal("same IPv4 counted twice")
		}
		if _, rejected := l.CheckLimit(context.Background(), key, "invalid"); !rejected {
			t.Fatal("invalid source bypassed limit")
		}
		DeleteLimiter("local")
	}
}

func TestLocalLimitReductionRejectsEstablishedExtraIPs(t *testing.T) {
	Init()
	l := AddLimiter("vless", "local", []panel.UserInfo{{Id: 1, Uuid: "credential", DeviceLimit: 6}}, nil, nil, "https://panel.example")
	defer DeleteLimiter("local")
	key := format.UserTag("local", "credential")
	for i := 1; i <= 6; i++ {
		l.CheckLimit(context.Background(), key, fmt.Sprintf("192.0.2.%d", i))
	}
	l.UpdateUser("local", nil, nil, []panel.UserInfo{{Id: 1, Uuid: "credential", DeviceLimit: 2}})
	allowed := 0
	for i := 1; i <= 6; i++ {
		if l.TouchDevice(context.Background(), key, fmt.Sprintf("192.0.2.%d", i)) {
			allowed++
		}
	}
	if allowed != 2 {
		t.Fatalf("established IPs still allowed=%d, want 2", allowed)
	}
}

func testRedisConfig(addr string) *conf.GlobalDeviceLimitConfig {
	return &conf.GlobalDeviceLimitConfig{Enable: true, RedisAddr: addr, FailClosed: true, Expiry: 60, RefreshInterval: 5, HandoverGrace: deviceGracePtr(0)}
}

func TestSixNodesShareTwoAtomicIPSlots(t *testing.T) {
	s := miniredis.RunT(t)
	Init()
	var group sync.WaitGroup
	results := make(chan bool, 6)
	for i := 0; i < 6; i++ {
		tag := fmt.Sprintf("node-%d", i)
		l := AddLimiter("vless", tag, []panel.UserInfo{{Id: 1, Uuid: "shared", DeviceLimit: 2}}, nil, testRedisConfig(s.Addr()), "https://panel.example")
		t.Cleanup(func() { DeleteLimiter(tag) })
		group.Add(1)
		go func(i int) {
			defer group.Done()
			_, rejected := l.CheckLimit(context.Background(), format.UserTag(tag, "shared"), fmt.Sprintf("192.0.2.%d", i+1))
			results <- !rejected
		}(i)
	}
	group.Wait()
	close(results)
	allowed := 0
	for ok := range results {
		if ok {
			allowed++
		}
	}
	if allowed != 2 {
		t.Fatalf("admitted %d of 6 IPs across 6 nodes, want 2", allowed)
	}
	if len(s.Keys()) != 1 {
		t.Fatalf("shared UUID split across keys: %v", s.Keys())
	}
}

func TestSixExistingNodeSessionsConvergeToTwoAfterLimitReduction(t *testing.T) {
	s := miniredis.RunT(t)
	Init()
	var nodes []*Limiter
	for i := 1; i <= 6; i++ {
		tag := fmt.Sprintf("node-%d", i)
		l := AddLimiter("vless", tag, []panel.UserInfo{{Id: 1, Uuid: "shared", DeviceLimit: 6}}, nil, testRedisConfig(s.Addr()), "https://panel.example")
		t.Cleanup(func() { DeleteLimiter(tag) })
		if _, rejected := l.CheckLimit(context.Background(), format.UserTag(tag, "shared"), fmt.Sprintf("192.0.2.%d", i)); rejected {
			t.Fatal("initial six-slot admission")
		}
		nodes = append(nodes, l)
	}
	allowed := 0
	for i, l := range nodes {
		tag := fmt.Sprintf("node-%d", i+1)
		l.UpdateUser(tag, nil, nil, []panel.UserInfo{{Id: 1, Uuid: "shared", DeviceLimit: 2}})
		if l.TouchDevice(context.Background(), format.UserTag(tag, "shared"), fmt.Sprintf("192.0.2.%d", i+1)) {
			allowed++
		}
	}
	if allowed != 2 {
		t.Fatalf("existing sessions allowed across nodes=%d, want 2", allowed)
	}
}

func TestHealthyRedisDenialOnRenewalIsNeverFailOpen(t *testing.T) {
	for _, failClosed := range []bool{false, true} {
		tracker := newDeviceTracker(nil)
		store := &fakeDeviceStore{allow: func(context.Context, string, string, int) (bool, error) { return true, nil }}
		now := time.Now()
		if ok, err := tracker.Observe(context.Background(), store, failClosed, "user", "192.0.2.1", 1, 2, now); !ok || err != nil {
			t.Fatal(ok, err)
		}
		store.allow = func(context.Context, string, string, int) (bool, error) { return false, nil }
		if ok, err := tracker.Observe(context.Background(), store, failClosed, "user", "192.0.2.1", 1, 2, now.Add(tracker.refresh)); ok || err != nil {
			t.Fatalf("renewal denial ignored: failClosed=%v allowed=%v err=%v", failClosed, ok, err)
		}
	}
}

func TestRedisShrinksExistingSixToTwoBeforeRenewal(t *testing.T) {
	s := miniredis.RunT(t)
	now := time.Now()
	s.SetTime(now)
	r, err := newRedisDeviceStore(testRedisConfig(s.Addr()), "https://panel.example")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	key := r.key("node|credential")
	ctx := context.Background()
	for i := 1; i <= 6; i++ {
		if err := r.client.ZAdd(ctx, key, redis.Z{Score: float64(now.UnixMilli() - int64(7-i)*1000), Member: fmt.Sprintf("192.0.2.%d", i)}).Err(); err != nil {
			t.Fatal(err)
		}
	}
	allowed := 0
	for i := 1; i <= 6; i++ {
		ok, err := r.Allow(ctx, "node|credential", fmt.Sprintf("192.0.2.%d", i), 2)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			allowed++
		}
	}
	if allowed != 2 {
		t.Fatalf("renewed %d of 6 IPs, want 2", allowed)
	}
	if n := r.client.ZCard(ctx, key).Val(); n != 2 {
		t.Fatalf("Redis members=%d", n)
	}
}

func TestRedisUsesServerClockAndHandoverRevokesOldSession(t *testing.T) {
	s := miniredis.RunT(t)
	clock := time.Unix(1700000000, 0) // Deliberately different from node time.
	s.SetTime(clock)
	config := testRedisConfig(s.Addr())
	config.HandoverGrace = deviceGracePtr(15)
	r, err := newRedisDeviceStore(config, "https://panel.example")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	if ok, err := r.Allow(ctx, "node-a|user", "192.0.2.1", 1); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if score := r.client.ZScore(ctx, r.key("node-a|user"), "192.0.2.1").Val(); score != float64(clock.UnixMilli()) {
		t.Fatalf("score uses node clock: %.0f", score)
	}
	s.SetTime(clock.Add(16 * time.Second))
	if ok, err := r.Allow(ctx, "node-b|user", "192.0.2.2", 1); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, err := r.Allow(ctx, "node-a|user", "192.0.2.1", 1); ok || err != nil {
		t.Fatalf("old IP survived handover: %v %v", ok, err)
	}
}

func TestFailClosedStopsExistingSessionAfterRedisOutage(t *testing.T) {
	s := miniredis.RunT(t)
	Init()
	l := AddLimiter("vless", "node", []panel.UserInfo{{Id: 1, Uuid: "credential", DeviceLimit: 2}}, nil, testRedisConfig(s.Addr()), "https://panel.example")
	defer DeleteLimiter("node")
	key := format.UserTag("node", "credential")
	if _, rejected := l.CheckLimit(context.Background(), key, "192.0.2.1"); rejected {
		t.Fatal("initial admission")
	}
	l.devices.mu.Lock()
	l.devices.users[key]["192.0.2.1"].lastRedisTouch = time.Time{}
	l.devices.mu.Unlock()
	s.Close()
	if l.TouchDevice(context.Background(), key, "192.0.2.1") {
		t.Fatal("existing session survived failed renewal")
	}
}

func TestDeletingOneNodeDoesNotEraseOtherNodesLeases(t *testing.T) {
	s := miniredis.RunT(t)
	Init()
	users := []panel.UserInfo{{Id: 1, Uuid: "credential", DeviceLimit: 2}}
	a := AddLimiter("vless", "a", users, nil, testRedisConfig(s.Addr()), "https://panel.example")
	b := AddLimiter("vless", "b", users, nil, testRedisConfig(s.Addr()), "https://panel.example")
	defer DeleteLimiter("a")
	defer DeleteLimiter("b")
	a.CheckLimit(context.Background(), format.UserTag("a", "credential"), "192.0.2.1")
	b.CheckLimit(context.Background(), format.UserTag("b", "credential"), "192.0.2.2")
	a.UpdateUser("a", nil, users, nil)
	if _, rejected := b.CheckLimit(context.Background(), format.UserTag("b", "credential"), "192.0.2.3"); !rejected {
		t.Fatal("deleting a node released live shared slots")
	}
}
