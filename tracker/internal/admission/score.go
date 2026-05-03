package admission

import (
	"crypto/ed25519"
	"math"
	"time"

	admission "github.com/token-bay/token-bay/shared/admission"
	"github.com/token-bay/token-bay/shared/ids"
	signing "github.com/token-bay/token-bay/shared/signing"
	"github.com/token-bay/token-bay/tracker/internal/config"
)

// Signals carries the five raw signal values that feed ComputeLocalScore.
// Tests use these to assert per-signal correctness independently of the
// final weighted compose.
type Signals struct {
	SettlementReliability float64 // [0, 1]; -1 if undefined (no settlements)
	DisputeRate           float64 // [0, 1]; -1 if undefined (no settlements)
	TenureDays            int     // capped at cfg.TenureCapDays
	NetFlow               int64   // signed credits (earned - spent)
	BalanceCushionLog2    int     // clamped to [-8, 8]
}

// ComputeLocalScore returns the composite credit score in [0, 1] plus the
// five raw signals. Implements admission-design §5.2.
//
// A nil state (consumer never seen locally) returns cfg.TrialTierScore with
// zero-valued Signals — callers can branch on Signals.TenureDays == 0 to
// detect the trial-tier path.
//
// Weight redistribution: if a consumer has zero settlements, reliability
// and inverse-dispute are undefined. Their combined weight redistributes
// proportionally across the remaining three signals (tenure, net_flow,
// cushion) preserving the score-scale's [0, 1] semantics.
func ComputeLocalScore(state *ConsumerCreditState, cfg config.AdmissionConfig, now time.Time) (float64, Signals) {
	if state == nil {
		return cfg.TrialTierScore, Signals{}
	}

	signals := computeSignals(state, cfg, now)

	// Normalize.
	tenureNorm := 0.0
	if cfg.TenureCapDays > 0 {
		tenureNorm = math.Min(1.0, float64(signals.TenureDays)/float64(cfg.TenureCapDays))
	}
	netFlowNorm := sigmoid(float64(signals.NetFlow) / float64(maxInt(1, cfg.NetFlowNormalizationConstant)))
	cushionNorm := float64(signals.BalanceCushionLog2+8) / 16.0 // [-8,8] → [0,1]

	w := cfg.ScoreWeights
	if signals.SettlementReliability < 0 {
		// Undefined-signal redistribution: reliability + inverse_dispute
		// weights spread proportionally across the other three.
		undef := w.SettlementReliability + w.InverseDisputeRate
		defined := w.Tenure + w.NetCreditFlow + w.BalanceCushion
		if defined <= 0 {
			return cfg.TrialTierScore, signals
		}
		factor := 1.0 + undef/defined
		score := factor * (w.Tenure*tenureNorm + w.NetCreditFlow*netFlowNorm + w.BalanceCushion*cushionNorm)
		return clamp01(score), signals
	}

	inverseDispute := 1.0 - signals.DisputeRate
	score := w.SettlementReliability*signals.SettlementReliability +
		w.InverseDisputeRate*inverseDispute +
		w.Tenure*tenureNorm +
		w.NetCreditFlow*netFlowNorm +
		w.BalanceCushion*cushionNorm
	return clamp01(score), signals
}

// computeSignals materializes the five raw signals from state. Reliability
// and DisputeRate are -1 when no settlements exist (sentinel for "undefined").
func computeSignals(st *ConsumerCreditState, cfg config.AdmissionConfig, now time.Time) Signals {
	tenureDays := int(now.Sub(st.FirstSeenAt) / (24 * time.Hour))
	if tenureDays < 0 {
		tenureDays = 0
	}
	if tenureDays > cfg.TenureCapDays {
		tenureDays = cfg.TenureCapDays
	}

	var totalSettled, cleanCount uint32
	for _, b := range st.SettlementBuckets {
		totalSettled += b.Total
		cleanCount += b.A
	}
	var filed, upheld uint32
	for _, b := range st.DisputeBuckets {
		filed += b.A
		upheld += b.B
	}
	var earned, spent uint32
	for _, b := range st.FlowBuckets {
		earned += b.A
		spent += b.B
	}

	reliability := -1.0
	disputeRate := -1.0
	if totalSettled > 0 {
		reliability = float64(cleanCount) / float64(totalSettled)
		// (filed - upheld) / total settlements, clamped to [0, 1]
		net := int64(filed) - int64(upheld)
		if net < 0 {
			net = 0
		}
		disputeRate = float64(net) / float64(totalSettled)
		if disputeRate > 1 {
			disputeRate = 1
		}
	}

	netFlow := int64(earned) - int64(spent)
	cushion := floorLog2Ratio(st.LastBalanceSeen, int64(maxInt(1, cfg.StarterGrantCredits)))
	cushion = clampInt(cushion, -8, 8)

	return Signals{
		SettlementReliability: reliability,
		DisputeRate:           disputeRate,
		TenureDays:            tenureDays,
		NetFlow:               netFlow,
		BalanceCushionLog2:    cushion,
	}
}

// sigmoid maps R → (0, 1) with sigmoid(0) = 0.5. Used to normalize NetFlow
// into the score scale.
func sigmoid(x float64) float64 {
	return 1.0 / (1.0 + math.Exp(-x))
}

// clamp01 clamps a float to [0, 1].
func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

// clampInt clamps an int to [lo, hi] inclusive.
func clampInt(x, lo, hi int) int {
	if x < lo {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}

// maxInt returns the larger of two ints.
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// floorLog2Ratio returns floor(log2(num/den)) as an int. Returns math.MinInt
// when num <= 0 (semantically -∞; callers clamp the result).
func floorLog2Ratio(num, den int64) int {
	if num <= 0 || den <= 0 {
		return math.MinInt
	}
	return int(math.Floor(math.Log2(float64(num) / float64(den))))
}

// ---------------------------------------------------------------------------
// resolveCreditScore — admission-design §5.1 step 1
// ---------------------------------------------------------------------------

// ScoreSource indicates which path resolveCreditScore took. Returned to
// callers (Decide) for logging / metrics; not part of the admission
// decision itself.
type ScoreSource uint8

const (
	// ScoreSourceUnspecified is the zero value; never returned by
	// resolveCreditScore itself.
	ScoreSourceUnspecified ScoreSource = iota
	// ScoreSourceAttestation means a valid peer attestation was used, post-clamp.
	ScoreSourceAttestation
	// ScoreSourceLocal means local ConsumerCreditState was present and used.
	ScoreSourceLocal
	// ScoreSourceTrialTier means no attestation and no local history —
	// cfg.TrialTierScore was returned.
	ScoreSourceTrialTier
)

// resolveCreditScore implements admission-design §5.1 step 1. Five
// fall-through checks on the attestation; on any miss falls through to
// local-history compute; on empty local returns cfg.TrialTierScore.
//
// The MaxAttestationScoreImported clamp (§8.6) bounds peer-issued scores
// so a malicious-but-undetected peer cannot inflate above the regional
// ceiling for an arbitrary consumer.
func (s *Subsystem) resolveCreditScore(consumerID ids.IdentityID, att *admission.SignedCreditAttestation, now time.Time) (float64, ScoreSource) {
	if att != nil {
		if score, ok := s.tryAttestation(att, now); ok {
			return score, ScoreSourceAttestation
		}
	}
	// Local fall-through.
	if st, ok := consumerShardFor(s.consumerShards, consumerID).get(consumerID); ok {
		score, sigs := ComputeLocalScore(st, s.cfg, now)
		// "≥ 1 settled entry" gate per §5.1 step 1: only honor local if
		// the consumer has done real work.
		if hasAnySettlement(sigs) {
			return score, ScoreSourceLocal
		}
	}
	return s.cfg.TrialTierScore, ScoreSourceTrialTier
}

// tryAttestation runs the four §5.1 step-1 checks. Returns (score, true)
// on success and (0, false) on any miss — caller treats the latter as
// "fall through to local".
func (s *Subsystem) tryAttestation(att *admission.SignedCreditAttestation, now time.Time) (float64, bool) {
	if att.Body == nil {
		return 0, false
	}
	if err := admission.ValidateCreditAttestationBody(att.Body); err != nil {
		return 0, false
	}
	// Federation peer-set check.
	var issuer ids.IdentityID
	if len(att.Body.IssuerTrackerId) != len(issuer) {
		return 0, false
	}
	copy(issuer[:], att.Body.IssuerTrackerId)
	if !s.peers.Contains(issuer) {
		return 0, false
	}
	// Expiry. Wall-clock comparison; the validator already enforced
	// expires_at > computed_at and ttl ≤ 7d.
	if uint64(now.Unix()) >= att.Body.ExpiresAt {
		return 0, false
	}
	// Signature verification — pubkey reconstructed from issuer ID. v1
	// uses the raw IdentityID bytes as the pubkey; tracker spec §5.2 will
	// resolve a proper "issuer pubkey from federation roster" lookup once
	// federation is wired (see admission.go.trackerIDFromPubkey for the
	// matching v1 substitution on the issuer side).
	pub := ed25519.PublicKey(issuer[:])
	if !signing.VerifyCreditAttestation(pub, att) {
		return 0, false
	}
	score := float64(att.Body.Score) / 10000.0
	if score > s.cfg.MaxAttestationScoreImported {
		score = s.cfg.MaxAttestationScoreImported
	}
	return score, true
}

// hasAnySettlement reports whether ComputeLocalScore's signal output came
// from a non-trivial state (≥ 1 settlement) — used to gate the local
// score path. Reliability is the canonical "I have settlement history"
// indicator (set to ≥ 0 only when totalSettled > 0).
func hasAnySettlement(s Signals) bool {
	return s.SettlementReliability >= 0
}
