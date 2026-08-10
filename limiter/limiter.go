package limiter

import (
	"errors"
	"strings"
	"sync"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/format"
	"github.com/wyx2685/v2node/common/rate"
)

const (
	defaultMaxTrackedIPsPerUser = 256
	defaultMaxTrackedIPsPerNode = 32768
)

var limitLock sync.RWMutex
var limiters = map[string]*Limiter{}

func Init() {
	limitLock.Lock()
	old := limiters
	limiters = map[string]*Limiter{}
	limitLock.Unlock()
	for _, l := range old {
		l.clear()
	}
}

type onlineUserState struct {
	uid int
	ips map[string]struct{}
}

type Limiter struct {
	Nodetype   string // Node type, e.g. "v2ray", "trojan", "shadowsocks"
	SpeedLimit int    // Node speed limit in Mbps

	// userMu makes deletion a generation boundary: after UpdateUser removes a
	// user, an in-flight CheckLimit cannot recreate its speed bucket or online
	// state from a stale UserLimitInfo pointer.
	userMu        sync.RWMutex
	userLimitInfo sync.Map // map[string]*UserLimitInfo
	speedLimiter  sync.Map // map[string]*rate.DynamicBucket

	aliveMu   sync.RWMutex
	aliveList map[int]int // panel snapshot: uid -> alive IP count

	onlineMu    sync.Mutex
	online      map[string]*onlineUserState // current reporting interval
	previous    map[int]map[string]struct{} // last reported interval by uid
	trackedIPs  int                         // entries in online
	previousIPs int                         // entries in previous
	maxPerUser  int
	maxPerNode  int
}

type UserLimitInfo struct {
	UID               int
	SpeedLimit        int
	DeviceLimit       int
	DynamicSpeedLimit int
	ExpireTime        int64
	OverLimit         bool
}

// AddLimiter accepts optional per-user and per-node IP caps for compatibility
// with older callers. The caps bound attacker-controlled source-IP cardinality.
func AddLimiter(nodetype string, tag string, users []panel.UserInfo, aliveList map[int]int, caps ...int) *Limiter {
	maxPerUser := defaultMaxTrackedIPsPerUser
	maxPerNode := defaultMaxTrackedIPsPerNode
	if len(caps) > 0 && caps[0] > 0 {
		maxPerUser = caps[0]
	}
	if len(caps) > 1 && caps[1] > 0 {
		maxPerNode = caps[1]
	}
	if maxPerNode < maxPerUser {
		maxPerNode = maxPerUser
	}
	l := &Limiter{
		Nodetype:   nodetype,
		aliveList:  aliveList,
		online:     make(map[string]*onlineUserState),
		previous:   make(map[int]map[string]struct{}),
		maxPerUser: maxPerUser,
		maxPerNode: maxPerNode,
	}
	if l.aliveList == nil {
		l.aliveList = make(map[int]int)
	}
	for i := range users {
		l.userLimitInfo.Store(format.UserTag(tag, users[i].Uuid), userLimitFromPanel(users[i]))
	}
	limitLock.Lock()
	old := limiters[tag]
	limiters[tag] = l
	limitLock.Unlock()
	if old != nil {
		old.clear()
	}
	return l
}

func userLimitFromPanel(user panel.UserInfo) *UserLimitInfo {
	return &UserLimitInfo{
		UID:         user.Id,
		SpeedLimit:  user.SpeedLimit,
		DeviceLimit: user.DeviceLimit,
	}
}

func GetLimiter(tag string) (*Limiter, error) {
	limitLock.RLock()
	info, ok := limiters[tag]
	limitLock.RUnlock()
	if !ok {
		return nil, errors.New("not found")
	}
	return info, nil
}

func DeleteLimiter(tag string) {
	limitLock.Lock()
	l := limiters[tag]
	delete(limiters, tag)
	limitLock.Unlock()
	if l != nil {
		l.clear()
	}
}

func (l *Limiter) clear() {
	l.userMu.Lock()
	defer l.userMu.Unlock()
	l.userLimitInfo.Clear()
	l.speedLimiter.Clear()
	l.aliveMu.Lock()
	l.aliveList = make(map[int]int)
	l.aliveMu.Unlock()
	l.onlineMu.Lock()
	l.online = make(map[string]*onlineUserState)
	l.previous = make(map[int]map[string]struct{})
	l.trackedIPs = 0
	l.previousIPs = 0
	l.onlineMu.Unlock()
}

func (l *Limiter) UpdateUser(tag string, added []panel.UserInfo, deleted []panel.UserInfo, modified []panel.UserInfo) {
	l.userMu.Lock()
	defer l.userMu.Unlock()
	for i := range deleted {
		key := format.UserTag(tag, deleted[i].Uuid)
		l.userLimitInfo.Delete(key)
		l.speedLimiter.Delete(key)
		l.removeOnlineUser(key, deleted[i].Id)
		l.deleteAliveUser(deleted[i].Id)
	}
	for i := range modified {
		key := format.UserTag(tag, modified[i].Uuid)
		l.updateUserLimit(key, func(info *UserLimitInfo) {
			info.SpeedLimit = modified[i].SpeedLimit
			info.DeviceLimit = modified[i].DeviceLimit
		})
		l.updateSpeedBucket(key, modified[i].SpeedLimit)
	}
	for i := range added {
		key := format.UserTag(tag, added[i].Uuid)
		l.userLimitInfo.Store(key, userLimitFromPanel(added[i]))
	}
}

func (l *Limiter) updateUserLimit(key string, update func(*UserLimitInfo)) bool {
	for {
		value, ok := l.userLimitInfo.Load(key)
		if !ok {
			return false
		}
		old := value.(*UserLimitInfo)
		next := *old
		update(&next)
		if l.userLimitInfo.CompareAndSwap(key, old, &next) {
			return true
		}
	}
}

func (l *Limiter) updateSpeedBucket(key string, userLimit int) {
	limit := int64(determineSpeedLimit(l.SpeedLimit, userLimit)) * 1_000_000 / 8
	if limit <= 0 {
		l.speedLimiter.Delete(key)
		return
	}
	created := rate.NewDynamicBucket(limit)
	actual, loaded := l.speedLimiter.LoadOrStore(key, created)
	if loaded {
		actual.(*rate.DynamicBucket).Update(limit)
	}
}

func (l *Limiter) removeOnlineUser(key string, uid int) {
	l.onlineMu.Lock()
	if state := l.online[key]; state != nil {
		l.trackedIPs -= len(state.ips)
		delete(l.online, key)
	}
	if ips := l.previous[uid]; ips != nil {
		l.previousIPs -= len(ips)
	}
	delete(l.previous, uid)
	l.onlineMu.Unlock()
}

func (l *Limiter) UpdateDynamicSpeedLimit(tag, uuid string, limit int, expire time.Time) error {
	l.userMu.Lock()
	defer l.userMu.Unlock()
	key := format.UserTag(tag, uuid)
	if !l.updateUserLimit(key, func(info *UserLimitInfo) {
		info.DynamicSpeedLimit = limit
		info.ExpireTime = expire.Unix()
	}) {
		return errors.New("not found")
	}
	return nil
}

func (l *Limiter) CheckLimit(taguuid string, ip string, noUDPsource bool) (*rate.DynamicBucket, bool) {
	l.userMu.RLock()
	defer l.userMu.RUnlock()
	ip = strings.TrimPrefix(ip, "::ffff:")

	value, ok := l.userLimitInfo.Load(taguuid)
	if !ok {
		return nil, true
	}
	info := value.(*UserLimitInfo)
	userLimit := determineSpeedLimit(info.SpeedLimit, info.DynamicSpeedLimit)
	if info.ExpireTime != 0 && info.ExpireTime < time.Now().Unix() {
		l.updateUserLimit(taguuid, func(updated *UserLimitInfo) {
			updated.DynamicSpeedLimit = 0
			updated.ExpireTime = 0
		})
		userLimit = info.SpeedLimit
	}

	if noUDPsource || l.Nodetype == "hysteria2" || l.Nodetype == "tuic" {
		if l.trackOnlineIP(taguuid, ip, info.UID, info.DeviceLimit) {
			return nil, true
		}
	}

	limit := int64(determineSpeedLimit(l.SpeedLimit, userLimit)) * 1_000_000 / 8
	if limit <= 0 {
		return nil, false
	}
	if value, ok := l.speedLimiter.Load(taguuid); ok {
		return value.(*rate.DynamicBucket), false
	}
	created := rate.NewDynamicBucket(limit)
	actual, loaded := l.speedLimiter.LoadOrStore(taguuid, created)
	if loaded {
		return actual.(*rate.DynamicBucket), false
	}
	return created, false
}

// trackOnlineIP returns true when a device-limited user must be rejected.
// Unlimited users are always allowed, but excess IPs are deliberately not
// retained once either memory cap is reached.
func (l *Limiter) trackOnlineIP(taguuid, ip string, uid, deviceLimit int) bool {
	alive := l.aliveCount(uid)
	l.onlineMu.Lock()
	defer l.onlineMu.Unlock()

	state := l.online[taguuid]
	if state != nil {
		if _, exists := state.ips[ip]; exists {
			return false
		}
	}

	old := l.previous[uid]
	if _, wasPreviouslyReported := old[ip]; wasPreviouslyReported {
		// Moving an IP between reporting periods does not consume another cap
		// slot. Always move it into the current set so it remains reportable;
		// previously reported IPs must not be accepted without being tracked.
		delete(old, ip)
		l.previousIPs--
		if len(old) == 0 {
			delete(l.previous, uid)
		}
		if state == nil {
			state = &onlineUserState{uid: uid, ips: make(map[string]struct{})}
			l.online[taguuid] = state
		}
		state.ips[ip] = struct{}{}
		l.trackedIPs++
		return false
	}

	userTracked := len(old)
	if state != nil {
		userTracked += len(state.ips)
	}
	if deviceLimit > 0 && max(alive, userTracked) >= deviceLimit {
		return true
	}

	// Current and prior-period entries share the same caps. When a new IP is
	// actually online, prefer it over one unseen IP from the same user's prior
	// period. This keeps reports useful during disjoint rotations without ever
	// retaining two full generations of attacker-controlled strings.
	retained := l.trackedIPs + l.previousIPs
	if (userTracked >= l.maxPerUser || retained >= l.maxPerNode) && len(old) > 0 {
		for staleIP := range old {
			delete(old, staleIP)
			break
		}
		l.previousIPs--
		userTracked--
		retained--
		if len(old) == 0 {
			delete(l.previous, uid)
		}
	}

	perUserFull := userTracked >= l.maxPerUser
	nodeFull := retained >= l.maxPerNode
	if perUserFull || nodeFull {
		return deviceLimit > 0
	}
	if state == nil {
		state = &onlineUserState{uid: uid, ips: make(map[string]struct{})}
		l.online[taguuid] = state
	}
	state.ips[ip] = struct{}{}
	l.trackedIPs++
	return false
}

func (l *Limiter) GetOnlineDevice() (*[]panel.OnlineUser, error) {
	l.onlineMu.Lock()
	onlineUsers := make([]panel.OnlineUser, 0, l.trackedIPs)
	nextPrevious := make(map[int]map[string]struct{}, len(l.online))
	nextPreviousIPs := 0
	for _, state := range l.online {
		ips := nextPrevious[state.uid]
		if ips == nil {
			ips = make(map[string]struct{}, len(state.ips))
			nextPrevious[state.uid] = ips
		}
		for ip := range state.ips {
			if _, exists := ips[ip]; !exists {
				ips[ip] = struct{}{}
				nextPreviousIPs++
			}
			onlineUsers = append(onlineUsers, panel.OnlineUser{UID: state.uid, IP: ip})
		}
	}
	// Replace maps instead of deleting entries in-place so a connection storm's
	// peak map buckets become collectible after every reporting interval.
	l.online = make(map[string]*onlineUserState)
	l.previous = nextPrevious
	l.trackedIPs = 0
	l.previousIPs = nextPreviousIPs
	l.onlineMu.Unlock()
	return &onlineUsers, nil
}

func (l *Limiter) SetAliveList(aliveList map[int]int) {
	if aliveList == nil {
		aliveList = make(map[int]int)
	}
	l.aliveMu.Lock()
	l.aliveList = aliveList
	l.aliveMu.Unlock()
}

func (l *Limiter) aliveCount(uid int) int {
	l.aliveMu.RLock()
	count := l.aliveList[uid]
	l.aliveMu.RUnlock()
	return count
}

func (l *Limiter) deleteAliveUser(uid int) {
	l.aliveMu.Lock()
	delete(l.aliveList, uid)
	l.aliveMu.Unlock()
}

// TrackedIPCount is exposed for diagnostics and stress tests.
func (l *Limiter) TrackedIPCount() (total, users int) {
	l.onlineMu.Lock()
	defer l.onlineMu.Unlock()
	return l.trackedIPs, len(l.online)
}

type UserIpList struct {
	Uid    int      `json:"Uid"`
	IpList []string `json:"Ips"`
}
