package dispatcher

import (
	"context"
	"sync"
	"time"

	"github.com/xtls/xray-core/app/observatory"
	"github.com/xtls/xray-core/features/extension"
)

type stickySessionEntry struct {
	tag      string
	lastSeen time.Time
}

// StickyBalancer maintains sticky session affinities and balances new sessions
// across healthy outbound targets reported by Observatory.
type StickyBalancer struct {
	mu          sync.RWMutex
	candidates  []string
	sessions    map[string]*stickySessionEntry
	observatory extension.Observatory
	obsCtx      context.Context
	stopCh      chan struct{}
}

var globalStickyBalancer = NewStickyBalancer()

// NewStickyBalancer creates and returns an initialized StickyBalancer.
func NewStickyBalancer() *StickyBalancer {
	b := &StickyBalancer{
		sessions: make(map[string]*stickySessionEntry),
		stopCh:   make(chan struct{}),
	}
	go b.cleanupLoop()
	return b
}

// GetStickyBalancer returns the process-wide StickyBalancer instance.
func GetStickyBalancer() *StickyBalancer {
	return globalStickyBalancer
}

// ConfigureStickyBalancer updates the candidate outbound tags for the global sticky balancer.
func ConfigureStickyBalancer(tags []string) {
	globalStickyBalancer.SetCandidates(tags)
}

// SetCandidates updates the candidate tags managed by the balancer.
func (b *StickyBalancer) SetCandidates(tags []string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.candidates = make([]string, len(tags))
	copy(b.candidates, tags)

	valid := make(map[string]bool, len(tags))
	for _, t := range tags {
		valid[t] = true
	}
	for k, v := range b.sessions {
		if !valid[v.tag] {
			delete(b.sessions, k)
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

// GetCandidates returns a copy of configured candidate tags.
func (b *StickyBalancer) GetCandidates() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	res := make([]string, len(b.candidates))
	copy(res, b.candidates)
	return res
}

// GetHealthyCandidates returns the subset of candidates considered healthy/alive.
func (b *StickyBalancer) GetHealthyCandidates() []string {
	b.mu.RLock()
	candidates := b.candidates
	obs := b.observatory
	ctx := b.obsCtx
	b.mu.RUnlock()

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
	for _, s := range result.Status {
		if s != nil {
			statusMap[s.OutboundTag] = s
		}
	}

	var healthy []string
	for _, tag := range candidates {
		if st, ok := statusMap[tag]; ok {
			if st.Alive {
				healthy = append(healthy, tag)
			}
		} else {
			// Untested candidates are tentatively treated as alive
			healthy = append(healthy, tag)
		}
	}

	if len(healthy) == 0 {
		// If all candidates are marked down by probes, keep routing attempts alive
		return candidates
	}
	return healthy
}

// PickOutbound selects a sticky outbound for a session. If the session has an
// existing assignment to a healthy tag, it remains sticky ("giữ cố định").
// Otherwise, it assigns the least-loaded healthy candidate tag.
func (b *StickyBalancer) PickOutbound(sessionKey string) string {
	healthy := b.GetHealthyCandidates()
	if len(healthy) == 0 {
		return ""
	}
	if len(healthy) == 1 {
		return healthy[0]
	}

	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()

	if sessionKey != "" {
		if entry, exists := b.sessions[sessionKey]; exists {
			for _, h := range healthy {
				if h == entry.tag {
					entry.lastSeen = now
					return entry.tag
				}
			}
			// Previously assigned outbound is no longer healthy, reassign
		}
	}

	// Count active sessions per candidate tag
	counts := make(map[string]int, len(healthy))
	for _, h := range healthy {
		counts[h] = 0
	}
	for _, entry := range b.sessions {
		if now.Sub(entry.lastSeen) < 60*time.Minute {
			if _, ok := counts[entry.tag]; ok {
				counts[entry.tag]++
			}
		}
	}

	// Pick healthy candidate with minimum active sessions
	bestTag := healthy[0]
	minCount := counts[bestTag]
	for _, h := range healthy[1:] {
		if counts[h] < minCount {
			bestTag = h
			minCount = counts[h]
		}
	}

	if sessionKey != "" {
		b.sessions[sessionKey] = &stickySessionEntry{
			tag:      bestTag,
			lastSeen: now,
		}
	}

	return bestTag
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
			for k, v := range b.sessions {
				if now.Sub(v.lastSeen) > 60*time.Minute {
					delete(b.sessions, k)
				}
			}
			b.mu.Unlock()
		}
	}
}

// Close terminates the balancer cleanup loop.
func (b *StickyBalancer) Close() {
	select {
	case <-b.stopCh:
	default:
		close(b.stopCh)
	}
}
