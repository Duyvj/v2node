package limiter

import (
	"context"
	"sort"
	"sync"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
)

type deviceEntry struct {
	uid            int
	lastSeen       time.Time
	lastRedisTouch time.Time
	remoteApproved bool
	approvedLimit  int
	inFlight       chan struct{}
	admitted       bool
}

type deviceStore interface {
	Allow(context.Context, string, string, int) (bool, error)
}

const deviceRefreshCooldown = 5 * time.Second

// Renewals must finish before bytes pass. Concurrent requests for one IP share
// the result. The local limit still applies during a fail-open Redis outage.
type deviceTracker struct {
	mu            sync.Mutex
	users         map[string]map[string]*deviceEntry
	ttl           time.Duration
	refresh       time.Duration
	grace         time.Duration
	maxIPsPerUser int
	circuitUntil  time.Time
	closed        bool
}

func newDeviceTracker(c *conf.GlobalDeviceLimitConfig) *deviceTracker {
	t := &deviceTracker{
		users: make(map[string]map[string]*deviceEntry),
		ttl:   60 * time.Second, refresh: 7 * time.Second,
		grace: 15 * time.Second, maxIPsPerUser: 256,
	}
	if c != nil {
		applyDeviceDefaults(c)
		t.ttl = time.Duration(c.Expiry) * time.Second
		t.refresh = time.Duration(c.RefreshInterval) * time.Second
		t.grace = time.Duration(*c.HandoverGrace) * time.Second
		t.maxIPsPerUser = c.MaxIPsPerUser
	}
	return t
}

func (t *deviceTracker) Observe(ctx context.Context, remote deviceStore, failClosed bool, userKey, ip string, uid, limit int, now time.Time) (bool, error) {
	for {
		t.mu.Lock()
		if t.closed {
			t.mu.Unlock()
			return false, nil
		}
		entries := t.users[userKey]
		if entries == nil {
			entries = make(map[string]*deviceEntry)
			t.users[userKey] = entries
		}
		t.pruneLocked(entries, now)
		// A reduced limit applies to previously admitted addresses too.
		if limit > 0 && len(entries) > limit {
			t.trimLocked(entries, limit)
		}
		entry := entries[ip]
		if entry == nil {
			if limit > 0 && len(entries) >= limit && !t.evictHandoverLocked(entries, limit, now) {
				t.mu.Unlock()
				return false, nil
			}
			if t.maxIPsPerUser > 0 && len(entries) >= t.maxIPsPerUser {
				t.mu.Unlock()
				return limit <= 0, nil
			}
			entry = &deviceEntry{uid: uid}
			entries[ip] = entry
		}
		entry.uid = uid
		entry.lastSeen = now
		if remote == nil || limit <= 0 {
			entry.admitted = true
			t.mu.Unlock()
			return true, nil
		}
		if entry.inFlight != nil {
			done := entry.inFlight
			t.mu.Unlock()
			select {
			case <-done:
				t.mu.Lock()
				retained := t.users[userKey][ip] == entry
				t.mu.Unlock()
				if !retained {
					return false, nil
				}
				now = time.Now()
				continue
			case <-ctx.Done():
				return false, ctx.Err()
			}
		}
		if entry.remoteApproved && entry.approvedLimit == limit && now.Sub(entry.lastRedisTouch) < t.refresh {
			t.mu.Unlock()
			return true, nil
		}
		if !failClosed && now.Before(t.circuitUntil) {
			entry.admitted = true
			t.mu.Unlock()
			return true, nil
		}
		done := make(chan struct{})
		entry.inFlight = done
		t.mu.Unlock()

		allowed, err := remote.Allow(ctx, userKey, ip, limit)
		t.mu.Lock()
		entry.inFlight = nil
		if err != nil {
			allowed = !failClosed // Local capacity was already checked.
			if !failClosed {
				t.circuitUntil = time.Now().Add(deviceRefreshCooldown)
			}
		}
		// An old request must not resurrect or remove a replacement entry.
		current := t.users[userKey]
		if t.closed || current[ip] != entry {
			allowed = false
		} else if err == nil && allowed {
			entry.remoteApproved = true
			entry.approvedLimit = limit
			// Start the lease before network delay, not after it.
			entry.lastRedisTouch = now
		} else if !allowed {
			delete(current, ip)
			if len(current) == 0 {
				delete(t.users, userKey)
			}
		}
		if allowed {
			entry.admitted = true
		}
		close(done)
		t.mu.Unlock()
		return allowed, err
	}
}

func (t *deviceTracker) Close() {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
}

// A delayed count cannot identify which IP owns a slot. Cross-node atomic
// admission requires Redis; do not treat the legacy panel count as a lease.
func (t *deviceTracker) SetAliveList(_ map[int]int) {}

func (t *deviceTracker) Delete(userKey string) {
	t.mu.Lock()
	delete(t.users, userKey)
	t.mu.Unlock()
}

func (t *deviceTracker) trimLocked(entries map[string]*deviceEntry, limit int) {
	ips := make([]string, 0, len(entries))
	for ip := range entries {
		ips = append(ips, ip)
	}
	sort.Slice(ips, func(i, j int) bool {
		a, b := entries[ips[i]].lastSeen, entries[ips[j]].lastSeen
		if a.Equal(b) {
			return ips[i] < ips[j]
		}
		return a.After(b)
	})
	for _, ip := range ips[limit:] {
		delete(entries, ip)
	}
}

func (t *deviceTracker) evictHandoverLocked(entries map[string]*deviceEntry, limit int, now time.Time) bool {
	if t.grace <= 0 {
		return false
	}
	needed := len(entries) - limit + 1
	silent := now.Add(-t.grace)
	candidates := make([]string, 0, len(entries))
	for ip, entry := range entries {
		if entry.inFlight == nil && !entry.lastSeen.After(silent) {
			candidates = append(candidates, ip)
		}
	}
	if len(candidates) < needed {
		return false
	}
	sort.Slice(candidates, func(i, j int) bool {
		return entries[candidates[i]].lastSeen.Before(entries[candidates[j]].lastSeen)
	})
	for _, ip := range candidates[:needed] {
		delete(entries, ip)
	}
	return true
}

func (t *deviceTracker) pruneLocked(entries map[string]*deviceEntry, now time.Time) {
	cutoff := now.Add(-t.ttl)
	for ip, entry := range entries {
		if entry.inFlight == nil && !entry.lastSeen.After(cutoff) {
			delete(entries, ip)
		}
	}
}

func (t *deviceTracker) Snapshot(now time.Time) ([]panel.OnlineUser, map[string]struct{}) {
	t.mu.Lock()
	defer t.mu.Unlock()
	online := make([]panel.OnlineUser, 0)
	active := make(map[string]struct{}, len(t.users))
	for userKey, entries := range t.users {
		t.pruneLocked(entries, now)
		if len(entries) == 0 {
			delete(t.users, userKey)
			continue
		}
		active[userKey] = struct{}{}
		for ip, entry := range entries {
			if entry.admitted {
				online = append(online, panel.OnlineUser{UID: entry.uid, IP: ip})
			}
		}
	}
	return online, active
}
