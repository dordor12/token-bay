# Admission — Subsystem Design Spec

| Field | Value |
|---|---|
| Parent | [Token-Bay Architecture](../2026-04-22-token-bay-architecture-design.md) |
| Depends on | [Wire-format v1](../shared/2026-04-24-wire-format-v1-design.md), [Tracker](../tracker/2026-04-22-tracker-design.md), [Ledger](../ledger/2026-04-22-ledger-design.md), [Federation](../federation/2026-04-22-federation-design.md), [Reputation](../reputation/2026-04-22-reputation-design.md) |
| Status | Design draft |
| Date | 2026-04-25 |
| Scope | A new tracker subsystem that decides whether to admit, queue, or reject a `broker_request` based on regional supply pressure and per-consumer credit quality. Sits in front of the broker. Defines a portable `SignedCreditAttestation` for cross-region credit-history transfer, an event-driven derived-state model persisted via append-only tlog, and the algorithms that bind these together. |

## 1. Purpose

The tracker today admits any consumer whose `balance ≥ cost` (ledger spec §3.4). This is insufficient when:

- Aggregate regional capacity is constrained — the network must ration without rejecting indiscriminately.
- Consumer behavior diverges — a 6-month-tenured consumer with clean settlement history is indistinguishable from a 1-day-old consumer at the same balance, even though they pose very different risks to the network.

This subsystem introduces a **gatekeeper** in front of `broker_request` that combines two new signals — regional supply pressure and per-consumer credit score — to produce one of three decisions: `ADMIT`, `QUEUE{position, eta}`, or `REJECT{reason, retry_after_s}`.

Mortgage analogy throughout: the regional tracker is a bank evaluating each loan against (a) its own current liquidity (supply pressure) and (b) the borrower's transaction history (credit score). Both signals matter continuously, not just during scarcity.

## 2. Scope

### 2.1 In scope (v1)

- New tracker module: `tracker/internal/admission`.
- New shared types in `shared/admission/`: `CreditAttestationBody`, `SignedCreditAttestation`, `FetchHeadroomRequest`, `FetchHeadroomResponse`, `TickSource` enum.
- Cross-cutting amendment to tracker spec §2.1 — extend the `heartbeat()` message with three optional fields for headroom transport.
- Sign/verify helpers in `shared/signing` for `SignedCreditAttestation`.
- Validation helpers for all new bodies.
- New file types under tracker data dir: `admission.tlog` (append-only event log), `admission.snapshot.<seq>` (periodic in-memory state dumps).
- Admin HTTP endpoints under `/admission/*`.
- Prometheus metrics for admission decisions, persistence health, attestation handling.
- Configuration surface as a new `admission:` block in tracker config.

### 2.2 Out of scope (v1)

- Live cross-region supply gossip. Admission is *regional-only* in v1 — each tracker decides based on its own region's supply.
- Plugin-side rendering of queue/reject UX. This spec defines the RPC contract; rendering is plugin work.
- Dynamic pricing / cost multipliers tied to credit score.
- Operator-review automation for disputes.
- ML-style score features beyond the five ledger-derived signals.
- Settle-ack piggyback on next `broker_request` (out-of-spec optimization for the tracker's settlement traffic — see §12).

### 2.3 Non-goals (clarifying boundaries)

Admission does **not**:

- **Pick which seeder to match** — that remains `internal/broker` (tracker spec §5.1). Admission decides "should we run the broker?"; broker decides "which seeder serves this request?".
- **Check `balance ≥ cost`** — that remains `internal/ledger`. Balance and credit-score are orthogonal: a rich-but-untrusted consumer can be queued; a poor-but-trusted one is still rejected for insufficient funds.
- **Validate envelope structure** — that remains `internal/api` and the wire-format spec's `ValidateEnvelopeBody`.

## 3. Interfaces

### 3.1 Exposed to plugins (via existing long-lived tracker connection)

- `broker_request(envelope)` — *unchanged at the wire level.* Tracker now runs admission before broker selection. Response now carries one of:
  - `seeder_assignment{seeder_addr, seeder_pubkey, reservation_token}` (ADMIT path; existing tracker spec §5.1 response)
  - `queued{request_id, position_band, eta_band}` (NEW; queued path)
  - `rejected{reason, retry_after_s}` (NEW; reject path)
- `issue_attestation()` → `SignedCreditAttestation | NO_LOCAL_HISTORY` — consumer requests a portable credit attestation from any tracker where they have local ledger history. Rate-limited per consumer (default 6/hour).

### 3.2 Tracker → seeder (via existing long-lived connection)

- `fetch_headroom(model_filter?)` — refines a borderline admission decision; returns the seeder's most recent (cached up to 30s) headroom state. Tracker-initiated.

### 3.3 Tracker spec heartbeat extension (cross-cutting amendment)

The existing `heartbeat()` message (tracker spec §2.1) is extended with three optional fields. Backwards-compatible — older seeders that don't include them continue to work; tracker treats absence as "no change, use cached value."

```proto
// Added to existing tracker heartbeat message:
optional uint32     headroom_estimate = 100;  // fixed-point 0..10000
optional TickSource source            = 101;  // HEURISTIC | USAGE_PROBE
optional uint32     probe_age_s       = 102;  // 0 if HEURISTIC
```

(Field numbers ≥ 100 leave room for future heartbeat fields below.)

### 3.4 Consumed (in-process)

- Ledger event bus — admission subscribes via in-process callback; receives every committed entry and dispute-state-change.
- Reputation subsystem — admission emits `heartbeat_reliability` as a signal source (§5.5).
- Federation peer set — admission queries `federation.peers().contains(issuer_id)` during attestation validation.

## 4. Data structures

### 4.1 Wire types (`shared/admission/admission.proto`)

Proto package `tokenbay.admission.v1`. Go package `shared/admission`. All multi-byte integers transmitted as protobuf varints / fixed; lengths validated by helpers in §6.

#### `CreditAttestationBody`

```proto
message CreditAttestationBody {
  bytes  identity_id        = 1;   // 32 — consumer Ed25519 pubkey hash
  bytes  issuer_tracker_id  = 2;   // 32 — issuing tracker pubkey hash
  uint32 score              = 3;   // fixed-point 0..10000 (= 0.0..1.0)

  // Raw signals (so receiving region can re-weight if it wants):
  uint32 tenure_days            = 4;   // capped 365
  uint32 settlement_reliability = 5;   // fixed-point 0..10000
  uint32 dispute_rate           = 6;   // fixed-point 0..10000 (low = good)
  int64  net_credit_flow_30d    = 7;   // signed credit delta over rolling 30d
  int32  balance_cushion_log2   = 8;   // log2(balance/starter_grant), clamped [-8, 8]

  uint64 computed_at = 9;          // unix seconds; tracker wall-clock
  uint64 expires_at  = 10;         // computed_at + ttl
}

message SignedCreditAttestation {
  CreditAttestationBody body        = 1;
  bytes                 tracker_sig = 2;  // 64 — Ed25519 over DeterministicMarshal(body)
}
```

#### `FetchHeadroom` RPC

```proto
message FetchHeadroomRequest {
  uint64 request_nonce = 1;
  string model_filter  = 2;   // optional; "" for any model
}

message FetchHeadroomResponse {
  uint64     request_nonce      = 1;
  uint32     headroom_estimate  = 2;   // fixed-point 0..10000
  TickSource source             = 3;
  uint32     probe_age_s        = 4;
  bool       can_probe_usage    = 5;
  repeated PerModelHeadroom per_model = 6;
}

message PerModelHeadroom {
  string model              = 1;
  uint32 headroom_estimate  = 2;
}

enum TickSource {
  TICK_SOURCE_UNSPECIFIED = 0;  // validation rejects this
  TICK_SOURCE_HEURISTIC   = 1;
  TICK_SOURCE_USAGE_PROBE = 2;
}
```

`FetchHeadroom` is **unsigned** — it travels over the already-authenticated mTLS/QUIC tracker↔seeder connection.

### 4.2 In-memory tracker-private types (never serialized over wire)

#### `SupplySnapshot`

Atomically swapped published value, read by hot path under `RWMutex.RLock`.

```go
type SupplySnapshot struct {
    ComputedAt          time.Time
    TotalHeadroom       float64           // [0, ∞)
    PerModel            map[string]float64
    ContributingSeeders uint32
    SilentSeeders       uint32
    Pressure            float64           // demand_rate / supply_estimate
    RecentRejectRate    float64           // OVER_CAPACITY rejects per minute
}
```

#### `QueueEntry`

Single max-heap per tracker, keyed on `effective_priority(now)`.

```go
type QueueEntry struct {
    RequestID    uuid.UUID
    ConsumerID   IdentityID
    EnvelopeHash [32]byte
    CreditScore  float64    // [0, 1]
    EnqueuedAt   time.Time
}

func (e *QueueEntry) EffectivePriority(now time.Time, agingAlpha float64) float64 {
    waitMinutes := now.Sub(e.EnqueuedAt).Minutes()
    return e.CreditScore + agingAlpha * waitMinutes
}
```

#### `ConsumerCreditState` (time-bucketed for O(1) eviction)

```go
type DayBucket struct {
    Total uint32
    Clean uint32  // for settlement_buckets only
    // ... or filed/upheld for dispute_buckets, earned/spent for flow_buckets
}

type ConsumerCreditState struct {
    FirstSeenAt        time.Time
    SettlementBuckets  [30]DayBucket  // {total, clean}
    DisputeBuckets     [30]DayBucket  // {filed, upheld}
    FlowBuckets        [30]DayBucket  // {earned, spent}
    LastBalanceSeen    int64           // mirrored from ledger projection
    LastBucketRollAt   time.Time       // for daily rotation
}
```

Memory: ~200 bytes/consumer. Worst-case 10⁶ identities → ~200MB. Active region typically 10⁴–10⁵ → 2–20MB.

#### `SeederHeartbeatState` (rolling 10-minute window)

```go
type MinuteBucket struct {
    Expected uint32  // heartbeats expected at config cadence
    Actual   uint32  // heartbeats received
}

type SeederHeartbeatState struct {
    Buckets             [10]MinuteBucket
    LastBucketRollAt    time.Time
    LastHeadroomEstimate uint32          // cached most-recent value
    LastHeadroomSource   TickSource
    LastHeadroomTs       time.Time
    CanProbeUsage        bool
}
```

### 4.3 On-disk persistence (separate from ledger SQL)

#### `admission.tlog` — append-only event log

Sequential file. Each record:

```
TLogRecord {
  seq:        uint64        // monotonic per file
  ts:         uint64        // unix seconds, tracker wall-clock
  kind:       uint8         // 0=SETTLEMENT, 1=DISPUTE_FILED, 2=DISPUTE_RESOLVED,
                            //   3=HEARTBEAT_BUCKET_ROLL, 4=SNAPSHOT_MARK,
                            //   5=OPERATOR_OVERRIDE
  payload:    bytes         // protobuf-encoded, kind-specific
  crc32:      uint32        // CRC32C over (seq | ts | kind | payload)
}
```

Records are framed with a 4-byte length prefix. `fsync` is **batched** every 5ms (configurable) to amortize syscall cost. Dispute records (kinds 1, 2) use synchronous `fdatasync` for stricter durability — they have no ledger backing to recover from.

File rotates at 1 GiB; rotation creates `admission.tlog.<previous_max_seq>` and starts a fresh file. Old files never deleted (repo rule #4).

#### `admission.snapshot.<seq>` — in-memory state dump

Binary snapshot of `ConsumerCreditState` map + `SeederHeartbeatState` map at a point in time. Format:

```
SnapshotFile {
  magic:           uint32     // 0xADMSNAP1
  format_version:  uint32     // 1
  seq:             uint64     // tlog seq at which this snapshot was taken
  ts:              uint64
  consumers:       repeated ConsumerCreditStateSerialized
  seeders:         repeated SeederHeartbeatStateSerialized
  trailer_crc32:   uint32     // over the entire file body
}
```

Emit cadence: every 10 minutes (`snapshot_interval_s`). Retention: last 3 (`snapshots_retained`).

## 5. Algorithms

### 5.1 `Decide` (admission hot path)

**Entry:** `Decide(envelope, credit_input) → AdmissionResult`

**Steps:**

1. **Resolve `credit_score`:**
   - If `credit_input != nil`:
     - `ValidateCreditAttestationBody(credit_input.Body)` — fail → fall through to next clause
     - `VerifyCreditAttestation(issuer_pubkey, credit_input)` — fail → fall through
     - `now < credit_input.Body.ExpiresAt` — fail → fall through
     - `federation.peers().contains(credit_input.Body.IssuerTrackerID)` — fail → fall through
     - `credit_input.Body.Score / 10000.0`, capped at `max_attestation_score_imported` (default 0.95).
   - Else if `consumerStates[consumer_id]` exists with ≥ 1 settled entry: compute fresh from §5.2.
   - Else: `trial_tier_score` (default 0.4).

2. **Read latest `SupplySnapshot`** under `RWMutex.RLock`. Never blocks; goroutine in §5.3 publishes atomically.

3. **Compute `pressure = max(0.0, demand_rate / supply_estimate)`.** `demand_rate` is an EWMA of `broker_request` arrivals (5s half-life). `supply_estimate` is `SupplySnapshot.TotalHeadroom`.

4. **Branch:**
   - `pressure < pressure_admit_threshold` (default 0.85): `ADMIT`. Skip queue.
   - `pressure < pressure_reject_threshold` (default 1.5) AND queue size < `queue_cap`: enqueue with `effective_priority = credit_score`. Return `QUEUE{position_band, eta_band}` (bucketed per §6.5).
   - Else: `REJECT{reason: REGION_OVERLOADED, retry_after_s}` per §5.4 backoff calculation.

5. **Emit:** one structured log line + bump `admission_decisions_total{result}`.

**Latency target:** p99 < 1ms with no attestation; < 2ms with attestation (Ed25519 verify dominates).

### 5.2 `ComputeLocalScore`

**Entry:** `ComputeLocalScore(consumer_id) → (score, signals)`

**Steps:**

1. Look up `state := consumerStates[consumer_id]`. O(1).
2. Compute the five signals:
   - `tenure_days = min(365, days_since(state.FirstSeenAt))`
   - `settlement_reliability = sum(buckets.Clean) / max(1, sum(buckets.Total))` over `state.SettlementBuckets`
   - `dispute_rate = (sum(buckets.Filed) - sum(buckets.Upheld)) / max(1, total_settlements)` over `state.DisputeBuckets`
   - `net_flow = sum(buckets.Earned) - sum(buckets.Spent)` over `state.FlowBuckets`
   - `balance_cushion_log2 = clamp(floor(log2(max(1, state.LastBalanceSeen / starter_grant))), -8, 8)`
3. Normalize each to `[0, 1]`:
   - `tenure_norm = tenure_days / tenure_cap_days` (capped 1.0)
   - `reliability` already in [0, 1]
   - `inverse_dispute = 1 - dispute_rate`
   - `net_flow_norm = sigmoid(net_flow / net_flow_normalization_constant)`
   - `cushion_norm = (balance_cushion_log2 + 8) / 16`
4. **Compose** with config weights (must sum to 1.0):
   ```
   score = w_reliability      · reliability
         + w_inverse_dispute  · inverse_dispute
         + w_tenure           · tenure_norm
         + w_net_flow         · net_flow_norm
         + w_cushion          · cushion_norm
   ```
   Defaults: 0.30 / 0.10 / 0.20 / 0.30 / 0.10.
5. **Undefined-signal handling:** if a consumer has zero settlements, `reliability` and `inverse_dispute` are undefined; their weight (0.40) is redistributed proportionally to the other three signals. Tenure and cushion are always defined.

**Cost:** O(30) bucket iterations + arithmetic. Sub-microsecond.

### 5.3 Supply aggregation (background, every 5s)

Goroutine on `time.Ticker(5 * time.Second)` inside `internal/admission`.

**Steps:**

1. Snapshot the seeder registry (read-only iteration over the tracker-spec §4.1 `SeederRecord` map).
2. For each seeder, compute weighted contribution:
   ```
   contribution = headroom_estimate / 10000
                · model_weight                        // 1.0 in v1; per-model awareness wires via PerModelHeadroom
                · freshness_decay(now − last_heartbeat_ts)
                · heartbeat_reliability(seeder_id)
   ```
   `freshness_decay`: 1.0 if `last_heartbeat_ts` < 60s ago; linear decay to 0 at `heartbeat_freshness_decay_max_s` (default 300s); 0 thereafter.
3. Sum into `total_headroom` and `per_model`.
4. Update `pressure = demand_rate_ewma / total_headroom`.
5. Atomically swap the published `SupplySnapshot` (`atomic.Pointer[SupplySnapshot]`).
6. Emit edge-detection events: when `pressure` crosses 1.0 in either direction, log + metric `admission_pressure_threshold_crossing{direction}`.

**Cost:** O(N_seeders) per 5s. Trivial at design target (~200 seeders).

### 5.4 Queue draining

**Entry:** runs as a callback when broker observes `seeder_state: BUSY → IDLE` or new seeder enrolls.

**Steps:**

1. Loop while queue non-empty:
   - Peek heap top.
   - `min_serve_priority = max(0.0, 0.5 · (pressure − 1.0))`. At pressure 1.0: 0.0 (anyone serves). At pressure 2.0: 0.5. At pressure 3.0: 1.0 (only top-credit, effectively no one).
   - If `top.EffectivePriority(now) < min_serve_priority` AND `pressure > 1.0`: stop. Aging boost will catch up.
   - Pop top. Hand to broker for normal §5.1 selection.
   - If broker returns `NO_CAPACITY`: re-enqueue with `EnqueuedAt = now − wait_already_accumulated` so aging boost is preserved.
2. Queue timeout: any `QueueEntry` with `now − EnqueuedAt > queue_timeout_s` (default 300s) → `REJECT{reason: QUEUE_TIMEOUT}`. Sweep runs on the same tick.

**Backoff calculation for `REJECT.retry_after_s` (§5.1 step 4):**
```
queue_drain_estimate = queue_size · avg_recent_service_time
retry_after_s = clamp(30 + jitter(0, 30) + queue_drain_estimate, 60, 600)
```
Jitter prevents thundering-herd retries.

### 5.5 Attestation issuance

**Entry:** `IssueAttestation(consumer_id) → SignedCreditAttestation | error`

**Steps:**

1. Rate-limit check: per-consumer 6 issuances/hour (configurable).
2. If `consumerStates[consumer_id]` empty (no local history): return `NO_LOCAL_HISTORY`.
3. `(score, signals) := ComputeLocalScore(consumer_id)`.
4. Build:
   ```
   body := CreditAttestationBody{
     IdentityID:           consumer_id,
     IssuerTrackerID:      tracker.identity,
     Score:                uint32(score · 10000),
     TenureDays:           uint32(signals.tenure_days),
     SettlementReliability: uint32(signals.reliability · 10000),
     DisputeRate:          uint32(signals.dispute_rate · 10000),
     NetCreditFlow30d:     signals.net_flow,
     BalanceCushionLog2:   int32(signals.balance_cushion_log2),
     ComputedAt:           now,
     ExpiresAt:            now + attestation_ttl_seconds,
   }
   ```
5. `sig := SignCreditAttestation(tracker_priv, body)` (uses `DeterministicMarshal` per wire-format-v1 §4.1).
6. Return `SignedCreditAttestation{Body: body, TrackerSig: sig}`.
7. Bump `admission_attestations_issued_total`.

**No state change.** Issuance is a pure read — nothing appended to ledger, nothing gossiped.

### 5.6 `OnLedgerEvent` (incremental state update)

Subscribed to the ledger's in-process event bus.

**On `kind = SETTLEMENT_FINALIZED`:**
1. Locate the relevant day-bucket via `bucket_idx = (timestamp_day) % 30`. If the bucket's day-stamp is older than 30 days: zero it (rotation).
2. Update buckets:
   - `consumerStates[consumer_id].SettlementBuckets[idx].Total++`
   - if `flags & 1 == 0`: `.Clean++`
   - `consumerStates[consumer_id].FlowBuckets[idx].Spent += cost_credits`
   - `consumerStates[seeder_id].FlowBuckets[idx].Earned += cost_credits` (seeder may also be a consumer; same hashmap, same operation)
3. Update running-balance mirrors:
   - `consumerStates[consumer_id].LastBalanceSeen -= cost_credits`
   - `consumerStates[seeder_id].LastBalanceSeen += cost_credits`

   These are admission's local mirror of the ledger's authoritative balance projection. They're allowed to drift if events are lost (mitigated by §5.7 cross-check + §9.1 `/admission/recompute/<consumer_id>` operator endpoint, which re-reads the ledger projection and resets the mirror).
4. If `consumerStates[consumer_id].FirstSeenAt` unset: initialize to `timestamp`. (Same for seeder_id.)
5. Append `TLogRecord{kind: SETTLEMENT, payload: {consumer_id, seeder_id, flags, cost_credits, ts}}` (batched fsync).

**On `kind = TRANSFER_IN` / `TRANSFER_OUT`** (ledger spec §3.1 entry kinds 1, 2): update `LastBalanceSeen` for the affected identity (`+= cost_credits` for transfer_in to consumer; `-= cost_credits` for transfer_out). No bucket updates — transfers are not usage and don't feed reliability/dispute/flow signals. Append corresponding `TLogRecord`.

**On `kind = STARTER_GRANT`** (ledger spec §3.1 entry kind 3): initialize `consumerStates[consumer_id]` if absent; set `FirstSeenAt = timestamp`, `LastBalanceSeen = cost_credits`. Append `TLogRecord`.

**On `kind = DISPUTE_FILED`:** synchronous `fdatasync` tlog write; update `DisputeBuckets[idx].Filed++`.

**On `kind = DISPUTE_RESOLVED`:** synchronous `fdatasync`; if `status == UPHELD`, increment `.Upheld`.

### 5.7 `StartupReplay`

On boot:

1. Find the latest readable `admission.snapshot.<seq>` (validate magic + length + trailer CRC; on failure fall back to next-older snapshot per §6.3).
2. Load it into `consumerStates` + `seederStates`.
3. Open `admission.tlog`. Seek to records with `seq > snapshot.seq`.
4. Replay each record (CRC-validate; on first failure halt at last-good and surface to operator).
5. Cross-check: query ledger for entries with `seq > tlog_max_seq`. For any found, synthesize equivalent `TLogRecord`s by reading the ledger entries directly, append to tlog, update in-memory state.
6. Begin serving admission decisions.

**Recovery time:** snapshot load + ≤10 min of tlog replay typically. Goal < 30s for nominal regional state.

### 5.8 `SnapshotEmit`

Goroutine on `time.Ticker(snapshot_interval_s)`:

1. Acquire read-lock on `consumerStates` + `seederStates`.
2. Stream-write `admission.snapshot.<current_tlog_seq>` (write to `.tmp`, fsync, atomic rename).
3. Release lock.
4. Delete oldest snapshot if total > `snapshots_retained`.
5. Append `TLogRecord{kind: SNAPSHOT_MARK, payload: {snapshot_seq}}` so replay knows where to start.

Acquires lock briefly (only the snapshot start point); does not block hot path on writes (in-memory state is COW or copy-on-write-style snapshotted into a temp map).

## 6. Validation, signing, testing

### 6.1 Validation helpers (in `shared/admission`)

```go
func ValidateCreditAttestationBody(b *CreditAttestationBody) error
func ValidateFetchHeadroomRequest(r *FetchHeadroomRequest) error
func ValidateFetchHeadroomResponse(r *FetchHeadroomResponse) error
```

`ValidateCreditAttestationBody` enforces:
- `len(IdentityID) == 32`
- `len(IssuerTrackerID) == 32`
- `Score ≤ 10000`
- `TenureDays ≤ 365`
- `SettlementReliability ≤ 10000`
- `DisputeRate ≤ 10000`
- `BalanceCushionLog2 ∈ [-8, 8]`
- `ComputedAt > 0`
- `ExpiresAt > ComputedAt`
- `ExpiresAt − ComputedAt ≤ attestation_max_ttl_seconds` (default 7 days)

`ValidateFetchHeadroomResponse` enforces:
- `Source != TICK_SOURCE_UNSPECIFIED`
- `HeadroomEstimate ≤ 10000`
- All `PerModel.HeadroomEstimate ≤ 10000`

### 6.2 Sign/verify helpers (in `shared/signing`)

Following wire-format-v1 §6:

```go
func SignCreditAttestation(priv ed25519.PrivateKey, body *admission.CreditAttestationBody) ([]byte, error)
func VerifyCreditAttestation(pub ed25519.PublicKey, signed *admission.SignedCreditAttestation) bool
```

Both go through `signing.DeterministicMarshal` — the single canonical entry point for "bytes to sign" (wire-format-v1 §4.1).

### 6.3 Required tests

Per wire-format-v1 §7:

1. **Round-trip** for `CreditAttestationBody`, `SignedCreditAttestation`, `FetchHeadroomRequest`, `FetchHeadroomResponse`, all `TickSource` enum values.
2. **Byte-stable golden fixture** at `shared/admission/testdata/credit_attestation.golden.hex` — populated body with deterministic values + fixture keypair → specific hex bytes; CI diff-fails on drift.
3. **Sign / verify positive + tampered** — flip bits in body fields, in sig, swap pubkey; all must reject.
4. **Validation rejection matrix** — one row per invariant in §6.1.
5. **Nil-safety** — `SignCreditAttestation(priv, nil)` returns error; `VerifyCreditAttestation(pub, nil)` returns false; `VerifyCreditAttestation(pub, &SignedCreditAttestation{Body: nil})` returns false.

Coverage target: ≥ 90% line coverage on `shared/admission` and `shared/signing` additions.

### 6.4 Tracker-internal tests (in `tracker/internal/admission`)

- **Decision matrix** (table-driven): every combination of `{attestation: nil|valid|expired|forged|ejected_issuer}` × `{local_history: none|some|extensive}` × `{pressure: low|medium|high|extreme}` → expected `AdmissionResult` shape.
- **Aging boost correctness**: a 0.3-credit consumer waiting 10 min outranks a fresh 0.5-credit consumer.
- **Queue overflow**: at `queue_cap`, next request returns `REJECT{reason: REGION_OVERLOADED, retry_after_s: ∈ [60, 600]}`.
- **Persistence round-trip**: emit snapshot, restart, replay tlog, verify in-memory state byte-equal to pre-restart.
- **Crash recovery**: kill mid-`OnLedgerEvent`, verify tlog gap is detected and filled from ledger on next boot.
- **Concurrency**: `go test -race ./internal/admission/...` clean. Mandatory.

### 6.5 Bucketed response semantics

`QUEUE` responses use **bands**, not exact values, to limit information leakage (§7.5):

```
position_band: ENUM { POS_1_TO_10 | POS_11_TO_50 | POS_51_TO_200 | POS_200_PLUS }
eta_band:      ENUM { ETA_LT_30S | ETA_30S_TO_2M | ETA_2M_TO_5M | ETA_5M_PLUS }
```

## 7. Failure handling

| # | Failure mode | Detection | Response | Always recoverable? |
|---|---|---|---|---|
| 7.1 | Crash mid-`OnLedgerEvent` | tlog `seq` gap vs ledger `seq` at startup | Synthesize from ledger entries; append to tlog | Yes |
| 7.2 | tlog corruption (per-record) | `crc32` mismatch on replay | Truncate at last-valid; replay forward; cross-check ledger; flag affected disputes for operator | Yes (operator review for disputes) |
| 7.3 | Snapshot corruption | magic / length / trailer CRC mismatch | Fall back to next-older snapshot (3 retained); worst case cold-start replay from tlog beginning | Yes |
| 7.4 | All snapshots + tlog corrupt | replay fails entirely | `admission_degraded_mode_active = 1`; everyone treated as cold-start; rebuild from incoming events | Decisions still flow |
| 7.5 | Federation peer ejected after issuing attestation | `federation.peers().contains(issuer)` returns false at decision time | Treat attestation as absent; fall through to local-history or trial-tier | Yes |
| 7.6 | Malformed `SignedCreditAttestation` | `Validate` or `Verify` fails | Treat as absent; never error to consumer | Yes |
| 7.7 | `FetchHeadroom` timeout | 200ms timeout | Use cached heartbeat-derived headroom; decision proceeds | Yes |
| 7.8 | Queue overflow | `len(queue) ≥ queue_cap` | `REJECT{reason: REGION_OVERLOADED, retry_after_s}` per §5.4 | Yes |
| 7.9 | No history + no attestation | both fallthroughs in §5.1 step 1 | `trial_tier_score` (default 0.4) | Yes |
| 7.10 | Heartbeat-reliability skew during clock anomalies | wall-clock comparison in bucket-roll goroutine | Tolerate; rolling 10-min window washes out transient distortion | Yes |
| 7.11 | Federation peer set churn during a `Decide` | snapshot semantics | Decision uses whatever the peer set says at that instant | N/A — already-admitted requests unaffected |

**Two design properties enforced:**

1. **Admission never blocks on external state.** Every failure mode falls through to a *decision* — possibly degraded, never an error to the consumer. Plugin always sees `ADMIT | QUEUE | REJECT`.
2. **Recovery from any single point of failure.** Tlog corruption + snapshot corruption + ledger reachable → recoverable. Tlog reachable + snapshot corrupt → recoverable. Snapshot reachable + tlog corrupt at tail → recoverable.

## 8. Security model

### 8.1 Forged or replayed attestation

Forgery requires the issuing tracker's Ed25519 private key. Replay-after-expiry is caught by the post-`Verify` `expires_at` check. Replay-before-expiry within TTL is *intended* — that's the design (consumers re-present the same attestation to multiple trackers within 24h).

**Residual:** a peer compromised at the key level can mint forgeries. Detected via ledger spec §4.4 equivocation path; ejected peers' attestations rejected going forward (no retroactive invalidation of already-admitted requests).

### 8.2 Sybil — fresh-identity flooding

Per-identity `broker_request` rate limit (tracker spec §9, default 2/sec) + per-IP `enroll` limit (default 1/min) bound the rate. Trial-tier consumers are *queueable*, not unconditional admit — under pressure, Sybil flood just queues everyone. `admission_trial_tier_decisions_total` feeds reputation spec's signal set for pattern detection.

**Residual:** during low-pressure periods, Sybils ARE admitted at trial-tier. Accepted v1 trade-off — refusing trial-tier admission entirely would lock out genuine new users.

### 8.3 Cherry-picking attestations across regions

Acknowledged residual risk. A consumer with history in multiple regions may request attestations from each and present the highest. Mitigations:

- Attestations are computed from local-only history — ceiling is whatever the consumer's actual best regional record supports (cannot fabricate).
- Aging boost reduces cherry-pick value during genuine crunch.
- 24h TTL caps stale cherry-pick value.
- `max_attestation_score_imported` (default 0.95) clamps inflated peer attestations.

V2 mitigations (§12): monotonic nonce per consumer + federation gossip of latest issued; trackers reject stale-nonce attestations.

### 8.4 Resource exhaustion at admission layer

Hot path is O(1) hashmap + O(log queue_size) heap insert + Ed25519 verify when attestation present. Per-identity rate limits cap distributed flood. `IssueAttestation` rate-limited 6/hour. `FetchHeadroom` is tracker-initiated only.

**Residual:** memory growth in `ConsumerCreditState` map at Sybil scale — operator can configure cap with LRU-evict-to-snapshot-only-mode. Not a v1 concern at design target.

### 8.5 Information-leakage via admission decisions

Mitigations:
- Position and ETA banded (§6.5), not exact.
- `REJECT.retry_after_s` includes ±50% jitter.
- No leak of *why* a request was queued vs admitted.
- All responses on per-identity authenticated TLS/QUIC — no cross-identity observability.

**Residual:** operator with admin-API access sees full queue; same privilege boundary as any tracker-internal state.

### 8.6 Adversarial peer trackers issuing inflated attestations

Defense: `max_attestation_score_imported` (default 0.95) clamps any attestation's effective score. Operator can blocklist a specific peer's attestations independently of full federation eject (`admission.peers.attestation_blocklist[]` config).

**Residual:** federated trust is cooperative. v1 accepts that a malicious-but-undetected peer can inflate up to the regional ceiling for some duration. Reputation gossip (federation spec) is the longer-term answer.

## 9. Operator interface

### 9.1 Admin HTTP endpoints (tracker spec §8 internal port)

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/admission/status` | Live `SupplySnapshot` + queue depth + `pressure` + thresholds |
| `GET` | `/admission/queue` | Current queue contents (consumer_id, score, enqueued_at, effective_priority) |
| `GET` | `/admission/consumer/<id>` | Per-consumer credit state — five raw signals, composite score, attestation history |
| `GET` | `/admission/seeder/<id>` | Per-seeder heartbeat-reliability state |
| `POST` | `/admission/queue/drain` | Force-drain N entries from top of queue (operator override) |
| `POST` | `/admission/queue/eject/<request_id>` | Eject specific entry; consumer receives `REJECT` |
| `POST` | `/admission/snapshot` | Force snapshot emit (otherwise on cadence) |
| `POST` | `/admission/recompute/<consumer_id>` | Force score recomputation from ledger replay (drift verification) |
| `GET` | `/admission/peers/blocklist` | List peers blocklisted from attestation issuance |
| `POST` | `/admission/peers/blocklist/<peer_id>` | Add to blocklist |
| `DELETE` | `/admission/peers/blocklist/<peer_id>` | Remove from blocklist |

All `POST`/`DELETE` emit a `TLogRecord{kind: OPERATOR_OVERRIDE}` with operator identity + timestamp + parameters.

### 9.2 Prometheus metrics

**Hot-path:**
- `admission_decisions_total{result}`
- `admission_queue_depth` (gauge)
- `admission_queue_drained_per_sec` (rate)
- `admission_pressure` (gauge)
- `admission_supply_total_headroom` (gauge)
- `admission_demand_rate_ewma` (gauge)
- `admission_decision_duration_seconds` (histogram)

**Attestations:**
- `admission_attestations_issued_total`
- `admission_attestation_validation_failures_total{stage}` — stage ∈ length/range/expired/signature/protobuf/issuer_ejected
- `admission_attestation_age_seconds` (histogram on accepted)
- `admission_trial_tier_decisions_total`

**Persistence:**
- `admission_tlog_replay_gap_entries`
- `admission_tlog_corruption_records_total`
- `admission_snapshot_load_failures_total{which}` — latest/older/oldest
- `admission_degraded_mode_active` (gauge 0/1)

**Operational:**
- `admission_clock_jump_detected_total{direction}`
- `admission_fetchheadroom_timeouts_total`
- `admission_rejections_total{reason}`
- `admission_pressure_threshold_crossing{direction}`

### 9.3 Configuration block

Added to tracker config:

```yaml
admission:
  # Decision thresholds
  pressure_admit_threshold: 0.85
  pressure_reject_threshold: 1.5
  queue_cap: 512
  trial_tier_score: 0.4

  # Aging boost
  aging_alpha_per_minute: 0.05
  queue_timeout_s: 300

  # Score weights (must sum to 1.0)
  score_weights:
    settlement_reliability: 0.30
    inverse_dispute_rate:   0.10
    tenure:                 0.20
    net_credit_flow:        0.30
    balance_cushion:        0.10

  # Score normalization
  net_flow_normalization_constant: 10000
  tenure_cap_days: 30
  starter_grant_credits: 1000
  rolling_window_days: 30

  # Cold-start exit
  trial_settlements_required: 50
  trial_duration_hours: 72

  # Attestation issuance
  attestation_ttl_seconds: 86400
  attestation_max_ttl_seconds: 604800
  attestation_issuance_per_consumer_per_hour: 6
  max_attestation_score_imported: 0.95

  # Persistence
  tlog_path: ${data_dir}/admission.tlog
  snapshot_path_prefix: ${data_dir}/admission.snapshot
  snapshot_interval_s: 600
  snapshots_retained: 3
  fsync_batch_window_ms: 5

  # Heartbeat reliability
  heartbeat_window_minutes: 10
  heartbeat_freshness_decay_max_s: 300

  # Federation
  attestation_peer_blocklist: []
```

**Validation at startup** (loud failure beats silent misconfiguration):
- `score_weights.values().sum() != 1.0` (within 0.001) → reject
- `pressure_admit_threshold ≥ pressure_reject_threshold` → reject
- `attestation_ttl_seconds > attestation_max_ttl_seconds` → reject
- `trial_tier_score ∉ [0.0, 1.0]` → reject
- `tlog_path` parent dir non-existent → reject

## 10. Acceptance criteria

The spec is complete when an implementation passes all of:

**Functional:**
1. Cold-start consumer (no history, no attestation) → `trial_tier_score = 0.4`; admitted under low pressure, queued under high.
2. Consumer with clean 30-day history → score ≥ 0.9; admitted under any pressure ≤ `pressure_reject_threshold`.
3. Valid attestation from federation peer → score honored exactly (post-clamp).
4. Expired or signature-invalid attestation → fall back to local/cold-start; never errors out.
5. Higher-credit consumers serve before lower under pressure, modulo aging.
6. Aging boost: 0.3-credit consumer waiting 10 min serves before 0.5-credit consumer who just enqueued.
7. Queue overflow returns `REJECT{retry_after_s ∈ [60, 600]}` with jitter.
8. `IssueAttestation` produces verifiable `SignedCreditAttestation` whose embedded score matches local computation.

**Persistence + recovery:**
9. Crash mid-ledger-event → tlog catches up to ledger seq within bounded gap on restart.
10. tlog mid-record corruption → truncate at last-valid + replay forward; degraded scoring during replay never blocks decisions.
11. Latest snapshot deleted → admission loads next-older + replays tlog; total recovery < 30s for nominal regional state.
12. All snapshots corrupted → admission boots in degraded mode; rebuilds online from incoming events.

**Performance** (tracker spec §6 reference VM: 2 vCPU, 4GB RAM):
13. `admission_decision_duration_seconds` p99 < 1ms without attestation; < 2ms with attestation.
14. Sustained 100 broker_requests/sec for 1 hour with no metric degradation.
15. `admission_supply_total_headroom` updates within 5s of a heartbeat-headroom change.
16. tlog steady-state write rate ≤ ledger commit rate; no growing backlog.

**Security:**
17. Forged attestation (valid issuer, tampered post-sign) rejected.
18. Attestation from ejected federation peer rejected.
19. Inflated peer attestation (`score > max_attestation_score_imported`) clamped to ceiling.
20. `admission/queue` admin endpoint requires operator auth.

**Race-detector-clean:** `go test -race ./internal/admission/...` passes. `internal/admission` joins `internal/broker` + `internal/federation` on the tracker CLAUDE.md "always run with `-race`" list.

## 11. Cross-cutting amendments shipped by this plan

Three upstream-spec amendments:

1. **Tracker spec §2.1 — heartbeat schema.** Add three optional fields:
   - `headroom_estimate: uint32`
   - `source: TickSource` (the enum lives in `shared/admission`, imported by the heartbeat proto)
   - `probe_age_s: uint32`

   Backwards-compatible (proto3 optional). Older seeders continue to work; tracker treats absence as "no change."

2. **Tracker spec §2.1 / §5.1 — `broker_request` response variants.** The current spec returns either `seeder_assignment` or `NO_CAPACITY`. Two new response variants are introduced for admission outcomes:
   - `queued{request_id, position_band, eta_band}` — admission decided to queue rather than admit.
   - `rejected{reason, retry_after_s}` — admission decided to reject (region overloaded, queue full, queue timeout).

   Wire format: a `oneof` extension on the existing `BrokerRequestResponse` message. `seeder_assignment` and `NO_CAPACITY` remain valid alternatives. Receiving plugins MUST handle all four cases; `position_band`, `eta_band`, and `reason` are enums defined in `shared/admission`.

3. **Tracker spec §6 — concurrency-test list.** Add `internal/admission` to the modules with mandatory `-race`.

All three amendments ship in the same PR as the admission implementation so the repo is never internally inconsistent.

## 12. Future work

- **Settle-ack piggyback on next `broker_request`** — tracker spec §5.2 amendment to reduce per-prompt round-trips. Out of scope here; logged as a separate optimization PR.
- **Monotonic attestation nonces** + federation gossip of latest issued — gaming-resistant cherry-pick prevention.
- **Live cross-region supply gossip** — promotes admission to fully federated; new low-latency federation message type.
- **Dynamic pricing** based on credit score — alternative to admission gating; changes ledger pricing semantics.
- **ML-style score features** beyond the five ledger-derived signals — temporal patterns, behavioral clustering.
- **Plugin-side queue UI** — render `QUEUE{position_band, eta_band}`; allow opt-out ("just reject me, don't queue").
- **Operator review automation for disputes** — heuristic auto-resolution of clear-cut cases.
- **Adaptive heartbeat-emission cadence** — emit headroom field only when changed by >threshold; cuts heartbeat payload by ~70-80% in steady state.
- **Tightened `FetchHeadroom` trigger band** — fire only when pressure ∈ [0.85, 1.5] AND consumer credit within ±0.1 of `min_serve_priority`. Eliminates PTY storms during normal operation.

## 13. Open questions

- **Cherry-picking residual** — bound at "one tier of admission priority" in v1; v2 mechanism (monotonic nonce) ready when operationally needed.
- **Trial-tier Sybil tolerance** — accepted in v1; no protocol fix without harming legitimate new users.
- **Bucket size for the 30-day window** — current proposal: 30 day-buckets. Alternatives: 60 12-hour buckets (higher resolution, 2× memory) or 7 4-day buckets (lower resolution, less memory). Day-buckets feel right for a 30-day window; revisit if memory becomes an issue.
- **`net_flow_normalization_constant` calibration** — default 10000 picked by analogy; needs operational tuning once we have real settlement rate data.
