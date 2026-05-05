package admission

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestDecide_LowPressure_Admits(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)))
	s.publishSupply(&SupplySnapshot{ComputedAt: now, TotalHeadroom: 10, Pressure: 0.5})

	res := s.Decide(makeID(0x11), nil, now)
	assert.Equal(t, OutcomeAdmit, res.Outcome)
	assert.Nil(t, res.Queued)
	assert.Nil(t, res.Rejected)
	assert.InDelta(t, s.cfg.TrialTierScore, res.CreditUsed, 1e-9)
}

func TestDecide_MediumPressure_Queues(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)))
	// 0.85 ≤ p < 1.5 → QUEUE
	s.publishSupply(&SupplySnapshot{ComputedAt: now, TotalHeadroom: 10, Pressure: 1.0})

	res := s.Decide(makeID(0x11), nil, now)
	assert.Equal(t, OutcomeQueue, res.Outcome)
	assert.NotNil(t, res.Queued)
	assert.Equal(t, PositionBand1To10, res.Queued.PositionBand, "first queued entry → band 1-10")
}

func TestDecide_HighPressure_Rejects(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)))
	s.publishSupply(&SupplySnapshot{ComputedAt: now, TotalHeadroom: 10, Pressure: 2.0})

	res := s.Decide(makeID(0x11), nil, now)
	assert.Equal(t, OutcomeReject, res.Outcome)
	assert.NotNil(t, res.Rejected)
	assert.Equal(t, RejectReasonRegionOverloaded, res.Rejected.Reason)
	assert.GreaterOrEqual(t, res.Rejected.RetryAfterS, uint32(60))
	assert.LessOrEqual(t, res.Rejected.RetryAfterS, uint32(600))
}

func TestDecide_QueueOverflow_Rejects(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)))
	s.publishSupply(&SupplySnapshot{ComputedAt: now, TotalHeadroom: 10, Pressure: 1.0})

	// Fill the queue to cap.
	for i := 0; i < s.cfg.QueueCap; i++ {
		_ = s.Decide(makeIDi(i), nil, now)
	}
	// Next request must REJECT.
	res := s.Decide(makeID(0xFE), nil, now)
	assert.Equal(t, OutcomeReject, res.Outcome)
	assert.Equal(t, RejectReasonRegionOverloaded, res.Rejected.Reason)
}

func TestDecide_PrePublishSnapshot_Admits(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)))
	// Don't publish — Supply() returns emptySupplySnapshot
	res := s.Decide(makeID(0x11), nil, now)
	assert.Equal(t, OutcomeAdmit, res.Outcome,
		"boot-time admit until aggregator publishes")
}

func TestBandPosition_Boundaries(t *testing.T) {
	cases := []struct {
		size int
		want PositionBand
	}{
		{1, PositionBand1To10},
		{10, PositionBand1To10},
		{11, PositionBand11To50},
		{50, PositionBand11To50},
		{51, PositionBand51To200},
		{200, PositionBand51To200},
		{201, PositionBand200Plus},
		{10000, PositionBand200Plus},
	}
	for _, c := range cases {
		t.Run("", func(t *testing.T) {
			assert.Equal(t, c.want, bandPosition(c.size))
		})
	}
}

func TestBandEta_Boundaries(t *testing.T) {
	cases := []struct {
		eta  time.Duration
		want EtaBand
	}{
		{15 * time.Second, EtaBandLessThan30s},
		{30 * time.Second, EtaBand30sTo2m},
		{2 * time.Minute, EtaBand30sTo2m},
		{3 * time.Minute, EtaBand2mTo5m},
		{5 * time.Minute, EtaBand2mTo5m},
		{10 * time.Minute, EtaBand5mPlus},
	}
	for _, c := range cases {
		t.Run("", func(t *testing.T) {
			assert.Equal(t, c.want, bandEta(c.eta))
		})
	}
}

func TestRetryAfter_RangeAndJitter(t *testing.T) {
	// Drive deterministic jitter via an injected RNG.
	got := make(map[uint32]struct{})
	for i := 0; i < 100; i++ {
		retry := computeRetryAfterS(50 /*queue*/, 200*time.Millisecond /*svc*/, fixedRand(int64(i)))
		assert.GreaterOrEqual(t, retry, uint32(60))
		assert.LessOrEqual(t, retry, uint32(600))
		got[retry] = struct{}{}
	}
	assert.Greater(t, len(got), 5, "jitter should produce a spread of values")
}
