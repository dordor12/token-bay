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

// OnLedgerEvent persists each in-memory state change to the tlog first,
// then applies it. A write failure aborts the apply so disk and memory
// stay consistent; the next call retries.
//
// During StartupReplay the s.replaying flag suppresses tlog writes
// (records are already on disk).
func (s *Subsystem) OnLedgerEvent(ev LedgerEvent) {
	seq := s.nextSeq.Add(1)

	switch ev.Kind {
	case LedgerEventSettlement:
		p := SettlementPayload{
			ConsumerID:  ev.ConsumerID,
			SeederID:    ev.SeederID,
			CostCredits: ev.CostCredits,
			Flags:       ev.Flags,
		}
		if err := s.persistEvent(seq, TLogKindSettlement, p, ev.Timestamp); err != nil {
			return
		}
		s.applySettlement(ev)
	case LedgerEventTransferIn:
		p := TransferPayload{ConsumerID: ev.ConsumerID, CostCredits: ev.CostCredits, Direction: TransferIn}
		if err := s.persistEvent(seq, TLogKindTransfer, p, ev.Timestamp); err != nil {
			return
		}
		s.applyTransfer(ev.ConsumerID, signedDelta(ev.CostCredits, true), ev.Timestamp)
	case LedgerEventTransferOut:
		p := TransferPayload{ConsumerID: ev.ConsumerID, CostCredits: ev.CostCredits, Direction: TransferOut}
		if err := s.persistEvent(seq, TLogKindTransfer, p, ev.Timestamp); err != nil {
			return
		}
		s.applyTransfer(ev.ConsumerID, signedDelta(ev.CostCredits, false), ev.Timestamp)
	case LedgerEventStarterGrant:
		p := StarterGrantPayload{ConsumerID: ev.ConsumerID, CostCredits: ev.CostCredits}
		if err := s.persistEvent(seq, TLogKindStarterGrant, p, ev.Timestamp); err != nil {
			return
		}
		s.applyStarterGrant(ev)
	case LedgerEventDisputeFiled:
		p := DisputePayload{ConsumerID: ev.ConsumerID, Upheld: false}
		if err := s.persistEvent(seq, TLogKindDisputeFiled, p, ev.Timestamp); err != nil {
			return
		}
		s.applyDispute(ev, false)
	case LedgerEventDisputeResolved:
		if !ev.DisputeUpheld {
			return
		}
		p := DisputePayload{ConsumerID: ev.ConsumerID, Upheld: true}
		if err := s.persistEvent(seq, TLogKindDisputeResolved, p, ev.Timestamp); err != nil {
			return
		}
		s.applyDispute(ev, true)
	case LedgerEventUnspecified:
		// no-op
	}
}

type binaryMarshaler interface {
	MarshalBinary() ([]byte, error)
}

// persistEvent appends one TLogRecord. No-op when the tlog is disabled
// or replay is in progress (records being applied came from disk;
// double-writing would create infinite-growth loops on every restart).
func (s *Subsystem) persistEvent(seq uint64, kind TLogKind, body binaryMarshaler, ts time.Time) error {
	if s.replaying.Load() || s.tlog == nil {
		return nil
	}
	payload, err := body.MarshalBinary()
	if err != nil {
		return err
	}
	return s.tlog.Append(TLogRecord{
		Seq:     seq,
		Kind:    kind,
		Payload: payload,
		Ts:      uint64(ts.Unix()), //nolint:gosec // G115 — post-1970
	})
}

func (s *Subsystem) applySettlement(ev LedgerEvent) {
	idx := dayBucketIndex(ev.Timestamp, rollingWindowDays)

	csh := consumerShardFor(s.consumerShards, ev.ConsumerID)
	cst := csh.getOrInit(ev.ConsumerID, ev.Timestamp)
	csh.mu.Lock()
	cst.SettlementBuckets[idx] = rotateIfStale(cst.SettlementBuckets[idx], ev.Timestamp, rollingWindowDays)
	cst.SettlementBuckets[idx].Total++
	if ev.Flags&1 == 0 {
		cst.SettlementBuckets[idx].A++
	}
	cst.FlowBuckets[idx] = rotateIfStale(cst.FlowBuckets[idx], ev.Timestamp, rollingWindowDays)
	cst.FlowBuckets[idx].B += uint32(ev.CostCredits) //nolint:gosec // G115 — cost is ≤ uint32 ceiling in practice
	cst.LastBalanceSeen -= int64(ev.CostCredits)     //nolint:gosec // G115 — same
	csh.mu.Unlock()

	if ev.SeederID != (ids.IdentityID{}) {
		ssh := consumerShardFor(s.consumerShards, ev.SeederID)
		sst := ssh.getOrInit(ev.SeederID, ev.Timestamp)
		ssh.mu.Lock()
		sst.FlowBuckets[idx] = rotateIfStale(sst.FlowBuckets[idx], ev.Timestamp, rollingWindowDays)
		sst.FlowBuckets[idx].A += uint32(ev.CostCredits) //nolint:gosec // G115
		sst.LastBalanceSeen += int64(ev.CostCredits)     //nolint:gosec // G115
		ssh.mu.Unlock()
	}
}

func (s *Subsystem) applyTransfer(id ids.IdentityID, delta int64, now time.Time) {
	sh := consumerShardFor(s.consumerShards, id)
	st := sh.getOrInit(id, now)
	sh.mu.Lock()
	st.LastBalanceSeen += delta
	sh.mu.Unlock()
}

func (s *Subsystem) applyStarterGrant(ev LedgerEvent) {
	sh := consumerShardFor(s.consumerShards, ev.ConsumerID)
	st := sh.getOrInit(ev.ConsumerID, ev.Timestamp)
	sh.mu.Lock()
	if st.FirstSeenAt.IsZero() || st.FirstSeenAt.After(ev.Timestamp) {
		st.FirstSeenAt = ev.Timestamp
	}
	st.LastBalanceSeen = int64(ev.CostCredits) //nolint:gosec // G115
	sh.mu.Unlock()
}

func (s *Subsystem) applyDispute(ev LedgerEvent, upheld bool) {
	idx := dayBucketIndex(ev.Timestamp, rollingWindowDays)
	sh := consumerShardFor(s.consumerShards, ev.ConsumerID)
	st := sh.getOrInit(ev.ConsumerID, ev.Timestamp)
	sh.mu.Lock()
	st.DisputeBuckets[idx] = rotateIfStale(st.DisputeBuckets[idx], ev.Timestamp, rollingWindowDays)
	if upheld {
		st.DisputeBuckets[idx].B++
	} else {
		st.DisputeBuckets[idx].A++
	}
	sh.mu.Unlock()
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
