package reputation

import (
	"context"
	"fmt"

	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/admission"
)

// RecordBrokerRequest is called once per broker_request submission, before
// admission gating, so admit/reject/queue all count toward the
// network_requests_per_h primary signal. decision is one of "admit",
// "reject", "queue" — recorded on the metric label only; the signal
// itself is a count.
func (s *Subsystem) RecordBrokerRequest(consumer ids.IdentityID, decision string) error {
	if s.closed.Load() {
		return ErrSubsystemClosed
	}
	ctx := context.Background()
	now := s.now()
	if err := s.store.ensureState(ctx, consumer, now); err != nil {
		return fmt.Errorf("reputation: RecordBrokerRequest: %w", err)
	}
	if err := s.store.appendEvent(ctx, consumer, RoleConsumer,
		SignalBrokerRequest, 1.0, now); err != nil {
		return fmt.Errorf("reputation: RecordBrokerRequest: %w", err)
	}
	_ = decision // metric label hookup lands in Task 17
	return nil
}

// RecordOfferOutcome is called by broker.runOffer per attempt. Outcome
// is one of "accept", "reject", "unreachable".
func (s *Subsystem) RecordOfferOutcome(seeder ids.IdentityID, outcome string) error {
	if s.closed.Load() {
		return ErrSubsystemClosed
	}
	var kind SignalKind
	switch outcome {
	case "accept":
		kind = SignalOfferAccept
	case "reject":
		kind = SignalOfferReject
	case "unreachable":
		kind = SignalOfferUnreachable
	default:
		return fmt.Errorf("reputation: RecordOfferOutcome: unknown outcome %q", outcome)
	}
	ctx := context.Background()
	now := s.now()
	if err := s.store.ensureState(ctx, seeder, now); err != nil {
		return err
	}
	return s.store.appendEvent(ctx, seeder, RoleSeeder, kind, 1.0, now)
}

// OnLedgerEvent implements admission.LedgerEventObserver. Silently
// no-ops on Subsystem-closed because the upstream call site does not
// expect an error return.
func (s *Subsystem) OnLedgerEvent(ev admission.LedgerEvent) {
	if s.closed.Load() {
		return
	}
	ctx := context.Background()
	now := s.now()
	switch ev.Kind {
	case admission.LedgerEventSettlement:
		// Ensure consumer rep_state row exists so longevity tracking
		// can work for consumers that only appear in settlements.
		// No signal is appended for the consumer side here — settlement
		// events only emit a seeder-side primary signal.
		_ = s.store.ensureState(ctx, ev.ConsumerID, now)
		sigMissing := ev.Flags&1 != 0
		var seederSig SignalKind
		if sigMissing {
			seederSig = SignalSettlementSigMissing
		} else {
			seederSig = SignalSettlementClean
		}
		if ev.SeederID != (ids.IdentityID{}) {
			_ = s.store.ensureState(ctx, ev.SeederID, now)
			_ = s.store.appendEvent(ctx, ev.SeederID, RoleSeeder,
				seederSig, 1.0, now)
		}
	case admission.LedgerEventDisputeFiled:
		_ = s.store.ensureState(ctx, ev.ConsumerID, now)
		_ = s.store.appendEvent(ctx, ev.ConsumerID, RoleConsumer,
			SignalDisputeFiled, 1.0, now)
	case admission.LedgerEventDisputeResolved:
		if !ev.DisputeUpheld {
			return
		}
		_ = s.store.ensureState(ctx, ev.ConsumerID, now)
		_ = s.store.appendEvent(ctx, ev.ConsumerID, RoleConsumer,
			SignalDisputeUpheld, 1.0, now)
	default:
		// Transfers, starter grants, etc. are not signals reputation cares
		// about in MVP. Silent ignore.
	}
}
