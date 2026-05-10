package reputation

import (
	"context"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
)

// medianMAD returns the median and the median absolute deviation of
// xs. xs is mutated (sorted) — callers pass a fresh slice or a copy.
// Returns (0, 0) when xs is empty.
func medianMAD(xs []float64) (float64, float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	sort.Float64s(xs)
	median := medianSortedCopy(xs)
	devs := make([]float64, len(xs))
	for i, x := range xs {
		devs[i] = math.Abs(x - median)
	}
	sort.Float64s(devs)
	mad := medianSortedCopy(devs)
	return median, mad
}

// medianSortedCopy returns the median of a sorted slice (does not sort).
func medianSortedCopy(xs []float64) float64 {
	n := len(xs)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return xs[n/2]
	}
	return 0.5 * (xs[n/2-1] + xs[n/2])
}

// zScore returns (sample - median) / mad. Returns 0 when mad is 0 (the
// population is degenerate and z is undefined; treating it as
// non-outlier is the right default for the evaluator).
func zScore(sample, median, mad float64) float64 {
	if mad == 0 {
		return 0
	}
	return (sample - median) / mad
}

// runOneCycle executes one full evaluator pass. Public to the package
// for deterministic tests; the goroutine in runEvaluator calls it on
// the configured tick.
func (s *Subsystem) runOneCycle(ctx context.Context) error {
	start := time.Now()
	defer func() { s.metrics.cycleSeconds.Observe(time.Since(start).Seconds()) }()

	now := s.now()
	// upper bound is half-open [from, to); add 1s so events with
	// observed_at == now are included in the current cycle.
	upper := now.Add(time.Second)
	longCutoff := now.Add(-time.Duration(s.cfg.SignalWindows.LongS) * time.Second)
	shortCutoff := now.Add(-time.Duration(s.cfg.SignalWindows.ShortS) * time.Second)
	medCutoff := now.Add(-time.Duration(s.cfg.SignalWindows.MediumS) * time.Second)

	primaryWindow := func(sig SignalKind) (time.Time, time.Time) {
		switch sig {
		case SignalBrokerRequest:
			return shortCutoff, upper // 1h
		case SignalProofRejection, SignalExhaustionClaim:
			return medCutoff, upper // 24h
		case SignalCostReportDeviation, SignalConsumerComplaint:
			return medCutoff, upper // 24h
		default:
			return medCutoff, upper
		}
	}

	type flag struct {
		id     ids.IdentityID
		signal SignalKind
		z      float64
		window string
	}
	var flagged []flag

	for _, sig := range PrimarySignals() {
		from, to := primaryWindow(sig)
		rows, err := s.store.aggregateBySignal(ctx, sig, from, to)
		if err != nil {
			return err
		}
		role := sig.Role()
		popSize, err := s.store.populationSize(ctx, role, from, to)
		if err != nil {
			return err
		}
		if popSize < s.cfg.MinPopulationForZScore {
			s.metrics.skips.WithLabelValues("undersized_population").Inc()
			continue
		}
		samples := make([]float64, len(rows))
		for i, r := range rows {
			samples[i] = r.Sum
		}
		median, mad := medianMAD(append([]float64{}, samples...))
		if mad == 0 {
			continue
		}
		for _, r := range rows {
			z := zScore(r.Sum, median, mad)
			if math.Abs(z) > s.cfg.ZScoreThreshold {
				flagged = append(flagged, flag{
					id:     r.IdentityID,
					signal: sig,
					z:      z,
					window: windowLabel(sig),
				})
			}
		}
	}

	states, err := s.store.loadAllStates(ctx)
	if err != nil {
		return err
	}
	flaggedSet := map[idsKey]bool{}
	for _, f := range flagged {
		flaggedSet[idsKey(f.id)] = true
		cur, ok := states[f.id]
		if !ok || cur.State == StateOK {
			reason := ReasonRecord{
				Kind: "zscore", Signal: signalLabel(f.signal),
				Z: f.z, Window: f.window, At: now.Unix(),
			}
			if err := s.store.transition(ctx, f.id, StateAudit, reason, now); err != nil {
				if !errors.Is(err, errInvalidTransition) {
					return err
				}
			} else {
				s.metrics.transitions.WithLabelValues("OK", "AUDIT", "zscore").Inc()
			}
		}
	}

	states, err = s.store.loadAllStates(ctx)
	if err != nil {
		return err
	}

	sevenDayAgo := now.Add(-7 * 24 * time.Hour)
	fortyEightHoursAgo := now.Add(-48 * time.Hour)

	for id, st := range states {
		switch st.State {
		case StateAudit:
			count := 0
			var lastAudit time.Time
			for _, r := range st.Reasons {
				if r.Kind != "zscore" && r.Kind != "breach" {
					continue
				}
				rt := time.Unix(r.At, 0)
				if rt.After(sevenDayAgo) {
					count++
					if rt.After(lastAudit) {
						lastAudit = rt
					}
				}
			}
			if count >= 3 {
				reason := ReasonRecord{
					Kind: "transition", Signal: "freeze_repeat",
					At: now.Unix(),
				}
				if err := s.store.transition(ctx, id, StateFrozen, reason, now); err == nil {
					s.metrics.transitions.WithLabelValues("AUDIT", "FROZEN", "freeze_repeat").Inc()
					s.notifyFreeze(ctx, id, "freeze_repeat", now)
				}
			} else if !lastAudit.IsZero() && lastAudit.Before(fortyEightHoursAgo) &&
				!flaggedSet[idsKey(id)] {
				reason := ReasonRecord{
					Kind: "transition", Signal: "audit_cleared",
					At: now.Unix(),
				}
				if err := s.store.transition(ctx, id, StateOK, reason, now); err == nil {
					s.metrics.transitions.WithLabelValues("AUDIT", "OK", "audit_cleared").Inc()
				}
			}
		case StateOK, StateFrozen:
			// No evaluator-driven transitions from these states beyond
			// OK→AUDIT (handled above).
		}
	}

	if err := s.recomputeScoresAndPublish(ctx, now, flaggedSet); err != nil {
		return err
	}

	if _, err := s.store.pruneEventsBefore(ctx, longCutoff); err != nil {
		return err
	}
	return nil
}

// signalLabel returns the on-disk label for a primary signal.
func signalLabel(sig SignalKind) string {
	switch sig {
	case SignalBrokerRequest:
		return "network_requests_per_h"
	case SignalProofRejection:
		return "proof_rejection_rate"
	case SignalExhaustionClaim:
		return "exhaustion_claim_rate"
	case SignalCostReportDeviation:
		return "cost_report_deviation"
	case SignalConsumerComplaint:
		return "consumer_complaint_rate"
	default:
		return "unknown"
	}
}

func windowLabel(sig SignalKind) string {
	switch sig {
	case SignalBrokerRequest:
		return "1h"
	default:
		return "24h"
	}
}

// recomputeScoresAndPublish builds a fresh scoreCache and atomically
// stores it. For MVP we don't yet derive completion_health,
// consumer_satisfaction, seeder_fairness from rep_events — that
// derivation is a bounded TODO from spec §7. The defaults (1.0) keep
// the score conservative until the derivation lands. The
// audit_recency_penalty, longevity, and clamp DO apply.
func (s *Subsystem) recomputeScoresAndPublish(ctx context.Context, now time.Time,
	flagged map[idsKey]bool,
) error {
	states, err := s.store.loadAllStates(ctx)
	if err != nil {
		return err
	}
	next := &scoreCache{states: make(map[idsKey]cachedEntry, len(states))}

	stateCounts := map[State]float64{}
	for id, st := range states {
		in := newDefaultInputs(st.FirstSeenAt, now)
		in.LastAuditAt = lastAuditTime(st.Reasons)
		sc := computeScore(in)

		if err := s.store.upsertScore(ctx, id, sc, now); err != nil {
			return err
		}
		s.metrics.score.Observe(sc)
		next.states[idsKey(id)] = cachedEntry{
			State:  st.State,
			Score:  sc,
			Since:  st.Since,
			Frozen: st.State == StateFrozen,
		}
		stateCounts[st.State]++
	}
	s.metrics.state.WithLabelValues("OK").Set(stateCounts[StateOK])
	s.metrics.state.WithLabelValues("AUDIT").Set(stateCounts[StateAudit])
	s.metrics.state.WithLabelValues("FROZEN").Set(stateCounts[StateFrozen])
	_ = flagged // reserved for future delta logging
	s.cache.Store(next)
	return nil
}

func lastAuditTime(reasons []ReasonRecord) time.Time {
	var latest time.Time
	for _, r := range reasons {
		if r.Kind != "zscore" && r.Kind != "breach" {
			continue
		}
		rt := time.Unix(r.At, 0)
		if rt.After(latest) {
			latest = rt
		}
	}
	return latest
}
