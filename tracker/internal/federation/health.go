package federation

import (
	"sync"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
)

// HealthConfig parameterizes the score formula; see spec §4.1 / §8.
// Defaults are filled by tracker/internal/config; this package does not
// fall back to defaults itself.
//
// Slice 8 added LatencyTarget + LatencyWeight as the fourth signal.
// Weights MUST sum to 1.0 (validator-enforced).
type HealthConfig struct {
	UptimeWindow        time.Duration
	RevGossipWindow     time.Duration
	RevGossipBufferSize int
	LatencyTarget       time.Duration // RTT at which latency sub-score reaches 0
	UptimeWeight        float64
	RevGossipWeight     float64
	LatencyWeight       float64
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
	equivocated     map[ids.TrackerID]struct{}
	latencyEWMA     map[ids.TrackerID]time.Duration // slice 8: smoothed RTT
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
		equivocated:     make(map[ids.TrackerID]struct{}),
		latencyEWMA:     make(map[ids.TrackerID]time.Duration),
	}
}

// OnLatencySample records a fresh RTT measurement for peer. Smoothed
// via EWMA with α=0.3 (favors recent samples but resists outliers).
// Negative inputs are clamped to zero.
func (h *PeerHealth) OnLatencySample(peer ids.TrackerID, rtt time.Duration) {
	if rtt < 0 {
		rtt = 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	prev, ok := h.latencyEWMA[peer]
	if !ok {
		h.latencyEWMA[peer] = rtt
		return
	}
	const alpha = 0.3
	h.latencyEWMA[peer] = time.Duration(float64(prev)*(1-alpha) + float64(rtt)*alpha)
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

// OnEquivocation flags peer as equivocating. Idempotent. The flag
// survives for the process lifetime unless cleared via
// ClearEquivocation (slice 17 admin path).
func (h *PeerHealth) OnEquivocation(peer ids.TrackerID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.equivocated[peer] = struct{}{}
}

// ClearEquivocation removes the sticky equivocation flag for peer,
// allowing the score to recover. Slice 17: invoked only by the admin
// API after operator review confirms a false positive. Idempotent —
// no-op when the flag was not set. Returns true if the flag was
// previously set (useful for audit logs).
func (h *PeerHealth) ClearEquivocation(peer ids.TrackerID) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, was := h.equivocated[peer]
	delete(h.equivocated, peer)
	return was
}

// Score computes the current health score for peer in [0, 1].
// See spec §4.1 for the formula. Fires onComputed with one of
// "ok" / "equivocated" / "no_data" so the metrics layer can count.
func (h *PeerHealth) Score(peer ids.TrackerID, now time.Time) float64 {
	h.mu.Lock()
	defer h.mu.Unlock()

	if _, equiv := h.equivocated[peer]; equiv {
		h.onComputed("equivocated")
		return 0
	}

	_, hasRoot := h.lastRoot[peer]
	rb, hasRing := h.revGossipDelays[peer]
	if !hasRoot && (!hasRing || rb.n == 0) {
		h.onComputed("no_data")
	} else {
		h.onComputed("ok")
	}

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

	latencySub := 1.0 // unknown latency → neutral
	if rtt, ok := h.latencyEWMA[peer]; ok && h.cfg.LatencyTarget > 0 {
		frac := 1.0 - float64(rtt)/float64(h.cfg.LatencyTarget)
		latencySub = clamp01(frac)
	}

	return h.cfg.UptimeWeight*uptimeSub +
		h.cfg.RevGossipWeight*revgossSub +
		h.cfg.LatencyWeight*latencySub
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
