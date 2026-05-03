package admission

import (
	"crypto/ed25519"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	admission "github.com/token-bay/token-bay/shared/admission"
	"github.com/token-bay/token-bay/shared/ids"
	signing "github.com/token-bay/token-bay/shared/signing"
	"github.com/token-bay/token-bay/tracker/internal/config"
	"github.com/token-bay/token-bay/tracker/internal/registry"
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

// ---------------------------------------------------------------------------
// Task 5: resolveCreditScore tests
// ---------------------------------------------------------------------------

func TestResolveCreditScore_NilAttestation_NoLocal_ReturnsTrialTier(t *testing.T) {
	s, _ := openTempSubsystem(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	score, source := s.resolveCreditScore(makeID(0x11), nil, now)
	assert.InDelta(t, s.cfg.TrialTierScore, score, 1e-9)
	assert.Equal(t, ScoreSourceTrialTier, source)
}

func TestResolveCreditScore_NilAttestation_LocalHistory_UsesLocal(t *testing.T) {
	s, _ := openTempSubsystem(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	id := makeID(0x11)

	// Seed local history.
	st := consumerShardFor(s.consumerShards, id).getOrInit(id, now.AddDate(0, 0, -10))
	st.SettlementBuckets[0] = DayBucket{Total: 50, A: 50, DayStamp: stripToDay(now)}
	st.LastBalanceSeen = 1000

	score, source := s.resolveCreditScore(id, nil, now)
	assert.Greater(t, score, s.cfg.TrialTierScore, "local should outrank trial-tier")
	assert.Equal(t, ScoreSourceLocal, source)
}

func TestResolveCreditScore_ValidAttestationFromPeer_UsedAndClamped(t *testing.T) {
	peerPriv, peerID := fixturePeerKeypair(t)
	s, _ := openTempSubsystem(t, WithPeerSet(staticPeers{peerID: true}))
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	consumerID := makeID(0x11)
	body := &admission.CreditAttestationBody{
		IdentityId:            consumerID[:],
		IssuerTrackerId:       peerID[:],
		Score:                 9800, // 0.98 — exceeds 0.95 default cap
		TenureDays:            120,
		SettlementReliability: 9700,
		DisputeRate:           50,
		NetCreditFlow_30D:     250000,
		BalanceCushionLog2:    3,
		ComputedAt:            uint64(now.Add(-time.Hour).Unix()),
		ExpiresAt:             uint64(now.Add(23 * time.Hour).Unix()),
	}
	att := signFixtureAttestation(t, peerPriv, body)

	score, source := s.resolveCreditScore(makeID(0x11), att, now)
	assert.InDelta(t, s.cfg.MaxAttestationScoreImported, score, 1e-9, "clamp at MaxAttestationScoreImported")
	assert.Equal(t, ScoreSourceAttestation, source)
}

func TestResolveCreditScore_ValidAttestationBelowClamp_PassThrough(t *testing.T) {
	peerPriv, peerID := fixturePeerKeypair(t)
	s, _ := openTempSubsystem(t, WithPeerSet(staticPeers{peerID: true}))
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	body := validAttestationBody(now, peerID, 0x11)
	body.Score = 7000 // 0.70 — below clamp
	att := signFixtureAttestation(t, peerPriv, body)

	score, _ := s.resolveCreditScore(makeID(0x11), att, now)
	assert.InDelta(t, 0.70, score, 1e-9)
}

func TestResolveCreditScore_AttestationFromUnknownPeer_FallsThrough(t *testing.T) {
	otherPriv, otherID := fixturePeerKeypair(t)
	s, _ := openTempSubsystem(t) // default peer-set is always-false
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	body := validAttestationBody(now, otherID, 0x11)
	body.Score = 9000
	att := signFixtureAttestation(t, otherPriv, body)

	score, source := s.resolveCreditScore(makeID(0x11), att, now)
	// no local history → trial tier
	assert.InDelta(t, s.cfg.TrialTierScore, score, 1e-9)
	assert.Equal(t, ScoreSourceTrialTier, source)
}

func TestResolveCreditScore_ExpiredAttestation_FallsThrough(t *testing.T) {
	peerPriv, peerID := fixturePeerKeypair(t)
	s, _ := openTempSubsystem(t, WithPeerSet(staticPeers{peerID: true}))
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	body := validAttestationBody(now.Add(-48*time.Hour), peerID, 0x11)
	body.ExpiresAt = uint64(now.Add(-1 * time.Hour).Unix()) // expired 1h ago
	att := signFixtureAttestation(t, peerPriv, body)

	score, source := s.resolveCreditScore(makeID(0x11), att, now)
	assert.InDelta(t, s.cfg.TrialTierScore, score, 1e-9)
	assert.Equal(t, ScoreSourceTrialTier, source)
}

func TestResolveCreditScore_TamperedAttestation_FallsThrough(t *testing.T) {
	peerPriv, peerID := fixturePeerKeypair(t)
	s, _ := openTempSubsystem(t, WithPeerSet(staticPeers{peerID: true}))
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	body := validAttestationBody(now, peerID, 0x11)
	att := signFixtureAttestation(t, peerPriv, body)
	att.Body.Score = 9999 // post-sign mutation invalidates sig

	score, source := s.resolveCreditScore(makeID(0x11), att, now)
	assert.InDelta(t, s.cfg.TrialTierScore, score, 1e-9)
	assert.Equal(t, ScoreSourceTrialTier, source)
}

func TestResolveCreditScore_MalformedAttestationBody_FallsThrough(t *testing.T) {
	peerPriv, peerID := fixturePeerKeypair(t)
	s, _ := openTempSubsystem(t, WithPeerSet(staticPeers{peerID: true}))
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	body := validAttestationBody(now, peerID, 0x11)
	body.IdentityId = []byte{1, 2, 3} // wrong length — ValidateCreditAttestationBody rejects
	att := signFixtureAttestation(t, peerPriv, body)

	score, source := s.resolveCreditScore(makeID(0x11), att, now)
	assert.InDelta(t, s.cfg.TrialTierScore, score, 1e-9)
	assert.Equal(t, ScoreSourceTrialTier, source)
}

// ---------------------------------------------------------------------------
// Task 5 inline helpers (extracted into helpers_test.go in Task 14)
// ---------------------------------------------------------------------------

func validAttestationBody(now time.Time, peerID ids.IdentityID, consumerByte byte) *admission.CreditAttestationBody {
	cid := makeID(consumerByte)
	return &admission.CreditAttestationBody{
		IdentityId:            cid[:],
		IssuerTrackerId:       peerID[:],
		Score:                 7000,
		TenureDays:            60,
		SettlementReliability: 9000,
		DisputeRate:           100,
		NetCreditFlow_30D:     50000,
		BalanceCushionLog2:    1,
		ComputedAt:            uint64(now.Add(-time.Hour).Unix()),
		ExpiresAt:             uint64(now.Add(23 * time.Hour).Unix()),
	}
}

func signFixtureAttestation(t *testing.T, priv ed25519.PrivateKey, body *admission.CreditAttestationBody) *admission.SignedCreditAttestation {
	t.Helper()
	sig, err := signing.SignCreditAttestation(priv, body)
	require.NoError(t, err)
	return &admission.SignedCreditAttestation{Body: body, TrackerSig: sig}
}

func fixturePeerKeypair(t *testing.T) (ed25519.PrivateKey, ids.IdentityID) {
	t.Helper()
	seed := []byte("admission-fixture-peer-seed-v1-x") // 32 bytes
	require.Len(t, seed, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	var id ids.IdentityID
	copy(id[:], pub[:32])
	return priv, id
}

// staticPeers is a map-backed PeerSet used in Task 5 tests.
type staticPeers map[ids.IdentityID]bool

func (s staticPeers) Contains(id ids.IdentityID) bool { return s[id] }

func openTempSubsystem(t *testing.T, opts ...Option) (*Subsystem, ids.IdentityID) {
	t.Helper()
	seed := []byte("admission-fixture-tracker-seed-1") // 32 bytes
	require.Len(t, seed, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(seed)

	cfg := defaultScoreConfig()
	reg, err := registry.New(16)
	require.NoError(t, err)

	s, err := Open(cfg, reg, priv, opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s, s.trackerID
}
