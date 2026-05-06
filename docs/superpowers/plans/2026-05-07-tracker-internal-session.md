# Tracker Internal Session — Refactor Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
>
> **Sequencing:** This plan applies **after** `docs/superpowers/plans/2026-05-07-tracker-broker.md` is merged. Every task here assumes the broker plan's end state as its starting point. If the broker plan is not yet merged, stop and merge it first — this plan rewrites broker source and the broker plan will not apply cleanly afterwards.

**Goal:** Extract the per-request lifecycle state (`Inflight`, `Reservations`, `Request`, `State`, four lifecycle errors) and the reservation-sweep helper from `tracker/internal/broker` into a new `tracker/internal/session` package. Broker keeps selection, settlement, queue drain, pricing, admin, and metrics — all of those import `session` for the lifecycle types.

**Architecture after this plan:**
- `tracker/internal/session` — `Inflight`, `Reservations`, `Request`, `State`, `Manager`, `Expiry`, lifecycle errors, `SweepExpiredAndFail`. Pure state package: no registry / ledger / admission / push imports. Always-`-race`.
- `tracker/internal/broker` — `*Broker` (Submit, queue drain), `*Settlement` (HandleUsageReport, HandleSettle, reaper *goroutine*), `Subsystems` (composite Open), `PriceTable`, `Selector`, `OfferLoop`, `Result`/`Outcome`/`Assignment` types, broker-specific errors, admin HTTP, metrics. Imports `tracker/internal/session`.

**Tech Stack:** unchanged from the broker plan. Go 1.25, stdlib `crypto/ed25519`, `crypto/sha256`, `github.com/google/uuid`, `github.com/rs/zerolog`, `github.com/stretchr/testify`.

---

## Reference

- Spec: `docs/superpowers/specs/tracker/2026-05-07-tracker-session-design.md`
- Sibling spec (amended by this plan): `docs/superpowers/specs/tracker/2026-05-07-tracker-broker-design.md`
- Sibling plan (prerequisite): `docs/superpowers/plans/2026-05-07-tracker-broker.md`
- Tracker root spec: `docs/superpowers/specs/tracker/2026-04-22-tracker-design.md`

## File map

```
tracker/internal/session/                                          -- NEW package
  doc.go                                                           -- new
  CLAUDE.md                                                        -- new (small package overview)
  errors.go                                                        -- new (4 lifecycle errors moved from broker/)
  errors_test.go                                                   -- new (parity check)
  request.go                                                       -- new (Request struct, State enum, allowed transitions table)
  request_test.go                                                  -- new
  inflight.go                                                      -- new (moved from broker/inflight.go)
  inflight_test.go                                                 -- new (moved from broker/inflight_test.go)
  reservations.go                                                  -- new (moved from broker/reservation.go, renamed for plurality + clarity)
  reservations_test.go                                             -- new (moved from broker/reservation_test.go)
  manager.go                                                       -- new (Manager, New, SweepExpiredAndFail, Expiry)
  manager_test.go                                                  -- new

tracker/internal/broker/                                            -- modified: drop moved files, rewrite imports
  errors.go                                                        -- modify (drop the four moved errors; keep broker-specific ones)
  errors_test.go                                                   -- modify (drop moved-error tests)
  inflight.go                                                      -- delete (moved to session)
  inflight_test.go                                                 -- delete (moved to session)
  reservation.go                                                   -- delete (moved to session/reservations.go)
  reservation_test.go                                              -- delete (moved to session/reservations_test.go)
  reaper.go                                                        -- modify (replace direct Sweep with session.Manager.SweepExpiredAndFail; keep ticker + DecLoad-by-Expiry)
  reaper_test.go                                                   -- modify (drive via session.Manager fakes)
  broker.go                                                        -- modify (s/broker.Inflight/session.Inflight/, s/broker.Reservations/session.Reservations/, etc.; OpenBroker now takes *session.Manager)
  broker_test.go                                                   -- modify (imports + constructor calls)
  selector.go                                                      -- modify if Selector references Request/State (probably not; Selector reads registry.SeederRecord)
  selector_test.go                                                 -- modify if needed
  offer_loop.go                                                    -- modify if it references Request fields directly
  offer_loop_test.go                                               -- modify if needed
  queue_drain.go                                                   -- modify (no Request mutations directly; no change expected beyond import sweep)
  queue_drain_test.go                                              -- modify (imports)
  settlement.go                                                    -- modify (s/Inflight/session.Inflight/, s/Reservations/session.Reservations/; OpenSettlement takes *session.Manager; reaper hookup unchanged in this file)
  settlement_test.go                                               -- modify (imports)
  subsystems.go                                                    -- modify (Open constructs *session.Manager once; threads to OpenBroker + OpenSettlement)
  subsystems_test.go                                               -- modify (assert shared Manager identity across subsystems)
  admin.go                                                         -- modify if admin handlers touch Inflight/Reservations directly (they do per broker spec §10)
  admin_test.go                                                    -- modify if needed
  metrics.go                                                       -- no change (metrics names are independent of state package)
  race_test.go                                                     -- modify (imports + constructor)
  deps.go                                                          -- no change (Deps unions unchanged)

tracker/internal/api/                                              -- modify: imports only (api references broker.Result, not session types)
  iface_check_test.go                                              -- no change (still asserts *broker.Broker / *broker.Settlement satisfy unions)

tracker/CLAUDE.md                                                  -- modify (add internal/session to always-`-race` list)

docs/superpowers/specs/tracker/
  2026-05-07-tracker-broker-design.md                              -- modify (amendment notes per session spec §7)
  2026-05-07-tracker-session-design.md                             -- already created (no change)
```

---

## Phase 1 — Session package scaffold

### Task 1: Create the package skeleton

**Files:**
- Create: `tracker/internal/session/doc.go`
- Create: `tracker/internal/session/CLAUDE.md`

- [ ] **Step 1: Write `doc.go`**:

```go
// Package session owns the per-request lifecycle state shared by
// tracker/internal/broker's selection and settlement subsystems:
// the Inflight state machine, the in-memory credit Reservations
// ledger, and the typed expiry sweep helpers driven by the broker's
// reservation-TTL reaper.
//
// session is a pure state package. It does not import internal/registry,
// internal/ledger, internal/admission, or any wire type beyond the
// EnvelopeBody it carries as request metadata. Goroutine ownership
// (timers, tickers) lives in broker; session exposes synchronous
// helpers only.
//
// See docs/superpowers/specs/tracker/2026-05-07-tracker-session-design.md.
package session
```

- [ ] **Step 2: Write `CLAUDE.md`**:

```markdown
# tracker/internal/session — Development Context

## What this is

Per-request lifecycle state shared by `tracker/internal/broker`'s
selection (`*Broker`) and settlement (`*Settlement`) subsystems.
Two stores (`Inflight`, `Reservations`) bundled into a `Manager`,
plus a `SweepExpiredAndFail` helper that the broker's reaper drives.

Authoritative spec: `docs/superpowers/specs/tracker/2026-05-07-tracker-session-design.md`.

## Non-negotiable rules

1. **No registry/ledger/admission/push imports.** Session is state,
   not orchestration. The broker is the only legitimate consumer.
2. **No goroutines.** Timers and tickers live in broker; session
   exposes synchronous helpers only.
3. **Race-clean is mandatory.** `go test -race` runs by default
   (see tracker/CLAUDE.md always-`-race` list).
4. **CAS transitions.** State changes go through `Inflight.Transition`
   only — direct field mutation is a bug.

## Tech stack

- Go 1.25, stdlib `sync`, `crypto/ed25519`, `time`
- Imports `shared/ids` and `shared/proto` (only for `EnvelopeBody`)
- Tests via `testify`
```

- [ ] **Step 3: Run `go vet`**

Run: `go vet ./tracker/internal/session/...`
Expected: success (empty package compiles).

- [ ] **Step 4: Commit**

```bash
git add tracker/internal/session/doc.go tracker/internal/session/CLAUDE.md
git commit -m "chore(tracker/session): package scaffold"
```

### Task 2: Move lifecycle errors into `session/errors.go`

**Files:**
- Create: `tracker/internal/session/errors.go`
- Create: `tracker/internal/session/errors_test.go`
- Modify: `tracker/internal/broker/errors.go`
- Modify: `tracker/internal/broker/errors_test.go`

- [ ] **Step 1: Write `session/errors.go`**:

```go
package session

import "errors"

var (
    ErrInsufficientCredits  = errors.New("session: insufficient credits")
    ErrDuplicateReservation = errors.New("session: duplicate reservation")
    ErrIllegalTransition    = errors.New("session: illegal state transition")
    ErrUnknownRequest       = errors.New("session: unknown request")
)
```

- [ ] **Step 2: Write `session/errors_test.go`** — sentinel identity check:

```go
package session

import (
    "errors"
    "fmt"
    "testing"
)

func TestErrorsAreSentinels(t *testing.T) {
    cases := []error{
        ErrInsufficientCredits,
        ErrDuplicateReservation,
        ErrIllegalTransition,
        ErrUnknownRequest,
    }
    for _, e := range cases {
        if e == nil {
            t.Fatal("nil sentinel")
        }
        wrapped := fmt.Errorf("ctx: %w", e)
        if !errors.Is(wrapped, e) {
            t.Fatalf("errors.Is broken for %v", e)
        }
    }
}
```

- [ ] **Step 3: Edit `broker/errors.go`** — remove the four moved errors. Keep broker-only errors (`ErrUnknownModel`, `ErrSeederMismatch`, `ErrModelMismatch`, `ErrCostOverspend`, `ErrSeederSigInvalid`, `ErrUnknownPreimage`, `ErrDuplicateSettle`).

Run: `grep -n 'ErrInsufficientCredits\|ErrDuplicateReservation\|ErrIllegalTransition\|ErrUnknownRequest' tracker/internal/broker/`
Expected before edit: hits in `errors.go` and `errors_test.go`. After Step 3 + Step 4: zero hits in `tracker/internal/broker/`.

- [ ] **Step 4: Edit `broker/errors_test.go`** — drop the four moved-error sentinel tests; keep broker-specific tests intact.

- [ ] **Step 5: Run, expect FAIL** — broker still imports its own (now removed) sentinels from selector/offer_loop/etc.

Run: `cd tracker && go build ./internal/broker/...`
Expected: build errors at the sites that previously used the four moved errors. They will be fixed in Phase 2 import sweep (Task 7); this build failure is the starting state for Phase 2.

- [ ] **Step 6: Verify session compiles**

Run: `cd tracker && go test ./internal/session/... -run TestErrorsAreSentinels -v`
Expected: PASS.

- [ ] **Step 7: Commit (broken-broker checkpoint)**

```bash
git add tracker/internal/session/errors.go tracker/internal/session/errors_test.go \
        tracker/internal/broker/errors.go tracker/internal/broker/errors_test.go
git commit -m "refactor(tracker/session): move 4 lifecycle errors out of broker"
```

A broken-broker commit is acceptable here — it lives between two commits and is recovered in Phase 2. The test suite stays green by Task 9.

---

## Phase 2 — Move `Inflight` and `Reservations`

### Task 3: Move `Request` + `State` to `session/request.go`

**Files:**
- Create: `tracker/internal/session/request.go`
- Create: `tracker/internal/session/request_test.go`

- [ ] **Step 1: Move the `Request` struct + `State` enum + iota constants** from `tracker/internal/broker/inflight.go` to `tracker/internal/session/request.go`. Same fields, same docstrings, same iota order. Package becomes `session`.

```go
package session

import (
    "crypto/ed25519"
    "time"

    "github.com/token-bay/token-bay/shared/ids"
    tbproto "github.com/token-bay/token-bay/shared/proto"
)

type State uint8

const (
    StateUnspecified State = iota
    StateSelecting
    StateAssigned
    StateServing
    StateCompleted
    StateFailed
)

// String returns a stable human-readable label, used by metrics and
// admin output. Locked to ensure metric label cardinality stays small.
func (s State) String() string { /* unchanged from broker pkg */ }

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
```

- [ ] **Step 2: Write `request_test.go`** — table-test allowed transitions per spec §5:

```go
func TestState_AllowedTransitions(t *testing.T) {
    cases := []struct {
        from, to State
        ok       bool
    }{
        {StateSelecting, StateAssigned, true},
        {StateSelecting, StateFailed, true},
        {StateAssigned, StateServing, true},
        {StateAssigned, StateFailed, true},
        {StateServing, StateCompleted, true},
        {StateServing, StateFailed, true},
        {StateAssigned, StateCompleted, false}, // skips SERVING
        {StateCompleted, StateServing, false},
        {StateFailed, StateCompleted, false},
    }
    for _, c := range cases {
        if got := IsAllowedTransition(c.from, c.to); got != c.ok {
            t.Errorf("Allowed(%v→%v) = %v, want %v", c.from, c.to, got, c.ok)
        }
    }
}
```

- [ ] **Step 3: Implement `IsAllowedTransition`** in `request.go`. Table-driven; visible so tests can use it.

- [ ] **Step 4: Run, expect PASS**

Run: `cd tracker && go test -race ./internal/session/... -run TestState -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/session/request.go tracker/internal/session/request_test.go
git commit -m "feat(tracker/session): Request + State + IsAllowedTransition"
```

### Task 4: Move `Inflight` to `session/inflight.go`

**Files:**
- Create: `tracker/internal/session/inflight.go`
- Create: `tracker/internal/session/inflight_test.go`

- [ ] **Step 1: Move the `Inflight` struct + every method** from broker plan Task 9 (`broker/inflight.go`) into `tracker/internal/session/inflight.go`. Mechanical move:
  - Package `broker` → `session`
  - References to `Request` / `State` / `StateXxx` lose the `broker.` prefix (already in this package)
  - References to `ErrIllegalTransition` / `ErrUnknownRequest` lose the `broker.` prefix (now in this package)
  - All other code byte-identical.

- [ ] **Step 2: Move `inflight_test.go`** with the same package rename. Tests exercise `NewInflight`, `Insert`, `Get`, `Transition`, `MarkSeeder`, `IndexByHash`, `LookupByHash`, `Sweep` (renamed `SweepTerminal` per session spec §3).

- [ ] **Step 3: Rename `Sweep` → `SweepTerminal`** for clarity (the reservation store has its own `SweepExpired`). Update all call sites in `inflight_test.go`.

- [ ] **Step 4: Run, expect PASS**

Run: `cd tracker && go test -race ./internal/session/... -run TestInflight -v`
Expected: PASS, race-clean.

- [ ] **Step 5: Delete the broker copy**

```bash
git rm tracker/internal/broker/inflight.go tracker/internal/broker/inflight_test.go
```

(Per Phase 2 import sweep Task 7, broker test runs are still broken until selector/offer_loop/broker/settlement get rewritten. This is expected.)

- [ ] **Step 6: Commit**

```bash
git add tracker/internal/session/inflight.go tracker/internal/session/inflight_test.go
git commit -m "refactor(tracker/session): move Inflight from broker"
```

### Task 5: Move `Reservations` to `session/reservations.go`

**Files:**
- Create: `tracker/internal/session/reservations.go`
- Create: `tracker/internal/session/reservations_test.go`

- [ ] **Step 1: Move `Reservation` (struct), `Reservations` (store), and every method** from broker plan Task 8 (`broker/reservation.go`) into `tracker/internal/session/reservations.go`. Same mechanical rename as Task 4: package, error prefixes, nothing else changes.

- [ ] **Step 2: Rename `Sweep` → `SweepExpired`** so callers don't confuse it with `Inflight.SweepTerminal`. The two sweeps run on different ticks for different purposes.

- [ ] **Step 3: Move `reservation_test.go` → `reservations_test.go`** with the same rename. Update test method calls.

- [ ] **Step 4: Run, expect PASS**

Run: `cd tracker && go test -race ./internal/session/... -run TestReserv -v`
Expected: PASS, race-clean.

- [ ] **Step 5: Delete the broker copy**

```bash
git rm tracker/internal/broker/reservation.go tracker/internal/broker/reservation_test.go
```

- [ ] **Step 6: Commit**

```bash
git add tracker/internal/session/reservations.go tracker/internal/session/reservations_test.go
git commit -m "refactor(tracker/session): move Reservations from broker"
```

---

## Phase 3 — `Manager` + `SweepExpiredAndFail`

### Task 6: Implement `Manager` + `Expiry` + composite sweep

**Files:**
- Create: `tracker/internal/session/manager.go`
- Create: `tracker/internal/session/manager_test.go`

- [ ] **Step 1: Write failing tests** for `Manager` + `SweepExpiredAndFail`:

```go
func TestManager_SweepExpiredAndFail_TransitionsAssignedToFailed(t *testing.T) {
    m := New()
    consumer := ids.IdentityID{0xCC}
    seeder   := ids.IdentityID{0xDD}
    reqID    := [16]byte{0x01}

    m.Inflight.Insert(&Request{
        RequestID: reqID, ConsumerID: consumer,
        AssignedSeeder: seeder, State: StateAssigned,
    })
    require.NoError(t, m.Reservations.Reserve(reqID, consumer, 100, 1000, time.Now().Add(-time.Second)))

    expired := m.SweepExpiredAndFail(time.Now())
    require.Len(t, expired, 1)
    require.Equal(t, reqID, expired[0].ReqID)
    require.Equal(t, StateAssigned, expired[0].PriorState)
    require.Equal(t, seeder, expired[0].AssignedSeeder)

    got, _ := m.Inflight.Get(reqID)
    require.Equal(t, StateFailed, got.State)
    require.Equal(t, uint64(0), m.Reservations.Reserved(consumer))
}

func TestManager_SweepExpiredAndFail_SelectingHasNoSeeder(t *testing.T) {
    m := New()
    reqID := [16]byte{0x02}
    m.Inflight.Insert(&Request{RequestID: reqID, State: StateSelecting})
    require.NoError(t, m.Reservations.Reserve(reqID, ids.IdentityID{1}, 1, 100, time.Now().Add(-time.Second)))
    expired := m.SweepExpiredAndFail(time.Now())
    require.Len(t, expired, 1)
    require.Equal(t, StateSelecting, expired[0].PriorState)
    require.Equal(t, ids.IdentityID{}, expired[0].AssignedSeeder) // zero
}

func TestManager_SweepExpiredAndFail_SkipsServingAndCompleted(t *testing.T) {
    // SERVING and COMPLETED don't transition; their reservations may still
    // be in the slot table after a 7.16-style ledger-append failure, but
    // SweepExpiredAndFail should not transition them.
    m := New()
    serving := [16]byte{0x03}
    done    := [16]byte{0x04}
    m.Inflight.Insert(&Request{RequestID: serving, State: StateServing})
    m.Inflight.Insert(&Request{RequestID: done,    State: StateCompleted, TerminatedAt: time.Now()})
    require.NoError(t, m.Reservations.Reserve(serving, ids.IdentityID{1}, 1, 100, time.Now().Add(-time.Second)))
    require.NoError(t, m.Reservations.Reserve(done,    ids.IdentityID{2}, 1, 100, time.Now().Add(-time.Second)))

    expired := m.SweepExpiredAndFail(time.Now())
    require.Len(t, expired, 2)
    for _, e := range expired {
        if e.ReqID == serving {
            require.Equal(t, StateServing, e.PriorState)
        }
        if e.ReqID == done {
            require.Equal(t, StateCompleted, e.PriorState)
        }
    }
    // Neither was transitioned to Failed:
    s, _ := m.Inflight.Get(serving); require.Equal(t, StateServing, s.State)
    d, _ := m.Inflight.Get(done);    require.Equal(t, StateCompleted, d.State)
}

func TestManager_SweepExpiredAndFail_RaceClean(t *testing.T) {
    m := New()
    var wg sync.WaitGroup
    for i := 0; i < 50; i++ {
        wg.Add(1)
        go func(i int) {
            defer wg.Done()
            id := [16]byte{byte(i)}
            m.Inflight.Insert(&Request{RequestID: id, State: StateAssigned})
            _ = m.Reservations.Reserve(id, ids.IdentityID{byte(i)}, 1, 100, time.Now().Add(-time.Millisecond))
        }(i)
    }
    wg.Wait()
    _ = m.SweepExpiredAndFail(time.Now())
}
```

- [ ] **Step 2: Run, expect FAIL** — `New`, `Manager`, `Expiry`, `SweepExpiredAndFail` undefined.

- [ ] **Step 3: Implement `manager.go`**:

```go
package session

import (
    "time"

    "github.com/token-bay/token-bay/shared/ids"
)

type Manager struct {
    Inflight     *Inflight
    Reservations *Reservations
}

func New() *Manager {
    return &Manager{
        Inflight:     NewInflight(),
        Reservations: NewReservations(),
    }
}

type Expiry struct {
    ReqID          [16]byte
    ConsumerID     ids.IdentityID
    Amount         uint64
    PriorState     State
    AssignedSeeder ids.IdentityID
}

// SweepExpiredAndFail does:
//   1. drop all reservation slots whose ExpiresAt ≤ now;
//   2. for each, read the matching Request's current state;
//   3. if PriorState ∈ {StateSelecting, StateAssigned}, transition to StateFailed.
//
// Returns one Expiry per dropped slot so the caller (broker reaper) can drive
// registry-side compensations (DecLoad on AssignedSeeder if PriorState == Assigned).
//
// SERVING / COMPLETED entries observed during sweep are reported but NOT
// transitioned — that's a 7.16-style ledger-append-failure path where the
// reservation needs operator attention, not an automatic Failed transition.
func (m *Manager) SweepExpiredAndFail(now time.Time) []Expiry {
    expired := m.Reservations.SweepExpired(now)
    if len(expired) == 0 {
        return nil
    }
    out := make([]Expiry, 0, len(expired))
    for _, slot := range expired {
        e := Expiry{
            ReqID:      slot.ReqID,
            ConsumerID: slot.ConsumerID,
            Amount:     slot.Amount,
        }
        if req, ok := m.Inflight.Get(slot.ReqID); ok {
            e.PriorState = req.State
            if req.State == StateAssigned {
                e.AssignedSeeder = req.AssignedSeeder
            }
            switch req.State {
            case StateSelecting:
                _ = m.Inflight.Transition(slot.ReqID, StateSelecting, StateFailed)
            case StateAssigned:
                _ = m.Inflight.Transition(slot.ReqID, StateAssigned, StateFailed)
            }
        }
        out = append(out, e)
    }
    return out
}
```

- [ ] **Step 4: Run, expect PASS**

Run: `cd tracker && go test -race ./internal/session/... -v`
Expected: all PASS, race-clean.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/session/manager.go tracker/internal/session/manager_test.go
git commit -m "feat(tracker/session): Manager + SweepExpiredAndFail composite sweep"
```

---

## Phase 4 — Broker import sweep + constructor updates

This phase rewrites broker source to import `tracker/internal/session`, swap type prefixes, and reduce `OpenBroker` / `OpenSettlement` / `Subsystems.Open` to take `*session.Manager` instead of separate `*Inflight` / `*Reservations` arguments.

### Task 7: Mechanical import + prefix sweep

**Files:**
- Modify: `tracker/internal/broker/broker.go`
- Modify: `tracker/internal/broker/broker_test.go`
- Modify: `tracker/internal/broker/settlement.go`
- Modify: `tracker/internal/broker/settlement_test.go`
- Modify: `tracker/internal/broker/queue_drain.go`
- Modify: `tracker/internal/broker/queue_drain_test.go`
- Modify: `tracker/internal/broker/selector.go` (if it touches Request/State — likely not)
- Modify: `tracker/internal/broker/selector_test.go`
- Modify: `tracker/internal/broker/offer_loop.go`
- Modify: `tracker/internal/broker/offer_loop_test.go`
- Modify: `tracker/internal/broker/race_test.go`

- [ ] **Step 1: Add the import alias** to every Go file that currently references `Inflight`, `Reservations`, `Request`, `State*`, or any of the four moved errors:

```go
import (
    session "github.com/token-bay/token-bay/tracker/internal/session"
)
```

Use the bare package name; the alias above is only an example.

- [ ] **Step 2: Sweep type/error prefixes**

For each file in the file map, perform these substitutions (semantically — your editor's "rename symbol" or careful sed will work):

| Was | Now |
|---|---|
| `*Inflight` (field/param/return type) | `*session.Inflight` |
| `*Reservations` | `*session.Reservations` |
| `*Request` | `*session.Request` |
| `Request{…}` (struct literal) | `session.Request{…}` |
| `State` (parameter type) | `session.State` |
| `StateUnspecified` / `StateSelecting` / `StateAssigned` / `StateServing` / `StateCompleted` / `StateFailed` | prefix with `session.` |
| `NewInflight()` | `session.NewInflight()` |
| `NewReservations()` | `session.NewReservations()` |
| `ErrInsufficientCredits` | `session.ErrInsufficientCredits` |
| `ErrDuplicateReservation` | `session.ErrDuplicateReservation` |
| `ErrIllegalTransition` | `session.ErrIllegalTransition` |
| `ErrUnknownRequest` | `session.ErrUnknownRequest` |
| `Reservation` (type, e.g. `[]Reservation` in reaper) | `session.Reservation` |

- [ ] **Step 3: Update reaper.go**

Old (broker plan Task 19):
```go
func (s *Settlement) runReap(now time.Time) {
    expired := s.resv.Sweep(now)
    for _, slot := range expired {
        if req, ok := s.inflt.Get(slot.ReqID); ok {
            switch req.State {
            case StateSelecting:
                _ = s.inflt.Transition(slot.ReqID, StateSelecting, StateFailed)
            case StateAssigned:
                _, _ = s.deps.Registry.DecLoad(req.AssignedSeeder)
                _ = s.inflt.Transition(slot.ReqID, StateAssigned, StateFailed)
            }
        }
    }
}
```

New:
```go
func (s *Settlement) runReap(now time.Time) {
    expired := s.mgr.SweepExpiredAndFail(now)
    for _, e := range expired {
        if e.PriorState == session.StateAssigned && e.AssignedSeeder != (ids.IdentityID{}) {
            _, _ = s.deps.Registry.DecLoad(e.AssignedSeeder)
        }
        // Metric increments stay here; session does not emit metrics.
        s.metrics.ReservationTTLExpired.Inc()
    }
}
```

The state CAS now lives in `session`; broker only does the `DecLoad` and the metric.

- [ ] **Step 4: Run, expect FAIL on constructor mismatch only**

Run: `cd tracker && go build ./internal/broker/...`
Expected: build errors point at `OpenBroker` and `OpenSettlement` signatures still expecting `*Inflight, *Reservations`. Fix in Task 8.

- [ ] **Step 5: Don't commit yet** — Task 8 fixes the constructors, then everything compiles. Single commit at the end of Task 8.

### Task 8: `OpenBroker` / `OpenSettlement` / `Subsystems.Open` take `*session.Manager`

**Files:**
- Modify: `tracker/internal/broker/broker.go` (`OpenBroker` signature + body)
- Modify: `tracker/internal/broker/settlement.go` (`OpenSettlement` signature + body)
- Modify: `tracker/internal/broker/subsystems.go` (`Open` constructs `session.New()`)
- Modify: `tracker/internal/broker/broker_test.go` (constructor calls)
- Modify: `tracker/internal/broker/settlement_test.go` (constructor calls)
- Modify: `tracker/internal/broker/subsystems_test.go` (assertion: shared `*session.Manager`)
- Modify: `tracker/internal/broker/race_test.go` (constructor)

- [ ] **Step 1: Update `OpenBroker`**

Was:
```go
func OpenBroker(cfg config.BrokerConfig, scfg config.SettlementConfig, deps Deps, inflt *Inflight, resv *Reservations) (*Broker, error)
```

Now:
```go
func OpenBroker(cfg config.BrokerConfig, scfg config.SettlementConfig, deps Deps, mgr *session.Manager) (*Broker, error) {
    if mgr == nil {
        mgr = session.New()
    }
    // … existing body, with b.inflt and b.resv now reading mgr.Inflight / mgr.Reservations
    b := &Broker{
        cfg: cfg, scfg: scfg, deps: deps,
        mgr: mgr,
        // …
    }
}
```

Field rename inside `Broker` struct: drop `inflt *Inflight` and `resv *Reservations`; add `mgr *session.Manager`. Every `b.inflt.X(…)` becomes `b.mgr.Inflight.X(…)`; every `b.resv.X(…)` becomes `b.mgr.Reservations.X(…)`. Same for `b.failAndRelease` and `Submit`.

- [ ] **Step 2: Update `OpenSettlement`**

Symmetric change. `s.inflt` and `s.resv` → `s.mgr` field; method calls take the `Inflight` / `Reservations` accessor on the way through.

- [ ] **Step 3: Update `Subsystems.Open`**

Was:
```go
func Open(cfg config.BrokerConfig, scfg config.SettlementConfig, deps Deps) (*Subsystems, error) {
    inflt := NewInflight()
    resv  := NewReservations()
    b, err := OpenBroker(cfg, scfg, deps, inflt, resv)
    // …
    s, err := OpenSettlement(scfg, deps, inflt, resv)
}
```

Now:
```go
func Open(cfg config.BrokerConfig, scfg config.SettlementConfig, deps Deps) (*Subsystems, error) {
    mgr := session.New()
    b, err := OpenBroker(cfg, scfg, deps, mgr)
    if err != nil { return nil, err }
    s, err := OpenSettlement(scfg, deps, mgr)
    if err != nil { _ = b.Close(); return nil, err }
    return &Subsystems{Broker: b, Settlement: s}, nil
}
```

- [ ] **Step 4: Update tests**

In `subsystems_test.go`, add an assertion that both subsystems hold the same `*session.Manager`:

```go
func TestSubsystems_ShareSessionManager(t *testing.T) {
    subs, err := Open(testBrokerCfg(), testSettlementCfg(), testDeps(t))
    require.NoError(t, err)
    defer subs.Close()
    require.NotNil(t, subs.Broker.mgr)
    require.Same(t, subs.Broker.mgr, subs.Settlement.mgr)
}
```

(Add an unexported accessor `mgrForTest()` if your style guide forbids touching unexported fields from `*_test.go` in the same package — but since the tests are in `package broker`, direct field access is fine.)

In `broker_test.go`, `settlement_test.go`, `race_test.go`, swap the constructor calls:

```go
b, _ := OpenBroker(testBrokerCfg(), testSettlementCfg(), testDeps(t), nil)              // was: …, nil, nil
s, _ := OpenSettlement(testSettlementCfg(), testDeps(t), nil)                            // was: …, nil, nil
```

When tests need to seed `Inflight` / `Reservations` directly:

```go
mgr := session.New()
mgr.Inflight.Insert(&session.Request{RequestID: …, State: session.StateAssigned, …})
_ = mgr.Reservations.Reserve(…)
b, _ := OpenBroker(testBrokerCfg(), testSettlementCfg(), testDeps(t), mgr)
```

- [ ] **Step 5: Run, expect PASS**

Run: `cd tracker && go test -race ./internal/broker/... -v`
Expected: every test from broker plan Phases 4–9 passes, race-clean. Coverage matches pre-refactor baseline.

- [ ] **Step 6: Run, expect PASS — full module**

Run: `cd tracker && go test -race ./... -v`
Expected: all PASS, including `internal/api/*` (which still imports `*broker.Broker` / `*broker.Settlement` unchanged).

- [ ] **Step 7: Commit**

```bash
git add tracker/internal/broker/ tracker/internal/session/
git commit -m "refactor(tracker/broker): consume session.Manager for Inflight + Reservations"
```

### Task 9: Admin handlers route through `session`

**Files:**
- Modify: `tracker/internal/broker/admin.go`
- Modify: `tracker/internal/broker/admin_test.go`

The admin endpoints from broker spec §10 (`/broker/inflight`, `/broker/reservations`, `/broker/reservations/release/<id>`, `/broker/inflight/fail/<id>`) read and mutate session state. Their implementation calls were `s.subs.Broker.inflt.Get(…)` / `s.subs.Broker.resv.Release(…)`; now they go through `s.subs.Broker.mgr.Inflight.Get(…)` / `s.subs.Broker.mgr.Reservations.Release(…)`.

- [ ] **Step 1: Update each admin handler** to read `mgr.Inflight` / `mgr.Reservations`. Audit-log the operator identity per broker plan Task 22.

- [ ] **Step 2: Update `admin_test.go`** assertions if they reference Inflight/Reservations directly.

- [ ] **Step 3: Run, expect PASS**

Run: `cd tracker && go test -race ./internal/broker/... -run TestAdmin -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add tracker/internal/broker/admin.go tracker/internal/broker/admin_test.go
git commit -m "refactor(tracker/broker/admin): route handlers through session.Manager"
```

---

## Phase 5 — Spec amendment + tracker CLAUDE.md + final gates

### Task 10: Amend the broker design spec

**Files:**
- Modify: `docs/superpowers/specs/tracker/2026-05-07-tracker-broker-design.md`

Apply the cross-cutting amendments listed in the session spec §7. Concretely:

- [ ] **§3.2 (Composite opener)** — replace the `*Inflight` + `*Reservations` constructor signatures with a single `*session.Manager` argument. Update the `Open` example accordingly.

- [ ] **§3.5 (Internal collaborators / Deps)** — no change; `Deps` does not carry session.

- [ ] **§4.1 (Reservations)** — replace the data-structure definition with: "Now lives in `tracker/internal/session.Reservations`. Public surface mirrored in the session design spec." Keep the operator-facing semantics description.

- [ ] **§4.2 (Inflight)** — same pattern: redirect to `tracker/internal/session.Inflight` + `session.Request`. Keep the state diagram inline.

- [ ] **§5.5 (Reservation TTL reaper)** — replace the algorithm to call `session.Manager.SweepExpiredAndFail(now)`; broker iterates `[]Expiry` and calls `Registry.DecLoad` for any `e.AssignedSeeder != zero`. The state CAS happens inside session.

- [ ] **§11 (Acceptance criteria)** — add: "Race-clean across `tracker/internal/{broker,session}` is required; both packages join the always-`-race` list."

- [ ] **§12 (Cross-cutting amendments)** — append a sixth item: "Lifecycle state (`Inflight`, `Reservations`, `Request`, `State`, four lifecycle errors) lives in `tracker/internal/session` per `docs/superpowers/specs/tracker/2026-05-07-tracker-session-design.md` and `docs/superpowers/plans/2026-05-07-tracker-internal-session.md`."

- [ ] **Step 5: Commit**

```bash
git add docs/superpowers/specs/tracker/2026-05-07-tracker-broker-design.md
git commit -m "docs(tracker/broker): amend spec to reference internal/session"
```

### Task 11: Update `tracker/CLAUDE.md`

**Files:**
- Modify: `tracker/CLAUDE.md`

- [ ] **Step 1: Add `internal/session` to the always-`-race` list.** Existing line in the "Things that look surprising and aren't bugs" / "Development workflow" section enumerates `internal/broker`, `internal/federation`, `internal/admission`. Add `internal/session`.

- [ ] **Step 2: Add `internal/session` to the project layout sentence** (`internal/<module>/ — server, api, registry, broker, ledger, federation, reputation, stunturn, admin, config, metrics, session`).

- [ ] **Step 3: Commit**

```bash
git add tracker/CLAUDE.md
git commit -m "docs(tracker): add internal/session to project layout and -race list"
```

### Task 12: Final acceptance — `make check`

- [ ] **Step 1: Race-clean confirmation**

Run: `make -C tracker test`
Expected: all `go test -race` clean across `tracker/...`.

- [ ] **Step 2: Lint**

Run: `make -C tracker lint`
Expected: `golangci-lint` clean. The new `tracker/internal/session/` package must pass without warnings.

- [ ] **Step 3: Cross-module sanity**

Run: `make check` from repo root.
Expected: all three modules green.

- [ ] **Step 4: Grep guard — no broker leakage**

Run: `grep -rn 'broker\.Inflight\|broker\.Reservations\|broker\.Request\b\|broker\.State[A-Z]\|broker\.ErrIllegalTransition\|broker\.ErrUnknownRequest\|broker\.ErrInsufficientCredits\|broker\.ErrDuplicateReservation' tracker/ shared/ plugin/`
Expected: zero hits. (The acceptance criterion in session spec §9 item 2.)

- [ ] **Step 5: Grep guard — session is consumed only by broker**

Run: `grep -rln 'tracker/internal/session' tracker/ shared/ plugin/`
Expected: hits only in `tracker/internal/session/` (self-references in tests) and `tracker/internal/broker/`. `cmd/run_cmd.go` does not import session — `broker.Open` constructs its own `*session.Manager` internally.

- [ ] **Step 6: Final commit hygiene**

Run: `git status`
Expected: clean tree.

- [ ] **Step 7: PR**

```bash
git push -u origin tracker/internal/session
gh pr create --base main --fill --title "tracker/session: extract per-request lifecycle state from broker"
gh pr checks --watch
```

Merge with `gh pr merge --squash --delete-branch` only when CI is green and a reviewer has approved per `CLAUDE.md` PR rules.

---

## Self-review notes (author)

- Sequencing: this plan strictly follows the broker plan; the file map assumes broker-plan files exist. Running this plan against `main` before broker-plan merge will fail at Task 7's import sweep.
- The "broken-broker checkpoint" between Task 2 and Task 8 is intentional — it scopes the moved-error commit cleanly. If the team's commit-passes-tests rule is strict, fold Tasks 2–8 into a single squash commit rather than the per-task commits shown.
- Reaper goroutine ownership stays in `*Settlement` (broker package). `session.Manager.SweepExpiredAndFail` is synchronous; broker drives the timer + DecLoad + metric. This matches the session spec invariant "no goroutines in session."
- `Inflight.SweepTerminal` (the diagnostic terminal-state cleanup) is exposed but not driven by anything in this plan. The broker spec §4.2 references it; if/when broker adds a separate sweeper goroutine for it, that's an in-broker change with no session-side effect.
- The `Manager` accessor pattern (`mgr.Inflight.X`, `mgr.Reservations.Y`) is verbose at call sites. Considered adding pass-through methods (`mgr.Reserve(…)`, `mgr.Insert(…)`) but rejected: every pass-through is one more method to keep in sync and one more place tests can diverge. Two named subfields, no proxies.
- `session` package has zero dependencies on `internal/registry`, `internal/ledger`, `internal/admission`, or `internal/server`. The grep guard in Task 12 step 4 is the standing acceptance check.
