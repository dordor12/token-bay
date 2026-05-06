package admission

import (
	"crypto/rand"
	"encoding/binary"
	mrand "math/rand"
	"time"

	sharedadmission "github.com/token-bay/token-bay/shared/admission"
	"github.com/token-bay/token-bay/shared/ids"
)

// Outcome enumerates Decide's three result shapes.
type Outcome uint8

const (
	// OutcomeUnspecified is the zero value; never returned by Decide.
	OutcomeUnspecified Outcome = iota
	// OutcomeAdmit signals the request is forwarded to a seeder immediately.
	OutcomeAdmit
	// OutcomeQueue signals the request is held in the admission queue.
	OutcomeQueue
	// OutcomeReject signals the request is turned away with a retry hint.
	OutcomeReject
)

// PositionBand maps queue position to a banded value (admission-design §6.5).
// Banding limits information leakage about exact queue depth.
type PositionBand uint8

const (
	// PositionBandUnspecified is the zero value.
	PositionBandUnspecified PositionBand = iota
	// PositionBand1To10 covers positions 1–10.
	PositionBand1To10
	// PositionBand11To50 covers positions 11–50.
	PositionBand11To50
	// PositionBand51To200 covers positions 51–200.
	PositionBand51To200
	// PositionBand200Plus covers positions > 200.
	PositionBand200Plus
)

// EtaBand maps queue ETA to a banded value (admission-design §6.5).
type EtaBand uint8

const (
	// EtaBandUnspecified is the zero value.
	EtaBandUnspecified EtaBand = iota
	// EtaBandLessThan30s covers ETA < 30s.
	EtaBandLessThan30s
	// EtaBand30sTo2m covers 30s ≤ ETA ≤ 2m.
	EtaBand30sTo2m
	// EtaBand2mTo5m covers 2m < ETA ≤ 5m.
	EtaBand2mTo5m
	// EtaBand5mPlus covers ETA > 5m.
	EtaBand5mPlus
)

// RejectReason mirrors the §5.1/§5.4 reasons for an OutcomeReject.
type RejectReason uint8

const (
	// RejectReasonUnspecified is the zero value.
	RejectReasonUnspecified RejectReason = iota
	// RejectReasonRegionOverloaded means pressure ≥ PressureRejectThreshold
	// or the queue is at capacity.
	RejectReasonRegionOverloaded
	// RejectReasonQueueTimeout means the entry was evicted after
	// QueueTimeoutS seconds without being served.
	RejectReasonQueueTimeout
)

// QueuedDetails carries the user-facing fields returned with OutcomeQueue.
type QueuedDetails struct {
	// RequestID is a 16-byte crypto-random identifier for the queued request.
	RequestID [16]byte
	// PositionBand is a coarse band for the consumer's queue position.
	PositionBand PositionBand
	// EtaBand is a coarse band for the estimated wait time.
	EtaBand EtaBand
}

// RejectedDetails carries the user-facing fields returned with OutcomeReject.
type RejectedDetails struct {
	// Reason explains why the request was rejected.
	Reason RejectReason
	// RetryAfterS is a suggested number of seconds to wait before retrying.
	RetryAfterS uint32
}

// Result is returned by Decide. Exactly one of {Queued, Rejected}
// is non-nil when Outcome is OutcomeQueue or OutcomeReject respectively;
// both are nil for OutcomeAdmit.
type Result struct {
	// Outcome is the branching result of the hot path.
	Outcome Outcome
	// Queued is non-nil when Outcome == OutcomeQueue.
	Queued *QueuedDetails
	// Rejected is non-nil when Outcome == OutcomeReject.
	Rejected *RejectedDetails
	// CreditUsed is the resolved credit score in [0, 1] that drove the decision.
	CreditUsed float64
}

// Decide is the hot path. Implements admission-design §5.1 step 4.
//
// It resolves the consumer's credit score (attestation → local → trial tier),
// records a demand tick for the EWMA, reads the current SupplySnapshot via
// atomic load (no lock), and branches:
//   - pressure < PressureAdmitThreshold → ADMIT
//   - pressure < PressureRejectThreshold AND queue not full → QUEUE
//   - otherwise → REJECT{RegionOverloaded, retry_after_s}
//
// Pre-publication (boot-time) Supply() returns emptySupplySnapshot whose
// ComputedAt is zero; Decide treats that as "unconditional admit" until the
// 5s aggregator first publishes.
func (s *Subsystem) Decide(consumerID ids.IdentityID, att *sharedadmission.SignedCreditAttestation, now time.Time) Result {
	start := time.Now()
	defer func() {
		if s.metrics != nil {
			s.metrics.DecisionDuration.Observe(time.Since(start).Seconds())
		}
	}()

	score, _ := s.resolveCreditScore(consumerID, att, now)
	s.recordDemandTick(now)

	supply := s.Supply()

	// Admit branch: low pressure or no supply data yet (boot-time).
	if supply.ComputedAt.IsZero() || supply.Pressure < s.cfg.PressureAdmitThreshold {
		s.bumpDecision("admit")
		return Result{Outcome: OutcomeAdmit, CreditUsed: score}
	}

	// Queue branch: medium pressure with room in the queue.
	if supply.Pressure < s.cfg.PressureRejectThreshold {
		s.queueMu.Lock()
		queueSize := s.queue.Len()
		if queueSize < s.cfg.QueueCap {
			entry := QueueEntry{
				RequestID:   newRequestID(),
				ConsumerID:  consumerID,
				CreditScore: score,
				EnqueuedAt:  now,
			}
			s.queue.AdvanceTime(now)
			s.queue.Push(entry)
			pos := s.queue.Len()
			s.queueMu.Unlock()
			s.bumpDecision("queue")
			if s.metrics != nil {
				s.metrics.QueueDepth.Set(float64(pos))
			}
			return Result{
				Outcome: OutcomeQueue,
				Queued: &QueuedDetails{
					RequestID:    entry.RequestID,
					PositionBand: bandPosition(pos),
					EtaBand:      bandEta(estimateEta(pos, supply)),
				},
				CreditUsed: score,
			}
		}
		s.queueMu.Unlock()
	}

	retry := computeRetryAfterS(s.queue.Len(), 200*time.Millisecond, mrand.New(mrand.NewSource(now.UnixNano()))) //nolint:gosec // G404 — jitter
	s.bumpDecision("reject")
	if s.metrics != nil {
		s.metrics.RejectionsByReason.WithLabelValues("region_overloaded").Inc()
	}
	return Result{
		Outcome: OutcomeReject,
		Rejected: &RejectedDetails{
			Reason:      RejectReasonRegionOverloaded,
			RetryAfterS: retry,
		},
		CreditUsed: score,
	}
}

func (s *Subsystem) bumpDecision(label string) {
	if s.metrics != nil {
		s.metrics.Decisions.WithLabelValues(label).Inc()
	}
}

// bandPosition maps a 1-indexed queue position to a PositionBand.
func bandPosition(p int) PositionBand {
	switch {
	case p <= 0:
		return PositionBandUnspecified
	case p <= 10:
		return PositionBand1To10
	case p <= 50:
		return PositionBand11To50
	case p <= 200:
		return PositionBand51To200
	default:
		return PositionBand200Plus
	}
}

// bandEta maps a duration to an EtaBand.
func bandEta(d time.Duration) EtaBand {
	switch {
	case d < 30*time.Second:
		return EtaBandLessThan30s
	case d <= 2*time.Minute:
		return EtaBand30sTo2m
	case d <= 5*time.Minute:
		return EtaBand2mTo5m
	default:
		return EtaBand5mPlus
	}
}

// estimateEta is a heuristic: queue position × average service time.
// The exact number is not surfaced to consumers (§6.5) — only the band.
func estimateEta(pos int, _ *SupplySnapshot) time.Duration {
	const avgServiceTime = 200 * time.Millisecond
	return time.Duration(pos) * avgServiceTime
}

// computeRetryAfterS implements admission-design §5.4 backoff:
//
//	retry_after_s = clamp(30 + jitter[0,30) + queue_drain_estimate, 60, 600)
//
// where queue_drain_estimate = queueSize × avgServiceTime.
func computeRetryAfterS(queueSize int, avgServiceTime time.Duration, r *mrand.Rand) uint32 {
	const baseS = 30
	jitter := r.Intn(30) // [0, 30)
	drainS := int(time.Duration(queueSize) * avgServiceTime / time.Second)
	v := baseS + jitter + drainS
	if v < 60 {
		v = 60
	}
	if v > 600 {
		v = 600
	}
	return uint32(v) //nolint:gosec // G115 — clamped to [60, 600], never negative
}

// newRequestID returns a 16-byte crypto-random request ID. Using crypto/rand
// prevents consumers from correlating each other's queue positions.
func newRequestID() [16]byte {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		// Fall back to time-based — not security critical, but avoid all-zeros
		// when /dev/urandom is unavailable.
		binary.BigEndian.PutUint64(id[:8], uint64(time.Now().UnixNano())) //nolint:gosec // G115 — fallback when /dev/urandom unavailable; non-security path
	}
	return id
}
