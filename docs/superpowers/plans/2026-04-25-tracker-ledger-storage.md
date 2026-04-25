# Tracker — `internal/ledger/storage` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land the SQLite-backed durable layer for the tracker's append-only ledger. End state: the `internal/ledger` orchestrator (next plan) can call a small typed API — `OpenStore`, `AppendEntry`, `Tip`, `EntryBySeq`, `EntryByHash`, `EntriesSince`, `Balance`, `LeavesForHour`, `PutMerkleRoot`, `GetMerkleRoot`, `PutPeerRoot`, peer-root lookups — and trust that each `AppendEntry` is atomic across the entries / balances tables.

**Architecture:** Pure-Go SQLite via `modernc.org/sqlite` (no cgo). WAL mode for durability. Schema follows ledger spec §5.2 with one simplification: no separate `merkle_leaves` table — entry hashes live in the `entries` table already, and "leaves for hour H" is a `SELECT hash FROM entries WHERE timestamp BETWEEN ? AND ?` query. Single `Store` struct holds the `*sql.DB` handle and a `sync.Mutex` that serializes writes (matching ledger spec §4.1's "ledger write lock"); reads run concurrently. Storage owns the schema; everything above it (chain hashing, balance arithmetic, signing) lives in `internal/ledger` (next plan).

**Tech Stack:** Go 1.23+, `modernc.org/sqlite` (new tracker dep), `database/sql`, `github.com/stretchr/testify`. No third-party migration tool — a tiny in-package `applyMigrations` helper writes the v1 schema; v2+ migrations are out of scope.

**Specs:**
- `docs/superpowers/specs/ledger/2026-04-22-ledger-design.md` §3 (data model), §4.1 (append), §5 (storage backend), §6 (failure handling).

**Dependency order:** Runs after `internal/ledger/entry` (just landed). The eventual `internal/ledger` orchestrator will import this package + `entry` + `signing` + a clock.

---

## 1. File map

```
tracker/
├── go.mod                                       ← MODIFY: add modernc.org/sqlite
├── internal/ledger/storage/
│   ├── doc.go                                   ← CREATE: package doc + non-negotiables
│   ├── schema.go                                ← CREATE: embedded SQL DDL + migration runner
│   ├── schema_test.go                           ← CREATE: schema applies on empty DB; idempotent
│   ├── store.go                                 ← CREATE: Store struct + Open/Close + write mutex
│   ├── store_test.go                            ← CREATE: open/close lifecycle tests
│   ├── tip.go                                   ← CREATE: Tip query
│   ├── tip_test.go                              ← CREATE
│   ├── append.go                                ← CREATE: AppendEntry (atomic txn)
│   ├── append_test.go                           ← CREATE: atomicity, idempotency by hash, balance update
│   ├── lookup.go                                ← CREATE: EntryBySeq / EntryByHash / Balance
│   ├── lookup_test.go                           ← CREATE
│   ├── stream.go                                ← CREATE: EntriesSince iterator
│   ├── stream_test.go                           ← CREATE: cursor semantics
│   ├── merkle.go                                ← CREATE: LeavesForHour / PutMerkleRoot / GetMerkleRoot
│   ├── merkle_test.go                           ← CREATE
│   ├── peer_roots.go                            ← CREATE: PutPeerRoot / GetPeerRoot / ListPeerRootsForHour
│   ├── peer_roots_test.go                       ← CREATE
│   └── testing.go                               ← CREATE: openTempStore(t) helper, _test.go-only via build tag
└── internal/ledger/storage/.gitkeep             ← REMOVE
```

Notes:
- One Go file per area of responsibility, keeping each test file scoped to its sibling.
- `testing.go` uses `//go:build test_helpers` is wrong here — it just needs to live in `_test.go`. Use a package-internal test helper file: `helpers_test.go`.
- No `testdata/` directory: SQLite tests use `t.TempDir()` + a fresh DB per test, so no fixtures to commit.
- All operations take `context.Context` so the eventual orchestrator can wire up timeouts and cancellation.

## 2. Conventions used in this plan

- All `go test` commands run from `/Users/dor.amid/.superset/worktrees/token-bay/cut-sunflower/tracker`.
- `PATH="$HOME/.local/share/mise/shims:$PATH"` if Go is managed by mise.
- One commit per task. Conventional-commit prefixes: `feat(tracker/ledger/storage):`, `chore(tracker):`, `test(tracker/ledger/storage):`.
- Co-Authored-By footer: `Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>`.
- Each task: failing test first (red), minimal impl (green), commit. Refactor-only commits get `refactor:`.
- SQLite tests pattern: `db := openTempStore(t)` returns a fresh `*Store` against a tempdir DB. `t.Cleanup` closes it. **Never share a DB file across tests** (`tracker/CLAUDE.md` rule).

---

## 3. Schema (decisions used by all tasks)

### 3.1 v1 DDL

```sql
-- v1 schema applied by applyMigrations in schema.go.
-- Migrations are append-only: a v2 schema lands as a separate set of statements,
-- never as ALTER on existing v1 tables. v1 tables are forever.

CREATE TABLE IF NOT EXISTS schema_version (
    version INTEGER PRIMARY KEY
);
INSERT OR IGNORE INTO schema_version(version) VALUES (1);

CREATE TABLE IF NOT EXISTS entries (
    seq           INTEGER PRIMARY KEY,            -- monotonic, strictly increasing
    prev_hash     BLOB NOT NULL,                  -- 32 bytes
    hash          BLOB NOT NULL UNIQUE,           -- SHA-256(canonical body)
    kind          INTEGER NOT NULL,               -- proto enum value
    consumer_id   BLOB NOT NULL,                  -- 32 bytes; zero for some kinds
    seeder_id     BLOB NOT NULL,                  -- 32 bytes; zero for some kinds
    model         TEXT NOT NULL DEFAULT '',
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cost_credits  INTEGER NOT NULL,
    timestamp     INTEGER NOT NULL,               -- unix seconds
    request_id    BLOB NOT NULL,                  -- 16 bytes; zero for non-usage
    flags         INTEGER NOT NULL DEFAULT 0,
    ref           BLOB NOT NULL,                  -- 32 bytes
    consumer_sig  BLOB,                           -- nullable per per-kind matrix
    seeder_sig    BLOB,                           -- nullable per per-kind matrix
    tracker_sig   BLOB NOT NULL,                  -- 64 bytes; mandatory
    canonical     BLOB NOT NULL                   -- DeterministicMarshal(EntryBody)
);

CREATE INDEX IF NOT EXISTS idx_entries_consumer ON entries(consumer_id, seq);
CREATE INDEX IF NOT EXISTS idx_entries_seeder   ON entries(seeder_id, seq);
CREATE INDEX IF NOT EXISTS idx_entries_time     ON entries(timestamp);

CREATE TABLE IF NOT EXISTS balances (
    identity_id BLOB PRIMARY KEY,                 -- 32 bytes
    credits     INTEGER NOT NULL,                 -- signed; negative allowed within starter grant band
    last_seq    INTEGER NOT NULL,                 -- chain position of last entry touching this identity
    updated_at  INTEGER NOT NULL                  -- unix seconds
);

CREATE TABLE IF NOT EXISTS merkle_roots (
    hour        INTEGER PRIMARY KEY,              -- unix seconds floor-divided by 3600
    root        BLOB NOT NULL,                    -- 32 bytes
    tracker_sig BLOB NOT NULL                     -- 64 bytes
);

CREATE TABLE IF NOT EXISTS peer_root_archive (
    tracker_id  BLOB NOT NULL,                    -- 32 bytes — peer tracker identity
    hour        INTEGER NOT NULL,
    root        BLOB NOT NULL,
    sig         BLOB NOT NULL,
    received_at INTEGER NOT NULL,
    PRIMARY KEY (tracker_id, hour)
);
```

### 3.2 Why one DB, one writer

`Store` serializes writes via a `sync.Mutex` matching ledger §4.1's "take the ledger write lock". Reads do not contend on this mutex — SQLite WAL allows readers concurrent with one writer. For v1 single-process trackers this is enough; horizontal scaling is deferred (ledger spec §7).

### 3.3 PRAGMAs

`Open` runs:
- `PRAGMA journal_mode=WAL` — durable + read-while-write.
- `PRAGMA synchronous=NORMAL` — `FULL` is overkill for WAL; `NORMAL` is the documented WAL pairing.
- `PRAGMA foreign_keys=ON` — defensive, no FKs in v1 but cheap.
- `PRAGMA busy_timeout=5000` — 5s for transient lock contention before failing.

These run inside `applyMigrations` so the file is properly initialized before any write.

### 3.4 What storage does NOT validate

Storage trusts its caller. It does not:
- Verify `entry.hash == sha256(canonical)` — orchestrator computes both and passes consistent values.
- Verify any signature.
- Verify `prev_hash == previous tip hash` — orchestrator does this with the tip query.
- Compute balance deltas — orchestrator passes in the new credits/last_seq pair.

Storage's invariants are local: row shapes valid, primary keys monotonic, transactions atomic. Anything else is the orchestrator's job.

---

## Task 1: Tracker SQLite dependency

**Files:**
- Modify: `tracker/go.mod`, `tracker/go.sum`

- [ ] **Step 1: Add the dep**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/cut-sunflower/tracker
PATH="$HOME/.local/share/mise/shims:$PATH" go get modernc.org/sqlite@latest
PATH="$HOME/.local/share/mise/shims:$PATH" go mod tidy
```

- [ ] **Step 2: Verify build**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/cut-sunflower/tracker
PATH="$HOME/.local/share/mise/shims:$PATH" go build ./...
```

Expected: clean build (no source uses sqlite yet, but the dep resolves).

- [ ] **Step 3: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/cut-sunflower
git add tracker/go.mod tracker/go.sum go.work.sum
git commit -m "$(cat <<'EOF'
chore(tracker): add modernc.org/sqlite dependency

Pure-Go SQLite driver picked in tracker scaffolding plan §1.1 for the
ledger storage backend. No cgo, ships cleanly cross-platform.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: Schema + migrations

**Files:**
- Create: `tracker/internal/ledger/storage/doc.go`, `tracker/internal/ledger/storage/schema.go`, `tracker/internal/ledger/storage/schema_test.go`, `tracker/internal/ledger/storage/helpers_test.go`

The schema runner is what every other test will invoke first via `openTempStore(t)`, so this is the leaf foundation.

- [ ] **Step 1: Write `doc.go`**

```go
// Package storage owns the SQLite-backed durable layer for the
// tracker's append-only ledger.
//
// Spec: docs/superpowers/specs/ledger/2026-04-22-ledger-design.md
//   §3 (data model), §4.1 (append algorithm), §5 (storage backend).
//
// Non-negotiables:
//
//  1. AppendEntry is atomic across all touched tables — entries,
//     balances, and (when applicable) merkle_roots — within a single
//     SQLite transaction.
//  2. The chain is append-only. There are no UPDATE or DELETE statements
//     against entries or merkle_roots. Balances are projections and
//     CAN be updated; the chain replay is the source of truth.
//  3. Storage trusts its caller. It validates row shape (lengths,
//     non-null) but does NOT verify hashes, signatures, balance math,
//     or chain linkage. The orchestrator (internal/ledger) does.
//  4. Writes are serialized through Store.writeMu. Reads do not take
//     the mutex — SQLite WAL allows concurrent readers.
package storage
```

- [ ] **Step 2: Write the failing schema test**

`schema_test.go`:

```go
package storage

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestApplyMigrations_CreatesV1Tables(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/ledger.db?_pragma=journal_mode(WAL)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	require.NoError(t, applyMigrations(ctx, db))

	for _, name := range []string{"schema_version", "entries", "balances", "merkle_roots", "peer_root_archive"} {
		var got string
		err := db.QueryRowContext(ctx,
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&got)
		require.NoError(t, err, "table %s missing", name)
		assert.Equal(t, name, got)
	}

	var version int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT version FROM schema_version").Scan(&version))
	assert.Equal(t, 1, version)
}

func TestApplyMigrations_Idempotent(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/ledger.db")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	require.NoError(t, applyMigrations(ctx, db))
	require.NoError(t, applyMigrations(ctx, db), "running twice must not error")

	var n int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_version").Scan(&n))
	assert.Equal(t, 1, n, "schema_version row should exist exactly once")
}
```

- [ ] **Step 3: Implement `schema.go`**

```go
package storage

import (
	"context"
	"database/sql"
	"fmt"
	_ "embed"
)

//go:embed schema_v1.sql
var schemaV1 string

// applyMigrations brings the database up to the latest schema. Idempotent.
// In v1 there is exactly one migration; v2 will append additional embedded
// .sql files and run them conditionally on schema_version.
func applyMigrations(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, schemaV1); err != nil {
		return fmt.Errorf("storage: apply v1 schema: %w", err)
	}
	return nil
}
```

Add `tracker/internal/ledger/storage/schema_v1.sql` with the DDL from §3.1 of this plan.

- [ ] **Step 4: Helpers file**

`helpers_test.go`:

```go
package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// openTempDB returns a SQLite handle backed by a fresh tempdir-scoped file
// with v1 migrations applied. Closes automatically via t.Cleanup.
//
// Used by tests in this package that need raw SQL access without the Store
// wrapper. Higher-level tests use openTempStore (added in Task 3).
func openTempDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "ledger.db") +
		"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, applyMigrations(context.Background(), db))
	return db
}
```

- [ ] **Step 5: Remove .gitkeep, run tests**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/cut-sunflower/tracker
rm internal/ledger/storage/.gitkeep
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race ./internal/ledger/storage/...
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add tracker/internal/ledger/storage/
git commit -m "$(cat <<'EOF'
feat(tracker/ledger/storage): v1 schema + idempotent migrations

Embeds the v1 DDL: entries, balances, merkle_roots, peer_root_archive,
schema_version. applyMigrations is idempotent so reopening a DB or
restarting the tracker is safe. Schema decisions in plan §3.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: Store struct + Open/Close

**Files:**
- Create: `tracker/internal/ledger/storage/store.go`, `tracker/internal/ledger/storage/store_test.go`
- Modify: `tracker/internal/ledger/storage/helpers_test.go` (add `openTempStore`)

- [ ] **Step 1: Write failing test**

`store_test.go`:

```go
package storage

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpen_CreatesFileAndAppliesSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	s, err := Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	// Schema applied: schema_version table accessible via Tip-style probe.
	row := s.db.QueryRow("SELECT version FROM schema_version")
	var v int
	require.NoError(t, row.Scan(&v))
	assert.Equal(t, 1, v)
}

func TestOpen_ReusesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	s1, err := Open(context.Background(), path)
	require.NoError(t, err)
	require.NoError(t, s1.Close())

	s2, err := Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })

	var v int
	require.NoError(t, s2.db.QueryRow("SELECT version FROM schema_version").Scan(&v))
	assert.Equal(t, 1, v, "reopening must not duplicate schema rows")
}

func TestOpen_RejectsEmptyPath(t *testing.T) {
	_, err := Open(context.Background(), "")
	require.Error(t, err)
}

func TestClose_Idempotent(t *testing.T) {
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "ledger.db"))
	require.NoError(t, err)
	require.NoError(t, s.Close())
	require.NoError(t, s.Close(), "closing twice should not error")
}
```

- [ ] **Step 2: Implement `store.go`**

```go
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	_ "modernc.org/sqlite"
)

// Store is the typed durable layer over a SQLite ledger DB. One Store per
// tracker process; the orchestrator owns its lifecycle.
type Store struct {
	db      *sql.DB
	writeMu sync.Mutex // serializes append-class operations; reads do not take this
	closed  bool
	closeMu sync.Mutex
}

// Open opens (or creates) the ledger DB at path, applies migrations, and
// returns a ready Store. Caller is responsible for calling Close.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("storage: Open requires a non-empty path")
	}
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: sql.Open: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: ping: %w", err)
	}
	if err := applyMigrations(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close releases the underlying SQLite handle. Safe to call multiple times.
func (s *Store) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.db.Close()
}
```

- [ ] **Step 3: Add `openTempStore` to helpers**

Append to `helpers_test.go`:

```go
// openTempStore returns a fresh Store backed by a tempdir-scoped DB file.
// Closes automatically via t.Cleanup.
func openTempStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.db")
	s, err := Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}
```

- [ ] **Step 4: Run tests, commit**

```bash
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race ./internal/ledger/storage/...
```

```bash
git add tracker/internal/ledger/storage/store.go tracker/internal/ledger/storage/store_test.go tracker/internal/ledger/storage/helpers_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/ledger/storage): Store with Open/Close lifecycle

Open applies migrations and configures WAL+busy-timeout PRAGMAs; Close
is idempotent. writeMu lives on the Store but isn't taken yet — append
plumbing in the next commits will use it.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: `Tip` query

**Files:**
- Create: `tracker/internal/ledger/storage/tip.go`, `tip_test.go`

`Tip` returns `(seq, hash, ok, error)` — `ok=false` on an empty chain. The orchestrator uses this to compute `prev_hash` for the next entry.

- [ ] **Step 1: Failing test**

`tip_test.go`:

```go
package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTip_EmptyChain(t *testing.T) {
	s := openTempStore(t)
	seq, hash, ok, err := s.Tip(context.Background())
	require.NoError(t, err)
	assert.False(t, ok, "empty chain has no tip")
	assert.Zero(t, seq)
	assert.Nil(t, hash)
}

// Tip after AppendEntry covered in append_test.go (Task 5).
```

- [ ] **Step 2: Implement `tip.go`**

```go
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Tip returns the seq and hash of the highest-numbered entry in the chain.
// Returns ok=false (and zero values) if the chain is empty.
func (s *Store) Tip(ctx context.Context) (seq uint64, hash []byte, ok bool, err error) {
	row := s.db.QueryRowContext(ctx, `SELECT seq, hash FROM entries ORDER BY seq DESC LIMIT 1`)
	var rawHash []byte
	err = row.Scan(&seq, &rawHash)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil, false, nil
	}
	if err != nil {
		return 0, nil, false, fmt.Errorf("storage: Tip: %w", err)
	}
	return seq, rawHash, true, nil
}
```

- [ ] **Step 3: Run + commit**

```bash
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race ./internal/ledger/storage/...
git add tracker/internal/ledger/storage/tip.go tracker/internal/ledger/storage/tip_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/ledger/storage): Tip query

Returns the highest-numbered entry's seq + hash, or ok=false on an
empty chain. Orchestrator uses this to compute the next entry's
prev_hash.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: `AppendEntry` (atomic transaction)

**Files:**
- Create: `tracker/internal/ledger/storage/append.go`, `append_test.go`

This is the keystone task. `AppendEntry` takes a fully-built `*tbproto.Entry` plus the typed balance updates the orchestrator computed, and writes them in one SQLite transaction. The function is the only writer-class operation in v1 — it acquires `writeMu` for the duration.

### 5.1 API shape

```go
// BalanceUpdate is what the orchestrator tells storage to apply during
// AppendEntry. Storage does NOT compute these values; it simply persists
// the (identity_id, credits, last_seq, updated_at) row by upsert.
type BalanceUpdate struct {
	IdentityID []byte // 32 bytes
	Credits    int64
	LastSeq    uint64
	UpdatedAt  uint64
}

// AppendInput bundles the entry plus the balance projections that must
// be updated atomically with it.
type AppendInput struct {
	Entry    *tbproto.Entry  // must have Body and a tracker_sig; not validated here
	Hash     [32]byte        // entry hash; orchestrator passes the hash it already computed
	Balances []BalanceUpdate // 0..2 rows; usage entries touch 2, transfers/grants touch 1
}

// AppendEntry persists one ledger entry plus its balance projections in a
// single durable transaction. Returns the entry's seq.
//
// Pre-conditions (orchestrator's responsibility, not re-checked here):
//   - entry.Body.PrevHash matches the current Tip's hash (or is zero on genesis)
//   - entry.Body.Seq equals current Tip seq + 1 (or 1 on genesis)
//   - hash matches sha256(DeterministicMarshal(entry.Body))
//   - all signatures pass entry.VerifyAll
//   - balance updates reflect the new credits after applying entry.Body.CostCredits
//
// Errors:
//   - duplicate hash → ErrDuplicateHash (the entries.hash UNIQUE constraint fires)
//   - duplicate seq → ErrDuplicateSeq (entries.seq PRIMARY KEY fires)
//   - any other DB error wrapped as fmt.Errorf
func (s *Store) AppendEntry(ctx context.Context, in AppendInput) (seq uint64, err error)
```

### 5.2 Why two error sentinels

`ErrDuplicateHash` lets the orchestrator detect "we already appended this entry" (idempotent retry path) without inspecting SQL error strings. `ErrDuplicateSeq` separately signals "concurrent writers raced past us" — for v1 with a single writer holding `writeMu` this means a programmer error, but defining it now keeps the contract clean if v2 splits writers.

### 5.3 Test cases

- happy-path append on empty chain → seq=1, balances upserted, Tip reflects new entry
- append on chain with N entries → seq=N+1
- entries with no balance updates (e.g., transfer_in pre-projection) — confirm 0 rows in `balances` for an empty `Balances` slice
- multiple balance updates in one append (usage entry: consumer + seeder) — confirm both rows present
- attempting same hash twice → ErrDuplicateHash, no second row written
- writer mutex serializes concurrent calls (use `errgroup` to call AppendEntry from N goroutines; the SUM of seq values should be 1+2+...+N — a single sequence)
- transaction rollback on conflict — try to append with a duplicate hash, then verify Tip is unchanged

(Tests in `append_test.go`; full enumeration in implementation.)

### 5.4 Implementation sketch

```go
// (in append.go)

var (
	ErrDuplicateHash = errors.New("storage: entry with this hash already exists")
	ErrDuplicateSeq  = errors.New("storage: entry with this seq already exists")
)

func (s *Store) AppendEntry(ctx context.Context, in AppendInput) (uint64, error) {
	if in.Entry == nil || in.Entry.Body == nil {
		return 0, errors.New("storage: AppendEntry requires non-nil entry + body")
	}
	body := in.Entry.Body

	// Marshal the canonical bytes once. Orchestrator computed Hash from these
	// same bytes, so we re-marshal here to persist them verbatim.
	canonical, err := signing.DeterministicMarshal(body)
	if err != nil {
		return 0, fmt.Errorf("storage: marshal canonical: %w", err)
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("storage: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
        INSERT INTO entries (
          seq, prev_hash, hash, kind, consumer_id, seeder_id, model,
          input_tokens, output_tokens, cost_credits, timestamp,
          request_id, flags, ref, consumer_sig, seeder_sig, tracker_sig, canonical
        ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		body.Seq, body.PrevHash, in.Hash[:], int32(body.Kind),
		body.ConsumerId, body.SeederId, body.Model,
		body.InputTokens, body.OutputTokens, body.CostCredits, body.Timestamp,
		body.RequestId, body.Flags, body.Ref,
		nullableBytes(in.Entry.ConsumerSig), nullableBytes(in.Entry.SeederSig),
		in.Entry.TrackerSig, canonical,
	); err != nil {
		return 0, classifyAppendError(err)
	}

	for _, b := range in.Balances {
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO balances (identity_id, credits, last_seq, updated_at)
            VALUES (?,?,?,?)
            ON CONFLICT(identity_id) DO UPDATE SET
                credits   = excluded.credits,
                last_seq  = excluded.last_seq,
                updated_at = excluded.updated_at
        `, b.IdentityID, b.Credits, b.LastSeq, b.UpdatedAt); err != nil {
			return 0, fmt.Errorf("storage: upsert balance: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("storage: commit: %w", err)
	}
	return body.Seq, nil
}

// nullableBytes converts an empty signature slice to NULL so the DB stores
// "no signature" distinguishably from "an empty 64-byte signature" (which
// the per-kind matrix never produces but the receiver still rejects).
func nullableBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// classifyAppendError maps SQLite UNIQUE / PRIMARY KEY violations to
// the package's typed error sentinels. Other errors are wrapped verbatim.
func classifyAppendError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "UNIQUE constraint failed: entries.hash"):
		return ErrDuplicateHash
	case strings.Contains(msg, "PRIMARY KEY") && strings.Contains(msg, "entries"):
		return ErrDuplicateSeq
	default:
		return fmt.Errorf("storage: append insert: %w", err)
	}
}
```

(Imports: `strings`, `errors`, `fmt`, `context`, `database/sql`, `signing`.)

### 5.5 Test fixtures

To keep `append_test.go` readable, define one helper in `helpers_test.go`:

```go
// builtUsageInput returns a minimal AppendInput for a USAGE entry at seq=N
// with the given prev_hash. The orchestrator equivalents live in
// internal/ledger; this is enough for storage to exercise its writes.
func builtUsageInput(t *testing.T, seq uint64, prevHash []byte) AppendInput {
    // build via entry.BuildUsageEntry; sign with throwaway keys; compute
    // entry.Hash; return AppendInput with two balance updates
    // (consumer debit, seeder credit).
}
```

(Full body in implementation; uses `entry.BuildUsageEntry`, `signing.SignEntry`, and `entry.Hash`.)

- [ ] **Step 1: Failing test for happy path** — write `TestAppendEntry_HappyPath` first; it fails with `undefined: Store.AppendEntry`.
- [ ] **Step 2: Implement** per §5.4.
- [ ] **Step 3: Tests for** duplicate hash, duplicate seq (synthetic — bypass the writeMu), concurrent writers serialized, multi-balance row, no-balance edge case, rollback on conflict.
- [ ] **Step 4: Run** `go test -race ./internal/ledger/storage/...`.
- [ ] **Step 5: Commit**

```bash
git add tracker/internal/ledger/storage/append.go tracker/internal/ledger/storage/append_test.go tracker/internal/ledger/storage/helpers_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/ledger/storage): AppendEntry atomic write

One SQLite txn writes the entry row + N balance upserts. Concurrent
writers serialized through Store.writeMu so seq sequencing is total.
Typed sentinels ErrDuplicateHash / ErrDuplicateSeq let the orchestrator
distinguish idempotent-retry from programmer-error without parsing SQL
error strings.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: Lookups — `EntryBySeq`, `EntryByHash`, `Balance`

**Files:**
- Create: `tracker/internal/ledger/storage/lookup.go`, `lookup_test.go`

Each lookup returns `(value, ok, error)` — `ok=false` on a clean miss, `error` only on actual DB problems. The eventual orchestrator wraps these with NotFound sentinels at its boundary; storage stays neutral.

API:

```go
func (s *Store) EntryBySeq(ctx context.Context, seq uint64) (*tbproto.Entry, bool, error)
func (s *Store) EntryByHash(ctx context.Context, hash []byte) (*tbproto.Entry, bool, error)
func (s *Store) Balance(ctx context.Context, identityID []byte) (BalanceRow, bool, error)
```

```go
// BalanceRow mirrors the balances table.
type BalanceRow struct {
    IdentityID []byte
    Credits    int64
    LastSeq    uint64
    UpdatedAt  uint64
}
```

Both entry lookups reconstruct the wire `*tbproto.Entry` from the row's `canonical` blob (parsed back into `EntryBody`) plus the three signature columns. This avoids a second source of truth: every column except canonical is denormalized from `canonical` for indexing, but the canonical bytes win on parse.

Tests:
- Happy-path round trip: append, lookup, fields match.
- Empty signatures (NULL) come back as zero-length slices on `tbproto.Entry`, not nil-vs-empty mismatch.
- Miss returns `ok=false, err=nil`.
- Garbage `canonical` (manually corrupted via raw SQL) → returns wrapped unmarshal error (covers the err != nil branch).

- [ ] **Step 1: Failing tests, then impl, then commit.**

```bash
git add tracker/internal/ledger/storage/lookup.go tracker/internal/ledger/storage/lookup_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/ledger/storage): EntryBySeq / EntryByHash / Balance lookups

Entry lookups parse from the row's canonical blob so the on-disk
canonical is the single source of truth — indexed columns are
projections. ok=false signals clean miss; errors only on real DB
problems.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 7: `EntriesSince` stream

**Files:**
- Create: `tracker/internal/ledger/storage/stream.go`, `stream_test.go`

Used by federation gossip and audit/reputation replay. Returns entries with `seq > sinceSeq` in ascending order. v1 returns a slice; if a region produces enough entries that buffering matters, v2 swaps to a row iterator with the same name.

```go
// EntriesSince returns up to limit entries with seq strictly greater than
// sinceSeq, ordered ascending by seq. limit=0 means "no limit" — callers
// should usually pass a sensible cap (e.g., 1000) to avoid loading the
// whole chain into memory.
func (s *Store) EntriesSince(ctx context.Context, sinceSeq uint64, limit int) ([]*tbproto.Entry, error)
```

Tests:
- `sinceSeq=0` returns everything.
- `sinceSeq=tip` returns empty.
- `limit=2` on a chain of 5 returns the next 2 (seqs `sinceSeq+1, sinceSeq+2`).
- Order is strictly ascending.
- Garbage canonical row → wrapped error (corrupt-row path).

- [ ] **Failing test, impl, commit.**

```bash
git commit -m "$(cat <<'EOF'
feat(tracker/ledger/storage): EntriesSince streaming query

Returns entries with seq > sinceSeq, ordered ascending, capped at limit.
Federation gossip + audit replay use this to catch up. v1 buffers into
a slice; v2 may switch to a row iterator without changing the contract.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 8: Merkle leaves + roots

**Files:**
- Create: `tracker/internal/ledger/storage/merkle.go`, `merkle_test.go`

```go
// LeavesForHour returns the entry hashes whose timestamp falls in the hour
// [hour*3600, (hour+1)*3600), ordered by seq ascending. The orchestrator
// computes the Merkle root over these leaves.
func (s *Store) LeavesForHour(ctx context.Context, hour uint64) ([][]byte, error)

// PutMerkleRoot persists an hourly Merkle root + tracker signature.
// Idempotent on duplicate hour: returns ErrDuplicateMerkleRoot.
func (s *Store) PutMerkleRoot(ctx context.Context, hour uint64, root []byte, trackerSig []byte) error

// GetMerkleRoot returns the persisted root for an hour, or ok=false on miss.
func (s *Store) GetMerkleRoot(ctx context.Context, hour uint64) (root, sig []byte, ok bool, err error)
```

Tests:
- LeavesForHour boundary semantics (start-inclusive, end-exclusive).
- LeavesForHour on empty hour → empty slice, no error.
- Put then Get round-trip.
- Duplicate Put → ErrDuplicateMerkleRoot, original row unchanged.

- [ ] **Failing tests, impl, commit.**

```bash
git commit -m "$(cat <<'EOF'
feat(tracker/ledger/storage): Merkle leaves + hourly roots

LeavesForHour bounded [hour*3600, (hour+1)*3600). PutMerkleRoot is
write-once per hour — duplicates return ErrDuplicateMerkleRoot so the
orchestrator can distinguish "already published" from "new failure".

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 9: Peer root archive

**Files:**
- Create: `tracker/internal/ledger/storage/peer_roots.go`, `peer_roots_test.go`

```go
type PeerRoot struct {
    TrackerID  []byte
    Hour       uint64
    Root       []byte
    Sig        []byte
    ReceivedAt uint64
}

func (s *Store) PutPeerRoot(ctx context.Context, p PeerRoot) error // idempotent on (tracker_id, hour)
func (s *Store) GetPeerRoot(ctx context.Context, trackerID []byte, hour uint64) (PeerRoot, bool, error)
func (s *Store) ListPeerRootsForHour(ctx context.Context, hour uint64) ([]PeerRoot, error)
```

Note: `PutPeerRoot` is idempotent only when the new row matches the existing one byte-for-byte. Conflicting roots for the same `(tracker_id, hour)` are equivocation evidence (ledger spec §4.4) — return `ErrPeerRootConflict` carrying both the existing and incoming roots so the equivocation-detection layer (out of scope here) can act on it.

```go
// ErrPeerRootConflict is returned when PutPeerRoot tries to write a row
// for a (tracker_id, hour) that already has a different root or sig.
// The wrapping error string includes both the existing and incoming
// hex-encoded roots so the equivocation-detection layer doesn't have
// to query separately.
var ErrPeerRootConflict = errors.New("storage: peer root conflict")
```

Tests:
- Put then Get round-trip.
- Idempotent put with identical row → no error, single row.
- Conflicting put → ErrPeerRootConflict; original row unchanged.
- ListPeerRootsForHour returns all peers' roots for that hour.

- [ ] **Failing tests, impl, commit.**

```bash
git commit -m "$(cat <<'EOF'
feat(tracker/ledger/storage): peer root archive + conflict detection

PutPeerRoot is idempotent on identical rows but returns
ErrPeerRootConflict when a (tracker_id, hour) row already has a
different root — that's the raw signal the equivocation-detection
layer needs (ledger spec §4.4).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 10: Restart durability integration test

**Files:**
- Create: `tracker/internal/ledger/storage/integration_test.go`

End-to-end at the storage layer:
1. Open a fresh Store, append 10 USAGE entries with synthetic balances.
2. Close.
3. Reopen the same path.
4. Tip returns seq=10.
5. EntriesSince(0, 100) returns all 10 in order.
6. Each balance row matches what was last upserted.
7. AppendEntry continues at seq=11.

This locks in the "WAL durability" promise from spec §6 ("Partial write (crash mid-transaction) … entry either visible or not"). It does not simulate a crash — that's hard to do reliably in Go — but it does verify the close/reopen path preserves everything.

- [ ] **Failing test, impl (no new code, just the integration test using existing methods), commit.**

```bash
git commit -m "$(cat <<'EOF'
test(tracker/ledger/storage): restart durability integration test

Append → Close → Reopen round-trip preserves all entries, balances,
and the tip. Locks the WAL durability promise from ledger spec §6
without simulating a real crash (which Go can't do portably).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 11: Final verification + tag

- [ ] **Step 1: Repo-wide check**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/cut-sunflower
PATH="$HOME/.local/share/mise/shims:$PATH" make test
```

Expected: shared + plugin + tracker green.

- [ ] **Step 2: Coverage spot-check**

```bash
cd tracker
PATH="$HOME/.local/share/mise/shims:$PATH" go test -coverprofile=coverage.out ./internal/ledger/storage/...
go tool cover -func=coverage.out | tail
```

Target: ≥ 90% on storage package. Lower bounds: error-path branches that require synthetic fault injection (e.g., DB closed mid-op) are acceptable to leave uncovered if the alternative is unreliable test code.

- [ ] **Step 3: Lint passthrough**

```bash
cd tracker && PATH="$HOME/.local/share/mise/shims:$PATH" make lint || echo "(golangci-lint not on PATH locally; CI runs it)"
go vet ./...
staticcheck ./internal/ledger/storage/... 2>/dev/null || true
```

- [ ] **Step 4: Tag (optional)**

```bash
git tag -a tracker-ledger-storage-v0 -m "Tracker ledger storage layer complete"
```

---

## Self-review

- **Spec coverage:** §3.1 entry layout (denormalized + canonical), §3.3 balance projection (live, upserted in same txn), §4.1 append (single durable txn under a write lock), §5.2 schema (with the merkle-leaves simplification documented in §3 of this plan). Equivocation detection itself is deferred to a higher layer; storage exposes the raw `ErrPeerRootConflict` signal.
- **What's not here:** The orchestrator (`internal/ledger`) — chain-hash linkage validation, balance arithmetic, hourly Merkle-root scheduler, exposure to the broker. Each is its own plan. Storage is the leaf this all sits on.
- **One-DB-one-writer:** Documented in plan §3.2; enforced by `Store.writeMu`. Read paths intentionally take no mutex so SQLite WAL's reader/writer concurrency works.
- **Error sentinels:** `ErrDuplicateHash`, `ErrDuplicateSeq`, `ErrDuplicateMerkleRoot`, `ErrPeerRootConflict`. Each lets the caller distinguish a *recoverable* condition (idempotent retry, conflict-evidence) from a *real* DB error without parsing strings.
- **Canonical-bytes round-trip:** Lookups parse from the `canonical` column and re-build `*tbproto.Entry`. This means a malformed canonical column would surface on read (not silently as a denormalized-but-wrong row). The integration test (Task 10) doesn't specifically tamper with canonical, but `lookup_test.go` Task 6 does.
- **Placeholders:** None. Every task ends green.

## Next plans

`internal/ledger` (the orchestrator) is the natural next step. It glues:
- `entry` (build, hash, verify)
- `signing` (tracker-sig the entry)
- `storage` (this plan, durable layer)
- A tracker pubkey/privkey pair (from the eventual `internal/config`)
- A clock (`time.Now`-shaped, mockable)

After that: `internal/registry` (already in flight on a parallel branch) is consumed by `internal/broker` alongside `internal/ledger`.
