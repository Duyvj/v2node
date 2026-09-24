package limiter

import (
	"errors"
	"net/netip"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/format"
	"github.com/wyx2685/v2node/common/rate"
)

var limitLock sync.RWMutex
var limiter = make(map[string]*Limiter)

const deviceTTL = 60 * time.Second
const maxTrackedIPs = 256

type Limiter struct {
	Nodetype string
	users    sync.Map // user tag -> *userState; membership changes only on panel updates
	alive    atomic.Pointer[aliveSnapshot]
	closed   atomic.Bool
}
type aliveSnapshot struct {
	counts  map[int]int
	updated time.Time
}
type userState struct {
	mu                      sync.Mutex
	uid                     int
	speed, devices, dynamic int
	expire                  time.Time
	deleted                 bool
	ips                     map[netip.Addr]int64
	bucket                  *rate.DynamicBucket
	bucketRate              int64
}

func Init() {
	limitLock.Lock()
	for _, l := range limiter {
		l.closed.Store(true)
	}
	limiter = make(map[string]*Limiter)
	limitLock.Unlock()
}
func AddLimiter(nodetype, tag string, users []panel.UserInfo, alive map[int]int) *Limiter {
	l := &Limiter{Nodetype: nodetype}
	l.UpdateAliveList(alive)
	l.UpdateUser(tag, users, nil, nil)
	limitLock.Lock()
	if old := limiter[tag]; old != nil {
		old.closed.Store(true)
	}
	limiter[tag] = l
	limitLock.Unlock()
	return l
}
func GetLimiter(tag string) (*Limiter, error) {
	limitLock.RLock()
	l := limiter[tag]
	limitLock.RUnlock()
	if l == nil {
		return nil, errors.New("limiter not found")
	}
	return l, nil
}
func DeleteLimiter(tag string) {
	limitLock.Lock()
	if l := limiter[tag]; l != nil {
		l.closed.Store(true)
	}
	delete(limiter, tag)
	limitLock.Unlock()
}
func (l *Limiter) UpdateAliveList(alive map[int]int) {
	copied := make(map[int]int, len(alive))
	for uid, n := range alive {
		if n > 0 {
			copied[uid] = n
		}
	}
	l.alive.Store(&aliveSnapshot{counts: copied, updated: time.Now()})
}
func (l *Limiter) UpdateUser(tag string, added, deleted, modified []panel.UserInfo) {
	for _, u := range deleted {
		key := format.UserTag(tag, u.Uuid)
		if value, ok := l.users.LoadAndDelete(key); ok {
			s := value.(*userState)
			s.mu.Lock()
			s.deleted = true
			s.ips = nil
			s.mu.Unlock()
		}
	}
	for _, u := range modified {
		if value, ok := l.users.Load(format.UserTag(tag, u.Uuid)); ok {
			s := value.(*userState)
			s.mu.Lock()
			s.uid = u.Id
			s.speed = u.SpeedLimit
			s.devices = u.DeviceLimit
			s.trim()
			s.updateBucket(time.Now())
			s.mu.Unlock()
		}
	}
	for _, u := range added {
		l.users.LoadOrStore(format.UserTag(tag, u.Uuid), &userState{uid: u.Id, speed: u.SpeedLimit, devices: u.DeviceLimit})
	}
}
func (l *Limiter) UpdateDynamicSpeedLimit(tag, uuid string, limit int, expire time.Time) error {
	v, ok := l.users.Load(format.UserTag(tag, uuid))
	if !ok {
		return errors.New("user not found")
	}
	s := v.(*userState)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleted {
		return errors.New("user removed")
	}
	s.dynamic = limit
	s.expire = expire
	s.updateBucket(time.Now())
	return nil
}

// The source-network flag remains for upstream compatibility. TCP and UDP use
// the same admission rules. No per-handshake sync.Map or bucket is allocated.
func (l *Limiter) CheckLimit(key, rawIP string, _ bool) (*rate.DynamicBucket, bool) {
	if l.closed.Load() {
		return nil, true
	}
	v, ok := l.users.Load(key)
	if !ok {
		return nil, true
	}
	ip, err := netip.ParseAddr(rawIP)
	if err != nil {
		return nil, true
	}
	ip = ip.Unmap()
	s := v.(*userState)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleted {
		return nil, true
	}
	now := time.Now()
	stamp := now.UnixNano()
	if _, exists := s.ips[ip]; !exists {
		s.prune(stamp)
		if s.devices > 0 {
			if len(s.ips) >= s.devices {
				return nil, true
			}
			// The legacy aggregate is a conservative hint for NEW IPs only.
			// It is not an atomic global lease and cannot prevent cross-node races.
			if a := l.alive.Load(); a != nil && now.Sub(a.updated) <= deviceTTL && a.counts[s.uid] >= s.devices {
				return nil, true
			}
		}
		if len(s.ips) < maxTrackedIPs {
			if s.ips == nil {
				s.ips = make(map[netip.Addr]int64)
			}
			s.ips[ip] = stamp
		} else if s.devices > 0 {
			return nil, true
		}
	} else {
		s.ips[ip] = stamp
	}
	s.updateBucket(now)
	return s.bucket, false
}
func (s *userState) updateBucket(now time.Time) {
	if !s.expire.IsZero() && !now.Before(s.expire) {
		s.dynamic = 0
		s.expire = time.Time{}
	}
	value := int64(determineSpeedLimit(s.speed, s.dynamic)) * 1000000 / 8
	if s.bucket == nil {
		s.bucket = rate.NewDynamicBucket(value)
		s.bucketRate = value
		return
	}
	if value == s.bucketRate {
		return
	}
	s.bucketRate = value
	if s.bucket != nil {
		s.bucket.Update(value)
	} else if value > 0 {
		s.bucket = rate.NewDynamicBucket(value)
	}
}
func (s *userState) prune(now int64) {
	for ip, seen := range s.ips {
		if now-seen >= int64(deviceTTL) {
			delete(s.ips, ip)
		}
	}
	if len(s.ips) == 0 {
		s.ips = nil
	}
}
func (s *userState) trim() {
	if s.devices <= 0 || len(s.ips) <= s.devices {
		return
	}
	ips := make([]netip.Addr, 0, len(s.ips))
	for ip := range s.ips {
		ips = append(ips, ip)
	}
	sort.Slice(ips, func(i, j int) bool {
		if s.ips[ips[i]] == s.ips[ips[j]] {
			return ips[i].Less(ips[j])
		}
		return s.ips[ips[i]] > s.ips[ips[j]]
	})
	for _, ip := range ips[s.devices:] {
		delete(s.ips, ip)
	}
}
func (l *Limiter) GetOnlineDevice() (*[]panel.OnlineUser, error) {
	now := time.Now().UnixNano()
	online := make([]panel.OnlineUser, 0, 64)
	l.users.Range(func(_, value any) bool {
		s := value.(*userState)
		s.mu.Lock()
		s.prune(now)
		if !s.deleted {
			for ip := range s.ips {
				online = append(online, panel.OnlineUser{UID: s.uid, IP: ip.String()})
			}
		}
		s.mu.Unlock()
		return true
	})
	return &online, nil
}
