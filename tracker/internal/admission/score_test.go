package admission

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/token-bay/token-bay/tracker/internal/config"
)

func defaultScoreConfig() config.AdmissionConfig {
	return config.AdmissionConfig{
		TrialTierScore:      0.4,
		AgingAlphaPerMinute: 0.05,
		QueueTimeoutS:       300,
		ScoreWeights: config.AdmissionScoreWeights{
			SettlementReliability: 0.30,
			InverseDisputeRate:    0.10,
			Tenure:                0.20,
			NetCreditFlow:         0.30,
			BalanceCushion:        0.10,
		},
		NetFlowNormalizationConstant: 10000,
		TenureCapDays:                30,
		StarterGrantCredits:          1000,
		RollingWindowDays:            30,
		PressureAdmitThreshold:       0.85,
		PressureRejectThreshold:      1.5,
		QueueCap:                     512,
		MaxAttestationScoreImported:  0.95,
	}
}

func TestComputeLocalScore_AllSignalsKnown_WeightedSum(t *testing.T) {
	cfg := defaultScoreConfig()
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	st := &ConsumerCreditState{FirstSeenAt: now.AddDate(0, 0, -30), LastBalanceSeen: 1000}
	// 100 settlements, all clean → reliability = 1.0
	st.SettlementBuckets[0] = DayBucket{Total: 100, A: 100, DayStamp: stripToDay(now)}
	// 0 disputes filed → dispute_rate = 0 → inverse = 1.0
	// Flow: earned 5000, spent 5000 → net = 0 → sigmoid(0) = 0.5
	st.FlowBuckets[0] = DayBucket{A: 5000, B: 5000, DayStamp: stripToDay(now)}

	score, sig := ComputeLocalScore(st, cfg, now)

	assert.InDelta(t, 1.0, sig.SettlementReliability, 1e-9)
	assert.InDelta(t, 0.0, sig.DisputeRate, 1e-9)
	assert.Equal(t, 30, sig.TenureDays)
	assert.Equal(t, int64(0), sig.NetFlow)
	assert.Equal(t, 0, sig.BalanceCushionLog2) // log2(1000/1000) = 0

	// Normalized:
	//   reliability        = 1.0
	//   inverse_dispute    = 1.0
	//   tenure_norm        = 30/30 = 1.0
	//   net_flow_norm      = sigmoid(0/10000) = 0.5
	//   cushion_norm       = (0 + 8) / 16 = 0.5
	// score = 0.30*1 + 0.10*1 + 0.20*1 + 0.30*0.5 + 0.10*0.5 = 0.80
	assert.InDelta(t, 0.80, score, 1e-6)
}

func TestComputeLocalScore_NoSettlements_RedistributesWeights(t *testing.T) {
	cfg := defaultScoreConfig()
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	st := &ConsumerCreditState{FirstSeenAt: now.AddDate(0, 0, -10), LastBalanceSeen: 1000}
	// no settlements, no disputes, no flow

	score, sig := ComputeLocalScore(st, cfg, now)
	assert.Equal(t, 10, sig.TenureDays)

	// Reliability + inverse-dispute weights (0.40 total) redistribute to the
	// remaining three (tenure 0.20, net_flow 0.30, cushion 0.10 — sum 0.60).
	// Adjusted weights: tenure 0.20 + 0.20*0.40/0.60 = 0.20 + 0.13333 = 0.33333
	//                   net_flow 0.30 + 0.30*0.40/0.60 = 0.30 + 0.20000 = 0.50000
	//                   cushion 0.10 + 0.10*0.40/0.60 = 0.10 + 0.06667 = 0.16667
	// tenure_norm = 10/30 = 0.33333
	// net_flow_norm = sigmoid(0) = 0.5
	// cushion_norm = (0+8)/16 = 0.5
	// score = 0.33333*0.33333 + 0.50000*0.5 + 0.16667*0.5 = 0.11111 + 0.25 + 0.08333 = 0.44444
	assert.InDelta(t, 0.44444, score, 1e-4)
}

func TestComputeLocalScore_TenureCappedAtCfg(t *testing.T) {
	cfg := defaultScoreConfig()
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	st := &ConsumerCreditState{FirstSeenAt: now.AddDate(-1, 0, 0)} // ~365 days

	_, sig := ComputeLocalScore(st, cfg, now)
	assert.Equal(t, cfg.TenureCapDays, sig.TenureDays)
}

func TestComputeLocalScore_DisputeRateClampedToZero(t *testing.T) {
	cfg := defaultScoreConfig()
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	st := &ConsumerCreditState{FirstSeenAt: now.AddDate(0, 0, -10)}

	// 5 settlements; 10 disputes filed, 5 upheld → (10-5)/5 = 1.0 (rate)
	st.SettlementBuckets[0] = DayBucket{Total: 5, A: 5, DayStamp: stripToDay(now)}
	st.DisputeBuckets[0] = DayBucket{A: 10, B: 5, DayStamp: stripToDay(now)}

	_, sig := ComputeLocalScore(st, cfg, now)
	assert.InDelta(t, 1.0, sig.DisputeRate, 1e-9)
}

func TestComputeLocalScore_BalanceCushionClamped(t *testing.T) {
	cfg := defaultScoreConfig()
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	t.Run("very high balance clamps to +8", func(t *testing.T) {
		st := &ConsumerCreditState{FirstSeenAt: now.AddDate(0, 0, -1), LastBalanceSeen: 1_000_000_000}
		_, sig := ComputeLocalScore(st, cfg, now)
		assert.Equal(t, 8, sig.BalanceCushionLog2)
	})

	t.Run("zero balance clamps to -8", func(t *testing.T) {
		st := &ConsumerCreditState{FirstSeenAt: now.AddDate(0, 0, -1), LastBalanceSeen: 0}
		_, sig := ComputeLocalScore(st, cfg, now)
		assert.Equal(t, -8, sig.BalanceCushionLog2)
	})
}

func TestComputeLocalScore_NilStateReturnsTrialTier(t *testing.T) {
	cfg := defaultScoreConfig()
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	score, _ := ComputeLocalScore(nil, cfg, now)
	assert.InDelta(t, cfg.TrialTierScore, score, 1e-9)
}

func TestSigmoid_ZeroIsHalf(t *testing.T) {
	assert.InDelta(t, 0.5, sigmoid(0), 1e-9)
}

func TestSigmoid_LargePositiveApproachesOne(t *testing.T) {
	assert.Greater(t, sigmoid(10), 0.999)
}

func TestSigmoid_LargeNegativeApproachesZero(t *testing.T) {
	assert.Less(t, sigmoid(-10), 0.001)
}

func TestClampInt_OutsideRange(t *testing.T) {
	assert.Equal(t, -8, clampInt(-100, -8, 8))
	assert.Equal(t, 8, clampInt(100, -8, 8))
	assert.Equal(t, 3, clampInt(3, -8, 8))
}

func TestFloorLog2Ratio(t *testing.T) {
	assert.Equal(t, 0, floorLog2Ratio(1000, 1000)) // log2(1)
	assert.Equal(t, 1, floorLog2Ratio(2000, 1000)) // log2(2)
	assert.Equal(t, 3, floorLog2Ratio(8000, 1000)) // log2(8)
	assert.Equal(t, -1, floorLog2Ratio(500, 1000)) // log2(0.5)
	assert.Equal(t, math.MinInt, floorLog2Ratio(0, 1000))
}
