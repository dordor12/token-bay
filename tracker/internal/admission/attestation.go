package admission

import (
	"errors"
	"math"
	"sync"
	"time"

	sharedadmission "github.com/token-bay/token-bay/shared/admission"
	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/shared/signing"
)

// Sentinel errors returned by IssueAttestation.
var (
	ErrNoLocalHistory = errors.New("admission: no local consumer history")
	ErrRateLimited    = errors.New("admission: per-consumer attestation issuance rate limit exceeded")
)

// IssueAttestation returns a signed attestation built from the consumer's
// local credit state. Implements admission-design §5.5.
//
// Returns ErrNoLocalHistory if no ConsumerCreditState exists for the
// consumer (caller must have ledger history before requesting). Returns
// ErrRateLimited if the per-consumer rate limit (default 6/hr) is hit.
func (s *Subsystem) IssueAttestation(consumerID ids.IdentityID, now time.Time) (*sharedadmission.SignedCreditAttestation, error) {
	st, ok := consumerShardFor(s.consumerShards, consumerID).get(consumerID)
	if !ok {
		return nil, ErrNoLocalHistory
	}

	if !s.attestRL.allow(consumerID, now) {
		return nil, ErrRateLimited
	}

	score, sigs := ComputeLocalScore(st, s.cfg, now)

	body := &sharedadmission.CreditAttestationBody{
		IdentityId:            consumerID[:],
		IssuerTrackerId:       s.trackerID[:],
		Score:                 uint32(math.Round(score * 10000)),
		TenureDays:            uint32(sigs.TenureDays), //nolint:gosec // G115: TenureDays is capped at cfg.TenureCapDays (a small positive int)
		SettlementReliability: nonNegativeFixedPoint(sigs.SettlementReliability),
		DisputeRate:           nonNegativeFixedPoint(sigs.DisputeRate),
		NetCreditFlow_30D:     sigs.NetFlow,
		BalanceCushionLog2:    int32(sigs.BalanceCushionLog2),                                                   //nolint:gosec // G115: clamped to [-8, 8] by computeSignals
		ComputedAt:            uint64(now.Unix()),                                                               //nolint:gosec // G115: Unix() ≥ 0 for any post-epoch timestamp
		ExpiresAt:             uint64(now.Add(time.Duration(s.cfg.AttestationTTLSeconds) * time.Second).Unix()), //nolint:gosec // G115: same as ComputedAt
	}
	if err := sharedadmission.ValidateCreditAttestationBody(body); err != nil {
		// Defensive — should never happen given clamps in ComputeLocalScore.
		return nil, err
	}
	sig, err := signing.SignCreditAttestation(s.priv, body)
	if err != nil {
		return nil, err
	}
	return &sharedadmission.SignedCreditAttestation{Body: body, TrackerSig: sig}, nil
}

// nonNegativeFixedPoint converts a [-1, 1] signal to a uint32 in [0, 10000].
// Sentinel -1 (undefined) maps to 0.
func nonNegativeFixedPoint(v float64) uint32 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		v = 1
	}
	return uint32(math.Round(v * 10000))
}

// rateLimiter is a per-identity sliding-window counter. One window per
// identity, sharded by IdentityID mod numShards. Each window holds the
// last N issuance timestamps; allow() prunes events older than 1h and
// rejects when len ≥ cap.
type rateLimiter struct {
	cap    int
	window time.Duration
	shards []*rlShard
}

// rlShard holds the sliding-window timestamp lists for a subset of consumer IDs.
type rlShard struct {
	mu sync.Mutex
	m  map[ids.IdentityID][]time.Time
}

// newRateLimiter constructs a sharded sliding-window rate limiter.
func newRateLimiter(perHourCap int, numShards int) *rateLimiter {
	rl := &rateLimiter{
		cap:    perHourCap,
		window: time.Hour,
		shards: make([]*rlShard, numShards),
	}
	for i := range rl.shards {
		rl.shards[i] = &rlShard{m: make(map[ids.IdentityID][]time.Time)}
	}
	return rl
}

// allow reports whether a new event for id at now is within the rate limit.
// It prunes stale events and records the new one when permitted.
// A cap ≤ 0 is treated as unlimited (no enforcement).
func (rl *rateLimiter) allow(id ids.IdentityID, now time.Time) bool {
	if rl.cap <= 0 {
		return true
	}
	sh := rl.shards[shardIndex(id, len(rl.shards))]
	sh.mu.Lock()
	defer sh.mu.Unlock()

	cutoff := now.Add(-rl.window)
	events := sh.m[id]
	// Prune events outside the sliding window.
	pruned := events[:0]
	for _, t := range events {
		if t.After(cutoff) {
			pruned = append(pruned, t)
		}
	}
	if len(pruned) >= rl.cap {
		sh.m[id] = pruned
		return false
	}
	sh.m[id] = append(pruned, now)
	return true
}
