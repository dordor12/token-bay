package reputation

import (
	"context"
	"errors"
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
	s.metrics.eventsIngested.WithLabelValues(signalLabel(SignalBrokerRequest)).Inc()
	_ = decision // decision label reserved for future per-outcome breakdown
	return nil
}

// RecordProofFidelity records the proof_fidelity_level signal (§3.1)
// classified by the api/broker_request handler. level is one of
// "full_two_signal", "partial", "degraded"; the rep_event value field
// encodes that bucket as 1.0 / 0.5 / 0.0 so the evaluator can compute
// a population mean. Unknown levels are stored as 0.0 and tagged
// "degraded" for the metric.
func (s *Subsystem) RecordProofFidelity(consumer ids.IdentityID, level string) error {
	if s.closed.Load() {
		return ErrSubsystemClosed
	}
	var value float64
	label := level
	switch level {
	case "full_two_signal":
		value = 1.0
	case "partial":
		value = 0.5
	case "degraded":
		value = 0.0
	default:
		value = 0.0
		label = "degraded"
	}
	ctx := context.Background()
	now := s.now()
	if err := s.store.ensureState(ctx, consumer, now); err != nil {
		return fmt.Errorf("reputation: RecordProofFidelity: %w", err)
	}
	if err := s.store.appendEvent(ctx, consumer, RoleConsumer,
		SignalProofFidelityLevel, value, now); err != nil {
		return fmt.Errorf("reputation: RecordProofFidelity: %w", err)
	}
	s.metrics.eventsIngested.WithLabelValues("proof_fidelity_" + label).Inc()
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
	if err := s.store.appendEvent(ctx, seeder, RoleSeeder, kind, 1.0, now); err != nil {
		return err
	}
	var label string
	switch kind {
	case SignalOfferAccept:
		label = "offer_accept"
	case SignalOfferReject:
		label = "offer_reject"
	case SignalOfferUnreachable:
		label = "offer_unreachable"
	}
	if label != "" {
		s.metrics.eventsIngested.WithLabelValues(label).Inc()
	}
	return nil
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
			if sigMissing {
				s.metrics.eventsIngested.WithLabelValues("settlement_sig_missing").Inc()
			} else {
				s.metrics.eventsIngested.WithLabelValues("settlement_clean").Inc()
			}
		}
	case admission.LedgerEventDisputeFiled:
		_ = s.store.ensureState(ctx, ev.ConsumerID, now)
		_ = s.store.appendEvent(ctx, ev.ConsumerID, RoleConsumer,
			SignalDisputeFiled, 1.0, now)
		s.metrics.eventsIngested.WithLabelValues("dispute_filed").Inc()
	case admission.LedgerEventDisputeResolved:
		if !ev.DisputeUpheld {
			return
		}
		_ = s.store.ensureState(ctx, ev.ConsumerID, now)
		_ = s.store.appendEvent(ctx, ev.ConsumerID, RoleConsumer,
			SignalDisputeUpheld, 1.0, now)
		s.metrics.eventsIngested.WithLabelValues("dispute_upheld").Inc()
	default:
		// Transfers, starter grants, etc. are not signals reputation cares
		// about in MVP. Silent ignore.
	}
}

// RecordCategoricalBreach handles spec §6.2 events: it appends a
// rep_events row and synchronously transitions the identity to the
// breach's immediate-action state, then refreshes that identity's
// cache entry.
func (s *Subsystem) RecordCategoricalBreach(id ids.IdentityID, kind BreachKind) error {
	if s.closed.Load() {
		return ErrSubsystemClosed
	}
	if kind == BreachUnspecified {
		return fmt.Errorf("reputation: RecordCategoricalBreach: BreachUnspecified")
	}

	s.breachMu.Lock()
	defer s.breachMu.Unlock()

	ctx := context.Background()
	now := s.now()

	if err := s.store.ensureState(ctx, id, now); err != nil {
		return err
	}
	// SignalUnspecified — categorical breaches don't slot into a primary
	// signal but still want a rep_events row for the audit trail. The
	// evaluator filters them out by event_type.
	if err := s.store.appendEvent(ctx, id, kind.signalRole(),
		SignalUnspecified, 1.0, now); err != nil {
		return err
	}

	target := kind.ImmediateAction()
	reason := ReasonRecord{
		Kind:       "breach",
		BreachKind: kind.String(),
		At:         now.Unix(),
	}

	cur, ok, err := s.store.readState(ctx, id)
	if err != nil {
		return err
	}
	switch {
	case !ok:
		// ensureState was just called; readState should always find it.
		// Be defensive and treat as fresh-OK.
		if err := s.store.transition(ctx, id, target, reason, now); err != nil {
			return err
		}
		s.metrics.breaches.WithLabelValues(kind.String()).Inc()
		s.metrics.transitions.WithLabelValues(StateOK.String(), target.String(), "breach").Inc()
	case cur.State == StateFrozen:
		// Terminal state; ignore further breaches.
		return nil
	case cur.State == target:
		// Same target state — append reason for §6.2 freeze counting
		// without re-running the (no-op) state transition.
		if err := s.store.appendReason(ctx, id, reason, now); err != nil {
			return err
		}
		s.metrics.breaches.WithLabelValues(kind.String()).Inc()
	default:
		if err := s.store.transition(ctx, id, target, reason, now); err != nil {
			if errors.Is(err, errInvalidTransition) {
				return nil
			}
			return err
		}
		s.metrics.breaches.WithLabelValues(kind.String()).Inc()
		s.metrics.transitions.WithLabelValues(cur.State.String(), target.String(), "breach").Inc()
	}
	return s.refreshOne(ctx, id)
}

// refreshOne updates a single identity's cache entry. Caller must hold
// breachMu.
func (s *Subsystem) refreshOne(ctx context.Context, id ids.IdentityID) error {
	row, ok, err := s.store.readState(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	scores, _ := s.store.loadAllScores(ctx)
	cur := s.cache.Load()
	var prevSize int
	if cur != nil {
		prevSize = len(cur.states)
	}
	next := &scoreCache{states: make(map[idsKey]cachedEntry, prevSize+1)}
	if cur != nil {
		for k, v := range cur.states {
			next.states[k] = v
		}
	}
	sc, hasScore := scores[id]
	if !hasScore {
		// Use whatever was previously cached, or DefaultScore.
		if cur != nil {
			if prev, hit := cur.states[idsKey(id)]; hit {
				sc = prev.Score
			} else {
				sc = s.cfg.DefaultScore
			}
		} else {
			sc = s.cfg.DefaultScore
		}
	}
	next.states[idsKey(id)] = cachedEntry{
		State:  row.State,
		Score:  sc,
		Since:  row.Since,
		Frozen: row.State == StateFrozen,
	}
	s.cache.Store(next)
	return nil
}
