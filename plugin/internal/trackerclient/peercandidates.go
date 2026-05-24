package trackerclient

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
)

// peerCandidates owns the in-memory ranked set of bootstrap-fetched
// trackers (slice 6). The supervisor consults it on every endpoint
// pick. Expired snapshots are treated as empty.
//
// Concurrency: mu is held for the duration of Update and NextEndpoint;
// callers never hold a reference into the candidates slice across
// unlocks (NextEndpoint returns a value, not a slice index).
type peerCandidates struct {
	mu        sync.Mutex
	endpoints []TrackerEndpoint // sorted by HealthScore DESC, then Addr ASC
	expiresAt time.Time
	now       func() time.Time
}

// newPeerCandidates returns an empty candidate set. now must not be nil.
func newPeerCandidates(now func() time.Time) *peerCandidates {
	return &peerCandidates{now: now}
}

// Update replaces the candidate set with the freshly-fetched list,
// pre-sorting by HealthScore DESC, then Addr ASC for deterministic
// tie-break. expiresAt is the signed list's expiry from the tracker.
func (p *peerCandidates) Update(peers []BootstrapPeer, expiresAt time.Time) {
	sorted := make([]TrackerEndpoint, 0, len(peers))
	for _, bp := range peers {
		sorted = append(sorted, TrackerEndpoint{
			Addr:         bp.Addr,
			IdentityHash: ids.IdentityID(bp.TrackerID),
		})
	}
	// Stable sort on a parallel score slice keeps ordering deterministic
	// across calls with the same input.
	scores := make([]float64, len(peers))
	for i, bp := range peers {
		scores[i] = bp.HealthScore
	}
	idx := make([]int, len(peers))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		if scores[idx[a]] != scores[idx[b]] {
			return scores[idx[a]] > scores[idx[b]]
		}
		return sorted[idx[a]].Addr < sorted[idx[b]].Addr
	})
	out := make([]TrackerEndpoint, len(peers))
	for i, j := range idx {
		out[i] = sorted[j]
	}

	p.mu.Lock()
	p.endpoints = out
	p.expiresAt = expiresAt
	p.mu.Unlock()
}

// NextEndpoint returns the merged endpoint to dial for the supervisor
// iteration with index `epIdx`. Base endpoints come first (operator
// trust root); candidates follow in HealthScore order. The returned
// `source` is "base" or "candidate" for metrics.
//
// If the candidate set is empty or expired, only base endpoints are
// returned (identical to pre-slice behavior).
func (p *peerCandidates) NextEndpoint(epIdx int, base []TrackerEndpoint) (TrackerEndpoint, string) {
	if len(base) == 0 {
		return TrackerEndpoint{}, "base" // caller validates non-empty base
	}
	p.mu.Lock()
	cands := p.endpoints
	expires := p.expiresAt
	p.mu.Unlock()

	if !expires.IsZero() && p.now().After(expires) {
		cands = nil
	}

	merged := mergeEndpoints(base, cands)
	ep := merged[epIdx%len(merged)]
	source := "base"
	if epIdx%len(merged) >= len(base) {
		source = "candidate"
	}
	return ep, source
}

// mergeEndpoints returns base concatenated with candidates, dropping any
// candidate whose Addr collides with a base entry (operator config wins).
// A candidate with the same Addr but different IdentityHash is dropped
// silently — that's a misconfig signal but not a runtime error.
func mergeEndpoints(base, cands []TrackerEndpoint) []TrackerEndpoint {
	baseAddrs := make(map[string]struct{}, len(base))
	for _, b := range base {
		baseAddrs[strings.ToLower(b.Addr)] = struct{}{}
	}
	out := make([]TrackerEndpoint, 0, len(base)+len(cands))
	out = append(out, base...)
	for _, c := range cands {
		if _, dup := baseAddrs[strings.ToLower(c.Addr)]; dup {
			continue
		}
		out = append(out, c)
	}
	return out
}
