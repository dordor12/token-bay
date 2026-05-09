# `tracker/internal/reputation` — MVP Design

| Field | Value |
|---|---|
| Parent | [Reputation Subsystem](2026-04-22-reputation-design.md) (subsystem-level spec) |
| Implements | spec §1, §2.1, §3, §4, §5, §6, §8, §11, §12 |
| Defers | spec §2.2 (federation), §7 (dispute), §9 (operator CLI) — separate plans |
| Status | Design draft |
| Date | 2026-05-09 |
| Component | `tracker/internal/reputation` |

## 1. Purpose and scope

This is the first concrete plan against the parent reputation spec. It builds the in-process subsystem that other tracker packages already expect — `broker.ReputationService` is wired but uses `fallbackReputation` today (`tracker/internal/broker/deps.go:85-90`) — and lands the detection pipeline that converts ingested signals into per-identity state and score.

In scope:

- `internal/reputation` package, opened once per tracker process.
- Signal ingest from broker offer outcomes, broker_request submissions, and the existing `LedgerEventObserver` stream (the same one admission consumes).
- Categorical-breach (§6.2) synchronous transitions.
- Periodic evaluator (default 60s) running z-score outlier detection (§6.1) and score recomputation (§5).
- SQLite-backed persistence (§8) at `<data_dir>/reputation.sqlite`.
- `Score` and `IsFrozen` exposed for broker consumption; `Status` exposed for in-process use by the future admin API.

Out of scope (deferred to follow-up plans, all referenced in the parent spec):

- Federation revocation outbound + inbound (spec §2.2). Reputation has no federation dependency in MVP.
- Dispute admin endpoints (spec §7). MVP captures auditable transition history in `rep_state.reasons` so the admin path can be built later without redesign.
- Operator CLI subcommands (`tracker admin rep show|freeze|unfreeze|thresholds`, spec §9).
- L2-proof attestation hooks (`proof_fidelity_level`, spec §3.1) — depends on attestation infra not yet present.
- Cross-region reputation portability (open question 10.2).

## 2. Boundaries and surface

The subsystem is one process-wide value with three doors.

### 2.1 Lifecycle

```go
func Open(cfg config.ReputationConfig, dbPath string, opts ...Option) (*Subsystem, error)
func (s *Subsystem) Close() error
```

`Open` opens its own SQLite DB (separate from the ledger DB), runs `CREATE TABLE IF NOT EXISTS` migration, starts one evaluator goroutine. Returns an error on bad config, DB-open failure, or migration failure.

`Close` signals the evaluator to stop, waits, closes the DB. Idempotent.

### 2.2 Inbound — signal sources

| Method | Caller (today / planned) | Effect |
|---|---|---|
| `OnLedgerEvent(ev admission.LedgerEvent)` | implements existing `admission.LedgerEventObserver` | Writes one or more `rep_events` rows for settlement, transfer, dispute. Settlements write a consumer-side row and a seeder-side row. |
| `RecordOfferOutcome(seederID, outcome string)` | broker.runOffer (already wired at `broker/broker.go:191,198`); we add the `"accept"` call site as part of this plan | Writes a seeder-side `rep_events` row with `event_type = OFFER_OUTCOME`, `value` carrying outcome enum. |
| `RecordBrokerRequest(consumerID, decision string)` | new; one call from the api/ broker_request handler, before admission gating, so admit/reject/queue all count | Writes a consumer-side `rep_events` row with `event_type = BROKER_REQUEST`, drives the `network_requests_per_h` primary signal. Placement before admission ensures the signal counts every submission attempt, not only post-gate broker work. |
| `RecordCategoricalBreach(id, kind BreachKind)` | broker (proof-reject counters), settlement (bad consumer-sig), admission (replay nonce) | Writes the `rep_events` row **and** synchronously runs the §6.2 transition (may move state to AUDIT or FROZEN immediately). |

`OnLedgerEvent` is the only ingest method that consumes a struct from another package; the others use primitives so reputation stays a leaf module with respect to its callers.

### 2.3 Outbound — consumed by other subsystems

| Method | Consumer | Behavior |
|---|---|---|
| `Score(id ids.IdentityID) (float64, ok bool)` | `broker.ReputationService.Score` | Reads in-process score cache; returns `(cfg.DefaultScore, false)` on miss. Never blocks on DB. |
| `IsFrozen(id ids.IdentityID) bool` | `broker.ReputationService.IsFrozen` | Reads in-process state cache; returns `false` on miss. |
| `Status(id ids.IdentityID) ReputationStatus` | future admin API; available now | Reads cache and returns `{State, Reasons, Since}`. |

### 2.4 What this package does *not* depend on

- No federation, no dispute, no admin API. Those subsystems will depend on reputation, not the other way round.
- Does not depend on the ledger package directly. The settlement→reputation hop goes through `LedgerEventObserver` so reputation does not need a ledger import.
- Does not own broker selection logic. It only publishes the score that broker reads.

## 3. Package layout

```
tracker/internal/reputation/
├── doc.go               package doc + concurrency model
├── reputation.go        Subsystem, Open, Close, Option, WithClock
├── score.go             Score / IsFrozen / Status — broker-facing cache reads
├── ingest.go            OnLedgerEvent, RecordOfferOutcome, RecordBrokerRequest, RecordCategoricalBreach
├── evaluator.go         periodic goroutine: population stats, z-scores, transitions, score refresh
├── state.go             State enum, ReputationStatus, transition table
├── signals.go           SignalKind enum, role tagging, primary/secondary classification
├── scoring.go           score formula §5
├── breach.go            BreachKind enum, immediate-action mapping for §6.2
├── storage.go           SQLite open + migration + helpers
├── storage_events.go    rep_events writer + windowed-aggregate reader
├── storage_state.go     rep_state CRUD + reasons-array append
├── storage_scores.go    rep_scores writer + bulk loader for cache
├── metrics.go           Prometheus metrics
├── errors.go            sentinel errors
├── testdata/
│   └── schema.sql       canonical schema, round-tripped against migration
└── *_test.go            one per source file
```

The package is a leaf relative to the rest of the tracker: it imports `shared/ids`, `tracker/internal/config`, `tracker/internal/admission` (only for the `LedgerEvent` / `LedgerEventObserver` types it implements), and `modernc.org/sqlite`. Nothing in `internal/broker`, `internal/ledger`, `internal/registry`, `internal/federation` is imported.

The `admission.LedgerEvent` import is a real coupling. If admission changes the event shape, reputation has to follow. This is acceptable because both subsystems are downstream of the same canonical event stream and decoupling them would require a third "events" package that doesn't earn its keep yet. If a third subsystem ever wants the same stream, that's the time to extract.

## 4. Concurrency model

Two synchronization primitives plus SQLite.

**Read cache (hot path):** `atomic.Pointer[scoreCache]`. The evaluator builds a fresh `scoreCache` struct per cycle (a map `IdentityID → {state, score, since}`) and atomic-swaps the pointer. `Score`, `IsFrozen`, `Status` do `Load()` and a map lookup with no lock. Mirrors admission's supply-snapshot pattern.

**Categorical-breach mutex:** one `sync.Mutex` guards the synchronous transition path in `RecordCategoricalBreach`. Categorical breaches are rare (at most a few per minute even under attack), so contention is irrelevant. The mutex ensures the read-modify-write of `rep_state` against a single identity is serialized and the cache pointer is refreshed atomically afterwards.

**SQLite single writer:** `modernc.org/sqlite` is single-writer; concurrent goroutines serialize on the DB connection. Acceptable: ingest writes are bounded (~1M/day per spec §8 = 11.6/sec mean) and the evaluator runs at 1/min.

**No sharded state.** Admission shards because every Decide is on the hot path; reputation reads from cache and writes outside the hot path, so a single DB connection is enough.

## 5. Storage schema

`<data_dir>/reputation.sqlite` opened with `_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)`.

```sql
CREATE TABLE IF NOT EXISTS rep_state (
  identity_id    BLOB    NOT NULL PRIMARY KEY CHECK(length(identity_id) = 32),
  state          INTEGER NOT NULL,                   -- 0=OK, 1=AUDIT, 2=FROZEN
  since          INTEGER NOT NULL,                   -- unix seconds; last state-change time
  first_seen_at  INTEGER NOT NULL,                   -- unix seconds; first ingest for this id
  reasons        TEXT    NOT NULL DEFAULT '[]',      -- JSON array of transition records
  updated_at     INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS rep_events (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  identity_id  BLOB    NOT NULL CHECK(length(identity_id) = 32),
  role         INTEGER NOT NULL,                   -- 0=consumer, 1=seeder, 2=role-agnostic
  event_type   INTEGER NOT NULL,                   -- enum (signals.go)
  value        REAL    NOT NULL,
  observed_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_rep_events_id_time   ON rep_events(identity_id, observed_at);
CREATE INDEX IF NOT EXISTS idx_rep_events_type_time ON rep_events(event_type, observed_at);

CREATE TABLE IF NOT EXISTS rep_scores (
  identity_id  BLOB    NOT NULL PRIMARY KEY CHECK(length(identity_id) = 32),
  score        REAL    NOT NULL,
  updated_at   INTEGER NOT NULL
);
```

Two deltas vs spec §8:

- Added `role` column on `rep_events` so the evaluator can pull "consumer-side" or "seeder-side" populations without re-deriving from `event_type`. Pure denormalization.
- Added `idx_rep_events_type_time` so per-signal population queries do not scan the full table.

**`reasons` JSON shape.** Append-only; every transition appends one element.

```json
{"kind":"zscore","signal":"exhaustion_claim_rate","z":-5.2,"window":"24h","at":1714000123}
{"kind":"breach","breach_kind":"bad_consumer_sig","at":1714000200}
{"kind":"manual","operator":"<id>","at":1714000300}
```

**Retention.** At the end of each evaluator cycle, `DELETE FROM rep_events WHERE observed_at < ?` for the long-window cutoff (`now - cfg.SignalWindows.LongS`). Bounded table size. No retention on `rep_state` (auditable history) or `rep_scores` (one row per identity).

**Migration.** Plain `CREATE TABLE IF NOT EXISTS`. No version table yet — admission and ledger don't have one either. When v2 schema lands, we add a `schema_version` row.

## 6. Data flow

### 6.1 Ingest (non-blocking, no detection)

```
broker.Submit                  ──► RecordBrokerRequest         ─┐
broker.runOffer                ──► RecordOfferOutcome          ─┤
ledger.Append (via admission   ──► OnLedgerEvent               ─┼──► INSERT rep_events
                event observer)                                 │
admission/settlement           ──► RecordCategoricalBreach     ─┘  (also runs sync transition)
```

Per-call cost: one `INSERT` into `rep_events`, plus an idempotent `INSERT OR IGNORE INTO rep_state` (eager creation on first sighting; subsequent ingests no-op the second insert), plus (categorical-breach only) one `UPDATE rep_state` and a cache pointer swap. Hot path is the broker's `Submit` and offer loop; both call into reputation off the latency-critical path (Submit is post-admission, runOffer is per-attempt and not in the request critical path).

### 6.2 Evaluation cycle (every `EvaluationIntervalS`, default 60s)

```
1. SELECT identity counts per role over the trailing long-window. Compute population_size{consumer,seeder}.

2. For each primary signal (consumer:  network_requests_per_h, proof_rejection_rate, exhaustion_claim_rate;
                            seeder:    cost_report_deviation, consumer_complaint_rate):
   a. SELECT signal samples per identity over the appropriate window.
   b. If population_size for the signal's role >= cfg.MinPopulationForZScore:
        compute population median + MAD.
        for each identity: z = (sample - median) / MAD;
                           if |z| > cfg.ZScoreThreshold: flag for AUDIT, capturing the reason record.
      Else: skip (metric reputation_evaluator_skips_total{reason="undersized_population"}).

3. State transitions (per identity, in one SQL transaction):
   - OK → AUDIT on a flagged primary signal.
   - AUDIT → FROZEN on >= 3 AUDIT entries within trailing 7d (counted via reasons array).
   - AUDIT → OK on no audit-trigger events in trailing 48h.
   FROZEN is terminal in the evaluator: only manual operator action (deferred) clears it.
   Severe categorical breaches that go straight to FROZEN are handled synchronously
   in RecordCategoricalBreach, not here.

4. Score recomputation per §5 for every identity touched this cycle.

5. Build a fresh scoreCache map; atomic-swap the read-cache pointer.

6. UPSERT rep_scores rows for identities whose score changed.

7. DELETE rep_events older than the long window.
```

Categorical breaches (`RecordCategoricalBreach`) take the breach mutex, run the same transition logic for one identity synchronously, refresh that identity's cache entry, and return. They do not wait for the next tick.

### 6.3 Read path

`Score` / `IsFrozen` / `Status` do one atomic load and one map lookup. No DB access. On cache miss they return `(cfg.DefaultScore, false)`, `false`, `{State: OK, Since: 0, Reasons: nil}` respectively.

## 7. Score formula (spec §5)

Implemented verbatim:

```
score = clamp(
   0.3 * completion_health
 + 0.3 * consumer_satisfaction
 + 0.2 * seeder_fairness
 + 0.2 * longevity_bonus
 - 0.4 * audit_recency_penalty
, 0, 1)
```

- `completion_health` — for an identity acting as seeder: `completion_rate * offer_acceptance_rate` over the medium window. For an identity acting as consumer: `1 - proof_rejection_rate` (defaults to 1.0 with no data). For a dual-role identity: average of the two terms it has data for.
- `consumer_satisfaction` — `1 - dispute_rate_against_this_identity` over the long window. Defaults to 1.0 with no data.
- `seeder_fairness` — `1 - clamp(|cost_report_deviation_z|/4, 0, 1)`. Defaults to 1.0 with no data.
- `longevity_bonus` — `min(1.0, log(days_since_first_seen + 1) / log(365))`. `days_since_first_seen` is computed from `rep_state.first_seen_at`, which is set on the first ingest for the identity and never updated. (Reading from `rep_events` would not work because the long-window retention prunes rows older than 7d.)
- `audit_recency_penalty` — `exp(-Δt / 7d)` where Δt is time since the most recent `kind: zscore` or `kind: breach` reasons entry; 0 if none.

Weights are read from constants in `scoring.go`, not config in MVP. (Spec open question 10.3 calls them "rough initial values" needing calibration; making them config-tunable is a follow-up if/when calibration data exists.)

**Recompute set.** The evaluator recomputes scores for any identity that either (a) has at least one `rep_events` row in this cycle's windows or (b) has a non-zero `audit_recency_penalty` (decays over 7d, so an audited identity needs continued recomputation even when silent). Identities outside the set keep their last cached score. This is correct because the only term that changes purely with time is `audit_recency_penalty`, captured by case (b); all other terms are derived from event windows and don't move when no events arrive.

## 8. Configuration

Existing `config.ReputationConfig` (`tracker/internal/config/config.go:89-95`) already provides:

| Field | Default | Used as |
|---|---|---|
| `EvaluationIntervalS` | 60 | evaluator tick |
| `SignalWindows.ShortS` | 3600 | 1h primary signals |
| `SignalWindows.MediumS` | 86400 | 24h primary signals + completion_health window |
| `SignalWindows.LongS` | 604800 | 7d audit-count window + retention cutoff |
| `ZScoreThreshold` | 2.5 | §6.1 trigger |
| `DefaultScore` | 0.5 | cache-miss fallback |
| `FreezeListCacheTTLS` | 600 | reserved for federation; unused in MVP |

Two new fields to add:

| Field | Default | Used as |
|---|---|---|
| `MinPopulationForZScore` | 100 | §6.1 bootstrap gate (open question 10.1) |
| `StoragePath` | `${data_dir}/reputation.sqlite` | DB file location, `${data_dir}` expansion in ApplyDefaults |

These follow the section pattern documented in `tracker/internal/config/CLAUDE.md` ("Adding a new section" / "Adding a required field"): defaults in `DefaultConfig`, validation in `validate.go`, a path-expansion stanza in `ApplyDefaults` mirroring the existing `admission.tlog_path` derivation.

The spec discrepancy in `ZScoreThreshold` (spec §6.1 says |z|>4, config default is 2.5) is resolved in favor of the config: this design and a one-line spec-§6.1 amendment will go in the same PR.

## 9. Error handling

Sentinel errors (`errors.go`):

- `ErrSubsystemClosed` — methods called after `Close`.
- (Other internal errors are logged and metered, never returned through the public API.)

Failure-mode matrix:

| Failure | Behavior |
|---|---|
| SQLite read fails on hot path | Return `(cfg.DefaultScore, false)` / `false`. Increment `reputation_storage_errors_total{op="read"}`. |
| SQLite write fails on ingest | Log + metric; drop the event. The next event implicitly recovers. No retry queue in MVP. |
| SQLite write fails inside categorical-breach transition | Log + metric; return without transitioning. Evaluator re-attempts next cycle if conditions still hold. |
| Evaluator goroutine panics | `recover()` in the loop; log + `reputation_evaluator_panics_total`; resume next tick. |
| Population below `MinPopulationForZScore` | Skip §6.1 silently; categorical breaches still fire. |
| First-time identity ID | Eager `rep_state` row on first ingest with `state=OK`, `first_seen_at=now`, `since=now`, empty reasons. Score cache populated on the next evaluator cycle; reads before that return defaults. |

Two principles:

1. The broker hot path (`Score`, `IsFrozen`) cannot be taken down by reputation. Defaults always available.
2. Audit history (`rep_state.reasons`) is never rewritten or truncated. Only appended.

## 10. Observability

Prometheus metrics (registered through tracker's existing collector):

- `reputation_state` (gauge, `state="OK|AUDIT|FROZEN"`) — count of identities in each state.
- `reputation_score` (histogram) — score distribution at last cycle.
- `reputation_transitions_total` (counter, `from`, `to`, `reason`) — every state change.
- `reputation_breach_total` (counter, `kind`) — categorical-breach events.
- `reputation_evaluator_cycle_seconds` (histogram) — wall-time of one evaluator cycle.
- `reputation_evaluator_skips_total` (counter, `reason`).
- `reputation_storage_errors_total` (counter, `op="read|write|migrate"`).
- `reputation_events_ingested_total` (counter, `event_type`).
- `reputation_evaluator_panics_total` (counter).

Logging (zerolog): state transitions at `info` with the full reason record; storage errors at `warn`; evaluator panics at `error`. No DEBUG output on the hot path.

## 11. Acceptance criteria

Maps to the parent spec's §12. Each criterion has at least one named test in §12 below.

| # | Criterion (spec §12) | MVP coverage |
|---|---|---|
| 1 | Sustained outlier triggers AUDIT within ≤ 1 evaluation cycle | Yes — periodic evaluator at 60s. |
| 2 | AUDIT → FROZEN on three AUDIT events in 7d | Yes — counted from `rep_state.reasons`. |
| 3 | Frozen identity's `broker_request` returns `IDENTITY_FROZEN` | Reputation half: `IsFrozen` returns true. Broker rejection wiring is its own (small) PR after this lands. |
| 4 | Score changes propagate within 1 minute | Yes — evaluator cadence + atomic-pointer cache swap. |
| 5 | Dispute workflow auditable | Reputation half: `rep_state.reasons` is append-only and human-readable. The dispute admin endpoint is deferred. |

## 12. Testing

Repo-wide TDD discipline. Race-clean is mandatory (`-race` per tracker CLAUDE.md §6).

Unit tests, one per source file:

- `storage_test.go` — migration idempotence; CRUD round-trips; window-aggregate query correctness.
- `signals_test.go` — role classifier; primary/secondary table.
- `scoring_test.go` — score formula vs fixture inputs; clamp 0..1; longevity log curve; audit penalty exponential decay.
- `state_test.go` — full transition table, time-driven via `WithClock`.
- `breach_test.go` — every `BreachKind` triggers the right synchronous transition and reasons append.
- `evaluator_test.go` — population threshold gating; median+MAD math against synthetic distribution; one full cycle end-to-end; retention deletion of stale rows.
- `ingest_test.go` — every public ingest method writes the right `rep_events` row with the right role tag.
- `score_test.go` — `Score` / `IsFrozen` / `Status` cache hit, cache miss, post-`Close`.

Integration test (`integration_test.go`):

End-to-end with a real SQLite DB at `t.TempDir()`. Drives ingest from synthetic broker + ledger events for ≥100 synthetic identities (to clear `MinPopulationForZScore`), advances the clock, calls evaluator twice, asserts state transitions and score changes against §11 acceptance criteria.

Race tests (`evaluator_race_test.go`): concurrent ingest while evaluator runs; assert no panics, deterministic final state. `-race` enforced.

Fixtures (`testdata/`):

- `schema.sql` — canonical schema, byte-stable round-trip against `Open`'s migration.
- `signal_population_normal.json` — synthetic 200-identity population for z-score correctness.

## 13. Open questions deferred (parent spec §10)

- **Cross-region portability (10.2):** out of scope until federation lands.
- **Weights calibration (10.3):** weights are constants in `scoring.go`. Promotion to config-tunable awaits real population data.
- **Longevity gaming (10.4):** acknowledged but not mitigated in MVP. `first_seen_at` is set once and never updated, so a long-dormant account retains its longevity bonus. The bonus contributes at most 0.2 to the score, and the parent spec calls out the issue in §10.4. Mitigation (decay-on-inactivity, or activity-minimum gating) is a follow-up after we have real abuse data.
- **Transparency (10.5):** MVP does not expose score outside the broker. `Status` returns `State`; numeric score is internal. Aligns with spec preference.

## 14. Migration / rollout

This package replaces `fallbackReputation` in broker. The replacement is a one-line wiring change in tracker `cmd/`: pass the real `*reputation.Subsystem` to `broker.Open` via `Deps.Reputation`. The fallback type stays in `broker/deps.go` and is used in tests that don't want a real reputation subsystem.

No data migration. The first time the tracker starts with this package, `reputation.sqlite` is created empty; every identity starts at default score, OK state. The system warms up over its first long window.

## 15. References

- Parent spec: `docs/superpowers/specs/reputation/2026-04-22-reputation-design.md`
- Tracker spec: `docs/superpowers/specs/tracker/2026-04-22-tracker-design.md`
- Existing seam: `tracker/internal/broker/deps.go:41-46`, `tracker/internal/broker/deps.go:85-90`
- Sibling subsystem (admission): `tracker/internal/admission/doc.go`, `docs/superpowers/specs/admission/2026-04-25-admission-design.md`
- Tracker config conventions: `tracker/internal/config/CLAUDE.md`
