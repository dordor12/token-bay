package federation

import (
	"sync"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
)

// HealthConfig parameterizes the score formula; see spec §4.1 / §8.
// Defaults are filled by tracker/internal/config; this package does not
// fall back to defaults itself.
type HealthConfig struct {
	UptimeWindow        time.Duration
	RevGossipWindow     time.Duration
	RevGossipBufferSize int
	UptimeWeight        float64
	RevGossipWeight     float64
}

// PeerHealth tracks per-peer health signals and computes a 0..1 score
// on demand. All maps are guarded by mu; no background goroutines.
type PeerHealth struct {
	cfg        HealthConfig
	now        func() time.Time
	onComputed func(outcome string)

	mu              sync.Mutex
	lastRoot        map[ids.TrackerID]time.Time
	revGossipDelays map[ids.TrackerID]*ringBuf16
}

// NewPeerHealth returns a fresh PeerHealth. now must not be nil.
// onComputed is fired by Score with the outcome label (one of
// "ok", "equivocated", "no_data"). May be nil; Task 12 wires it
// to the Prometheus counter.
func NewPeerHealth(cfg HealthConfig, now func() time.Time, onComputed func(outcome string)) *PeerHealth {
	if onComputed == nil {
		onComputed = func(string) {}
	}
	return &PeerHealth{
		cfg:             cfg,
		now:             now,
		onComputed:      onComputed,
		lastRoot:        make(map[ids.TrackerID]time.Time),
		revGossipDelays: make(map[ids.TrackerID]*ringBuf16),
	}
}

// OnRootAttestation records receipt time of the most recent successful
// KIND_ROOT_ATTESTATION from peer.
func (h *PeerHealth) OnRootAttestation(peer ids.TrackerID, recvAt time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastRoot[peer] = recvAt
}

// OnRevocation records a revocation issued by peer with the given
// revoked_at, observed at recvAt. Negative deltas are clamped to zero.
func (h *PeerHealth) OnRevocation(peer ids.TrackerID, revokedAt, recvAt time.Time) {
	delay := recvAt.Sub(revokedAt)
	if delay < 0 {
		delay = 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	rb, ok := h.revGossipDelays[peer]
	if !ok {
		rb = &ringBuf16{}
		h.revGossipDelays[peer] = rb
	}
	rb.push(delay)
}

// Score computes the current health score for peer in [0, 1].
// See spec §4.1 for the formula.
func (h *PeerHealth) Score(peer ids.TrackerID, now time.Time) float64 {
	h.mu.Lock()
	defer h.mu.Unlock()

	uptimeSub := 0.0
	if last, ok := h.lastRoot[peer]; ok {
		age := now.Sub(last)
		if age < 0 {
			age = 0
		}
		frac := 1.0 - float64(age)/float64(h.cfg.UptimeWindow)
		uptimeSub = clamp01(frac)
	}

	revgossSub := 1.0 // empty ring → neutral
	if rb, ok := h.revGossipDelays[peer]; ok && rb.n > 0 {
		mean := rb.mean()
		frac := 1.0 - float64(mean)/float64(h.cfg.RevGossipWindow)
		revgossSub = clamp01(frac)
	}

	return h.cfg.UptimeWeight*uptimeSub + h.cfg.RevGossipWeight*revgossSub
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// ringBuf16 is a fixed-capacity ring buffer of revocation-gossip
// delays per peer. push() evicts the oldest sample when full;
// mean() returns 0 for an empty buffer.
type ringBuf16 struct {
	xs   [16]time.Duration
	n    int // count of valid samples (≤ 16)
	head int // next write position
}

func (rb *ringBuf16) push(d time.Duration) {
	rb.xs[rb.head] = d
	rb.head = (rb.head + 1) % len(rb.xs)
	if rb.n < len(rb.xs) {
		rb.n++
	}
}

func (rb *ringBuf16) mean() time.Duration {
	if rb == nil || rb.n == 0 {
		return 0
	}
	var sum time.Duration
	for i := 0; i < rb.n; i++ {
		sum += rb.xs[i]
	}
	return sum / time.Duration(rb.n)
}
