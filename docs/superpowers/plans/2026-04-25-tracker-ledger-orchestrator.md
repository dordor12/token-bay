# Tracker — `internal/ledger` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land the ledger orchestrator — the layer that wires `internal/ledger/entry`, `internal/ledger/storage`, the tracker keypair, and a clock into a chain-integrity-preserving API. End state: callers can `IssueStarterGrant`, `AppendUsage`, `AppendTransferOut`, `SignedBalance`, and read entries with the orchestrator enforcing prev_hash linkage, seq monotonicity, signature verification, and balance arithmetic.

**Architecture:** A `Ledger` struct holds the open `*storage.Store`, the tracker `ed25519.PrivateKey`, a `nowFn func() time.Time`, and a `sync.Mutex` that serializes the entire append flow (tip-read → entry-build → sign → storage write). The mutex IS the spec's "ledger write lock" from §4.1 step 1. Storage's own `writeMu` is redundant when called from inside the orchestrator's critical section but stays in place to defend against direct callers.

The append flow takes pre-built bodies (with `seq` + `prev_hash` already filled) and pre-collected counterparty sigs, validates them against the current tip, signs with the tracker key, and persists. Stale-tip mismatches surface as `ErrStaleTip` for caller retry — holding the lock across a multi-party network round-trip would block all appends for the ~15 min settlement timeout (tracker spec §5.3), unworkable.

**Tech Stack:** Go 1.23+, stdlib `crypto/ed25519`, stdlib `time`, stdlib `sync`, `github.com/stretchr/testify`. No new third-party deps.

**Specs:**
- `docs/superpowers/specs/ledger/2026-04-22-ledger-design.md` §3 (data model), §4.1 (append algorithm), §4.2 (balance query)
- `docs/superpowers/specs/tracker/2026-04-22-tracker-design.md` §5.2 (settlement)

**Dependency order:** Runs after `internal/ledger/entry` and `internal/ledger/storage` (both landed). The eventual `internal/broker` will be the primary caller. `internal/config` (parallel session) will eventually load the tracker keypair; for v1 the orchestrator takes the keypair as an argument so the plan doesn't block on config landing.

---

## 1. File map

```
tracker/internal/ledger/
├── doc.go                                        ← CREATE: package doc + non-negotiables
├── ledger.go                                     ← CREATE: Ledger struct, Open, Close, options
├── ledger_test.go                                ← CREATE: lifecycle tests
├── append.go                                     ← CREATE: appendEntry internal glue
├── append_test.go                                ← CREATE: chain-integrity tests via IssueStarterGrant
├── starter_grant.go                              ← CREATE: IssueStarterGrant
├── starter_grant_test.go                         ← CREATE
├── usage.go                                      ← CREATE: AppendUsage (caller pre-signs)
├── usage_test.go                                 ← CREATE: stale-tip retry, sig verification
├── transfer.go                                   ← CREATE: AppendTransferOut
├── transfer_test.go                              ← CREATE
├── balance.go                                    ← CREATE: SignedBalance
├── balance_test.go                               ← CREATE: snapshot freshness, signature
├── reads.go                                      ← CREATE: Tip / EntryBySeq / EntryByHash / EntriesSince passthroughs
├── reads_test.go                                 ← CREATE
├── integration_test.go                           ← CREATE: end-to-end concurrency + restart
└── helpers_test.go                               ← CREATE: openTempLedger, deterministic keys

tracker/internal/ledger/storage/
└── balance_with_tip.go                           ← CREATE: atomic Balance+Tip read in one txn
└── balance_with_tip_test.go                      ← CREATE
```

Notes:
- The `internal/ledger` package directory does not yet exist on the filesystem (only `internal/ledger/entry` and `internal/ledger/storage` do). v1 of this plan creates the parent package alongside its subpackages.
- One Go file per area of responsibility, mirroring the storage plan's file split. Keeps each test file scoped to its sibling.
- `helpers_test.go` provides `openTempLedger(t)` returning a wired `*Ledger` with an in-memory tempdir SQLite + deterministic tracker keypair. All higher-level tests start from this helper.

## 2. Conventions used in this plan

- All `go test` commands run from `/Users/dor.amid/.superset/worktrees/token-bay/cut-sunflower/tracker`.
- `PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH"` if Go is managed by mise.
- One commit per task. Conventional-commit prefixes: `feat(tracker/ledger):`, `feat(tracker/ledger/storage):` (Task 3 only), `test(tracker/ledger):`.
- Co-Authored-By footer: `Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>`.
- Each task: failing test first (red), minimal impl (green), commit. Refactor-only commits get `refactor:`.
- Tests use `openTempLedger(t)` for the standard fixture; raw `storage.Open` only in the few storage-extension tests.

---

## 3. Design decisions used by all tasks

### 3.1 Lock scope

`Ledger.mu` covers the entire append critical section: read tip → build entry → verify counterparty sigs → sign with tracker → call `storage.AppendEntry`. This is the spec's §4.1 "ledger write lock". Reads (Tip, Balance, EntryBy*, EntriesSince) do not take the lock — storage's WAL gives concurrent readers for free.

`SignedBalance` does not take the lock either: it reads (balance, tip) in a single SQLite read transaction via the new `storage.BalanceWithTip` helper (Task 3). The result is internally consistent.

### 3.2 Body pre-fill contract for AppendUsage / AppendTransferOut

Caller fills `body.Seq` and `body.PrevHash` before calling. Orchestrator verifies they match current tip; mismatch returns `ErrStaleTip`. Caller retries.

Why not have the orchestrator stamp seq + prev_hash itself? Because consumer/seeder sigs are over the canonical body bytes which include those fields. The signing parties had to know seq + prev_hash to sign. Stamping them at append time would invalidate the sigs.

### 3.3 Balance arithmetic per kind

| Kind | Balance updates |
|---|---|
| STARTER_GRANT | `balance[consumer_id].credits += amount`, `last_seq = new_seq` |
| USAGE | `balance[consumer_id].credits -= cost`, `balance[seeder_id].credits += cost` |
| TRANSFER_OUT | `balance[consumer_id].credits -= amount` |

Balance reads inside the lock fetch the current row; orchestrator computes new credits, passes the typed `BalanceUpdate` slice to `storage.AppendEntry`. Spec §4.1 step 5 also requires "abort if balance would go below `-starter_grant_band`" — for v1 we hard-code `starter_grant_band = 0` (no overdraft permitted) and surface the violation as `ErrInsufficientBalance`. Future config can override.

### 3.4 ErrStaleTip retry contract

Returned when the body's `(prev_hash, seq)` doesn't match the current tip. The body is otherwise discarded — caller must re-collect sigs against fresh `(prev_hash, seq)` and retry. The retry budget lives in the broker layer, not here.

### 3.5 Genesis

Lazy. First append on an empty chain uses `prev_hash = make([]byte, 32)` and `seq = 1`. There is no separate genesis row or sidecar in v1; the first real entry is the chain root.

The spec §3.2 mentions a "tracker-signed genesis note in a sidecar" — that's deferred. v1's chain starts at the first real append. If the operator wants a marker, they `IssueStarterGrant` to a well-known marker identity at startup; the v1 orchestrator does not auto-create one.

### 3.6 Clock injection

`Open` accepts `WithClock(now func() time.Time)`. Default is `time.Now`. Tests pass a deterministic clock so `BalanceSnapshot.IssuedAt` and entry timestamps are reproducible.

### 3.7 Reads do not verify signatures or chain integrity per call

Read passthroughs (`Tip`, `EntryBySeq`, `EntryByHash`, `EntriesSince`) return entries verbatim from storage. They do not re-run `entry.VerifyAll` or check `prev_hash` linkage on every call.

`EntriesSince` does verify chain integrity in one place: a separate `AssertChainIntegrity(ctx, sinceSeq, untilSeq)` method (Task 9, integration test) walks the chain and verifies each entry's `prev_hash == hash(prev entry's body)`. This runs in CI and ad-hoc by operators, not on every read.

---

## Task 1: storage `BalanceWithTip` — atomic Balance + Tip read

**Files:**
- Create: `tracker/internal/ledger/storage/balance_with_tip.go`
- Create: `tracker/internal/ledger/storage/balance_with_tip_test.go`

This is the storage extension `SignedBalance` needs (plan §3.1). Single SQLite read transaction returns both rows so the snapshot is internally consistent.

### 1.1 API

```go
// BalanceTipSnapshot is a single-transaction view of an identity's balance
// and the current chain tip. ledger.SignedBalance uses this so the signed
// snapshot's chain_tip_seq and credits reflect the same point in history.
type BalanceTipSnapshot struct {
    Balance      BalanceRow
    BalanceFound bool      // false if the identity has no row yet
    TipSeq       uint64
    TipHash      []byte
    TipFound     bool      // false on empty chain
}

// BalanceWithTip reads identityID's balance row and the current chain tip
// in a single SQLite read transaction. Either may be missing; the caller
// inspects BalanceFound / TipFound. Returns an error only on actual DB
// problems.
func (s *Store) BalanceWithTip(ctx context.Context, identityID []byte) (BalanceTipSnapshot, error)
```

### 1.2 Implementation sketch

```go
func (s *Store) BalanceWithTip(ctx context.Context, identityID []byte) (BalanceTipSnapshot, error) {
    tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
    if err != nil { return BalanceTipSnapshot{}, fmt.Errorf("...: %w", err) }
    defer tx.Rollback()

    var snap BalanceTipSnapshot

    bRow := tx.QueryRowContext(ctx,
        `SELECT identity_id, credits, last_seq, updated_at FROM balances WHERE identity_id = ?`, identityID)
    err = bRow.Scan(&snap.Balance.IdentityID, &snap.Balance.Credits, &snap.Balance.LastSeq, &snap.Balance.UpdatedAt)
    switch {
    case errors.Is(err, sql.ErrNoRows):
        // BalanceFound stays false
    case err != nil:
        return snap, fmt.Errorf("BalanceWithTip balance: %w", err)
    default:
        snap.BalanceFound = true
    }

    tRow := tx.QueryRowContext(ctx, `SELECT seq, hash FROM entries ORDER BY seq DESC LIMIT 1`)
    err = tRow.Scan(&snap.TipSeq, &snap.TipHash)
    switch {
    case errors.Is(err, sql.ErrNoRows):
        // TipFound stays false
    case err != nil:
        return snap, fmt.Errorf("BalanceWithTip tip: %w", err)
    default:
        snap.TipFound = true
    }

    return snap, tx.Commit()
}
```

### 1.3 Test cases

- Empty chain, missing balance → `BalanceFound=false, TipFound=false`, no error.
- Append 1 entry, query balance for an identity that was touched → both `Found=true`.
- Append 1 entry, query balance for an unrelated identity → `BalanceFound=false, TipFound=true`.
- (Difficult to test: real cross-call atomicity. Skip; the txn boundary is structural.)

- [ ] **Step 1: Failing test in `balance_with_tip_test.go`** asserting `BalanceWithTip` exists.
- [ ] **Step 2: Implement.**
- [ ] **Step 3: Run `go test -race ./internal/ledger/storage/...`.**
- [ ] **Step 4: Commit**

```bash
git commit -m "$(cat <<'EOF'
feat(tracker/ledger/storage): BalanceWithTip atomic read

Single read transaction returns the identity's balance row and the chain
tip together, so the orchestrator's SignedBalance produces a snapshot
where chain_tip_seq and credits reflect the same point in history.
ledger spec §4.2 requires this.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: Package skeleton + `Ledger` + Open/Close

**Files:**
- Create: `tracker/internal/ledger/doc.go`, `ledger.go`, `ledger_test.go`, `helpers_test.go`

### 2.1 doc.go

```go
// Package ledger is the chain orchestrator for the tracker's append-only
// credit ledger. It wires together internal/ledger/entry (build, hash,
// verify), internal/ledger/storage (durable layer), the tracker keypair,
// and a clock into a chain-integrity-preserving API.
//
// Spec: docs/superpowers/specs/ledger/2026-04-22-ledger-design.md §4.
//
// Non-negotiables:
//
//  1. The Ledger.mu mutex covers the entire append critical section
//     (tip-read → build → sign → storage write). It IS the spec's
//     "ledger write lock" from §4.1 step 1. Append never holds the lock
//     across a network round-trip — caller pre-collects counterparty
//     sigs, orchestrator verifies them against current tip in one
//     atomic step.
//  2. Reads (Tip, EntryBy*, EntriesSince, SignedBalance) do not take
//     the append lock. SignedBalance's internal consistency comes from
//     storage.BalanceWithTip's single read transaction.
//  3. Stale-tip mismatches return ErrStaleTip. The orchestrator does
//     NOT retry — the broker layer above does.
//  4. Balance arithmetic happens here, not in storage. Storage trusts
//     the orchestrator to pass already-computed BalanceUpdate values.
package ledger
```

### 2.2 Ledger struct + Open

```go
type Ledger struct {
    store      *storage.Store
    trackerKey ed25519.PrivateKey
    trackerPub ed25519.PublicKey
    nowFn      func() time.Time
    mu         sync.Mutex
}

type Option func(*Ledger)

func WithClock(now func() time.Time) Option {
    return func(l *Ledger) { l.nowFn = now }
}

// Open wires the orchestrator over an open Store and a tracker keypair.
// The Ledger does not own the Store — the caller is responsible for
// closing it independently.
func Open(store *storage.Store, key ed25519.PrivateKey, opts ...Option) (*Ledger, error) {
    if store == nil {
        return nil, errors.New("ledger: Open requires a non-nil Store")
    }
    if len(key) != ed25519.PrivateKeySize {
        return nil, fmt.Errorf("ledger: tracker key length %d, want %d", len(key), ed25519.PrivateKeySize)
    }
    l := &Ledger{
        store:      store,
        trackerKey: key,
        trackerPub: key.Public().(ed25519.PublicKey),
        nowFn:      time.Now,
    }
    for _, opt := range opts {
        opt(l)
    }
    return l, nil
}

// Close is a no-op in v1: the Ledger does not own the Store. Reserved
// for v2 background goroutines (Merkle publication) that need shutdown
// hooks.
func (l *Ledger) Close() error { return nil }
```

### 2.3 Tests

- `TestOpen_HappyPath` — minimal wiring works.
- `TestOpen_RejectsNilStore` — nil store errors.
- `TestOpen_RejectsBadKey` — wrong-length key errors.
- `TestOpen_WithClock_OverridesNow` — option applies.
- `TestClose_NoOpReturnsNil` — Close is idempotent and returns nil.

### 2.4 helpers_test.go

```go
// openTempLedger returns a fresh Ledger backed by a tempdir-scoped Store
// and a deterministic tracker keypair. Closes both via t.Cleanup.
func openTempLedger(t *testing.T, opts ...Option) *Ledger {
    t.Helper()
    store := openTempStoreForLedger(t)
    _, priv := trackerKeypair()
    l, err := Open(store, priv, opts...)
    require.NoError(t, err)
    return l
}

func openTempStoreForLedger(t *testing.T) *storage.Store {
    t.Helper()
    s, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "ledger.db"))
    require.NoError(t, err)
    t.Cleanup(func() { _ = s.Close() })
    return s
}

func trackerKeypair() (ed25519.PublicKey, ed25519.PrivateKey) {
    seed := sha256.Sum256([]byte("ledger-test-tracker-key"))
    priv := ed25519.NewKeyFromSeed(seed[:])
    return priv.Public().(ed25519.PublicKey), priv
}

// fixedClock returns a clock fixed at t. Tests use this so timestamps
// in entries and snapshots are reproducible.
func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }
```

- [ ] **Failing tests, impl, commit.**

```bash
git commit -m "$(cat <<'EOF'
feat(tracker/ledger): package skeleton + Open/Close

Ledger struct holds the open Store, tracker keypair, mockable clock,
and the append mutex. Open validates inputs and applies optional
WithClock override. Close is a v1 no-op (reserved for v2 background
shutdown).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: Read passthroughs

**Files:**
- Create: `tracker/internal/ledger/reads.go`, `reads_test.go`

```go
func (l *Ledger) Tip(ctx context.Context) (uint64, []byte, bool, error)
func (l *Ledger) EntryBySeq(ctx context.Context, seq uint64) (*tbproto.Entry, bool, error)
func (l *Ledger) EntryByHash(ctx context.Context, hash []byte) (*tbproto.Entry, bool, error)
func (l *Ledger) EntriesSince(ctx context.Context, sinceSeq uint64, limit int) ([]*tbproto.Entry, error)
```

Each is a thin wrapper delegating to the corresponding storage method. No verification, no lock.

Tests: just the empty-chain miss path for each. The happy paths get covered transitively in Task 5+ where IssueStarterGrant produces entries to read back.

- [ ] **Failing tests, impl, commit.**

```bash
git commit -m "$(cat <<'EOF'
feat(tracker/ledger): read passthroughs

Tip / EntryBySeq / EntryByHash / EntriesSince delegate directly to
storage. No verification on read — that's the AssertChainIntegrity
audit's job (added in Task 9).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: Internal `appendEntry` glue

**Files:**
- Create: `tracker/internal/ledger/append.go`, `append_test.go`

The shared core of every public append. Takes a partially-built `*tbproto.EntryBody` (caller fills business fields + counterparty sigs), validates it, signs with the tracker key, computes the hash, builds the `storage.AppendInput`, persists.

### 4.1 API

```go
// ErrStaleTip means the entry's prev_hash / seq don't match the current
// chain tip. Caller should re-collect counterparty sigs against fresh
// (prev_hash, seq) and retry.
var ErrStaleTip = errors.New("ledger: entry prev_hash / seq does not match current tip")

// ErrInsufficientBalance means applying the entry's cost_credits would
// drive an identity's balance below the (v1: zero) starter-grant floor.
var ErrInsufficientBalance = errors.New("ledger: balance would go below permitted floor")

// appendInput is the orchestrator-internal request to appendEntry.
type appendInput struct {
    body         *tbproto.EntryBody
    consumerSig  []byte             // empty if none expected
    consumerPub  ed25519.PublicKey  // empty if no consumer involvement
    seederSig    []byte             // empty if none expected
    seederPub    ed25519.PublicKey
    deltas       []balanceDelta     // signed deltas, applied to current balance to compute new credits
}

type balanceDelta struct {
    identityID []byte
    delta      int64 // can be negative for debits
}

// appendEntry is the chain-integrity-preserving core of every public
// append method. Holds Ledger.mu for the entire critical section.
func (l *Ledger) appendEntry(ctx context.Context, in appendInput) (*tbproto.Entry, error)
```

### 4.2 Implementation flow

```go
func (l *Ledger) appendEntry(ctx context.Context, in appendInput) (*tbproto.Entry, error) {
    l.mu.Lock()
    defer l.mu.Unlock()

    // Step 1: read current tip.
    tipSeq, tipHash, hasTip, err := l.store.Tip(ctx)
    if err != nil { return nil, fmt.Errorf("ledger: tip: %w", err) }

    expectedPrev := tipHash
    expectedSeq := tipSeq + 1
    if !hasTip {
        expectedPrev = make([]byte, 32) // genesis
        expectedSeq = 1
    }

    if !bytes.Equal(in.body.PrevHash, expectedPrev) || in.body.Seq != expectedSeq {
        return nil, ErrStaleTip
    }

    // Step 2: per-spec field-shape validation. Public methods construct
    // bodies via entry.Build*Entry which already calls this — defensive.
    if err := tbproto.ValidateEntryBody(in.body); err != nil {
        return nil, fmt.Errorf("ledger: validate body: %w", err)
    }

    // Step 3: verify counterparty sigs.
    if len(in.consumerSig) != 0 {
        if !signing.VerifyEntry(in.consumerPub, in.body, in.consumerSig) {
            return nil, errors.New("ledger: consumer_sig invalid")
        }
    }
    if len(in.seederSig) != 0 {
        if !signing.VerifyEntry(in.seederPub, in.body, in.seederSig) {
            return nil, errors.New("ledger: seeder_sig invalid")
        }
    }

    // Step 4: tracker signs.
    trackerSig, err := signing.SignEntry(l.trackerKey, in.body)
    if err != nil { return nil, fmt.Errorf("ledger: tracker sign: %w", err) }

    // Step 5: compute new balances by reading current rows + applying deltas.
    balances, err := l.applyDeltas(ctx, in.body.Seq, in.body.Timestamp, in.deltas)
    if err != nil { return nil, err }

    // Step 6: build *Entry, compute hash, call storage.AppendEntry.
    hash, err := entry.Hash(in.body)
    if err != nil { return nil, fmt.Errorf("ledger: hash: %w", err) }

    e := &tbproto.Entry{
        Body:        in.body,
        ConsumerSig: in.consumerSig,
        SeederSig:   in.seederSig,
        TrackerSig:  trackerSig,
    }
    if _, err := l.store.AppendEntry(ctx, storage.AppendInput{
        Entry: e, Hash: hash, Balances: balances,
    }); err != nil {
        return nil, fmt.Errorf("ledger: append: %w", err)
    }
    return e, nil
}

// applyDeltas reads each affected identity's current balance, applies
// the signed delta, and returns the typed BalanceUpdate slice for storage.
// Returns ErrInsufficientBalance if any new balance would go below 0.
func (l *Ledger) applyDeltas(ctx context.Context, seq, timestamp uint64, deltas []balanceDelta) ([]storage.BalanceUpdate, error) {
    out := make([]storage.BalanceUpdate, 0, len(deltas))
    for _, d := range deltas {
        cur, _, err := l.store.Balance(ctx, d.identityID)
        if err != nil { return nil, fmt.Errorf("ledger: read balance: %w", err) }
        next := cur.Credits + d.delta
        if next < 0 {
            return nil, fmt.Errorf("%w: identity=%x current=%d delta=%d",
                ErrInsufficientBalance, d.identityID, cur.Credits, d.delta)
        }
        out = append(out, storage.BalanceUpdate{
            IdentityID: d.identityID,
            Credits:    next,
            LastSeq:    seq,
            UpdatedAt:  timestamp,
        })
    }
    return out, nil
}
```

### 4.3 Tests (using IssueStarterGrant in Task 5; some tested directly here)

- `TestAppendEntry_RejectsStaleTip` — pre-build a body with seq=99, get ErrStaleTip.
- `TestAppendEntry_RejectsBadConsumerSig` — body with valid shape but consumer sig over different bytes → "consumer_sig invalid".
- `TestAppendEntry_RejectsBadSeederSig` — same for seeder.
- `TestAppendEntry_InsufficientBalance` — try to debit an identity with zero balance.

These exercise paths not testable through the public API directly. Define them as `_test.go` package-level tests using a tiny test-only constructor that invokes `appendEntry` with synthesized inputs.

- [ ] **Failing tests, impl, commit.**

```bash
git commit -m "$(cat <<'EOF'
feat(tracker/ledger): appendEntry chain-integrity glue

The shared core of every public Append* method. Holds Ledger.mu for
the entire critical section (tip-read → sign → storage write).
ErrStaleTip surfaces when caller's body has wrong prev_hash/seq;
ErrInsufficientBalance when applying the cost would drive a balance
below zero.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: `IssueStarterGrant` — public API + balance arithmetic

**Files:**
- Create: `tracker/internal/ledger/starter_grant.go`, `starter_grant_test.go`

```go
// IssueStarterGrant credits identityID with amount credits, signed by the
// tracker. STARTER_GRANT entries have no counterparty sigs — only tracker_sig.
func (l *Ledger) IssueStarterGrant(ctx context.Context, identityID []byte, amount uint64) (*tbproto.Entry, error) {
    if amount == 0 {
        return nil, errors.New("ledger: starter grant amount must be > 0")
    }
    // Read tip outside the lock to construct prev_hash + seq for the body.
    // Stale-tip check inside the lock catches concurrent appends. This means
    // a starter grant will retry up to N times under contention; for the
    // operator's startup sequence that's effectively single-threaded, so 0
    // contention in practice.
    return l.issueWithRetry(ctx, func(prevHash []byte, seq uint64) (appendInput, error) {
        now := uint64(l.nowFn().Unix())
        body, err := entry.BuildStarterGrantEntry(entry.StarterGrantInput{
            PrevHash:   prevHash,
            Seq:        seq,
            ConsumerID: identityID,
            Amount:     amount,
            Timestamp:  now,
        })
        if err != nil { return appendInput{}, fmt.Errorf("ledger: build starter_grant: %w", err) }
        return appendInput{
            body: body,
            deltas: []balanceDelta{{identityID: identityID, delta: int64(amount)}},
        }, nil
    })
}

// issueWithRetry handles the orchestrator-internal "read tip → build →
// append, retry on stale tip" loop for tracker-only-signed kinds where
// rebuilding is cheap. Bounded at 8 attempts so a misbehaving caller
// can't infinite-loop.
func (l *Ledger) issueWithRetry(ctx context.Context, build func(prevHash []byte, seq uint64) (appendInput, error)) (*tbproto.Entry, error) {
    const maxAttempts = 8
    for i := 0; i < maxAttempts; i++ {
        tipSeq, tipHash, hasTip, err := l.Tip(ctx)
        if err != nil { return nil, err }
        prev := tipHash
        nextSeq := tipSeq + 1
        if !hasTip { prev = make([]byte, 32); nextSeq = 1 }

        in, err := build(prev, nextSeq)
        if err != nil { return nil, err }

        e, err := l.appendEntry(ctx, in)
        if errors.Is(err, ErrStaleTip) { continue }
        return e, err
    }
    return nil, errors.New("ledger: too many stale-tip retries")
}
```

### 5.1 Tests

- `TestIssueStarterGrant_HappyPath` — issue → entry returned, body has correct kind, tracker_sig set, hash retrievable, balance row reflects credit.
- `TestIssueStarterGrant_RejectsZeroAmount`.
- `TestIssueStarterGrant_FirstEntryIsGenesis` — prev_hash = 32 zero bytes, seq = 1.
- `TestIssueStarterGrant_ChainsCorrectly` — two grants in sequence, second's prev_hash equals first's hash.
- `TestIssueStarterGrant_BalanceAccumulates` — three grants to same identity → balance is sum.

- [ ] **Failing tests, impl, commit.**

```bash
git commit -m "$(cat <<'EOF'
feat(tracker/ledger): IssueStarterGrant public API

Tracker-only-signed entries get a small retry loop on stale tips since
rebuilding the body is cheap and tracker is the only signer. Chains
correctly into the existing tip; first call on an empty chain
produces seq=1 with zero prev_hash.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: `AppendUsage` — multi-sig public API

**Files:**
- Create: `tracker/internal/ledger/usage.go`, `usage_test.go`

### 6.1 API

```go
// UsageRecord is the typed input to AppendUsage. Callers (the broker)
// have already collected consumerSig + seederSig over the body bytes
// derived from these fields plus the current tip's prev_hash + seq.
type UsageRecord struct {
    PrevHash     []byte // 32 bytes — must match current tip
    Seq          uint64 // must match current tip + 1
    ConsumerID   []byte
    SeederID     []byte
    Model        string
    InputTokens  uint32
    OutputTokens uint32
    CostCredits  uint64
    Timestamp    uint64
    RequestID    []byte
    ConsumerSig  []byte             // 64 bytes; or empty + ConsumerSigMissing flag
    ConsumerPub  ed25519.PublicKey  // 32 bytes; required if ConsumerSig is set
    SeederSig    []byte             // 64 bytes; required
    SeederPub    ed25519.PublicKey  // 32 bytes; required
    ConsumerSigMissing bool
}

// AppendUsage records a settled USAGE entry. The body the caller
// constructed must match the current tip exactly — orchestrator returns
// ErrStaleTip on mismatch and the caller (broker) re-collects sigs.
//
// Sig verification, balance arithmetic, and tracker signing happen
// here under Ledger.mu.
func (l *Ledger) AppendUsage(ctx context.Context, r UsageRecord) (*tbproto.Entry, error)
```

### 6.2 Implementation

```go
func (l *Ledger) AppendUsage(ctx context.Context, r UsageRecord) (*tbproto.Entry, error) {
    if len(r.SeederSig) == 0 || len(r.SeederPub) != ed25519.PublicKeySize {
        return nil, errors.New("ledger: AppendUsage requires seeder sig + pubkey")
    }
    if !r.ConsumerSigMissing {
        if len(r.ConsumerSig) == 0 || len(r.ConsumerPub) != ed25519.PublicKeySize {
            return nil, errors.New("ledger: AppendUsage requires consumer sig + pubkey (or set ConsumerSigMissing)")
        }
    }

    body, err := entry.BuildUsageEntry(entry.UsageInput{
        PrevHash: r.PrevHash, Seq: r.Seq,
        ConsumerID: r.ConsumerID, SeederID: r.SeederID,
        Model: r.Model, InputTokens: r.InputTokens, OutputTokens: r.OutputTokens,
        CostCredits: r.CostCredits, Timestamp: r.Timestamp,
        RequestID: r.RequestID, ConsumerSigMissing: r.ConsumerSigMissing,
    })
    if err != nil { return nil, fmt.Errorf("ledger: build usage: %w", err) }

    return l.appendEntry(ctx, appendInput{
        body:        body,
        consumerSig: r.ConsumerSig,
        consumerPub: r.ConsumerPub,
        seederSig:   r.SeederSig,
        seederPub:   r.SeederPub,
        deltas: []balanceDelta{
            {identityID: r.ConsumerID, delta: -int64(r.CostCredits)},
            {identityID: r.SeederID, delta: int64(r.CostCredits)},
        },
    })
}
```

### 6.3 Tests

- `TestAppendUsage_HappyPath` — issue starter grant → append usage → both balances reflect transfer.
- `TestAppendUsage_StaleTipReturnsErr` — pre-build body with wrong prev_hash → `ErrStaleTip`.
- `TestAppendUsage_RejectsBadConsumerSig` — sign body with wrong key → "consumer_sig invalid".
- `TestAppendUsage_RejectsBadSeederSig` — same for seeder.
- `TestAppendUsage_ConsumerSigMissingFlag` — usage with flag set + empty consumer_sig → succeeds with NULL consumer_sig in DB.
- `TestAppendUsage_InsufficientBalance` — usage cost > consumer's balance → `ErrInsufficientBalance`.

- [ ] **Failing tests, impl, commit.**

```bash
git commit -m "$(cat <<'EOF'
feat(tracker/ledger): AppendUsage with caller-pre-signed body

Caller (broker) pre-fills body.PrevHash + body.Seq matching current
tip and supplies consumer + seeder sigs over those bytes. Orchestrator
verifies sigs, signs with tracker key, applies the consumer-debit /
seeder-credit balance pair, and persists — all under Ledger.mu.
ErrStaleTip on mismatch lets the broker re-collect sigs.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 7: `AppendTransferOut`

**Files:**
- Create: `tracker/internal/ledger/transfer.go`, `transfer_test.go`

```go
type TransferOutRecord struct {
    PrevHash    []byte
    Seq         uint64
    ConsumerID  []byte // identity moving credits out
    Amount      uint64
    Timestamp   uint64
    TransferRef []byte // 32-byte transfer UUID
    ConsumerSig []byte
    ConsumerPub ed25519.PublicKey
}

func (l *Ledger) AppendTransferOut(ctx context.Context, r TransferOutRecord) (*tbproto.Entry, error)
```

Implementation parallels AppendUsage but with one balance delta (`-amount` on consumer) and only consumer_sig (no seeder).

Tests parallel AppendUsage's: happy path, stale tip, bad consumer sig, missing consumer sig (transfer_out cannot use the consumer_sig_missing flag — verified by the entry builder + VerifyAll), insufficient balance.

- [ ] **Failing tests, impl, commit.**

```bash
git commit -m "$(cat <<'EOF'
feat(tracker/ledger): AppendTransferOut

Single-counterparty (consumer-only) signed entry that debits the
consumer's balance for an outbound cross-region transfer. Federation
uses TransferRef (UUID) to link the receiving region's TRANSFER_IN
entry. v1 does not implement TRANSFER_IN — that's a follow-up plan
once the recipient-encoding question is resolved.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 8: `SignedBalance`

**Files:**
- Create: `tracker/internal/ledger/balance.go`, `balance_test.go`

```go
// SignedBalance returns the tracker-signed balance snapshot for identityID.
// Snapshot is internally consistent — credits and chain_tip_seq reflect
// the same point in chain history (storage.BalanceWithTip is one
// transaction). expires_at is issued_at + 600 (10 min) per ledger §3.4.
func (l *Ledger) SignedBalance(ctx context.Context, identityID []byte) (*tbproto.SignedBalanceSnapshot, error) {
    if len(identityID) != 32 {
        return nil, fmt.Errorf("ledger: identity_id length %d, want 32", len(identityID))
    }
    snap, err := l.store.BalanceWithTip(ctx, identityID)
    if err != nil { return nil, fmt.Errorf("ledger: BalanceWithTip: %w", err) }

    issued := uint64(l.nowFn().Unix())
    body := &tbproto.BalanceSnapshotBody{
        IdentityId:   identityID,
        Credits:      snap.Balance.Credits, // 0 if not found, which is correct for an untouched identity
        ChainTipHash: snap.TipHash,         // nil if empty chain
        ChainTipSeq:  snap.TipSeq,          // 0 if empty chain
        IssuedAt:     issued,
        ExpiresAt:    issued + balanceSnapshotTTLSeconds,
    }
    if !snap.TipFound {
        body.ChainTipHash = make([]byte, 32) // 32 zero bytes for empty-chain snapshots
    }
    sig, err := signing.SignBalanceSnapshot(l.trackerKey, body)
    if err != nil { return nil, fmt.Errorf("ledger: sign snapshot: %w", err) }
    return &tbproto.SignedBalanceSnapshot{Body: body, TrackerSig: sig}, nil
}

const balanceSnapshotTTLSeconds = 600
```

Tests:
- `TestSignedBalance_HappyPath_Issued` — returns snapshot with correct credits, sig verifies.
- `TestSignedBalance_EmptyChain_EmptyIdentity` — credits=0, tip_hash=zeros, sig verifies.
- `TestSignedBalance_AfterStarterGrant` — issue grant → snapshot reflects credit + tip seq.
- `TestSignedBalance_ExpiresAt600s` — expires_at - issued_at == 600.
- `TestSignedBalance_RejectsBadIdentityLength`.

- [ ] **Failing tests, impl, commit.**

```bash
git commit -m "$(cat <<'EOF'
feat(tracker/ledger): SignedBalance via storage.BalanceWithTip

Reads balance + tip in one storage transaction so chain_tip_seq and
credits reflect the same point in history. Tracker-signs the body;
expires 600s after issued_at per ledger §3.4. Snapshots for
untouched identities return credits=0 with the current tip — that's
"this identity has no entries yet" rather than an error.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 9: `AssertChainIntegrity` audit method + integration test

**Files:**
- Create: `tracker/internal/ledger/integration_test.go` (also add `AssertChainIntegrity` to `reads.go` or a new `audit.go`)

```go
// AssertChainIntegrity walks entries with seq in (sinceSeq, untilSeq],
// verifying each entry's prev_hash equals the previous entry's hash.
// Returns nil on a chain whose entries link correctly; an error
// describing the first break otherwise.
//
// untilSeq=0 means "up to current tip". Used by ad-hoc CI audits and
// the v2 startup-validation hook; not called on every read.
func (l *Ledger) AssertChainIntegrity(ctx context.Context, sinceSeq, untilSeq uint64) error
```

Implementation:
1. Page through `EntriesSince(sinceSeq, batchSize)` until untilSeq.
2. Track the previous entry's hash; compare to next entry's `prev_hash`.
3. First mismatch returns `fmt.Errorf("chain break at seq=%d: prev_hash=%x, expected=%x", ...)`.

### 9.1 Integration test cases

- `TestIntegration_ConcurrentStarterGrants` — 100 concurrent `IssueStarterGrant` calls produce a strictly increasing seq sequence with no gaps. Chain integrity holds.
- `TestIntegration_ChainIntegrityHoldsAfterMixedAppends` — 5 starter grants + 5 usage + 3 transfer_out, in arbitrary interleaving. `AssertChainIntegrity(0, 0)` returns nil.
- `TestIntegration_RestartPreservesChainIntegrity` — append → close ledger AND store → reopen both → AssertChainIntegrity passes.
- `TestAssertChainIntegrity_DetectsTamperedPrevHash` — manually corrupt one entry's `prev_hash` via raw SQL → audit fails with "chain break at seq=N".

- [ ] **Failing tests, impl, commit.**

```bash
git commit -m "$(cat <<'EOF'
feat(tracker/ledger): AssertChainIntegrity audit + integration tests

AssertChainIntegrity walks the chain, verifying prev_hash linkage.
Integration tests confirm concurrent appends produce a coherent
chain, mixed-kind sequences pass audit, and chain integrity survives
a restart. Manually-tampered prev_hash is detected at the first
break.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 10: Final verification + tag

- [ ] **Step 1: Repo-wide check**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/cut-sunflower
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" make test
```

Expected: shared + plugin + tracker green.

- [ ] **Step 2: Coverage spot-check**

```bash
cd tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -coverprofile=coverage.out ./internal/ledger/...
go tool cover -func=coverage.out | tail
```

Target: ≥ 85% on the orchestrator package. Concurrency error paths (DB closed mid-tx) are acceptable to leave uncovered.

- [ ] **Step 3: Lint passthrough**

```bash
cd tracker && PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go vet ./...
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" staticcheck ./internal/ledger/... 2>/dev/null || true
```

- [ ] **Step 4: Tag (optional)**

```bash
git tag -a tracker-ledger-v0 -m "Tracker ledger orchestrator v1 complete"
```

---

## Self-review

- **Spec coverage:** §4.1 append (lock + tip-read + atomic write), §4.2 balance (BalanceWithTip + signed snapshot), §3.4 SignedBalanceSnapshot expiry — all addressed. §4.3 (hourly Merkle publication) and §4.4 (equivocation detection) deferred to follow-up plan.
- **Cross-package phasing:** Task 1 lands one method in `storage`; Tasks 2–9 stay in `internal/ledger`. The storage extension is small enough not to warrant its own plan.
- **Append flow contract:** Pre-built body with seq+prev_hash matching current tip. Stale-tip surfaces as `ErrStaleTip`; broker retries. Holding the lock across a 15-min settlement timeout was rejected upfront (plan §3.2).
- **Balance arithmetic:** Lives in orchestrator (`applyDeltas`), not storage. Storage trusts the orchestrator's computed `BalanceUpdate` values. v1 hard-codes the starter-grant overdraft band to 0; future config can override.
- **Genesis:** Lazy. First append on empty chain uses zero prev_hash + seq=1. No genesis sidecar in v1.
- **Reads do not verify:** Consistent with the storage plan — read passthroughs return entries verbatim. AssertChainIntegrity (Task 9) is the explicit audit hook.
- **Placeholders:** None. Every task ends green.

## Out of scope — next plans

1. **Hourly Merkle publication** (`internal/ledger/merkle_publisher.go`): a goroutine that wakes on hour boundaries, computes the root over `LeavesForHour`, calls `PutMerkleRoot`, and hands the triple to federation. Needs a clock and a shutdown hook (Ledger.Close grows a real body).

2. **TRANSFER_IN orchestrator method:** Open question — the entry validator requires `consumer_id` to be 32 zero bytes for TRANSFER_IN, so the recipient identity isn't on the entry itself. Either (a) amend the validator to allow consumer_id to identify the recipient, or (b) the orchestrator method takes recipient as a separate parameter and applies the credit outside the entry's body fields. Resolve before implementing.

3. **Equivocation detection:** consumes `peer_root_archive`. When a peer's gossiped root for an hour conflicts with our locally-computed root, generate `EquivocationEvidence` and trigger depeer. Federation-adjacent.

4. **Federation transfer-proof verification:** `apply_transfer_in(transfer_proof)` from the federation spec. Verifies the source tracker's published root + the inclusion proof + the consumer's transfer_out signature, then issues a TRANSFER_IN. Depends on (2) and on the federation module which doesn't exist yet.
