package limiter

import (
	"fmt"
	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/format"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestLimiter(t *testing.T, limit int) *Limiter {
	t.Helper()
	Init()
	l := AddLimiter("vless", "test", []panel.UserInfo{{Id: 1, Uuid: "user", DeviceLimit: limit}}, nil)
	t.Cleanup(func() { DeleteLimiter("test") })
	return l
}
func TestConcurrentTCPUDPAdmissionIsBounded(t *testing.T) {
	l := newTestLimiter(t, 2)
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 1; i <= 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, reject := l.CheckLimit("test|user", fmt.Sprintf("192.0.2.%d", i), i%2 == 0); !reject {
				accepted.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if accepted.Load() != 2 {
		t.Fatalf("accepted %d IPs, want 2", accepted.Load())
	}
}
func TestOnlineSnapshotDoesNotResetAdmission(t *testing.T) {
	l := newTestLimiter(t, 1)
	l.CheckLimit("test|user", "192.0.2.1", true)
	for i := 0; i < 3; i++ {
		online, _ := l.GetOnlineDevice()
		if len(*online) != 1 {
			t.Fatal("lost current IP")
		}
	}
	if _, reject := l.CheckLimit("test|user", "192.0.2.2", false); !reject {
		t.Fatal("snapshot freed active slot")
	}
	if _, reject := l.CheckLimit("test|user", "::ffff:192.0.2.1", false); reject {
		t.Fatal("mapped IP double counted")
	}
}
func TestExpiryAndLimitReduction(t *testing.T) {
	l := newTestLimiter(t, 3)
	for i := 1; i <= 3; i++ {
		l.CheckLimit("test|user", fmt.Sprintf("192.0.2.%d", i), true)
	}
	l.UpdateUser("test", nil, nil, []panel.UserInfo{{Id: 1, Uuid: "user", DeviceLimit: 1}})
	accepted := 0
	for i := 1; i <= 3; i++ {
		if _, reject := l.CheckLimit("test|user", fmt.Sprintf("192.0.2.%d", i), true); !reject {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("still %d IPs after reduction", accepted)
	}
	v, _ := l.users.Load("test|user")
	s := v.(*userState)
	s.mu.Lock()
	for ip := range s.ips {
		s.ips[ip] = time.Now().Add(-2 * deviceTTL).UnixNano()
	}
	s.mu.Unlock()
	if _, reject := l.CheckLimit("test|user", "192.0.2.10", false); reject {
		t.Fatal("expired slot not reclaimed")
	}
}
func TestUnlimitedTrackingIsBounded(t *testing.T) {
	l := newTestLimiter(t, 0)
	for i := 0; i < 1000; i++ {
		if _, reject := l.CheckLimit("test|user", fmt.Sprintf("198.18.%d.%d", i/250, i%250+1), false); reject {
			t.Fatal("unlimited user rejected")
		}
	}
	online, _ := l.GetOnlineDevice()
	if len(*online) != maxTrackedIPs {
		t.Fatalf("tracking count=%d", len(*online))
	}
}
func TestPolicyUpdatesAndReportsRaceSafely(t *testing.T) {
	l := newTestLimiter(t, 2)
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				switch worker {
				case 0:
					l.CheckLimit("test|user", "192.0.2.1", true)
				case 1:
					l.UpdateAliveList(map[int]int{1: i % 3})
				case 2:
					l.UpdateUser("test", nil, nil, []panel.UserInfo{{Id: 1, Uuid: "user", DeviceLimit: 2, SpeedLimit: i % 2}})
				case 3:
					l.GetOnlineDevice()
				}
			}
		}(worker)
	}
	wg.Wait()
}
func TestDynamicExpiryRestoresUserAndBucket(t *testing.T) {
	l := newTestLimiter(t, 2)
	key := format.UserTag("test", "user")
	bucket, reject := l.CheckLimit(key, "192.0.2.1", true)
	if reject || bucket.Get() != nil {
		t.Fatal("unlimited bucket")
	}
	l.UpdateDynamicSpeedLimit("test", "user", 1, time.Now().Add(time.Hour))
	if bucket.Get() == nil {
		t.Fatal("existing session did not receive new speed")
	}
	l.UpdateDynamicSpeedLimit("test", "user", 1, time.Now().Add(-time.Hour))
	if _, reject = l.CheckLimit(key, "192.0.2.1", true); reject || bucket.Get() != nil {
		t.Fatal("expired speed removed user or retained limit")
	}
	DeleteLimiter("test")
	if _, reject = l.CheckLimit(key, "192.0.2.1", true); !reject {
		t.Fatal("closed limiter admitted")
	}
}
func TestAliveSnapshotOwnsItsMap(t *testing.T) {
	l := newTestLimiter(t, 1)
	source := map[int]int{1: 1}
	l.UpdateAliveList(source)
	source[1] = 0
	if _, reject := l.CheckLimit("test|user", "192.0.2.1", true); !reject {
		t.Fatal("caller mutated alive snapshot")
	}
	l.UpdateAliveList(nil)
	l.CheckLimit("test|user", "192.0.2.1", true)
	v, _ := l.users.Load("test|user")
	s := v.(*userState)
	s.mu.Lock()
	_, ok := s.ips[netip.MustParseAddr("192.0.2.1")]
	s.mu.Unlock()
	if !ok {
		t.Fatal("normalized IP absent")
	}
}
