package admission

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/signing"
)

func TestIssueAttestation_NoLocalHistory_ReturnsSentinel(t *testing.T) {
	s, _ := openTempSubsystem(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	att, err := s.IssueAttestation(makeID(0x11), now)
	assert.Nil(t, att)
	assert.True(t, errors.Is(err, ErrNoLocalHistory))
}

func TestIssueAttestation_HappyPath_RoundTripsScoreAndVerifies(t *testing.T) {
	s, _ := openTempSubsystem(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	id := makeID(0x11)

	// Seed local history.
	st := consumerShardFor(s.consumerShards, id).getOrInit(id, now.AddDate(0, 0, -10))
	st.SettlementBuckets[0] = DayBucket{Total: 50, A: 50, DayStamp: stripToDay(now)}
	st.LastBalanceSeen = 1000

	att, err := s.IssueAttestation(id, now)
	require.NoError(t, err)
	require.NotNil(t, att)

	// Verifies under tracker's own pubkey.
	assert.True(t, signing.VerifyCreditAttestation(s.pub, att))

	// Body shape sanity.
	assert.Equal(t, id[:], att.Body.IdentityId)
	assert.Equal(t, s.trackerID[:], att.Body.IssuerTrackerId)
	assert.Greater(t, att.Body.Score, uint32(0))
	assert.LessOrEqual(t, att.Body.Score, uint32(10000))
	assert.Equal(t, uint64(now.Unix()), att.Body.ComputedAt)
	assert.Equal(t, uint64(now.Add(time.Duration(s.cfg.AttestationTTLSeconds)*time.Second).Unix()), att.Body.ExpiresAt)
}

func TestIssueAttestation_EmbeddedScoreMatchesLocalCompute(t *testing.T) {
	s, _ := openTempSubsystem(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	id := makeID(0x11)

	// Seed history with predictable signals.
	st := consumerShardFor(s.consumerShards, id).getOrInit(id, now.AddDate(0, 0, -30))
	st.SettlementBuckets[0] = DayBucket{Total: 100, A: 100, DayStamp: stripToDay(now)}
	st.LastBalanceSeen = 1000

	want, _ := ComputeLocalScore(st, s.cfg, now)
	att, err := s.IssueAttestation(id, now)
	require.NoError(t, err)
	gotScore := float64(att.Body.Score) / 10000.0
	assert.InDelta(t, want, gotScore, 1e-3, "embedded score must match ComputeLocalScore (admission-design §10 #8)")
}

func TestIssueAttestation_RateLimitExceeded_ReturnsSentinel(t *testing.T) {
	s, _ := openTempSubsystem(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	id := makeID(0x11)
	st := consumerShardFor(s.consumerShards, id).getOrInit(id, now.AddDate(0, 0, -10))
	st.SettlementBuckets[0] = DayBucket{Total: 10, A: 10, DayStamp: stripToDay(now)}

	// Default cap is 6/hr.
	for i := 0; i < s.cfg.AttestationIssuancePerConsumerPerHour; i++ {
		_, err := s.IssueAttestation(id, now.Add(time.Duration(i)*time.Minute))
		require.NoError(t, err)
	}
	// (cap+1)th call must reject.
	_, err := s.IssueAttestation(id, now.Add(time.Duration(s.cfg.AttestationIssuancePerConsumerPerHour)*time.Minute))
	assert.True(t, errors.Is(err, ErrRateLimited))
}

func TestIssueAttestation_RateLimitWindowSlides(t *testing.T) {
	s, _ := openTempSubsystem(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	id := makeID(0x11)
	st := consumerShardFor(s.consumerShards, id).getOrInit(id, now.AddDate(0, 0, -10))
	st.SettlementBuckets[0] = DayBucket{Total: 10, A: 10, DayStamp: stripToDay(now)}

	// Burn the cap.
	for i := 0; i < s.cfg.AttestationIssuancePerConsumerPerHour; i++ {
		_, err := s.IssueAttestation(id, now)
		require.NoError(t, err)
	}
	// One hour and one second later, the window has slid past — call succeeds.
	_, err := s.IssueAttestation(id, now.Add(time.Hour+time.Second))
	require.NoError(t, err)
}
