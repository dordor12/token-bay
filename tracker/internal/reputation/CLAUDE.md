# tracker/internal/reputation — Development Context

## What this is

L3 abuse-detection backstop for the tracker. Ingests signals from broker
and ledger events, computes z-scores against role-segmented populations,
runs the OK/AUDIT/FROZEN state machine, and publishes Score / IsFrozen
for the broker.

Authoritative spec:
`docs/superpowers/specs/reputation/2026-05-09-tracker-internal-reputation-design.md`.

Parent (subsystem-level) spec:
`docs/superpowers/specs/reputation/2026-04-22-reputation-design.md`.

## Non-negotiable rules

1. Broker hot path (`Score`, `IsFrozen`) cannot be taken down by reputation.
   Storage failures fall through to `cfg.Reputation.DefaultScore` (0.5)
   and `false`.
2. Audit history (`rep_state.reasons`) is append-only. Never edit, never
   truncate.
3. Reputation is a leaf module relative to the rest of the tracker.
   Imports allowed: `shared/ids`, `tracker/internal/admission` (for
   `LedgerEvent` and `LedgerEventObserver`), `tracker/internal/config`,
   stdlib, `modernc.org/sqlite`, `zerolog`, `prometheus/client_golang`,
   `testify`. Nothing in `internal/broker`, `internal/ledger`,
   `internal/registry`, `internal/federation` is imported.
4. Race-clean tests are mandatory. This package is on the always-`-race`
   list per tracker CLAUDE.md.

## Things that look surprising and aren't bugs

- `rep_state.first_seen_at` is set once on first ingest and never
  updated. The longevity bonus reads from it (not from `rep_events`,
  which gets pruned past 7 days).
- The evaluator skips §6.1 z-score detection when the population for
  a signal's role is below `cfg.MinPopulationForZScore` (default 100).
  Categorical breaches (§6.2) still fire below the threshold.
- Score recomputation only touches identities that had an event this
  cycle OR have a non-zero audit_recency_penalty. Silent untouched
  identities keep their last cached score because no formula term
  changed for them.
- `Score` returns `(cfg.DefaultScore, false)` on cache miss, not an
  error. The broker hot path treats `ok=false` as "no opinion".
- The ledger→reputation hook is not a generic event bus. Settlement
  in broker calls `reputation.OnLedgerEvent` directly, the same way
  it will call `admission.OnLedgerEvent` once that wiring lands.
