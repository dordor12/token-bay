// Package reputation is the L3 abuse-detection backstop for the tracker.
// It ingests signals from the broker (offer outcomes, broker_request
// submissions), settlement events (via admission.LedgerEventObserver),
// and categorical-breach reports; runs a per-minute evaluator that
// computes z-scores against role-segmented populations; transitions
// identities through OK / AUDIT / FROZEN; and publishes a Score and
// IsFrozen view that the broker consults when ranking and filtering
// seeders.
//
// Concurrency:
//   - Hot-path reads (Score / IsFrozen / Status) load an atomic.Pointer
//     score-cache map and do one map lookup. No DB, no lock.
//   - Ingest writes are non-blocking: one INSERT into rep_events plus an
//     INSERT OR IGNORE on rep_state.
//   - Categorical-breach transitions take one breach mutex and run
//     synchronously to keep state and cache consistent on a single
//     identity.
//   - The evaluator goroutine runs at cfg.Reputation.EvaluationIntervalS
//     (default 60s); it computes population stats, transitions, score
//     refresh, and an atomic cache swap.
//
// Spec: docs/superpowers/specs/reputation/2026-05-09-tracker-internal-reputation-design.md
package reputation
