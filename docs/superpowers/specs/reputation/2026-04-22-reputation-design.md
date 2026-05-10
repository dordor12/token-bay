# Reputation Subsystem — Subsystem Design Spec

| Field | Value |
|---|---|
| Parent | [Token-Bay Architecture](../2026-04-22-token-bay-architecture-design.md) |
| Status | Design draft |
| Date | 2026-04-22 |
| Scope | The statistical anomaly-detection and identity-flagging pipeline that sits next to each tracker. Derives per-identity signals from ledger entries + per-request telemetry, computes reputation scores, triggers audit-mode, freezes repeat offenders. Produces the revocation messages federated via the federation protocol. |

## 1. Purpose

Be the L3 backstop for the rate-limit exhaustion gate and general abuse detection:

- L1 (client-side logic) can be bypassed by a plugin fork.
- L2 (cryptographic proof) is not available in v1 and has deployment cost in v2.
- L3 (this subsystem) catches patterns over time. It is statistical, not instantaneous, but it is self-healing and works even when L1/L2 fail.

Reputation also produces the **score** that the tracker broker uses to rank seeders (parent spec §3.2), which is a separate use — it rewards well-behaved seeders with better offer placement.

## 2. Interfaces

### 2.1 Exposed to tracker

- `score(identity_id)` → `float 0..1` — current reputation score.
- `status(identity_id)` → `{ state: OK | AUDIT | FROZEN, reasons: [...], since: u64 }`
- `report_event(event)` — push signal events from tracker internals (accept/reject, offer latency, proof validation outcomes).
- `on_new_ledger_entry(entry)` — hook; the reputation system reacts to settled entries.

### 2.2 Exposed to federation

- `revocations_emitted()` → stream of `REVOCATION` messages to broadcast.
- `apply_revocation(msg)` — inbound revocation from peer; updates local advisory state (but scope-limited per federation §6.1).

### 2.3 Consumed

- Ledger subsystem (query entries for an identity, read running aggregates).
- Tracker broker (receive event reports).

## 3. Signals

### 3.1 Consumer signals

Collected per identity, windowed over rolling 1h, 24h, 7d:

- `network_requests_per_h` — `broker_request` count.
- `exhaustion_claim_rate` — fraction of those requests with accepted exhaustion proofs.
- `local_to_network_ratio` — if optional telemetry is enabled, ratio of locally-served to network-served requests.
- `proof_rejection_rate` — fraction of `broker_request` attempts rejected for bad proofs.
- `proof_fidelity_level` — per-proof classifier: `full_two_signal` (both `stop_failure` and `usage_probe` present and self-consistent), `partial` (one signal missing or degraded), or `degraded` (text-only / synthetic). Systematic `partial` / `degraded` proofs without corresponding local Claude Code environment limitations are an outlier signal.
- `dispute_rate` — fraction of settlements the consumer refused to counter-sign.
- `cross_region_transfer_rate`.

### 3.2 Seeder signals

- `offer_acceptance_rate`.
- `offer_response_latency_p95_ms`.
- `completion_rate` — fraction of accepted offers that produced a settled ledger entry.
- `cost_report_deviation` — seeder's reported (input, output) tokens vs. the statistical norm for the (model × request_complexity_bucket). Outliers either inflate cost or the peer group has genuine variance; the signal is useful only as part of a z-score.
- `consumer_complaint_rate` — consumers marking "seeder tampered" or similar.

### 3.3 Feature engineering

Signals are normalized per regional population (z-scores). A consumer with `exhaustion_claim_rate = 0.95` is only an outlier if the regional median is significantly lower.

## 4. State machine

Each identity is in one of three states:

```
     OK ─────(3 audit events in 7d OR severe breach)──────► FROZEN
      │                                                       ▲
      │ (|z| > 4 on any signal for 24h)                        │
      ▼                                                        │
   AUDIT ──(clean signals for 48h)──► OK                       │
      │                                                        │
      └──────────(repeat audit trigger 3x in 7d)───────────────┘
```

### 4.1 OK (default)

- Full network privileges.
- `score(id)` computed from a moving-average of recent reputation inputs (§5).

### 4.2 AUDIT

- L2 proofs mandatory (v1 self-attested proofs no longer accepted for this identity).
- Telemetry required (plugin must report local/network usage breakdown; opt-out is treated as noncompliance).
- Seeder offers: broker skips this identity for half-duration, and lowers score weight.
- Cleared after 48 hours of non-outlier signals; otherwise re-armed.

### 4.3 FROZEN

- `broker_request` rejected with `IDENTITY_FROZEN`.
- Seeder `advertise` ignored.
- `REVOCATION` emitted to federation.
- Manual operator action required to unfreeze (dispute process).

## 5. Reputation score

For identities in `OK` or `AUDIT`:

```
score = clamp(
  0.3 × completion_health
  + 0.3 × consumer_satisfaction
  + 0.2 × seeder_fairness
  + 0.2 × longevity_bonus
  - 0.4 × audit_recency_penalty
, 0, 1)
```

Each term is normalized:

- `completion_health` — completion_rate × offer_acceptance_rate (seeders) or reciprocal of proof_rejection_rate (consumers).
- `consumer_satisfaction` — derived from complaint_rate (inverted).
- `seeder_fairness` — cost_report_deviation (inverted; small deviation = high score).
- `longevity_bonus` — `log(days_since_enrollment) / log(365)` capped at 1.0.
- `audit_recency_penalty` — exponential decay over 7 days from last audit event.

Scores refresh every minute; cached for tracker broker queries.

## 6. Detection algorithms

### 6.1 Z-score outlier

```
z = (sample - population_median) / population_mad
if |z| > cfg.Reputation.ZScoreThreshold: trigger audit
```

The default in `tracker/internal/config/config.go` is 2.5; the |z|>4 figure was an early sketch and is documented for reference only.

Using median + MAD (median absolute deviation) instead of mean + stddev for robustness against the very outliers we're trying to detect.

Per-signal z-scores computed independently. An identity is triggered if **any** primary signal crosses the threshold; secondary signals that cross are logged but don't trigger alone.

**Primary signals:**
- Consumer: `network_requests_per_h`, `proof_rejection_rate`, `exhaustion_claim_rate`
- Seeder: `cost_report_deviation`, `consumer_complaint_rate`

**Secondary signals:** everything else in §3.

### 6.2 Categorical breaches

Some events are severe enough to trigger AUDIT (single incident) or FROZEN (single incident) regardless of z-scores:

| Event | Action |
|---|---|
| Invalid exhaustion proof signature (vs. corrupted) | AUDIT |
| Ledger entry with inconsistent seeder_sig | AUDIT (both consumer + seeder inspected) |
| Replay of a confirmed nonce | AUDIT |
| 10 consecutive `broker_request` rejections for bad proof | AUDIT |
| Equivocation detected for tracker (not identity — separate flow) | tracker depeering via federation |

### 6.3 Coordinated abuse detection (forward-looking)

Detecting a pool of sybils acting in concert is beyond v1's statistical toolkit. Identify it as future work (clustering on behavior fingerprints, timing correlation, etc.).

## 7. Dispute process

Frozen identities can appeal via `POST /tracker/admin/dispute` (or an in-plugin `token-bay dispute` command). Dispute submission:

```json
{
  "identity_id": "...",
  "evidence": "free-form text + attached ledger entry hashes + captured telemetry",
  "contact": "..."
}
```

Tracker operator reviews manually. For the educational version, no automated dispute — this is deliberately operator-gated.

## 8. Storage

```sql
CREATE TABLE rep_state (
  identity_id BLOB(32) PRIMARY KEY,
  state       INT NOT NULL,     -- 0=OK, 1=AUDIT, 2=FROZEN
  since       INT NOT NULL,
  reasons     TEXT              -- JSON array
);

CREATE TABLE rep_events (       -- append-only stream of raw signal events
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  identity_id BLOB(32),
  event_type  INT,
  value       REAL,
  observed_at INT
);

CREATE INDEX idx_rep_events_id ON rep_events(identity_id, observed_at);

CREATE TABLE rep_scores (       -- materialized latest score
  identity_id BLOB(32) PRIMARY KEY,
  score       REAL,
  updated_at  INT
);
```

Sized modestly: a region with 10K active identities and modest activity produces ~1M events/day; 40MB/day of raw signals, easily manageable.

## 9. Operator controls

- `tracker admin rep show <id>` — inspect state, recent events, score breakdown.
- `tracker admin rep freeze <id> [--reason]` — manual freeze (used for dispute denials).
- `tracker admin rep unfreeze <id>` — manual unfreeze after dispute approval.
- `tracker admin rep thresholds` — view/edit z-score thresholds and weights (requires restart for persistence in v1).

## 10. Open questions

- **Bootstrapping new regions.** Population size below ~100 active identities makes z-scores meaningless. Fall back to absolute-rate thresholds during bootstrap phase; transition criterion TBD.
- **Cross-region reputation portability.** v1: reputation is region-local. A user who leaves region X for Y rebuilds. Not strictly wrong (trust is local), but user-hostile. v2: signed reputation snapshots portable across regions with a freshness discount.
- **Weights calibration.** `0.3/0.3/0.2/0.2/-0.4` from §5 are rough initial values. Calibrate against simulated abuse scenarios before v1.
- **Gaming the longevity bonus.** Old inactive accounts become valuable to take over. Consider decaying longevity if inactive, or requiring recent activity minimums.
- **Transparency.** Should identities see their own reputation score? Arguably yes for user trust; but exposing internals makes gaming easier. Lean toward showing `state` only (OK/AUDIT/FROZEN) and not the numeric score in v1.

## 11. Failure handling

| Failure | Behavior |
|---|---|
| Storage unavailable | Tracker broker defaults score to 0.5 and state to OK; logs alert. No new freeze/audit can happen until recovered. |
| Misclassification (false FROZEN) | Dispute process. Event log retained for manual review. |
| Model drift (thresholds become stale) | Periodic review in operator runbook; no automatic retraining in v1. |

## 12. Acceptance criteria

- Sustained outlier behavior (e.g., `exhaustion_claim_rate` 95%+ when regional median is 5%) triggers AUDIT within the next evaluation cycle (≤ 1 minute).
- AUDIT → FROZEN transition on three AUDIT events in 7 days.
- Frozen identity's `broker_request` returns `IDENTITY_FROZEN`.
- Score changes propagate to tracker broker within 1 minute.
- Dispute workflow produces an auditable operator action record.
