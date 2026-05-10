package federation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/token-bay/token-bay/shared/ids"
)

func TestPeerHealth_UptimeSubScore(t *testing.T) {
	now := time.Unix(10000, 0)
	clock := now
	h := NewPeerHealth(HealthConfig{
		UptimeWindow:        2 * time.Hour,
		RevGossipWindow:     600 * time.Second,
		RevGossipBufferSize: 16,
		UptimeWeight:        1.0,
		RevGossipWeight:     0.0,
	}, func() time.Time { return clock }, nil)

	var p ids.TrackerID
	p[0] = 0xAA

	// Never seen → 0.
	require.InDelta(t, 0.0, h.Score(p, now), 0.0001)

	// Just attested → 1.0 (full uptime, full weight).
	h.OnRootAttestation(p, now)
	require.InDelta(t, 1.0, h.Score(p, now), 0.0001)

	// 1h later → 0.5.
	require.InDelta(t, 0.5, h.Score(p, now.Add(1*time.Hour)), 0.0001)

	// 2h later → 0.0 (clamped at zero).
	require.InDelta(t, 0.0, h.Score(p, now.Add(2*time.Hour)), 0.0001)

	// 3h later → still 0.0 (clamped).
	require.InDelta(t, 0.0, h.Score(p, now.Add(3*time.Hour)), 0.0001)
}

func TestPeerHealth_RevGossipSubScore_Empty(t *testing.T) {
	now := time.Unix(10000, 0)
	h := NewPeerHealth(HealthConfig{
		UptimeWindow: 2 * time.Hour, RevGossipWindow: 600 * time.Second,
		RevGossipBufferSize: 16,
		UptimeWeight:        0.0, RevGossipWeight: 1.0,
	}, func() time.Time { return now }, nil)

	var p ids.TrackerID
	p[0] = 0xBB

	// No samples → revgoss=1.0 (neutral).
	require.InDelta(t, 1.0, h.Score(p, now), 0.0001)
}

func TestPeerHealth_RevGossipSubScore_Samples(t *testing.T) {
	now := time.Unix(10000, 0)
	h := NewPeerHealth(HealthConfig{
		UptimeWindow: 2 * time.Hour, RevGossipWindow: 600 * time.Second,
		RevGossipBufferSize: 16,
		UptimeWeight:        0.0, RevGossipWeight: 1.0,
	}, func() time.Time { return now }, nil)

	var p ids.TrackerID
	p[0] = 0xBB

	// One sample at 0s delay → revgoss=1.0.
	h.OnRevocation(p, now, now)
	require.InDelta(t, 1.0, h.Score(p, now), 0.0001)

	// Add a sample at 600s delay → average=300s → revgoss=0.5.
	h.OnRevocation(p, now.Add(-600*time.Second), now)
	require.InDelta(t, 0.5, h.Score(p, now), 0.0001)

	// Add a sample at 600s delay → still averaged in; mean=400 → 1-400/600=0.333.
	h.OnRevocation(p, now.Add(-600*time.Second), now)
	require.InDelta(t, 1.0-400.0/600.0, h.Score(p, now), 0.0001)
}

func TestPeerHealth_RevGossipSubScore_RingWrap(t *testing.T) {
	now := time.Unix(10000, 0)
	h := NewPeerHealth(HealthConfig{
		UptimeWindow: 2 * time.Hour, RevGossipWindow: 600 * time.Second,
		RevGossipBufferSize: 16,
		UptimeWeight:        0.0, RevGossipWeight: 1.0,
	}, func() time.Time { return now }, nil)

	var p ids.TrackerID
	p[0] = 0xCC

	// Push 16 samples at 0s delay (revgoss=1.0).
	for i := 0; i < 16; i++ {
		h.OnRevocation(p, now, now)
	}
	require.InDelta(t, 1.0, h.Score(p, now), 0.0001)

	// Now push 16 samples at 600s delay; ring is full, oldest 0s
	// samples are evicted; mean=600s; revgoss=0.0.
	for i := 0; i < 16; i++ {
		h.OnRevocation(p, now.Add(-600*time.Second), now)
	}
	require.InDelta(t, 0.0, h.Score(p, now), 0.0001)
}

func TestPeerHealth_EquivocationGate(t *testing.T) {
	now := time.Unix(10000, 0)
	h := NewPeerHealth(HealthConfig{
		UptimeWindow: 2 * time.Hour, RevGossipWindow: 600 * time.Second,
		RevGossipBufferSize: 16,
		UptimeWeight:        0.7, RevGossipWeight: 0.3,
	}, func() time.Time { return now }, nil)

	var p ids.TrackerID
	p[0] = 0xEE

	// Max signals + no equiv → 1.0.
	h.OnRootAttestation(p, now)
	require.InDelta(t, 0.7+0.3, h.Score(p, now), 0.0001)

	// Flag equivocation → score forced to 0 even with max signals.
	h.OnEquivocation(p)
	require.InDelta(t, 0.0, h.Score(p, now), 0.0001)

	// Idempotent: re-flag is fine.
	h.OnEquivocation(p)
	require.InDelta(t, 0.0, h.Score(p, now), 0.0001)
}

func TestPeerHealth_RevGossipSubScore_NegativeDelayClampedToZero(t *testing.T) {
	now := time.Unix(10000, 0)
	h := NewPeerHealth(HealthConfig{
		UptimeWindow: 2 * time.Hour, RevGossipWindow: 600 * time.Second,
		RevGossipBufferSize: 16,
		UptimeWeight:        0.0, RevGossipWeight: 1.0,
	}, func() time.Time { return now }, nil)
	var p ids.TrackerID
	p[0] = 0xDD

	// revoked_at in the future → negative delay treated as 0 → revgoss=1.0.
	h.OnRevocation(p, now.Add(60*time.Second), now)
	require.InDelta(t, 1.0, h.Score(p, now), 0.0001)
}
