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
