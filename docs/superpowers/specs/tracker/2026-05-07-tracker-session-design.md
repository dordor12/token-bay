# `tracker/internal/session` — Subsystem Design Spec

| Field | Value |
|---|---|
| Parent | [Regional Tracker](2026-04-22-tracker-design.md) |
| Sibling | [Broker](2026-05-07-tracker-broker-design.md) |
| Status | Design draft |
| Date | 2026-05-07 |
| Scope | Carve out the per-request lifecycle state — `Inflight` (state machine), `Reservations` (in-memory credit ledger), shared lifecycle errors — from the broker spec into a separate Go package `tracker/internal/session`. The broker spec retains selection (`*Broker.Submit`) and settlement (`*Settlement.HandleUsageReport` / `HandleSettle`) but consumes the lifecycle state from `session/` instead of owning it. |

## 1. Purpose

The 2026-05-07 broker spec packages two sibling subsystems (`*Broker`, `*Settlement`) and the state they share (`*Inflight`, `*Reservations`) in a single Go package. That state has two callers (broker writes during selection; settlement writes during finalization) and a third lifecycle-only goroutine (the TTL reaper) — three consumers of one piece of state, which is exactly the seam where a separate package belongs.

`tracker/internal/session` owns that state. Its only job is to be the authoritative, race-clean store of in-flight broker requests and their associated credit reservations, plus the typed sweep helpers the broker uses to drive lifecycle goroutines. It does **not** import `internal/registry`, `internal/ledger`, `internal/admission`, or any wire type from `shared/proto/rpc.proto` beyond the `EnvelopeBody` carried as request metadata.

## 2. Non-goals

- **Selection.** That stays in `broker/Submit` per the broker spec §5.1.
- **Settlement.** That stays in `broker/HandleUsageReport` + `broker/HandleSettle` per the broker spec §5.2–§5.3.
- **Goroutine ownership.** `session` exposes `Sweep…` helpers; the broker's reaper goroutine drives them. Session does not start, stop, or hold timers.
- **Pricing.** `PriceTable` stays in `broker/`. Session is state, not economics.
- **Wire types.** The `Result` / `Outcome` / `Assignment` types stay in `broker/` — they describe broker outcomes, not session state.
- **Registry / load tracking.** Session never calls `registry.IncLoad` / `DecLoad`. Session tells broker which seeder was assigned at the moment of expiry; broker calls registry.

## 3. Public Go surface

```go
package session

// State is the in-flight request lifecycle. Mirrors broker spec §4.2.
type State uint8

const (
    StateUnspecified State = iota
    StateSelecting
    StateAssigned
    StateServing
    StateCompleted
    StateFailed
)

// Request is the per-broker-request record. Settlement coordination fields
// (PreimageHash, SettleSig, TerminatedAt) live alongside selection fields so
// both subsystems mutate one struct under one set of locks.
type Request struct {
    RequestID       [16]byte
    ConsumerID      ids.IdentityID
    EnvelopeBody    *tbproto.EnvelopeBody
    EnvelopeHash    [32]byte
    MaxCostReserved uint64
    AssignedSeeder  ids.IdentityID
    SeederPubkey    ed25519.PublicKey
    OfferAttempts   []ids.IdentityID
    StartedAt       time.Time
    State           State

    PreimageHash [32]byte
    SettleSig    chan []byte
    TerminatedAt time.Time
}

// Inflight is the in-memory request store. CAS-based transitions guarantee
// at-most-one settlement per request under concurrent contention.
type Inflight struct{ /* unexported */ }

func NewInflight() *Inflight

func (f *Inflight) Insert(r *Request)
func (f *Inflight) Get(id [16]byte) (*Request, bool)
func (f *Inflight) Transition(id [16]byte, from, to State) error  // ErrIllegalTransition / ErrUnknownRequest
func (f *Inflight) MarkSeeder(id [16]byte, seeder ids.IdentityID, pub ed25519.PublicKey) error
func (f *Inflight) IndexByHash(id [16]byte, h [32]byte) error
func (f *Inflight) LookupByHash(h [32]byte) (*Request, bool)
func (f *Inflight) SweepTerminal(now time.Time, ttl time.Duration) []*Request

// Reservation is one slot in the in-memory credit reservation ledger.
type Reservation struct {
    ReqID      [16]byte
    ConsumerID ids.IdentityID
    Amount     uint64
    ExpiresAt  time.Time
}

type Reservations struct{ /* unexported */ }

func NewReservations() *Reservations

func (r *Reservations) Reserve(reqID [16]byte, consumer ids.IdentityID, amount, snapshotCredits uint64, expiresAt time.Time) error
func (r *Reservations) Release(reqID [16]byte) (ids.IdentityID, uint64, bool)
func (r *Reservations) Reserved(consumer ids.IdentityID) uint64
func (r *Reservations) SweepExpired(now time.Time) []Reservation

// Manager bundles the two stores. Constructed once by broker.Open and
// threaded into both *Broker and *Settlement.
type Manager struct {
    Inflight     *Inflight
    Reservations *Reservations
}

func New() *Manager

// Expiry is what the reaper sees: a reservation slot that has expired
// together with the *current* state of the matching Inflight request and
// (if assigned) the seeder whose load needs decrementing. Returned by
// SweepExpiredAndFail; the caller (broker reaper) drives the registry-side
// compensations.
type Expiry struct {
    ReqID          [16]byte
    ConsumerID     ids.IdentityID
    Amount         uint64
    PriorState     State
    AssignedSeeder ids.IdentityID  // zero unless PriorState == StateAssigned
}

// SweepExpiredAndFail does, atomically per slot:
//   1. drop the reservation slot
//   2. read the matching Request's current state
//   3. if state ∈ {StateSelecting, StateAssigned}, transition to StateFailed
// Returns the typed expiry records so the caller can run side effects
// (DecLoad, metrics, audit log).
func (m *Manager) SweepExpiredAndFail(now time.Time) []Expiry
```

## 4. Errors

```go
var (
    ErrInsufficientCredits   = errors.New("session: insufficient credits")
    ErrDuplicateReservation  = errors.New("session: duplicate reservation")
    ErrIllegalTransition     = errors.New("session: illegal state transition")
    ErrUnknownRequest        = errors.New("session: unknown request")
)
```

These are the four lifecycle errors the broker spec previously declared in `broker/errors.go`. The broker package keeps the broker-specific errors (`ErrUnknownModel`, `ErrSeederMismatch`, `ErrModelMismatch`, `ErrCostOverspend`, `ErrSeederSigInvalid`, `ErrUnknownPreimage`, `ErrDuplicateSettle`).

## 5. State machine invariants

Identical to broker spec §4.2. Repeated here so this spec is self-contained:

```
SELECTING → ASSIGNED   (broker offer accepted)
SELECTING → FAILED     (no eligible seeders; max attempts exceeded; ctx cancel)
ASSIGNED  → SERVING    (usage_report received)
ASSIGNED  → FAILED     (reservation TTL expired before usage_report)
SERVING   → COMPLETED  (ledger entry appended)
SERVING   → FAILED     (ledger append failed)
```

`Transition` is the only state mutator. Concurrent callers race cleanly: exactly one wins, the loser sees `ErrIllegalTransition`. This guarantees at-most-one settlement per request.

## 6. Concurrency model

- `Inflight.mu sync.RWMutex` guards `byID` + `byHash`.
- `Reservations.mu sync.Mutex` guards `byID` + `byReq`.
- `Manager.SweepExpiredAndFail` takes Reservations' lock first, then per-expired-slot grabs Inflight's lock for the CAS. No nested lock acquisitions — the two locks are independent.
- All public methods are race-clean under `-race`. Per the tracker CLAUDE.md, `tracker/internal/broker` and `tracker/internal/admission` are "always run with `-race`"; `tracker/internal/session` joins them on that list because broker and admission both consume it.

## 7. Cross-cutting amendments to the broker spec

The 2026-05-07 broker spec is amended in the same PR that lands the session refactor:

1. **§3.2 (Composite opener) and §3.5 (Deps).** `Open` now takes `session.Manager` (or `session.New()` is called internally). Subsystems hold a `*session.Manager` instead of separate `*Inflight` / `*Reservations` fields. The broker spec is updated to import `tracker/internal/session` and use `session.Inflight`, `session.Reservations`, `session.Request`, `session.State*`, `session.Err*`.
2. **§4.1, §4.2.** Sections describing `Reservations` and `Inflight` data structures get a one-line redirect: "Now lives in `tracker/internal/session`. Public surface mirrored here." The detailed type definitions move to this spec.
3. **§5.5 (Reservation TTL reaper).** Algorithm rewritten to call `session.Manager.SweepExpiredAndFail(now)` and iterate returned `Expiry` records to call `Registry.DecLoad` for each `AssignedSeeder` that's non-zero. The state CAS to `StateFailed` happens inside `session`; the registry-touching step happens in broker.
4. **Acceptance criteria §11.** Performance and race-clean criteria pass through to session unchanged.
5. **`tracker/CLAUDE.md`.** `internal/session` joins the `-race` mandatory list.

## 8. Failure handling

Same as the broker spec failure modes 7.1–7.20, except 7.18 (reservation TTL expires before settlement) is now jointly owned: `session.SweepExpiredAndFail` performs the state CAS and the slot drop; broker's reaper performs the `Registry.DecLoad` and emits metrics. Operator-forced reservation release (broker spec §10) routes through `session.Reservations.Release` from broker's admin handler.

## 9. Acceptance criteria

The session extraction is complete when:

1. `tracker/internal/session` package exists with the public surface in §3 and §4, race-clean under `go test -race ./...`.
2. `tracker/internal/broker` no longer defines `Inflight`, `Reservations`, `Request`, `State`, `Reservation`, `ErrInsufficientCredits`, `ErrDuplicateReservation`, `ErrIllegalTransition`, `ErrUnknownRequest`. `grep` confirms.
3. `tracker/internal/broker` imports `tracker/internal/session` and uses the moved types via the `session.` prefix.
4. The broker subsystem's external behavior is byte-identical to its pre-refactor behavior. The full broker test suite (Phases 4–9 of the broker plan) passes unchanged after import rewrites.
5. The broker spec sections §3.2, §3.5, §4.1, §4.2, §5.5 carry the amendment notes from §7 above.
6. `tracker/CLAUDE.md` lists `internal/session` on the always-`-race` list.

## 10. Open questions

- **Inflight terminal TTL sweep.** The broker spec §4.2 calls for `Inflight.Sweep(now, terminalTTL)` but the broker plan (Task 19) only reaps reservations. The session spec exposes `Inflight.SweepTerminal` as the public method; whether broker drives it from the reaper goroutine alongside `SweepExpiredAndFail` or from a dedicated ticker is a broker-implementation decision deferred to plan-execution time.
- **Manager vs. two-struct API.** This spec exposes both: callers can pass `*session.Manager` for ergonomic single-arg construction or grab `mgr.Inflight` / `mgr.Reservations` directly. Final shape may collapse to one form once the broker code lands.
