package admission

import (
	"time"

	"github.com/token-bay/token-bay/shared/ids"
)

// LedgerEventKind classifies an in-process ledger event observed by
// admission. Mirrors admission-design §5.6.
type LedgerEventKind uint8

const (
	// LedgerEventUnspecified is the zero value; OnLedgerEvent ignores it.
	LedgerEventUnspecified LedgerEventKind = iota
	// LedgerEventSettlement: a usage entry has finalized.
	LedgerEventSettlement
	// LedgerEventTransferIn: peer-region credit landing in this consumer.
	LedgerEventTransferIn
	// LedgerEventTransferOut: peer-region credit leaving this consumer.
	LedgerEventTransferOut
	// LedgerEventStarterGrant: starter grant issued.
	LedgerEventStarterGrant
	// LedgerEventDisputeFiled: a dispute was opened.
	LedgerEventDisputeFiled
	// LedgerEventDisputeResolved: a dispute was closed.
	// DisputeUpheld == true means the consumer prevailed.
	LedgerEventDisputeResolved
)

// LedgerEvent is the single input to admission's OnLedgerEvent. Real
// ledger emission is a separate plan; tests drive this directly.
type LedgerEvent struct {
	Kind          LedgerEventKind
	ConsumerID    ids.IdentityID
	SeederID      ids.IdentityID // zero for non-usage kinds
	CostCredits   uint64
	Flags         uint32 // bit 0 = consumer_sig_missing (settlements only)
	DisputeUpheld bool   // settlements: false; resolved disputes: true → UPHELD
	Timestamp     time.Time
}

// LedgerEventObserver is the consumer-side contract. Subsystem implements
// it; admission-aware ledger callers (broker, settlement) will dispatch
// to one or more observers via a tiny pub/sub introduced in a follow-up
// ledger plan.
type LedgerEventObserver interface {
	OnLedgerEvent(ev LedgerEvent)
}

// OnLedgerEvent updates in-memory bucket state per admission-design §5.6.
// Persistence (admission.tlog) is plan 3 — this revision is in-memory only.
func (s *Subsystem) OnLedgerEvent(ev LedgerEvent) {
	switch ev.Kind {
	case LedgerEventSettlement:
		s.applySettlement(ev)
	case LedgerEventTransferIn:
		s.applyTransfer(ev.ConsumerID, signedDelta(ev.CostCredits, true), ev.Timestamp)
	case LedgerEventTransferOut:
		s.applyTransfer(ev.ConsumerID, signedDelta(ev.CostCredits, false), ev.Timestamp)
	case LedgerEventStarterGrant:
		s.applyStarterGrant(ev)
	case LedgerEventDisputeFiled:
		s.applyDispute(ev, false)
	case LedgerEventDisputeResolved:
		if ev.DisputeUpheld {
			s.applyDispute(ev, true)
		}
		// rejected resolutions: no state change beyond the original Filed.
	case LedgerEventUnspecified:
		// no-op
	}
}

func (s *Subsystem) applySettlement(ev LedgerEvent) {
	idx := dayBucketIndex(ev.Timestamp, rollingWindowDays)

	cst := consumerShardFor(s.consumerShards, ev.ConsumerID).getOrInit(ev.ConsumerID, ev.Timestamp)
	cst.SettlementBuckets[idx] = rotateIfStale(cst.SettlementBuckets[idx], ev.Timestamp, rollingWindowDays)
	cst.SettlementBuckets[idx].Total++
	if ev.Flags&1 == 0 {
		cst.SettlementBuckets[idx].A++
	}
	cst.FlowBuckets[idx] = rotateIfStale(cst.FlowBuckets[idx], ev.Timestamp, rollingWindowDays)
	cst.FlowBuckets[idx].B += uint32(ev.CostCredits) //nolint:gosec // G115 — cost is ≤ uint32 ceiling in practice
	cst.LastBalanceSeen -= int64(ev.CostCredits)     //nolint:gosec // G115 — same

	if ev.SeederID != (ids.IdentityID{}) {
		sst := consumerShardFor(s.consumerShards, ev.SeederID).getOrInit(ev.SeederID, ev.Timestamp)
		sst.FlowBuckets[idx] = rotateIfStale(sst.FlowBuckets[idx], ev.Timestamp, rollingWindowDays)
		sst.FlowBuckets[idx].A += uint32(ev.CostCredits) //nolint:gosec // G115
		sst.LastBalanceSeen += int64(ev.CostCredits)     //nolint:gosec // G115
	}
}

func (s *Subsystem) applyTransfer(id ids.IdentityID, delta int64, now time.Time) {
	st := consumerShardFor(s.consumerShards, id).getOrInit(id, now)
	st.LastBalanceSeen += delta
}

func (s *Subsystem) applyStarterGrant(ev LedgerEvent) {
	st := consumerShardFor(s.consumerShards, ev.ConsumerID).getOrInit(ev.ConsumerID, ev.Timestamp)
	if st.FirstSeenAt.IsZero() || st.FirstSeenAt.After(ev.Timestamp) {
		st.FirstSeenAt = ev.Timestamp
	}
	st.LastBalanceSeen = int64(ev.CostCredits) //nolint:gosec // G115
}

func (s *Subsystem) applyDispute(ev LedgerEvent, upheld bool) {
	idx := dayBucketIndex(ev.Timestamp, rollingWindowDays)
	st := consumerShardFor(s.consumerShards, ev.ConsumerID).getOrInit(ev.ConsumerID, ev.Timestamp)
	st.DisputeBuckets[idx] = rotateIfStale(st.DisputeBuckets[idx], ev.Timestamp, rollingWindowDays)
	if upheld {
		st.DisputeBuckets[idx].B++
	} else {
		st.DisputeBuckets[idx].A++
	}
}

// signedDelta returns +cost for positive transfers (in) and -cost for
// negative transfers (out). Centralized so the int64 conversion lives
// in one place.
func signedDelta(cost uint64, positive bool) int64 {
	d := int64(cost) //nolint:gosec // G115 — cost is settlement-bounded uint64
	if !positive {
		return -d
	}
	return d
}
