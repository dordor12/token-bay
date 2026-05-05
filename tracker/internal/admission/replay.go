package admission

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// LedgerSource is the cross-check interface admission consults during
// StartupReplay. Real ledger emission is a separate plan; tests inject a
// fake.
type LedgerSource interface {
	// EventsAfter returns ledger events with seq > minSeq. May return zero
	// entries; must never return an unbounded sequence (caller iterates
	// once).
	EventsAfter(ctx context.Context, minSeq uint64) ([]LedgerEvent, error)
}

// nullLedgerSource is the default — no cross-check.
type nullLedgerSource struct{}

func (nullLedgerSource) EventsAfter(context.Context, uint64) ([]LedgerEvent, error) {
	return nil, nil
}

// WithLedgerSource injects a LedgerSource (production wiring lands in the
// ledger event-bus plan).
func WithLedgerSource(src LedgerSource) Option {
	return func(s *Subsystem) { s.ledgerSrc = src }
}

// WithSkipAutoReplay leaves startup replay to the caller's explicit
// StartupReplay invocation. Production wiring will call StartupReplay
// from cmd before serving requests.
func WithSkipAutoReplay() Option {
	return func(s *Subsystem) { s.skipAutoReplay = true }
}

// DegradedMode reports whether admission is running with no recoverable
// snapshot. While true, decisions still flow but persistence-derived
// features (recompute, /admission/queue replay) are suppressed.
func (s *Subsystem) DegradedMode() bool {
	return s.degradedMode.Load() != 0
}

// SnapshotLoadFailures is read by the metrics Collector (Task 12).
func (s *Subsystem) SnapshotLoadFailures() uint64 { return s.snapshotLoadFailures.Load() }

// TLogCorruptions is read by the metrics Collector (Task 12).
func (s *Subsystem) TLogCorruptions() uint64 { return s.tlogCorruptions.Load() }

// StartupReplay executes the admission-design §5.7 replay decision tree.
// Idempotent — safe to call once at boot. Caller is expected to acquire
// the subsystem before any external callers issue Decide.
func (s *Subsystem) StartupReplay(ctx context.Context) error {
	s.replaying.Store(true)
	defer s.replaying.Store(false)

	snap, err := s.loadNewestUsableSnapshot()
	switch {
	case err == nil && snap != nil:
		s.applySnapshot(snap)
	case errors.Is(err, errNoUsableSnapshot):
		s.degradedMode.Store(1)
	case err != nil:
		return err
	}

	startSeq := uint64(0)
	if snap != nil {
		startSeq = snap.Seq
	}
	if err := s.replayTLog(startSeq); err != nil {
		return err
	}

	src := s.ledgerSrc
	if src == nil {
		src = nullLedgerSource{}
	}
	var lastSeq uint64
	if s.tlog != nil {
		lastSeq = s.tlog.LastSeq()
	}
	missing, err := src.EventsAfter(ctx, lastSeq)
	if err != nil {
		return fmt.Errorf("admission/replay: ledger cross-check: %w", err)
	}
	for _, ev := range missing {
		s.OnLedgerEvent(ev)
	}
	return nil
}

var errNoUsableSnapshot = errors.New("admission: no usable snapshot")

// loadNewestUsableSnapshot returns:
//   - (nil, nil) when no persistence is configured or no snapshots exist
//     (clean first-boot — not degraded).
//   - (snap, nil) when at least one snapshot loads successfully.
//   - (nil, errNoUsableSnapshot) when snapshots exist but every one failed
//     to load (degraded mode).
func (s *Subsystem) loadNewestUsableSnapshot() (*Snapshot, error) {
	if s.snapshotPrefix == "" {
		return nil, nil
	}
	files, err := enumerateSnapshots(s.snapshotPrefix)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, nil
	}
	for i := len(files) - 1; i >= 0; i-- {
		snap, err := readSnapshot(files[i].path)
		if err == nil {
			return snap, nil
		}
		s.snapshotLoadFailures.Add(1)
	}
	return nil, errNoUsableSnapshot
}

// applySnapshot installs every consumer + seeder state into the live
// shard maps.
func (s *Subsystem) applySnapshot(snap *Snapshot) {
	now := time.Now()
	for id, st := range snap.Consumers {
		live := consumerShardFor(s.consumerShards, id).getOrInit(id, now)
		csh := consumerShardFor(s.consumerShards, id)
		csh.mu.Lock()
		*live = *st
		csh.mu.Unlock()
	}
	for id, st := range snap.Seeders {
		live := seederShardFor(s.seederShards, id).getOrInit(id, now)
		ssh := seederShardFor(s.seederShards, id)
		ssh.mu.Lock()
		*live = *st
		ssh.mu.Unlock()
	}
}

// replayTLog reads every record with seq > startSeq and re-applies it.
// Trailing-frame truncation is healed by the reader; mid-file CRC
// corruption surfaces ErrTLogCorrupt.
func (s *Subsystem) replayTLog(startSeq uint64) error {
	if s.tlogPath == "" {
		return nil
	}
	files, err := enumerateTLogFiles(s.tlogPath)
	if err != nil {
		return err
	}
	for _, f := range files {
		recs, _, err := readTLogFile(f)
		if err != nil {
			if errors.Is(err, ErrTLogCorrupt) {
				s.tlogCorruptions.Add(1)
			}
			return err
		}
		for _, r := range recs {
			if r.Seq <= startSeq {
				continue
			}
			s.applyTLogRecord(r)
		}
	}
	return nil
}

// applyTLogRecord routes a replayed record to the same in-memory mutator
// OnLedgerEvent uses. The s.replaying flag (set by StartupReplay)
// suppresses tlog write-through.
func (s *Subsystem) applyTLogRecord(r TLogRecord) {
	ts := time.Unix(int64(r.Ts), 0).UTC() //nolint:gosec // G115 — post-1970
	switch r.Kind {
	case TLogKindSettlement:
		var p SettlementPayload
		if err := p.UnmarshalBinary(r.Payload); err == nil {
			s.OnLedgerEvent(LedgerEvent{
				Kind: LedgerEventSettlement, ConsumerID: p.ConsumerID,
				SeederID: p.SeederID, CostCredits: p.CostCredits,
				Flags: p.Flags, Timestamp: ts,
			})
		}
	case TLogKindDisputeFiled:
		var p DisputePayload
		if err := p.UnmarshalBinary(r.Payload); err == nil {
			kind := LedgerEventDisputeFiled
			if p.Upheld {
				kind = LedgerEventDisputeResolved
			}
			s.OnLedgerEvent(LedgerEvent{
				Kind: kind, ConsumerID: p.ConsumerID,
				DisputeUpheld: p.Upheld, Timestamp: ts,
			})
		}
	case TLogKindDisputeResolved:
		var p DisputePayload
		if err := p.UnmarshalBinary(r.Payload); err == nil {
			s.OnLedgerEvent(LedgerEvent{
				Kind: LedgerEventDisputeResolved, ConsumerID: p.ConsumerID,
				DisputeUpheld: p.Upheld, Timestamp: ts,
			})
		}
	case TLogKindTransfer:
		var p TransferPayload
		if err := p.UnmarshalBinary(r.Payload); err == nil {
			kind := LedgerEventTransferIn
			if p.Direction == TransferOut {
				kind = LedgerEventTransferOut
			}
			s.OnLedgerEvent(LedgerEvent{
				Kind: kind, ConsumerID: p.ConsumerID,
				CostCredits: p.CostCredits, Timestamp: ts,
			})
		}
	case TLogKindStarterGrant:
		var p StarterGrantPayload
		if err := p.UnmarshalBinary(r.Payload); err == nil {
			s.OnLedgerEvent(LedgerEvent{
				Kind: LedgerEventStarterGrant, ConsumerID: p.ConsumerID,
				CostCredits: p.CostCredits, Timestamp: ts,
			})
		}
	case TLogKindSnapshotMark, TLogKindOperatorOverride, TLogKindHeartbeatBucketRoll:
		// Audit-only — no in-memory mutation.
	}
}
