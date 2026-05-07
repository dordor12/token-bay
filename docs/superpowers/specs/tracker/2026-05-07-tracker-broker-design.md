# Broker — Subsystem Design Spec

| Field | Value |
|---|---|
| Parent | [Regional Tracker](2026-04-22-tracker-design.md) |
| Depends on | [Wire-format v1](../shared/2026-04-24-wire-format-v1-design.md), [Ledger](../ledger/2026-04-22-ledger-design.md), [Admission](../admission/2026-04-25-admission-design.md), [Tracker internal API + server](2026-05-03-tracker-internal-api-server-design.md), [Tracker registry](2026-04-22-tracker-design.md#41-seeder-registry-entry-in-memory) |
| Status | Design draft |
| Date | 2026-05-07 |
| Scope | The `tracker/internal/broker` Go package. Covers (a) the broker selection algorithm and offer round-trip that turns a `broker_request` into a seeder assignment, (b) the in-memory in-flight-request state machine and credit reservation ledger that span the broker → settlement lifecycle, (c) the settlement path that consumes a seeder's `usage_report`, collects the consumer's counter-signature, and appends a `USAGE` entry to the ledger. Implements tracker-design §5.1 (selection), §5.2 (settlement), §5.3 (timeouts), §6 (concurrency) for the broker, and the §11.2 admission amendment that turns `broker_request`'s response into a four-variant oneof. |

## 1. Purpose

`tracker/internal/broker` is the part of the tracker that, after the admission gate has cleared a request, picks a capable seeder, runs the offer round-trip, hands the consumer an assignment, and — when the seeder later reports usage — finalizes a `USAGE` entry on the ledger. It owns the two pieces of regional tracker state that span an entire request lifecycle: the credit *reservation* (consumer balance debited the moment an envelope is admitted, released only when the request settles or expires) and the *in-flight request record* (state machine from `SELECTING` through `COMPLETED`).

The broker is **not** a wire-handling package. The plugin-facing `broker_request`, `usage_report`, and `settle` RPCs are dispatched by `internal/api`; this package exposes Go-typed `Submit` / `HandleUsageReport` entry points that `internal/api` calls.

## 2. Scope

### 2.1 In scope (v1)

- New `tracker/internal/broker` package containing two sibling subsystems — `*Broker` (selection) and `*Settlement` (finalization) — sharing internal state.
- Selection algorithm per tracker-design §5.1: validate envelope, reserve `max_cost`, filter and score candidates from the registry, run the offer push loop with `MaxOfferAttempts`.
- Settlement path per tracker-design §5.2: receive `usage_report`, build entry preimage, push `SettlementPush` to consumer, await consumer counter-sig with timeout, append USAGE entry via `internal/ledger`.
- Reservation ledger and in-flight registry, both in-memory, sharded under `-race`.
- Pricing helper (`PriceTable`) covering the v1 model set with operator-overridable rates via a new `pricing:` config block.
- Wire amendment: introduce `BrokerRequestResponse` oneof (`SeederAssignment | NoCapacity | Queued | Rejected`) per admission-design §11.2. Add new top-level proto messages `SeederAssignment`, `Queued`, `Rejected`, and enums `PositionBand`, `EtaBand`, `RejectReason`.
- `api`-layer integration: `installBrokerRequest` runs admission gate before broker, translates the four outcomes to the new oneof; `installUsageReport` and `installSettle` swap from notImpl stubs to real handlers.
- Plugin `trackerclient.BrokerRequest` updated to return a sum-typed result reflecting the new oneof.
- Queue-drain consumer: a polled goroutine that pops admission-queued entries when capacity frees and re-enters `Submit` for them.
- Reservation TTL reaper goroutine.
- Prometheus metrics for broker decisions, offer outcomes, reservation lifecycle, settlement results.

### 2.2 Out of scope (v1)

- RTT-aware selection. The score formula's γ·rtt term is preserved in config but `rttMs=0` is hard-wired in v1; the term collapses to zero. Logged as future work (§14).
- Admission attestation collection in `broker_request`. The current envelope schema has no attestation field; admission falls through to local-history or trial-tier scoring.
- Settle-ack piggyback on the next `broker_request` (admission-design §12 future work).
- Cross-region transfer mid-flight. A consumer migrating regions while a request is in flight is not supported; the request will fail and the reservation TTL-reap.
- Tunnel-failure feedback from the consumer. If a seeder accepts but the tunnel never establishes, the broker only learns this via reservation TTL expiry. A consumer-initiated `tunnel_failed` notification is logged as future work.
- Operator override of in-flight requests beyond the admin endpoints in §10.
- Horizontal sharding of the broker subsystem. Single-process v1 only.

### 2.3 Non-goals (clarifying boundaries)

The broker does **not**:

- **Decide whether to admit a request.** That is `internal/admission`. The broker's `Submit` assumes admission has already returned `OutcomeAdmit`.
- **Validate the envelope's structure or signatures.** That is `internal/api` plus `shared/proto.ValidateEnvelopeBody` and `shared/signing.VerifyEnvelope`. The broker treats the envelope it receives as already-verified.
- **Verify the balance proof signature.** `internal/api` calls `signing.VerifyBalanceSnapshot` and the freshness check before invoking the broker. The broker reads `balance_proof.body.credits` as ground truth.
- **Verify exhaustion proofs.** `internal/api` does that via `shared/exhaustionproof.Verify`.
- **Pick or measure consumer↔seeder RTT.** v1 passes `rttMs=0` (§4.4.3).
- **Sign ledger entries.** `internal/ledger.AppendUsage` does the tracker signature; the broker provides typed input only.

## 3. Interfaces

### 3.1 Exposed to `internal/api`

```go
type Broker struct { /* private */ }

// Submit is the broker_request entrypoint. Caller (api/) must have already:
//   - decoded EnvelopeSigned and called signing.VerifyEnvelope
//   - called signing.VerifyBalanceSnapshot and confirmed body.balance_proof
//     freshness (issued_at within 600s of now)
//   - verified body.exhaustion_proof
//   - called admission.Decide and received OutcomeAdmit
//
// Returns one of: Result{Outcome: Admit, Admit: *Assignment} or
// Result{Outcome: NoCapacity, NoCap: *NoCapacityDetails}. Submit never
// returns OutcomeQueue or OutcomeReject — those are admission's outcomes,
// surfaced by api/ before Submit is called.
func (*Broker) Submit(ctx context.Context, env *tbproto.EnvelopeSigned) (*Result, error)

// RegisterQueued caches an envelope for a request_id that admission has
// queued. The drain goroutine calls Submit with this cached envelope when
// admission later pops it.
func (*Broker) RegisterQueued(envelope *tbproto.EnvelopeSigned, requestID [16]byte, deliver func(*Result))

func (*Broker) Close() error
```

```go
type Settlement struct { /* private */ }

// HandleUsageReport is the usage_report entrypoint. peerID is the seeder's
// authenticated identity from the connection that delivered the RPC.
//
// Returns UsageAck immediately on receipt; the consumer counter-sig wait
// and ledger append happen asynchronously inside Settlement (see §5.2).
func (*Settlement) HandleUsageReport(ctx context.Context, peerID ids.IdentityID, r *tbproto.UsageReport) (*tbproto.UsageAck, error)

// HandleSettle is the settle RPC entrypoint. Caller (api/) routes the
// consumer's (preimage_hash, sig) into Settlement, which dispatches it
// to the matching in-flight goroutine via an internal map. Returns a
// SettleAck on success, or RPC_STATUS_NOT_FOUND if no settlement is
// awaiting this preimage_hash.
func (*Settlement) HandleSettle(ctx context.Context, peerID ids.IdentityID, r *tbproto.SettleRequest) (*tbproto.SettleAck, error)

func (*Settlement) Close() error
```

### 3.2 Composite opener

```go
type Subsystems struct {
    Broker     *Broker
    Settlement *Settlement
}

func Open(cfg config.BrokerConfig, scfg config.SettlementConfig, deps Deps, opts ...Option) (*Subsystems, error)
```

`Open` constructs the shared `*Inflight` and `*Reservations` once, threads them into both, starts the queue-drain goroutine (owned by `Broker`) and the reservation-TTL reaper goroutine (owned by `Settlement`). `cmd/run_cmd` calls `Open` once and passes the two subsystems into `api.Deps`.

### 3.3 Wire amendment — `BrokerRequestResponse` oneof

`shared/proto/rpc.proto` changes:

```proto
// New top-level response message replacing the bare BrokerResponse.
message BrokerRequestResponse {
  oneof outcome {
    SeederAssignment seeder_assignment = 1;
    NoCapacity       no_capacity       = 2;
    Queued           queued            = 3;
    Rejected         rejected          = 4;
  }
}

// Renamed from the existing BrokerResponse; same field shape.
message SeederAssignment {
  bytes seeder_addr        = 1;  // utf-8 host:port
  bytes seeder_pubkey      = 2;  // 32
  bytes reservation_token  = 3;  // 16 — equals request_id
}

// NoCapacity stays as today.
message NoCapacity {
  string reason = 1;
}

message Queued {
  bytes        request_id    = 1;  // 16
  PositionBand position_band = 2;
  EtaBand      eta_band      = 3;
}

message Rejected {
  RejectReason reason        = 1;
  uint32       retry_after_s = 2;
}

enum PositionBand {
  POSITION_BAND_UNSPECIFIED = 0;
  POSITION_BAND_1_TO_10     = 1;
  POSITION_BAND_11_TO_50    = 2;
  POSITION_BAND_51_TO_200   = 3;
  POSITION_BAND_200_PLUS    = 4;
}

enum EtaBand {
  ETA_BAND_UNSPECIFIED = 0;
  ETA_BAND_LT_30S      = 1;
  ETA_BAND_30S_TO_2M   = 2;
  ETA_BAND_2M_TO_5M    = 3;
  ETA_BAND_5M_PLUS     = 4;
}

enum RejectReason {
  REJECT_REASON_UNSPECIFIED        = 0;
  REJECT_REASON_REGION_OVERLOADED  = 1;
  REJECT_REASON_QUEUE_TIMEOUT      = 2;
}
```

`shared/proto/validate.go` gains `ValidateBrokerRequestResponse` enforcing exactly-one-of-set, per-variant invariants (`request_id` length 16; `retry_after_s ∈ [60, 600]`; banded enums non-`UNSPECIFIED`).

The existing top-level `BrokerResponse` message is **renamed** to `SeederAssignment` in the same PR. There is no v1 client deployed against the old name — `installBrokerRequest` is presently a `notImpl` stub and `trackerclient.BrokerRequest` is unwired into a real flow — so a clean rename is preferred over a parallel/deprecation path.

### 3.4 Plugin `trackerclient.BrokerRequest` update

`(*Client).BrokerRequest` returns a sum-typed result mirroring the oneof:

```go
type BrokerOutcome int

const (
    BrokerOutcomeAssignment BrokerOutcome = iota + 1
    BrokerOutcomeNoCapacity
    BrokerOutcomeQueued
    BrokerOutcomeRejected
)

type BrokerResult struct {
    Outcome    BrokerOutcome
    Assignment *SeederAssignment    // populated iff Assignment
    NoCap      *NoCapacityResult    // populated iff NoCapacity
    Queued     *QueuedResult        // populated iff Queued
    Rejected   *RejectedResult      // populated iff Rejected
}

func (c *Client) BrokerRequest(ctx context.Context, env *tbproto.EnvelopeSigned) (*BrokerResult, error)
```

The trackerclient round-trip test and the `fakeserver` handler shape are updated in the same PR.

### 3.5 Internal collaborators (Deps)

```go
type Deps struct {
    Logger     zerolog.Logger
    Now        func() time.Time
    Registry   RegistryService
    Ledger     LedgerService
    Admission  AdmissionService
    Reputation ReputationService
    Pusher     PushService
    Pricing    *PriceTable
    TrackerKey ed25519.PrivateKey
}

type RegistryService interface {
    Match(registry.Filter) []registry.SeederRecord
    Get(ids.IdentityID) (registry.SeederRecord, bool)
    IncLoad(ids.IdentityID) (int, error)
    DecLoad(ids.IdentityID) (int, error)
}

type LedgerService interface {
    Tip(ctx context.Context) (uint64, []byte, bool, error)
    AppendUsage(ctx context.Context, r ledger.UsageRecord) (*tbproto.Entry, error)
}

type AdmissionService interface {
    PopReadyForBroker(now time.Time, minServePriority float64) (admission.QueueEntry, bool)
}

type ReputationService interface {
    Score(ids.IdentityID) (float64, bool)         // ok=false → caller defaults to 0.5
    IsFrozen(ids.IdentityID) bool
    RecordOfferOutcome(seeder ids.IdentityID, outcome string) // advisory
}

type PushService interface {
    PushOfferTo(ids.IdentityID, *tbproto.OfferPush) (<-chan *tbproto.OfferDecision, bool)
    PushSettlementTo(ids.IdentityID, *tbproto.SettlementPush) (<-chan *tbproto.SettleAck, bool)
}
```

`*registry.Registry`, `*ledger.Ledger`, `*admission.Subsystem`, and `*server.Server` satisfy these unions structurally — the same convention used by `internal/api`.

`AdmissionService.PopReadyForBroker` is a new method added to `internal/admission` (§3.6).

### 3.6 Admission addition: `PopReadyForBroker`

```go
// In tracker/internal/admission, alongside Decide:
func (s *Subsystem) PopReadyForBroker(now time.Time, minServePriority float64) (QueueEntry, bool)
```

Returns the top queue entry if its `EffectivePriority(now) >= minServePriority`; pops and returns it, or returns `(_, false)` when the queue is empty or the head doesn't clear the bar. Implements admission-design §5.4 step 1's pop semantics.

The broker passes `minServePriority` based on current admission `pressure`:
```
minServePriority = max(0.0, 0.5 * (pressure - 1.0))
```
which the broker reads via a new advisory `admission.PressureGauge() float64` accessor.

## 4. Data structures

### 4.1 `Reservations` — in-memory credit reservation ledger

```go
type Reservations struct {
    mu    sync.Mutex
    byID  map[ids.IdentityID]uint64        // consumer → total reserved
    byReq map[[16]byte]reservationSlot
}

type reservationSlot struct {
    ConsumerID ids.IdentityID
    Amount     uint64
    ExpiresAt  time.Time
}
```

API:

| Method | Behavior |
|---|---|
| `Reserve(reqID, consumer, amount, snapshotCredits, expiresAt)` | Checks `snapshotCredits − byID[consumer] ≥ amount`. On success: debits, inserts slot. Returns `ErrInsufficientCredits` on shortfall, `ErrDuplicateReservation` if `reqID` already present. |
| `Release(reqID) (consumer, amount, ok)` | Removes the slot, decrements `byID`. Idempotent — repeat returns `ok=false`. |
| `Reserved(consumer) uint64` | Read-only total. |
| `Sweep(now) []reservationSlot` | Removes slots with `ExpiresAt ≤ now`; returns the dropped slots so the caller can log/metric. |

The reservation amount is *not* persisted across tracker restart. A tracker crash midway through an in-flight request will, on restart, see the consumer's ledger balance unchanged (no entry was ever appended) and accept new requests from them; in-flight reservations are lost, which is the correct semantic — the request itself is also lost, so there's nothing to settle.

### 4.2 `Inflight` — in-flight request state machine

```go
type State uint8

const (
    StateUnspecified State = iota
    StateSelecting
    StateAssigned
    StateServing
    StateCompleted
    StateFailed
)

type Request struct {
    RequestID       [16]byte
    ConsumerID      ids.IdentityID
    EnvelopeBody    *tbproto.EnvelopeBody
    EnvelopeHash    [32]byte                // sha256(DeterministicMarshal(body))
    MaxCostReserved uint64
    AssignedSeeder  ids.IdentityID          // zero until StateAssigned
    SeederPubkey    ed25519.PublicKey
    OfferAttempts   []ids.IdentityID
    StartedAt       time.Time
    State           State

    // Settlement coordination — populated on usage_report:
    PreimageHash    [32]byte
    SettleSig       chan []byte             // consumer's sig, dispatched by HandleSettle
    TerminatedAt    time.Time               // when entered Completed/Failed
}

type Inflight struct {
    mu     sync.RWMutex
    byID   map[[16]byte]*Request
    byHash map[[32]byte]*Request           // for HandleSettle's preimage_hash dispatch
}
```

Allowed state transitions:

```
SELECTING → ASSIGNED   (broker offer accepted)
SELECTING → FAILED     (no eligible seeders; max attempts exceeded; ctx cancel)
ASSIGNED  → SERVING    (usage_report received)
ASSIGNED  → FAILED     (reservation TTL expired before usage_report)
SERVING   → COMPLETED  (ledger entry appended)
SERVING   → FAILED     (ledger append failed; settlement timeout with append failure)
```

`Transition(id, from, to)` is a CAS — returns `ErrIllegalTransition` if the current state is not `from`. Concurrent callers race cleanly: exactly one wins, the loser sees the error. This is how settlement guards against double-finalize.

`Sweep(now, terminalTTL)` removes entries with `State ∈ {COMPLETED, FAILED}` and `TerminatedAt ≤ now − terminalTTL` (default 10 min per spec §4.2). Sweep also removes the corresponding `byHash` mapping.

### 4.3 `PriceTable` — model pricing

```go
type ModelPrices struct {
    InCreditsPerToken  uint64
    OutCreditsPerToken uint64
}

type PriceTable struct {
    byModel map[string]ModelPrices
}

func DefaultPriceTable() *PriceTable
func NewPriceTable(m map[string]ModelPrices) *PriceTable

func (p *PriceTable) MaxCost(env *tbproto.EnvelopeBody) (uint64, error)
func (p *PriceTable) ActualCost(model string, in, out uint32) (uint64, error)
```

`DefaultPriceTable` covers `claude-opus-4-7`, `claude-sonnet-4-6`, and `claude-haiku-4-5-20251001` with placeholder rates (1500 in / 7500 out per million tokens normalized to credits — the exact integers are operator-tunable and locked in `config.go` defaults). Operators override via a new `pricing:` config block (§9.1).

Both methods return `ErrUnknownModel` for absent models. The broker treats unknown models as a wire-validation failure surfaced to the consumer as `RPC_STATUS_INVALID` with code `UNKNOWN_MODEL`. For settlement (`ActualCost`), unknown model is structurally impossible (matched against the in-flight request); seeing it is an internal error.

### 4.4 Selector

#### 4.4.1 Filter

Built from the envelope and config:

```go
filter := registry.Filter{
    RequireAvailable: true,
    Model:            env.Body.Model,
    Tier:             env.Body.Tier,
    MinHeadroom:      cfg.HeadroomThreshold, // 0.2 default
    MaxLoad:          cfg.LoadThreshold,     // 5 default
}
```

After `registry.Match(filter)`, the broker further excludes:
- Seeders in `inflight.OfferAttempts` for the current request (don't re-offer).
- Seeders with `reputation.IsFrozen(seeder.id) == true`.

#### 4.4.2 Score

```
score = α · reputation_score
      + β · headroom_estimate
      − γ · rttMs
      − δ · load
```

Defaults from `config.BrokerScoreWeights`: `α=0.4, β=0.3, γ=0.2, δ=0.1`. `reputation_score` is `reputation.Score(seeder.id)` defaulting to `0.5` when reputation is unknown.

#### 4.4.3 RTT placeholder

`rttMs = 0` in v1. The γ term collapses; effective formula is `0.4·rep + 0.3·headroom − 0.1·load`. Open question §13.

#### 4.4.4 Tie-breaking

Stable sort by score descending, with `IdentityID` as the secondary key (lex byte order). Stable order makes the test suite deterministic.

### 4.5 Offer push round-trip

Per offer attempt:

1. `registry.IncLoad(seeder.id)` — bump before push so concurrent `Match` calls see the incremented load.
2. Build `OfferPush{consumer_id, envelope_hash, model, max_input_tokens, max_output_tokens}`.
3. `pusher.PushOfferTo(seeder.id, push)` — returns `(decisionCh, ok)`.
4. If `!ok`: `registry.DecLoad`, log `seeder_unreachable`, advance to next candidate.
5. `select { case dec := <-decisionCh: …; case <-ctx.Done(): …; case <-time.After(cfg.OfferTimeoutMs): … }`.
6. On `dec.Accept == false` or timeout: `registry.DecLoad`, `reputation.RecordOfferOutcome(seeder.id, reason)`, advance.
7. On `dec.Accept == true`: load stays incremented (held until settlement releases it via `DecLoad` in settlement's terminal path). Persist the seeder + ephemeral pubkey in `Inflight`; transition `SELECTING → ASSIGNED`.

The tracker-design §5.3 `offer_timeout_ms` (1500 ms) is enforced both by `server.PushOfferTo` and by the broker's `select` — defense in depth.

## 5. Algorithms

### 5.1 `Submit(env)` — happy path

Pre-condition: `internal/api` has fully validated the envelope and admission has returned `OutcomeAdmit`.

```
1. body := env.Body
2. requestID := uuidv4Random()
3. envHash := sha256(DeterministicMarshal(body))
4. cost, err := pricing.MaxCost(body)
   on ErrUnknownModel: return error → api/ → RPC_STATUS_INVALID code UNKNOWN_MODEL
5. expiresAt := now + cfg.Settlement.ReservationTTLS
6. err := reservations.Reserve(requestID, body.ConsumerId, cost,
                                body.BalanceProof.Body.Credits, expiresAt)
   on ErrInsufficientCredits:
     return Result{Outcome: NoCapacity,
                   NoCap: &NoCapacityDetails{Reason: "insufficient_credits"}}
7. inflight.Insert(&Request{State: SELECTING, …})
8. tried := []
9. for attempt := 0; attempt < cfg.MaxOfferAttempts; attempt++:
     candidates := selector.Pick(registry, body, weights, reputation, cfg, tried)
     if len(candidates) == 0:
       break
     seeder := candidates[0]
     accept, ephPub, err := offerLoop.runOne(ctx, seeder, body, envHash, cfg)
     if err != nil:
       continue
     if !accept:
       tried = append(tried, seeder.IdentityID)
       continue
     inflight.MarkSeeder(requestID, seeder.IdentityID, ephPub)
     inflight.Transition(SELECTING, ASSIGNED)
     return Result{
       Outcome: Admit,
       Admit: &Assignment{
         SeederAddr:       seeder.NetCoords.ExternalAddr.String(),
         SeederPubkey:     ephPub,
         ReservationToken: requestID[:],
         RequestID:        requestID,
       },
     }
10. // loop exhausted or no candidates
    reservations.Release(requestID)
    inflight.Fail(requestID)
    return Result{Outcome: NoCapacity,
                  NoCap: &NoCapacityDetails{Reason: "no_eligible_seeder"}}
```

Latency target: p50 < 50 ms / p99 < 200 ms when a suitable seeder exists (spec §11). Almost all the latency lives in the offer round-trip; broker-internal work is a few μs of map lookups and arithmetic.

### 5.2 `HandleUsageReport(report)` — settlement

```
1. req, ok := inflight.Get(report.RequestId)
   if !ok: return RPC_STATUS_NOT_FOUND
2. if req.State ∉ {ASSIGNED, SERVING}: return RPC_STATUS_INVALID code INVALID_STATE
3. if req.AssignedSeeder != peerID: return RPC_STATUS_INVALID code SEEDER_MISMATCH
4. if report.Model != req.EnvelopeBody.Model: return RPC_STATUS_INVALID code MODEL_MISMATCH
5. inflight.Transition(ASSIGNED, SERVING)  // idempotent if already SERVING (CAS may noop)
6. actualCost, err := pricing.ActualCost(report.Model, report.InputTokens, report.OutputTokens)
   on err: return RPC_STATUS_INTERNAL  // structurally impossible
7. if actualCost > req.MaxCostReserved + tolerance(5%):
     return RPC_STATUS_INVALID code COST_OVERSPEND
8. tip_seq, tip_hash, _, _ := ledger.Tip(ctx)
9. body := entry.BuildUsageEntry(entry.UsageInput{
     PrevHash:           tip_hash,
     Seq:                tip_seq + 1,
     ConsumerID:         req.ConsumerID,
     SeederID:           peerID,
     Model:              report.Model,
     InputTokens:        report.InputTokens,
     OutputTokens:       report.OutputTokens,
     CostCredits:        actualCost,
     Timestamp:          unixSeconds(now),
     RequestID:          report.RequestId,
     ConsumerSigMissing: false,
   })
10. preimageHash := sha256(DeterministicMarshal(body))
11. // verify seeder's claim — sig is over DeterministicMarshal(body):
    seederPub := req.SeederPubkey
    bodyBytes := DeterministicMarshal(body)
    if !ed25519.Verify(seederPub, bodyBytes, report.SeederSig):
      return RPC_STATUS_INVALID code SEEDER_SIG_INVALID
12. req.PreimageHash = preimageHash
    req.SettleSig    = make(chan []byte, 1)
    inflight.IndexByHash(req)              // for HandleSettle dispatch
13. push := &SettlementPush{PreimageHash: preimageHash[:], PreimageBody: bodyBytes}
    ackCh, ok := pusher.PushSettlementTo(req.ConsumerID, push)
    // !ok means consumer is not currently connected; the goroutine in
    // step 14 will fall through the timer and append with
    // ConsumerSigMissing=true.
14. spawn goroutine:
      timer := time.After(cfg.Settlement.SettlementTimeoutS * time.Second)
      select {
        case sig := <-req.SettleSig:
          // consumer counter-signed via Settle unary RPC
          appendUsage(req, body, sig, ConsumerSigMissing: false)
        case <-ackCh:
          // consumer accepted the push but sig arrives via Settle separately
          // (SettleAck has no sig field — see §3.3 of the wire spec)
          // continue waiting on req.SettleSig until timer.
        case <-timer:
          appendUsage(req, body, nil, ConsumerSigMissing: true)
        case <-ctx.Done():
          // tracker shutting down; reservation released by reaper on next pass
          return
      }
15. return UsageAck{} immediately to seeder.

// helper:
func appendUsage(req, body, sig, missing):
   _, err := ledger.AppendUsage(ledger.UsageRecord{
     PrevHash: body.PrevHash, Seq: body.Seq,
     ConsumerID: req.ConsumerID, SeederID: req.AssignedSeeder,
     Model: body.Model,
     InputTokens: body.InputTokens, OutputTokens: body.OutputTokens,
     CostCredits: body.CostCredits, Timestamp: body.Timestamp,
     RequestID: body.RequestID,
     ConsumerSigMissing: missing,
     ConsumerSig: sig, ConsumerPub: consumerPub,
     SeederSig: report.SeederSig, SeederPub: seederPubkey,
   })
   if err == ErrStaleTip:
     // contend with another in-flight settlement; rebuild preimage at fresh tip and retry
     retry up to cfg.Settlement.StaleTipRetries (default 3)
   if err != nil:
     inflight.Fail(req.RequestID)
     // reservation NOT released — operator drains via admin endpoint
     log.Error("ledger append failed")
     return
   reservations.Release(req.RequestID)
   registry.DecLoad(req.AssignedSeeder)
   inflight.Complete(req.RequestID)
```

`HandleUsageReport` returns `UsageAck` to the seeder *as soon as step 15* — before the consumer counter-sig arrives. This decouples the seeder's local liveness (its tracker connection) from the consumer's slower counter-sig path. The seeder doesn't need to wait for settlement to finish before tearing down its end of the tunnel.

### 5.3 `HandleSettle(req)` — consumer counter-sig dispatch

```
1. r, ok := inflight.LookupByHash(req.PreimageHash)
   if !ok: return RPC_STATUS_NOT_FOUND
2. // non-blocking send into the per-request 1-buffered channel
   select {
     case r.SettleSig <- req.ConsumerSig:  // delivered
     default:                              // already full → duplicate
       return RPC_STATUS_INVALID code DUPLICATE_SETTLE
   }
3. return SettleAck{}
```

The dispatcher map is keyed by `preimage_hash` because that's what the consumer sees in `SettlementPush`. The 1-buffered channel ensures the unary `Settle` returns immediately and the receiver goroutine in step 14 picks the sig up on its own schedule.

### 5.4 Queue drain consumer

A goroutine owned by `*Broker`, started in `Open`, ticking on `time.NewTicker(cfg.Broker.QueueDrainIntervalMs)` (default 1000 ms — operator-tunable knob added to `config.BrokerConfig` for this PR):

```
on each tick:
  pressure := admission.PressureGauge()
  minServePriority := max(0.0, 0.5 * (pressure - 1.0))
  for {
    entry, ok := admission.PopReadyForBroker(now, minServePriority)
    if !ok: break
    cached := pendingQueued[entry.RequestID]
    if cached == nil: continue   // stale; admission TTL evicted but our cache lost it
    delete(pendingQueued, entry.RequestID)
    result, _ := broker.Submit(ctx, cached.envelope)
    cached.deliver(result)       // unblocks the api/ goroutine holding the RPC stream
  }
```

The `pendingQueued` map is populated by `RegisterQueued`, called from `api/installBrokerRequest` when admission returns `OutcomeQueue`. The api handler blocks on a `chan *Result` until either (a) the drain delivers, or (b) admission's queue timeout fires and admission emits a `Reject{QueueTimeout}` which api converts to a wire `Rejected{queue_timeout}`.

### 5.5 Reservation TTL reaper

Owned by `*Settlement`, started in `Open`, ticking every 30 s:

```
on each tick:
  expired := reservations.Sweep(now)
  for _, slot := range expired {
    req, ok := inflight.Get(slot.RequestID)  // by request_id == reservation key
    if ok && req.State ∈ {SELECTING, ASSIGNED}:
      inflight.Fail(req.RequestID)
      registry.DecLoad(req.AssignedSeeder)  // ASSIGNED → load was held
    metric: broker_reservation_ttl_expired_total
  }
```

A reservation whose request reached `SERVING` but ledger-append failed is **not** TTL-reaped — operator intervention via `/admin/broker/reservations/release/<request_id>` is the recovery path (§10).

## 6. Concurrency model

- **No global lock.** `Reservations` and `Inflight` each have their own `sync.Mutex` / `sync.RWMutex`. Selection reads the registry's per-shard locks via `Match`, no broker-side locking.
- **Inflight CAS.** All state transitions go through `Transition(from, to)`; concurrent settle vs. timeout-reaper races are won by exactly one side.
- **Per-request goroutine.** Each `HandleUsageReport` invocation spawns one goroutine for the consumer-sig wait. Goroutine count bounded by the cap on concurrent requests-per-tracker (≤10³ per spec §6).
- **Ledger writes are serialized** by `*ledger.Ledger`'s internal mutex; the broker hands off and waits.
- **Settlement timer + ctx cancel** are racy by design — both can fire; whichever gets the lock first wins via `inflight.Transition(SERVING, COMPLETED)` CAS. The other side's append silently no-ops on the CAS error.
- **Race detector mandatory.** `tracker/internal/broker` joins `internal/admission`, `internal/federation` on the always-`-race` list (tracker CLAUDE.md, spec §6).

Capacity at the design target VM (2 vCPU, 4 GB):
- `Submit` p50 < 50 ms / p99 < 200 ms (§11.1).
- 100 concurrent in-flight requests with no leak.
- Reservation map ≤ 10⁵ entries at worst case → ~3 MB RSS.

## 7. Failure handling

| # | Failure mode | Detection | Response |
|---|---|---|---|
| 7.1 | Envelope sig invalid / proto malformed | api/ pre-call to `signing.VerifyEnvelope` | `RPC_STATUS_INVALID` from api/, broker not invoked |
| 7.2 | Balance proof expired (>600 s) or wrong tracker sig | api/ | `RPC_STATUS_INVALID` code `BALANCE_PROOF_STALE` |
| 7.3 | Exhaustion proof invalid | api/ | `RPC_STATUS_INVALID` code `EXHAUSTION_PROOF_INVALID`; api emits reputation event |
| 7.4 | Unknown model | broker `pricing.MaxCost` | `RPC_STATUS_INVALID` code `UNKNOWN_MODEL` |
| 7.5 | Insufficient credits after reserved-debit check | broker `Reservations.Reserve` | `Result{NoCapacity, "insufficient_credits"}` |
| 7.6 | No eligible seeders (filter empty) | broker selector | `Result{NoCapacity, "no_eligible_seeder"}` |
| 7.7 | All offer attempts rejected/timeout | broker offer loop | `Result{NoCapacity, "all_seeders_rejected"}` |
| 7.8 | Seeder accepts then disconnects | not detectable until reservation TTL | Reaper → `inflight.Fail`; `registry.DecLoad`; metric `seeder_post_accept_disconnect_total` |
| 7.9 | usage_report for unknown request_id | settlement step 1 | `RPC_STATUS_NOT_FOUND` |
| 7.10 | usage_report from wrong seeder | settlement step 3 | `RPC_STATUS_INVALID` code `SEEDER_MISMATCH` |
| 7.11 | usage_report cost > reserved + 5% | settlement step 7 | `RPC_STATUS_INVALID` code `COST_OVERSPEND` |
| 7.12 | usage_report seeder sig invalid | settlement step 11 | `RPC_STATUS_INVALID` code `SEEDER_SIG_INVALID` |
| 7.13 | Consumer counter-sig timeout | settlement step 14 timer | Append entry with `ConsumerSigMissing=true`; reservation released; metric `broker_consumer_sig_missing_total` |
| 7.14 | `Settle` for unknown preimage_hash | `HandleSettle` step 1 | `RPC_STATUS_NOT_FOUND` |
| 7.15 | Duplicate `Settle` for same preimage | `HandleSettle` channel full | `RPC_STATUS_INVALID` code `DUPLICATE_SETTLE` |
| 7.16 | Ledger append fails | `appendUsage` helper in §5.2 | `inflight.Fail`; reservation NOT released; alert via metric `broker_ledger_append_failure_total`; operator drains |
| 7.17 | Stale tip during ledger append | `ErrStaleTip` from ledger | Rebuild preimage at fresh tip, retry up to `StaleTipRetries` (default 3); on exhaustion → 7.16 |
| 7.18 | Reservation TTL expires before settlement | reaper | `inflight.Fail`; `registry.DecLoad` if assigned |
| 7.19 | Subsystem closing during in-flight request | ctx cancel | Cancel offer loops; reaper releases reservations on next tick; consumer sees RPC error |
| 7.20 | Admission queue timeout for queued envelope | admission emits `Reject{QueueTimeout}` | api/ converts to wire `Rejected{queue_timeout}`; broker drops `pendingQueued[requestID]` |

Two design invariants enforced:

1. **No reservation leaks under happy or single-failure paths.** Every `Reserve` is paired with a `Release` via settlement, broker selection failure, or TTL reaper. Multiple-failure paths (e.g., 7.16) require operator intervention by design.
2. **No double settlement.** `Inflight.Transition(SERVING, COMPLETED)` CAS guarantees exactly one ledger append per request; concurrent settle attempts no-op on the CAS error.

## 8. Security model

### 8.1 Forged offer acceptance

Threat: a malicious peer impersonates a seeder and accepts an offer they cannot serve, denying capacity to the consumer.

Defense: `PushOfferTo` is dispatched only to the connection registered for the seeder's authenticated identity (`server.connsByPeer[id]`); spoofing requires breaking the connection-level mTLS / QUIC handshake. `OfferDecision.ephemeral_pubkey` is then handed to the consumer for the tunnel handshake; if the seeder cannot prove possession of the matching private key, the consumer's tunnel setup fails and broker observes 7.8.

### 8.2 Cost overspending

Threat: a colluding seeder reports inflated `input_tokens` / `output_tokens` to drain the consumer's balance.

Defense: §5.2 step 7 caps `actualCost` at `MaxCostReserved + 5%`. The 5% tolerance accounts for last-token rounding; >5% is treated as adversarial. Repeated overspend attempts feed reputation via `RecordOfferOutcome("overspend")`.

### 8.3 Reservation denial-of-service

Threat: an adversary submits envelopes that pass admission but get queued or no-capacity'd, holding reservations hostage.

Defense: `Reservations.Reserve` is *only* called on the admit path (step 6); queued envelopes hold no reservation until they're dequeued. No-capacity returns release the reservation in step 10. The TTL reaper (default 1200 s) is the backstop for any case where the broker's release path is bypassed (e.g., 7.18).

### 8.4 Settle replay

Threat: an adversary replays a captured `Settle{preimage_hash, sig}` to re-finalize a request.

Defense: the `Inflight.byHash` map drops the entry on `inflight.Complete`; replays return `RPC_STATUS_NOT_FOUND` (7.14). The 10-min terminal-state retention does not include `byHash` indexing past completion.

### 8.5 Information leakage via offer pushes

Threat: a seeder learns identifying information about the consumer or their request beyond what's needed to accept/reject.

Defense: `OfferPush` carries `consumer_id` (32-byte hash, no PII), `envelope_hash` (opaque), `model`, and the `max_input_tokens`/`max_output_tokens` ceilings. The seeder cannot read the prompt body — that crosses the consumer↔seeder tunnel only after offer acceptance. Existing wire-format validation already enforces this shape.

### 8.6 Unauthenticated `usage_report`

Threat: an adversary submits a forged usage report claiming to be the assigned seeder.

Defense: §5.2 step 3 checks `req.AssignedSeeder == peerID` where `peerID` is from the connection's authenticated identity. §5.2 step 11 verifies the seeder's Ed25519 signature over the entry preimage. Both must pass before the consumer is asked to counter-sign.

## 9. Configuration

### 9.1 New `pricing:` config block

```yaml
pricing:
  models:
    claude-opus-4-7:
      in_credits_per_token:  15
      out_credits_per_token: 75
    claude-sonnet-4-6:
      in_credits_per_token:  3
      out_credits_per_token: 15
    claude-haiku-4-5-20251001:
      in_credits_per_token:  1
      out_credits_per_token: 5
```

`config.PricingConfig` lives alongside `BrokerConfig` and `SettlementConfig`. `DefaultConfig` populates the same three rows. `Validate` rejects empty model names, zero rates, and a fully empty table. Cross-cutting amendment to the config-design spec.

### 9.2 New broker config knobs

Added to existing `config.BrokerConfig`:

```yaml
broker:
  # … existing …
  queue_drain_interval_ms: 1000
  inflight_terminal_ttl_s: 600     # how long Completed/Failed entries linger for diagnostics
```

Added to existing `config.SettlementConfig`:

```yaml
settlement:
  # … existing …
  stale_tip_retries: 3
```

Validation:
- `queue_drain_interval_ms` ∈ [100, 60_000].
- `inflight_terminal_ttl_s` ≥ 60.
- `stale_tip_retries` ∈ [0, 10].

## 10. Operator interface

Admin HTTP endpoints under `/broker/*` (tracker spec §8 internal port):

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/broker/inflight` | List in-flight requests (request_id, consumer_id_redacted, state, age) |
| `GET` | `/broker/inflight/<request_id>` | Per-request detail (state, seeder, reservation amount) |
| `GET` | `/broker/reservations` | Per-consumer reserved totals + slot details |
| `POST` | `/broker/reservations/release/<request_id>` | Operator-forced reservation release after a 7.16 ledger failure |
| `POST` | `/broker/inflight/fail/<request_id>` | Operator-forced inflight `Fail` transition |

All `POST` endpoints emit a structured audit log line with operator identity + parameters.

### 10.1 Prometheus metrics

**Hot-path:**
- `broker_submit_decisions_total{result}` — result ∈ admit / no_capacity_reason
- `broker_submit_duration_seconds` (histogram)
- `broker_offer_attempts_total{outcome}` — outcome ∈ accept / reject / timeout / unreachable
- `broker_inflight_count` (gauge)

**Reservations:**
- `broker_reservations_active` (gauge, sum of byID totals)
- `broker_reservations_active_count` (gauge, count of slots)
- `broker_reservation_ttl_expired_total`

**Settlement:**
- `broker_settlement_decisions_total{result}` — result ∈ ok / consumer_sig_missing / cost_overspend / sig_invalid / ledger_failed
- `broker_settlement_duration_seconds` (histogram, from usage_report receipt to ledger append)
- `broker_consumer_sig_missing_total`
- `broker_ledger_append_failure_total`
- `broker_stale_tip_retries_total`

**Operational:**
- `broker_queue_drain_pops_total`
- `broker_queue_drain_admit_outcomes_total{outcome}`
- `broker_seeder_post_accept_disconnect_total`

## 11. Acceptance criteria

The implementation is complete when:

**Functional:**
1. `Submit` happy path returns `Result{Admit}` with a populated `*Assignment` for an envelope whose admission outcome is `OutcomeAdmit` and at least one eligible seeder exists.
2. `Submit` returns `Result{NoCapacity, "insufficient_credits"}` when `body.balance_proof.credits − reservations.Reserved(consumer) < max_cost`.
3. `Submit` returns `Result{NoCapacity, "no_eligible_seeder"}` when the registry has no candidates passing the filter.
4. `Submit` returns `Result{NoCapacity, "all_seeders_rejected"}` after `MaxOfferAttempts` rejected offers.
5. `HandleUsageReport` builds the entry preimage, pushes `SettlementPush` to the consumer, and on consumer counter-sig calls `ledger.AppendUsage` with `ConsumerSigMissing=false`.
6. `HandleUsageReport` falls back to `ConsumerSigMissing=true` after `Settlement.SettlementTimeoutS` without a consumer sig.
7. Admission `Queue` outcome leads to `Queued` wire response; queue drain re-enters `Submit` and delivers the eventual `Admit` result via the originally-blocked RPC stream.
8. Admission `Reject` outcome leads to `Rejected` wire response with banded `retry_after_s ∈ [60, 600]`.

**Persistence + recovery:**
9. Tracker restart drops all in-flight reservations (in-memory only); ledger entries from completed settlements survive intact in SQLite.
10. Ledger-append failure leaves reservation held; admin `/broker/reservations/release/<request_id>` releases.

**Performance** (tracker spec §6 reference VM: 2 vCPU, 4 GB RAM):
11. `broker_submit_duration_seconds` p50 < 50 ms / p99 < 200 ms when a suitable seeder exists.
12. 100 concurrent `Submit` calls leave `Reservations.byID` empty after all settlements complete.
13. `broker_settlement_duration_seconds` p99 < 1 s for the in-process portion (excluding the consumer-sig wait).

**Security:**
14. Forged `usage_report` with mismatched seeder identity rejected with `SEEDER_MISMATCH`.
15. Forged seeder signature over preimage rejected with `SEEDER_SIG_INVALID`.
16. `Settle` replay after completion returns `RPC_STATUS_NOT_FOUND`.
17. Cost overspend > 5% rejected with `COST_OVERSPEND`.

**Race-detector-clean:** `go test -race ./tracker/internal/broker/...` passes. `internal/broker` joins `internal/admission`, `internal/federation` on the tracker CLAUDE.md "always run with `-race`" list (already present per tracker spec §6).

## 12. Cross-cutting amendments shipped by this plan

Five upstream-spec amendments, all in the same PR as the broker implementation so the repo is never internally inconsistent.

1. **`shared/proto/rpc.proto`** — rename `BrokerResponse` → `SeederAssignment`; add `BrokerRequestResponse` (oneof), `Queued`, `Rejected`, and enums `PositionBand`, `EtaBand`, `RejectReason`. New validation helper `ValidateBrokerRequestResponse`. (No deployed clients exist for the old name.)
2. **`shared/proto/rpc.proto`** — keep `OfferPush`, `OfferDecision`, `SettlementPush`, `SettleRequest`, `SettleAck`, `UsageReport`, `UsageAck` as-is; broker integrates with the existing shapes.
3. **`tracker/internal/admission`** — add `PopReadyForBroker(now, minServePriority) (QueueEntry, bool)` and `PressureGauge() float64` accessors. No behavioral change to admission's hot path.
4. **`tracker/internal/config`** — new `PricingConfig` section; new `Broker.QueueDrainIntervalMs` and `Broker.InflightTerminalTTLS` knobs; new `Settlement.StaleTipRetries` knob. `DefaultConfig` populated; `Validate` rules added per §9.2. Config-design spec updated.
5. **`plugin/internal/trackerclient.BrokerRequest`** — return type changes from `*BrokerResponse` to a sum-typed `*BrokerResult` mirroring the new oneof. Plugin tracker-client design spec updated. All in-tree callers updated in the same PR.

6. **`tracker/internal/api`** — `installBrokerRequest` swapped from `notImpl` to a real handler that gates through `admission.Decide` and forwards admit results to `broker.Submit`; `installUsageReport` and `installSettle` similarly wired to `Settlement.HandleUsageReport` / `Settlement.HandleSettle`. `api.Deps` gains `Settlement SettlementService`; `api.BrokerService` typed against the new `*broker.Result`. `cmd/run_cmd` constructs `broker.Subsystems` and threads both into `api.Deps` + `server.Deps`.

## 13. Open questions

- **RTT estimation.** The `γ·rtt` term is dead in v1. Plausible v2 sources: the STUN reflection round-trip, geo-IP /16 distance heuristic, or measured-RTT samples piggybacked on heartbeats. Calibration depends on real deployment data.
- **Tunnel-failure feedback path.** Today the broker only learns about a failed seeder↔consumer tunnel via reservation TTL. A consumer-initiated `tunnel_failed(request_id, reason)` notification would let the broker fail fast and feed reputation. Logged for v2.
- **Reservation persistence across restart.** v1 drops all in-flight reservations on tracker crash. For long-running consumers, the lost balance-hold could over-admit on restart. Persisting reservations in a write-through file (similar to admission's tlog) is a v2 option.
- **Pricing source of truth.** The default `PriceTable` rates are operator-configurable but have no federated agreement mechanism. Cross-region settlements assume each region's tracker uses the same rates; divergence causes reconciliation pain. Consider gossiping a pricing schedule via federation.
- **Queue-drain back-pressure.** Polling at 1 Hz is fine at v1 capacity; at higher throughput, an event-driven push from admission's queue would lower idle latency. Profile after deployment.

## 14. Future work

- Settle-ack piggyback on next `broker_request` (admission-design §12 — shaves a per-prompt round-trip).
- Operator-review automation for repeated `consumer_sig_missing` patterns (reputation feedback hook).
- Adaptive `MaxOfferAttempts` based on regional offer-accept rate.
- Per-model headroom propagation through the selector (admission already collects per-model headroom; broker currently uses the aggregate field).
- Bandwidth credits for TURN-relay flows (root spec §9 open item).
- Persistence of the in-flight registry for restart-survivability of long-running requests.
