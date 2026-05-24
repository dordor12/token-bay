package federation

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/token-bay/token-bay/shared/ids"
)

func TestHealthWatcher_DepeersAfterSustainedLowScore(t *testing.T) {
	// Build a Federation with a single steady peer in the registry,
	// score it below threshold, and tick the watcher to assert depeer.
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	peerID := ids.TrackerID(sha256.Sum256(pub))

	reg := NewRegistry()
	require.NoError(t, reg.Add(PeerInfo{TrackerID: peerID, PubKey: pub, Addr: "x", State: PeerStateSteady}))

	// PeerHealth gives this peer score=0 because it has never been
	// observed (no_data) under UptimeWeight=1.0, RevGossipWeight=0.
	now := time.Unix(10000, 0)
	cfg := Config{
		Health:                   HealthConfig{UptimeWindow: 2 * time.Hour, UptimeWeight: 1.0},
		LowHealthThreshold:       0.2,
		LowHealthSustainedWindow: 5 * time.Minute,
		HealthWatchInterval:      30 * time.Second,
	}
	cfg = cfg.withDefaults()
	f := &Federation{
		cfg:    cfg,
		reg:    reg,
		health: NewPeerHealth(cfg.Health, func() time.Time { return now }, nil),
		peers:  map[ids.TrackerID]*Peer{}, // empty Stop-able set
	}

	state := &healthWatcherState{belowSince: map[ids.TrackerID]time.Time{}}
	// First tick: peer is below threshold, state records belowSince.
	f.runHealthWatcherTick(now, state)
	require.Contains(t, state.belowSince, peerID)
	// Peer NOT depeered yet (window not elapsed).
	_, ok := reg.Get(peerID)
	require.True(t, ok)

	// Tick again after window. Watcher fires Depeer.
	f.runHealthWatcherTick(now.Add(6*time.Minute), state)
	_, ok = reg.Get(peerID)
	require.False(t, ok, "peer should be depeered after sustained low score")
}

func TestHealthWatcher_HysteresisForgivesBriefDip(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	peerID := ids.TrackerID(sha256.Sum256(pub))

	reg := NewRegistry()
	require.NoError(t, reg.Add(PeerInfo{TrackerID: peerID, PubKey: pub, Addr: "x", State: PeerStateSteady}))

	now := time.Unix(10000, 0)
	cfg := Config{
		Health:                   HealthConfig{UptimeWindow: 2 * time.Hour, UptimeWeight: 1.0},
		LowHealthThreshold:       0.2,
		LowHealthSustainedWindow: 5 * time.Minute,
		HealthWatchInterval:      30 * time.Second,
	}
	cfg = cfg.withDefaults()
	h := NewPeerHealth(cfg.Health, func() time.Time { return now }, nil)
	f := &Federation{
		cfg:    cfg,
		reg:    reg,
		health: h,
		peers:  map[ids.TrackerID]*Peer{},
	}

	state := &healthWatcherState{belowSince: map[ids.TrackerID]time.Time{}}
	// Tick 1: score=0 (no_data) → belowSince set.
	f.runHealthWatcherTick(now, state)
	require.Contains(t, state.belowSince, peerID)

	// Peer recovers: OnRootAttestation → score=1.0.
	h.OnRootAttestation(peerID, now.Add(1*time.Minute))

	// Tick 2: score ≥ threshold → belowSince cleared.
	f.runHealthWatcherTick(now.Add(2*time.Minute), state)
	require.NotContains(t, state.belowSince, peerID)

	// Window elapses but no depeer because hysteresis reset.
	_, ok := reg.Get(peerID)
	require.True(t, ok)
}
