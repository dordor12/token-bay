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

	mu       sync.Mutex
	lastRoot map[ids.TrackerID]time.Time
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
		cfg:        cfg,
		now:        now,
		onComputed: onComputed,
		lastRoot:   make(map[ids.TrackerID]time.Time),
	}
}

// OnRootAttestation records receipt time of the most recent successful
// KIND_ROOT_ATTESTATION from peer.
func (h *PeerHealth) OnRootAttestation(peer ids.TrackerID, recvAt time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastRoot[peer] = recvAt
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

	// Revgoss + equiv come in later tasks; for Task 1 they are zero
	// and the weight on revgoss is also zero in this test.
	return h.cfg.UptimeWeight*uptimeSub + h.cfg.RevGossipWeight*0.0
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
