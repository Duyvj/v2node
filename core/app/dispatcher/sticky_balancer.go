package dispatcher

import (
	"context"
	"sync"
	"time"

	"github.com/xtls/xray-core/app/observatory"
	"github.com/xtls/xray-core/features/extension"
)

const defaultStickyBalancerGroup = "default_balancer"

type stickySessionEntry struct {
	group    string
	tag      string
	lastSeen time.Time
}

// StickyBalancer maintains per-balancer session affinities and balances new
// sessions across healthy outbound targets reported by Observatory.
type StickyBalancer struct {
	mu              sync.RWMutex
	candidateGroups map[string][]string
	sessions        map[string]*stickySessionEntry
	observatory     extension.Observatory
	obsCtx          context.Context
	stopCh          chan struct{}
	closeOnce       sync.Once
}

// NewStickyBalancer creates and returns an initialized StickyBalancer.
func NewStickyBalancer() *StickyBalancer {
	b := &StickyBalancer{
		candidateGroups: make(map[string][]string),
		sessions:        make(map[string]*stickySessionEntry),
		stopCh:          make(chan struct{}),
	}
	go b.cleanupLoop()
	return b
}

// ConfigureStickyBalancerGroups must be called before this dispatcher starts.
// Each core owns its groups and observation feature; preparing a replacement
// core cannot mutate the routing state of an existing, serving core.
func (d *DefaultDispatcher) ConfigureStickyBalancerGroups(groups map[string][]string) {
	if d.stickyBalancer == nil {
		d.stickyBalancer = NewStickyBalancer()
	}
	d.stickyBalancer.SetCandidateGroups(groups)
}

// SetCandidates updates the default candidate group.
func (b *StickyBalancer) SetCandidates(tags []string) {
	b.SetCandidateGroups(map[string][]string{defaultStickyBalancerGroup: tags})
}

// SetCandidateGroups replaces candidate groups and drops affinities that no
// longer reference a configured candidate.
func (b *StickyBalancer) SetCandidateGroups(groups map[string][]string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	updated := make(map[string][]string, len(groups))
	for group, tags := range groups {
		if group == "" || len(tags) == 0 {
			continue
		}
		seen := make(map[string]struct{}, len(tags))
		candidates := make([]string, 0, len(tags))
		for _, tag := range tags {
			if tag == "" {
				continue
			}
			if _, exists := seen[tag]; exists {
				continue
			}
			seen[tag] = struct{}{}
			candidates = append(candidates, tag)
		}
		if len(candidates) > 0 {
			updated[group] = candidates
		}
	}
	b.candidateGroups = updated

	for key, entry := range b.sessions {
		candidates, exists := updated[entry.group]
		if !exists || !containsCandidate(candidates, entry.tag) {
			delete(b.sessions, key)
		}
	}
}

// SetObservatory attaches the active Observatory feature to the balancer.
func (b *StickyBalancer) SetObservatory(ctx context.Context, obs extension.Observatory) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.obsCtx = ctx
	b.observatory = obs
}

// HasGroup reports whether a routing balancer has sticky candidates.
func (b *StickyBalancer) HasGroup(group string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.candidateGroups[group]) > 0
}

// GetCandidates returns a copy of the default group's configured tags.
func (b *StickyBalancer) GetCandidates() []string {
	return b.GetCandidatesForGroup(defaultStickyBalancerGroup)
}

// GetCandidatesForGroup returns a copy of a group's configured tags.
func (b *StickyBalancer) GetCandidatesForGroup(group string) []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]string(nil), b.candidateGroups[group]...)
}

// GetHealthyCandidates returns the healthy candidates from the default group.
func (b *StickyBalancer) GetHealthyCandidates() []string {
	return b.GetHealthyCandidatesForGroup(defaultStickyBalancerGroup)
}

// GetHealthyCandidatesForGroup returns candidates considered alive by
// Observatory. Outbounds not probed yet remain eligible so a fresh core can
// start serving before its first health-check cycle completes.
func (b *StickyBalancer) GetHealthyCandidatesForGroup(group string) []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.healthyCandidatesLocked(group)
}

// Caller holds mu throughout observation and selection, preventing a reload
// from assigning an outbound removed between those two operations.
func (b *StickyBalancer) healthyCandidatesLocked(group string) []string {
	candidates := append([]string(nil), b.candidateGroups[group]...)
	obs := b.observatory
	ctx := b.obsCtx

	if len(candidates) == 0 {
		return nil
	}
	if obs == nil || ctx == nil {
		return candidates
	}

	report, err := obs.GetObservation(ctx)
	if err != nil || report == nil {
		return candidates
	}

	result, ok := report.(*observatory.ObservationResult)
	if !ok || result == nil || len(result.Status) == 0 {
		return candidates
	}

	statusMap := make(map[string]*observatory.OutboundStatus, len(result.Status))
	for _, status := range result.Status {
		if status != nil {
			statusMap[status.OutboundTag] = status
		}
	}

	healthy := make([]string, 0, len(candidates))
	for _, tag := range candidates {
		if status, found := statusMap[tag]; !found || status.Alive {
			healthy = append(healthy, tag)
		}
	}
	if len(healthy) == 0 {
		// Keep routing attempts alive if every probe is temporarily down.
		return candidates
	}
	return healthy
}

// PickOutbound selects a sticky outbound from the default group.
func (b *StickyBalancer) PickOutbound(sessionKey string) string {
	return b.PickOutboundForGroup(defaultStickyBalancerGroup, sessionKey)
}

// PickOutboundForGroup selects a sticky outbound for one balancer group. If a
// prior assignment is still healthy it remains fixed; otherwise it fails over
// once and records the replacement so recovery cannot change the IP again.
func (b *StickyBalancer) PickOutboundForGroup(group, sessionKey string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	healthy := b.healthyCandidatesLocked(group)
	if len(healthy) == 0 {
		return ""
	}

	now := time.Now()

	key := stickySessionKey(group, sessionKey)
	if sessionKey != "" {
		if entry, exists := b.sessions[key]; exists {
			if containsCandidate(healthy, entry.tag) {
				entry.lastSeen = now
				return entry.tag
			}
		}
	}

	counts := make(map[string]int, len(healthy))
	for _, tag := range healthy {
		counts[tag] = 0
	}
	for _, entry := range b.sessions {
		if entry.group == group && now.Sub(entry.lastSeen) < 60*time.Minute {
			if _, exists := counts[entry.tag]; exists {
				counts[entry.tag]++
			}
		}
	}

	bestTag := healthy[0]
	for _, tag := range healthy[1:] {
		if counts[tag] < counts[bestTag] {
			bestTag = tag
		}
	}

	if sessionKey != "" {
		b.sessions[key] = &stickySessionEntry{group: group, tag: bestTag, lastSeen: now}
	}
	return bestTag
}

func containsCandidate(candidates []string, tag string) bool {
	for _, candidate := range candidates {
		if candidate == tag {
			return true
		}
	}
	return false
}

func stickySessionKey(group, sessionKey string) string {
	return group + "\x00" + sessionKey
}

func (b *StickyBalancer) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-b.stopCh:
			return
		case now := <-ticker.C:
			b.mu.Lock()
			for key, entry := range b.sessions {
				if now.Sub(entry.lastSeen) > 60*time.Minute {
					delete(b.sessions, key)
				}
			}
			b.mu.Unlock()
		}
	}
}

// Close terminates the balancer cleanup loop.
func (b *StickyBalancer) Close() {
	b.closeOnce.Do(func() {
		close(b.stopCh)
	})
}
