# tracker/internal/registry Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the live, in-memory seeder registry — `tracker/internal/registry` — providing a sharded `Registry` of `SeederRecord` entries with safe concurrent register/update/lookup, filtered candidate matching for the broker, and a sweep for stale entries.

**Architecture:** Pure in-memory store. `Registry` holds a fixed number of `*shard`s (default 16). Each shard owns a `map[ids.IdentityID]*SeederRecord` guarded by its own `sync.RWMutex` — sharding by `binary.BigEndian.Uint64(id[:8]) % numShards` to scatter contention. All exported readers (Get / Snapshot / Match) return value copies of records so callers cannot mutate the store. All exported mutators are typed methods on `Registry`; `Advertise` updates capabilities + availability + headroom atomically. The registry knows nothing about persistence, network, reputation freezing, or selection scoring — those live in their respective modules.

**Tech Stack:** Go 1.23 stdlib (`sync`, `time`, `net/netip`, `encoding/binary`, `errors`); `github.com/stretchr/testify` for tests; `github.com/token-bay/token-bay/shared/ids` for `IdentityID`; `github.com/token-bay/token-bay/shared/proto` for `PrivacyTier`.

**Spec:** `docs/superpowers/specs/tracker/2026-04-22-tracker-design.md` — §3 (internal modules), §4.1 (SeederRecord), §5.1 (broker filter — registry provides the candidate set), §6 (concurrency model — sharded fine-grained locks).

**Repo path note:** This worktree lives at `/Users/dor.amid/.superset/worktrees/token-bay/sand-star`. All absolute paths in this plan use that prefix. If the user is operating from `/Users/dor.amid/git/token-bay` instead, substitute that prefix throughout.

---

## 1. File map

```
tracker/
├── go.mod                                          ← MODIFY: pick up shared/ require (first tracker import of shared)
├── internal/
│   └── registry/
│       ├── .gitkeep                                ← REMOVE (Task 2)
│       ├── doc.go                                  ← CREATE: package doc
│       ├── errors.go                               ← CREATE: sentinel errors
│       ├── errors_test.go                          ← CREATE
│       ├── record.go                               ← CREATE: SeederRecord, Capabilities, NetCoords
│       ├── record_test.go                          ← CREATE
│       ├── shard.go                                ← CREATE: internal sharded store
│       ├── shard_test.go                           ← CREATE
│       ├── registry.go                             ← CREATE: Registry + New + Register/Get/Deregister/...
│       └── registry_test.go                        ← CREATE
```

`tracker/go.mod` will gain the `require github.com/token-bay/token-bay/shared v0.0.0-...` line automatically when the first import lands (Task 3). The existing `replace` directive (already present per the scaffolding) wires it locally.

## 2. Conventions used in this plan

- All `go test` and `go build` commands run from `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker`. Steps that need a different cwd say so.
- One commit per task. Conventional-commit prefixes: `feat(tracker/registry):`, `test(tracker/registry):`, `chore(tracker):`, `docs(tracker/registry):`.
- Co-Authored-By footer on every commit:
  ```
  Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
  ```
- TDD discipline per repo CLAUDE.md: write the failing test, run it and confirm it fails for the *expected* reason, write the minimum implementation, run again to confirm green, commit. Do not bundle multiple red→green cycles in one commit.
- Lint policy: respect `.golangci.yml` (errcheck, gofumpt, gosec, ineffassign, misspell, revive[exported], staticcheck, unused). Every exported symbol needs a doc comment (`revive:exported`).
- Test coverage: aim ≥ 90% on every file. Use `go test -race -cover ./internal/registry/...` after each task to spot drops.
- Time handling: pass `time.Time` in from callers; the registry never calls `time.Now()` itself. Tests use a fixed `time.Date(...)` reference and arithmetic from there.
- Network addresses: use `net/netip.AddrPort` (immutable, comparable, no allocations) — not `net.Addr`.

---

## Task 1: Wire `shared/` into `tracker/go.mod`

**Why this is its own task:** the existing `tracker/go.mod` carries a `replace` directive but no `require` (per its own comment, deliberately). The first registry source file that imports `shared/ids` will trigger the require to materialize. We do that wiring up-front so the rest of the tasks see green builds the moment they import.

**Files:**
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/go.mod`
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/go.sum`

- [ ] **Step 1: Add a single throwaway shared import to confirm wiring**

Create a temporary scratch file `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/_scratch.go` (note the leading underscore — Go ignores files starting with `_`, so this won't break the build later if something forgets to remove it; we will remove it explicitly in step 4). Write:

```go
// +build ignore

package registry

import _ "github.com/token-bay/token-bay/shared/ids"
```

Wait — the underscore-prefixed filename is enough for `go build` to skip the file without a build tag. But `go mod tidy` *does* consider it. We want `tidy` to see the import so the require materializes. So drop the build tag and **do not** prefix with underscore — use a normal filename and remove it in step 4.

Replace the above with this file content at `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/scratch_share_wire.go`:

```go
// Package registry — TEMPORARY scratch file used only to materialize the
// `require github.com/token-bay/token-bay/shared` line in tracker/go.mod.
// Removed in the same task; do not commit this file.
package registry

import _ "github.com/token-bay/token-bay/shared/ids"
```

- [ ] **Step 2: Run `go mod tidy`**

Run (from `tracker/`):
```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
go mod tidy
```

Expected: `tracker/go.mod` now contains a line like:
```
require github.com/token-bay/token-bay/shared v0.0.0-00010101000000-000000000000
```

The pre-existing `replace github.com/token-bay/token-bay/shared => ../shared` resolves it locally.

- [ ] **Step 3: Verify `go build` succeeds**

Run:
```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
go build ./...
```

Expected: no errors.

- [ ] **Step 4: Remove the scratch file**

Run:
```bash
rm /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/scratch_share_wire.go
```

- [ ] **Step 5: Re-run `go mod tidy` — DO NOT remove the require**

Run:
```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
go mod tidy
```

If `tidy` removes the `require` (because no `.go` file imports `shared/` yet), that is fine — the require comes back in Task 3 when `record.go` imports `shared/proto`. But the `replace` directive must remain. Verify:

```bash
grep -E "^(require|replace)" /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/go.mod
```

Expected: `replace github.com/token-bay/token-bay/shared => ../shared` is still present.

- [ ] **Step 6: Commit (only if `go.mod` / `go.sum` changed)**

If neither file changed (because `tidy` reverted), skip the commit and proceed to Task 2. If they did change:

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star
git add tracker/go.mod tracker/go.sum
git commit -m "$(cat <<'EOF'
chore(tracker): wire shared module require for upcoming registry imports

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: Package doc

**Files:**
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/doc.go`
- Remove: `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/.gitkeep`

- [ ] **Step 1: Write `doc.go`**

Write to `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/doc.go`:

```go
// Package registry holds the tracker's live, in-memory seeder registry.
//
// A SeederRecord captures everything the broker needs to pick a candidate
// for a consumer's request: the seeder's identity, what it can serve
// (Capabilities), how busy it is (Load), how recently we heard from it
// (LastHeartbeat), and where to reach it (NetCoords). The registry is a
// pure in-memory store; persistence is intentionally out of scope.
//
// Concurrency model: the registry shards by hash(IdentityID) into a fixed
// number of shards (configurable, default 16). Each shard owns its own
// sync.RWMutex. Read-heavy paths (Get, Snapshot, Match) take RLocks per
// shard; mutators take the write lock on a single shard. There is no
// global lock, so contention scales with the number of shards.
//
// All readers return value copies of SeederRecord (and the slices it
// contains). Callers cannot mutate the store through a returned record.
//
// The registry intentionally does not know about: ledger state, reputation
// freezing, broker scoring, network I/O, or persistence. Those concerns
// live in their respective tracker modules.
//
// Spec: docs/superpowers/specs/tracker/2026-04-22-tracker-design.md §3, §4.1, §5.1, §6.
package registry
```

- [ ] **Step 2: Remove `.gitkeep`**

```bash
rm /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/.gitkeep
```

- [ ] **Step 3: Verify build**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
go build ./internal/registry/...
```
Expected: no errors (an empty package with only a doc comment compiles fine).

- [ ] **Step 4: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star
git add tracker/internal/registry/doc.go
git rm tracker/internal/registry/.gitkeep
git commit -m "$(cat <<'EOF'
docs(tracker/registry): package doc — sharded in-memory seeder index

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: Record types — `SeederRecord`, `Capabilities`, `NetCoords`

These are pure value types. No methods (yet). The test asserts shape: zero-value behavior, deep-equal of constructed records, and that the slice/struct fields hold what the spec §4.1 requires.

**Files:**
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/record.go`
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/record_test.go`

- [ ] **Step 1: Write the failing test**

Write to `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/record_test.go`:

```go
package registry

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/shared/proto"
)

func TestSeederRecord_ZeroValue(t *testing.T) {
	var r SeederRecord
	assert.Equal(t, ids.IdentityID{}, r.IdentityID)
	assert.Equal(t, uint64(0), r.ConnSessionID)
	assert.Equal(t, Capabilities{}, r.Capabilities)
	assert.False(t, r.Available)
	assert.Equal(t, 0.0, r.HeadroomEstimate)
	assert.Equal(t, 0.0, r.ReputationScore)
	assert.Equal(t, NetCoords{}, r.NetCoords)
	assert.Equal(t, 0, r.Load)
	assert.True(t, r.LastHeartbeat.IsZero())
}

func TestSeederRecord_PopulatedFields(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	addr := netip.MustParseAddrPort("203.0.113.7:51820")
	local := []netip.AddrPort{
		netip.MustParseAddrPort("192.168.1.10:51820"),
		netip.MustParseAddrPort("[fe80::1]:51820"),
	}

	r := SeederRecord{
		IdentityID:    ids.IdentityID{0x01, 0x02, 0x03},
		ConnSessionID: 42,
		Capabilities: Capabilities{
			Models:      []string{"claude-opus-4-7", "claude-sonnet-4-6"},
			MaxContext:  200_000,
			Tiers:       []proto.PrivacyTier{proto.PrivacyTier_PRIVACY_TIER_STANDARD},
			Attestation: nil,
		},
		Available:        true,
		HeadroomEstimate: 0.75,
		ReputationScore:  0.92,
		NetCoords: NetCoords{
			ExternalAddr:    addr,
			LocalCandidates: local,
		},
		Load:          3,
		LastHeartbeat: now,
	}

	assert.Equal(t, [32]byte{0x01, 0x02, 0x03}, r.IdentityID.Bytes())
	assert.Equal(t, []string{"claude-opus-4-7", "claude-sonnet-4-6"}, r.Capabilities.Models)
	assert.Equal(t, uint32(200_000), r.Capabilities.MaxContext)
	assert.Equal(t, []proto.PrivacyTier{proto.PrivacyTier_PRIVACY_TIER_STANDARD}, r.Capabilities.Tiers)
	assert.Nil(t, r.Capabilities.Attestation)
	assert.True(t, r.Available)
	assert.Equal(t, 0.75, r.HeadroomEstimate)
	assert.Equal(t, 0.92, r.ReputationScore)
	assert.Equal(t, addr, r.NetCoords.ExternalAddr)
	assert.Equal(t, local, r.NetCoords.LocalCandidates)
	assert.Equal(t, 3, r.Load)
	assert.Equal(t, now, r.LastHeartbeat)
}

func TestCapabilities_TEEAttestationOptional(t *testing.T) {
	c := Capabilities{
		Models:      []string{"claude-opus-4-7"},
		MaxContext:  100_000,
		Tiers:       []proto.PrivacyTier{proto.PrivacyTier_PRIVACY_TIER_TEE},
		Attestation: []byte{0xDE, 0xAD, 0xBE, 0xEF},
	}
	assert.Equal(t, []byte{0xDE, 0xAD, 0xBE, 0xEF}, c.Attestation)
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
go test ./internal/registry/...
```
Expected: FAIL with `undefined: SeederRecord`, `undefined: Capabilities`, `undefined: NetCoords`.

- [ ] **Step 3: Write minimal `record.go`**

Write to `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/record.go`:

```go
package registry

import (
	"net/netip"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/shared/proto"
)

// Capabilities describes what work a seeder can serve.
//
// Models lists the Claude model IDs (e.g. "claude-opus-4-7") the seeder will
// honor. MaxContext is the largest context window the seeder advertises across
// those models. Tiers lists the privacy tiers the seeder offers; if it
// includes proto.PrivacyTier_PRIVACY_TIER_TEE, Attestation must hold the
// enclave attestation bytes.
//
// Per spec §4.1 the registry only stores capabilities; it does not validate
// the attestation bytes — that is the broker / TEE-tier subsystem's job.
type Capabilities struct {
	Models      []string
	MaxContext  uint32
	Tiers       []proto.PrivacyTier
	Attestation []byte
}

// NetCoords are the network coordinates the tracker records for a seeder so
// the consumer can reach it.
//
// ExternalAddr is the seeder's reflexive address as observed by the tracker
// over its long-lived connection (refreshed on heartbeat per spec §5.4).
// LocalCandidates are additional candidate addresses the seeder advertises
// for hole-punching (RFC 5245-style ICE candidates, slimmed down).
type NetCoords struct {
	ExternalAddr    netip.AddrPort
	LocalCandidates []netip.AddrPort
}

// SeederRecord is the in-memory entry the registry keeps per seeder.
//
// All fields mirror spec §4.1. The registry returns value copies of this
// struct; callers cannot mutate the store via a returned record.
type SeederRecord struct {
	IdentityID       ids.IdentityID
	ConnSessionID    uint64
	Capabilities     Capabilities
	Available        bool
	HeadroomEstimate float64
	ReputationScore  float64
	NetCoords        NetCoords
	Load             int
	LastHeartbeat    time.Time
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
go test ./internal/registry/...
```
Expected: PASS.

- [ ] **Step 5: Lint clean**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
golangci-lint run ./internal/registry/...
```
Expected: no findings.

- [ ] **Step 6: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star
git add tracker/internal/registry/record.go tracker/internal/registry/record_test.go tracker/go.mod tracker/go.sum
git commit -m "$(cat <<'EOF'
feat(tracker/registry): SeederRecord + Capabilities + NetCoords value types

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: Sentinel errors

**Files:**
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/errors.go`
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/errors_test.go`

- [ ] **Step 1: Write the failing test**

Write to `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/errors_test.go`:

```go
package registry

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSentinelErrors_NotNil(t *testing.T) {
	assert.NotNil(t, ErrUnknownSeeder)
	assert.NotNil(t, ErrInvalidShardCount)
	assert.NotNil(t, ErrInvalidHeadroom)
	assert.NotNil(t, ErrLoadUnderflow)
}

func TestSentinelErrors_Distinct(t *testing.T) {
	all := []error{
		ErrUnknownSeeder,
		ErrInvalidShardCount,
		ErrInvalidHeadroom,
		ErrLoadUnderflow,
	}
	for i := 0; i < len(all); i++ {
		for j := i + 1; j < len(all); j++ {
			assert.False(t, errors.Is(all[i], all[j]),
				"errors %v and %v should be distinct sentinels", all[i], all[j])
		}
	}
}

func TestSentinelErrors_HumanReadable(t *testing.T) {
	assert.Contains(t, ErrUnknownSeeder.Error(), "unknown seeder")
	assert.Contains(t, ErrInvalidShardCount.Error(), "shard count")
	assert.Contains(t, ErrInvalidHeadroom.Error(), "headroom")
	assert.Contains(t, ErrLoadUnderflow.Error(), "load")
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
go test ./internal/registry/...
```
Expected: FAIL with `undefined: ErrUnknownSeeder` (and the other three).

- [ ] **Step 3: Write minimal `errors.go`**

Write to `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/errors.go`:

```go
package registry

import "errors"

// ErrUnknownSeeder is returned when an operation references an IdentityID that
// is not currently in the registry.
var ErrUnknownSeeder = errors.New("registry: unknown seeder")

// ErrInvalidShardCount is returned by New when numShards is not a positive int.
var ErrInvalidShardCount = errors.New("registry: shard count must be positive")

// ErrInvalidHeadroom is returned when a headroom value is outside [0.0, 1.0].
var ErrInvalidHeadroom = errors.New("registry: headroom must be within [0.0, 1.0]")

// ErrLoadUnderflow is returned by DecLoad when the seeder's load is already 0
// (catching a double-decrement bug at the call site rather than silently
// clamping).
var ErrLoadUnderflow = errors.New("registry: load underflow")
```

- [ ] **Step 4: Run test to verify it passes**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
go test ./internal/registry/...
```
Expected: PASS.

- [ ] **Step 5: Lint clean**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
golangci-lint run ./internal/registry/...
```
Expected: no findings.

- [ ] **Step 6: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star
git add tracker/internal/registry/errors.go tracker/internal/registry/errors_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/registry): sentinel errors

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: Internal `shard` type

The shard is the unit of locking. It is package-internal — exported only through the `Registry` facade. We test it directly because it's the easiest place to lock down the lock semantics.

**Files:**
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/shard.go`
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/shard_test.go`

- [ ] **Step 1: Write the failing test**

Write to `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/shard_test.go`:

```go
package registry

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/token-bay/token-bay/shared/ids"
)

func TestShard_NewIsEmpty(t *testing.T) {
	s := newShard()
	_, ok := s.get(ids.IdentityID{0x01})
	assert.False(t, ok)
}

func TestShard_PutThenGet_ReturnsCopy(t *testing.T) {
	s := newShard()
	now := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	rec := SeederRecord{
		IdentityID:    ids.IdentityID{0x01},
		Available:     true,
		LastHeartbeat: now,
		Capabilities: Capabilities{
			Models: []string{"claude-opus-4-7"},
		},
	}
	s.put(rec)

	got, ok := s.get(rec.IdentityID)
	assert.True(t, ok)
	assert.Equal(t, rec.IdentityID, got.IdentityID)
	assert.Equal(t, rec.Available, got.Available)
	assert.Equal(t, rec.LastHeartbeat, got.LastHeartbeat)

	// Mutate the returned copy; ensure the store is untouched.
	got.Available = false
	got.Capabilities.Models[0] = "MUTATED"
	got2, ok := s.get(rec.IdentityID)
	assert.True(t, ok)
	assert.True(t, got2.Available, "store should be insulated from mutations on returned copy")
	assert.Equal(t, "claude-opus-4-7", got2.Capabilities.Models[0],
		"models slice should be defensively copied on read")
}

func TestShard_PutOverwrites(t *testing.T) {
	s := newShard()
	id := ids.IdentityID{0x07}
	s.put(SeederRecord{IdentityID: id, Load: 1})
	s.put(SeederRecord{IdentityID: id, Load: 2})

	got, ok := s.get(id)
	assert.True(t, ok)
	assert.Equal(t, 2, got.Load)
}

func TestShard_DeleteRemoves(t *testing.T) {
	s := newShard()
	id := ids.IdentityID{0x07}
	s.put(SeederRecord{IdentityID: id})
	s.delete(id)
	_, ok := s.get(id)
	assert.False(t, ok)
}

func TestShard_DeleteMissing_NoOp(t *testing.T) {
	s := newShard()
	assert.NotPanics(t, func() {
		s.delete(ids.IdentityID{0xFF})
	})
}

func TestShard_Update_AppliesMutation(t *testing.T) {
	s := newShard()
	id := ids.IdentityID{0x09}
	s.put(SeederRecord{IdentityID: id, Load: 0})

	err := s.update(id, func(r *SeederRecord) error {
		r.Load = 5
		return nil
	})
	assert.NoError(t, err)

	got, _ := s.get(id)
	assert.Equal(t, 5, got.Load)
}

func TestShard_Update_UnknownReturnsErr(t *testing.T) {
	s := newShard()
	err := s.update(ids.IdentityID{0xAA}, func(r *SeederRecord) error {
		t.Fatal("update fn should not be called for unknown seeder")
		return nil
	})
	assert.ErrorIs(t, err, ErrUnknownSeeder)
}

func TestShard_Update_PropagatesFnError(t *testing.T) {
	s := newShard()
	id := ids.IdentityID{0x10}
	s.put(SeederRecord{IdentityID: id, Load: 0})

	sentinel := assertErr("boom")
	err := s.update(id, func(r *SeederRecord) error {
		r.Load = 99
		return sentinel
	})
	assert.ErrorIs(t, err, sentinel)

	// On error, the mutation must NOT be persisted (rollback semantics).
	got, _ := s.get(id)
	assert.Equal(t, 0, got.Load, "mutation should be discarded when update fn returns error")
}

func TestShard_Snapshot_ReturnsAllRecords(t *testing.T) {
	s := newShard()
	s.put(SeederRecord{IdentityID: ids.IdentityID{0x01}, Load: 1})
	s.put(SeederRecord{IdentityID: ids.IdentityID{0x02}, Load: 2})

	out := s.snapshot()
	assert.Len(t, out, 2)
}

func TestShard_SweepStale_RemovesAndCounts(t *testing.T) {
	s := newShard()
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour)
	fresh := now.Add(-1 * time.Minute)

	s.put(SeederRecord{IdentityID: ids.IdentityID{0x01}, LastHeartbeat: old})
	s.put(SeederRecord{IdentityID: ids.IdentityID{0x02}, LastHeartbeat: fresh})

	removed := s.sweepStale(now.Add(-1 * time.Hour))
	assert.Equal(t, 1, removed)

	_, ok1 := s.get(ids.IdentityID{0x01})
	_, ok2 := s.get(ids.IdentityID{0x02})
	assert.False(t, ok1)
	assert.True(t, ok2)
}

// assertErr is a tiny helper so the test file can compose simple sentinel
// errors without pulling in another package.
type assertErr string

func (e assertErr) Error() string { return string(e) }
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
go test ./internal/registry/...
```
Expected: FAIL with `undefined: newShard` and friends.

- [ ] **Step 3: Write minimal `shard.go`**

Write to `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/shard.go`:

```go
package registry

import (
	"net/netip"
	"sync"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/shared/proto"
)

// shard is the unit of locking inside Registry. Each shard owns a private map
// of records and an RWMutex; the registry routes operations to shards by
// hashing the IdentityID.
type shard struct {
	mu   sync.RWMutex
	recs map[ids.IdentityID]*SeederRecord
}

func newShard() *shard {
	return &shard{recs: make(map[ids.IdentityID]*SeederRecord)}
}

// put stores rec, overwriting any existing entry with the same IdentityID.
// The shard takes its own deep copy so the caller cannot reach into the store
// by mutating the slices they passed in.
func (s *shard) put(rec SeederRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := cloneRecord(rec)
	s.recs[rec.IdentityID] = &stored
}

// get returns a deep copy of the record so callers cannot mutate the store.
func (s *shard) get(id ids.IdentityID) (SeederRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.recs[id]
	if !ok {
		return SeederRecord{}, false
	}
	return cloneRecord(*r), true
}

// delete removes the record. No-op when absent.
func (s *shard) delete(id ids.IdentityID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.recs, id)
}

// update applies fn to the record under the shard's write lock. Returns
// ErrUnknownSeeder when no record exists. If fn returns an error, the
// mutation is discarded — the shard rolls back to the pre-call state.
func (s *shard) update(id ids.IdentityID, fn func(*SeederRecord) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.recs[id]
	if !ok {
		return ErrUnknownSeeder
	}
	// Operate on a copy so we can roll back on error.
	working := cloneRecord(*r)
	if err := fn(&working); err != nil {
		return err
	}
	s.recs[id] = &working
	return nil
}

// snapshot returns a deep copy of every record currently in the shard.
func (s *shard) snapshot() []SeederRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]SeederRecord, 0, len(s.recs))
	for _, r := range s.recs {
		out = append(out, cloneRecord(*r))
	}
	return out
}

// sweepStale removes every record whose LastHeartbeat is at or before
// staleBefore. Returns the count removed.
func (s *shard) sweepStale(staleBefore time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for id, r := range s.recs {
		if !r.LastHeartbeat.After(staleBefore) {
			delete(s.recs, id)
			removed++
		}
	}
	return removed
}

// cloneRecord deep-copies the slices in a SeederRecord so the store and
// returned copies do not alias their backing arrays.
func cloneRecord(r SeederRecord) SeederRecord {
	out := r
	out.Capabilities.Models = cloneStrings(r.Capabilities.Models)
	out.Capabilities.Tiers = cloneTiers(r.Capabilities.Tiers)
	out.Capabilities.Attestation = cloneBytes(r.Capabilities.Attestation)
	out.NetCoords.LocalCandidates = cloneAddrPorts(r.NetCoords.LocalCandidates)
	return out
}

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func cloneTiers(in []proto.PrivacyTier) []proto.PrivacyTier {
	if in == nil {
		return nil
	}
	out := make([]proto.PrivacyTier, len(in))
	copy(out, in)
	return out
}

func cloneBytes(in []byte) []byte {
	if in == nil {
		return nil
	}
	out := make([]byte, len(in))
	copy(out, in)
	return out
}

func cloneAddrPorts(in []netip.AddrPort) []netip.AddrPort {
	if in == nil {
		return nil
	}
	out := make([]netip.AddrPort, len(in))
	copy(out, in)
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
go test -race ./internal/registry/...
```
Expected: PASS.

- [ ] **Step 5: Lint clean**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
golangci-lint run ./internal/registry/...
```
Expected: no findings. The unexported `shard` type does not need an exported-doc-comment per `revive:exported`, but the helpers (`cloneRecord`, etc.) are unexported so they don't either.

- [ ] **Step 6: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star
git add tracker/internal/registry/shard.go tracker/internal/registry/shard_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/registry): internal shard with deep-copy isolation + rollback update

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: `Registry` constructor + shard routing

The constructor validates `numShards` and pre-allocates the shard array. We expose a private `shardFor(id)` so the tests can confirm routing is consistent (same ID → same shard).

**Files:**
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/registry.go`
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/registry_test.go`

- [ ] **Step 1: Write the failing test**

Write to `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/registry_test.go`:

```go
package registry

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/ids"
)

func TestNew_ValidShardCount(t *testing.T) {
	r, err := New(8)
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.Equal(t, 8, r.NumShards())
}

func TestNew_RejectsZeroShards(t *testing.T) {
	r, err := New(0)
	assert.Nil(t, r)
	assert.ErrorIs(t, err, ErrInvalidShardCount)
}

func TestNew_RejectsNegativeShards(t *testing.T) {
	r, err := New(-1)
	assert.Nil(t, r)
	assert.ErrorIs(t, err, ErrInvalidShardCount)
}

func TestRegistry_ShardFor_DeterministicForSameID(t *testing.T) {
	r, err := New(16)
	require.NoError(t, err)

	id := ids.IdentityID{0xDE, 0xAD, 0xBE, 0xEF, 0xCA, 0xFE, 0xBA, 0xBE}
	idx1 := r.shardIndex(id)
	idx2 := r.shardIndex(id)
	assert.Equal(t, idx1, idx2)
	assert.GreaterOrEqual(t, idx1, 0)
	assert.Less(t, idx1, 16)
}

func TestRegistry_ShardFor_DistributesAcrossShards(t *testing.T) {
	r, err := New(16)
	require.NoError(t, err)

	// Distinct IDs should end up in more than one shard. We don't assert a
	// distribution shape — just that 256 distinct IDs hit ≥ 4 shards (a very
	// loose hash-quality smoke check).
	hits := make(map[int]struct{})
	for i := 0; i < 256; i++ {
		var id ids.IdentityID
		id[0] = byte(i)
		hits[r.shardIndex(id)] = struct{}{}
	}
	assert.GreaterOrEqual(t, len(hits), 4, "shardIndex should spread distinct IDs across shards")
}

func TestDefaultShardCount_IsPositive(t *testing.T) {
	assert.Greater(t, DefaultShardCount, 0)
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
go test ./internal/registry/...
```
Expected: FAIL with `undefined: New`, `undefined: DefaultShardCount`.

- [ ] **Step 3: Write minimal `registry.go`**

Write to `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/registry.go`:

```go
package registry

import (
	"encoding/binary"

	"github.com/token-bay/token-bay/shared/ids"
)

// DefaultShardCount is the registry's default shard count when callers want a
// reasonable default rather than tuning it themselves. 16 strikes a balance
// between contention scattering and per-shard overhead for the v1 capacity
// target (≤ 10³ concurrent seeders per spec §6).
const DefaultShardCount = 16

// Registry is a sharded, in-memory store of SeederRecords. Safe for
// concurrent use. See package doc for the concurrency model.
type Registry struct {
	shards []*shard
}

// New returns a Registry with numShards shards. Returns ErrInvalidShardCount
// when numShards <= 0.
func New(numShards int) (*Registry, error) {
	if numShards <= 0 {
		return nil, ErrInvalidShardCount
	}
	r := &Registry{shards: make([]*shard, numShards)}
	for i := range r.shards {
		r.shards[i] = newShard()
	}
	return r, nil
}

// NumShards returns the registry's shard count. Useful for diagnostics and
// tests; callers should not depend on a specific value.
func (r *Registry) NumShards() int { return len(r.shards) }

// shardIndex returns the shard index for an IdentityID. The first eight bytes
// of the ID are interpreted as a big-endian uint64 and reduced modulo the
// shard count. Identity IDs are Ed25519-pubkey-derived hashes so the leading
// bytes are uniformly distributed; a more elaborate hash would be wasted work.
func (r *Registry) shardIndex(id ids.IdentityID) int {
	b := id.Bytes()
	return int(binary.BigEndian.Uint64(b[:8]) % uint64(len(r.shards)))
}

func (r *Registry) shardFor(id ids.IdentityID) *shard {
	return r.shards[r.shardIndex(id)]
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
go test ./internal/registry/...
```
Expected: PASS.

- [ ] **Step 5: Lint clean**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
golangci-lint run ./internal/registry/...
```

- [ ] **Step 6: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star
git add tracker/internal/registry/registry.go tracker/internal/registry/registry_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/registry): Registry constructor + IdentityID-modulo shard routing

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 7: `Register`, `Get`, `Deregister`

**Files:**
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/registry.go`
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/registry_test.go`

- [ ] **Step 1: Append the failing tests**

Append to `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/registry_test.go`:

```go
func TestRegistry_RegisterThenGet(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)

	rec := SeederRecord{
		IdentityID: ids.IdentityID{0x42},
		Available:  true,
		Capabilities: Capabilities{
			Models: []string{"claude-opus-4-7"},
		},
	}
	require.NoError(t, r.Register(rec))

	got, ok := r.Get(rec.IdentityID)
	require.True(t, ok)
	assert.Equal(t, rec.IdentityID, got.IdentityID)
	assert.True(t, got.Available)
	assert.Equal(t, []string{"claude-opus-4-7"}, got.Capabilities.Models)
}

func TestRegistry_Get_Missing(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)

	_, ok := r.Get(ids.IdentityID{0xFF})
	assert.False(t, ok)
}

func TestRegistry_Register_Upserts(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)

	id := ids.IdentityID{0x11}
	require.NoError(t, r.Register(SeederRecord{IdentityID: id, Load: 1}))
	require.NoError(t, r.Register(SeederRecord{IdentityID: id, Load: 9}))

	got, ok := r.Get(id)
	require.True(t, ok)
	assert.Equal(t, 9, got.Load, "second Register call should overwrite first")
}

func TestRegistry_Deregister_Removes(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)

	id := ids.IdentityID{0x22}
	require.NoError(t, r.Register(SeederRecord{IdentityID: id}))
	r.Deregister(id)

	_, ok := r.Get(id)
	assert.False(t, ok)
}

func TestRegistry_Deregister_MissingIsNoOp(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)
	assert.NotPanics(t, func() {
		r.Deregister(ids.IdentityID{0x99})
	})
}

func TestRegistry_Get_ReturnedSliceMutationDoesNotAffectStore(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)

	id := ids.IdentityID{0x33}
	require.NoError(t, r.Register(SeederRecord{
		IdentityID:   id,
		Capabilities: Capabilities{Models: []string{"claude-opus-4-7"}},
	}))

	got, _ := r.Get(id)
	got.Capabilities.Models[0] = "INJECTED"

	got2, _ := r.Get(id)
	assert.Equal(t, "claude-opus-4-7", got2.Capabilities.Models[0],
		"store must not be aliased through returned record's slice")
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
go test ./internal/registry/...
```
Expected: FAIL — `undefined: r.Register`, etc.

- [ ] **Step 3: Append `Register`, `Get`, `Deregister` to `registry.go`**

Append to `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/registry.go`:

```go
// Register inserts or replaces a SeederRecord. Caller is responsible for
// providing the full record — including LastHeartbeat. Idempotent upsert
// (no error on existing IdentityID).
func (r *Registry) Register(rec SeederRecord) error {
	r.shardFor(rec.IdentityID).put(rec)
	return nil
}

// Get returns a deep copy of the seeder's record. ok is false when no record
// exists for id.
func (r *Registry) Get(id ids.IdentityID) (SeederRecord, bool) {
	return r.shardFor(id).get(id)
}

// Deregister removes the seeder. No-op when no record exists.
func (r *Registry) Deregister(id ids.IdentityID) {
	r.shardFor(id).delete(id)
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
go test -race ./internal/registry/...
```
Expected: PASS.

- [ ] **Step 5: Lint clean**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
golangci-lint run ./internal/registry/...
```

- [ ] **Step 6: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star
git add tracker/internal/registry/registry.go tracker/internal/registry/registry_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/registry): Register / Get / Deregister with copy-on-read isolation

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 8: `Heartbeat` (last_heartbeat bump) + `UpdateExternalAddr`

`Heartbeat(id, now)` updates `LastHeartbeat` only. `UpdateExternalAddr(id, addr)` updates `NetCoords.ExternalAddr` only — the STUN module calls this on each reflective observation; the session module calls `Heartbeat`.

**Files:**
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/registry.go`
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/registry_test.go`

- [ ] **Step 1: Append the failing tests**

Append:

```go
func TestRegistry_Heartbeat_BumpsLastHeartbeat(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)

	id := ids.IdentityID{0x44}
	t0 := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(30 * time.Second)
	require.NoError(t, r.Register(SeederRecord{IdentityID: id, LastHeartbeat: t0}))

	require.NoError(t, r.Heartbeat(id, t1))

	got, _ := r.Get(id)
	assert.Equal(t, t1, got.LastHeartbeat)
}

func TestRegistry_Heartbeat_UnknownReturnsErr(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)
	err = r.Heartbeat(ids.IdentityID{0xCC}, time.Now())
	assert.ErrorIs(t, err, ErrUnknownSeeder)
}

func TestRegistry_UpdateExternalAddr_SetsAddr(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)

	id := ids.IdentityID{0x55}
	require.NoError(t, r.Register(SeederRecord{IdentityID: id}))

	addr := netip.MustParseAddrPort("198.51.100.4:51820")
	require.NoError(t, r.UpdateExternalAddr(id, addr))

	got, _ := r.Get(id)
	assert.Equal(t, addr, got.NetCoords.ExternalAddr)
}

func TestRegistry_UpdateExternalAddr_PreservesLocalCandidates(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)

	id := ids.IdentityID{0x56}
	local := []netip.AddrPort{netip.MustParseAddrPort("10.0.0.1:51820")}
	require.NoError(t, r.Register(SeederRecord{
		IdentityID: id,
		NetCoords:  NetCoords{LocalCandidates: local},
	}))

	require.NoError(t, r.UpdateExternalAddr(id, netip.MustParseAddrPort("203.0.113.9:443")))

	got, _ := r.Get(id)
	assert.Equal(t, local, got.NetCoords.LocalCandidates)
}

func TestRegistry_UpdateExternalAddr_UnknownReturnsErr(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)
	err = r.UpdateExternalAddr(ids.IdentityID{0xDD}, netip.MustParseAddrPort("1.1.1.1:80"))
	assert.ErrorIs(t, err, ErrUnknownSeeder)
}
```

You will also need to add the imports `"net/netip"` and `"time"` to the test file if they are not already present. They were imported in earlier tasks so they should already be there; if `go test` complains "imported and not used" or vice versa, fix the import block accordingly.

- [ ] **Step 2: Run test to verify it fails**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
go test ./internal/registry/...
```
Expected: FAIL — `undefined: r.Heartbeat`, `undefined: r.UpdateExternalAddr`.

- [ ] **Step 3: Append `Heartbeat` and `UpdateExternalAddr` to `registry.go`**

Add to the imports of `registry.go`: `"net/netip"`, `"time"`.

Append the methods:

```go
// Heartbeat sets LastHeartbeat to now. Returns ErrUnknownSeeder if the seeder
// is not in the registry.
func (r *Registry) Heartbeat(id ids.IdentityID, now time.Time) error {
	return r.shardFor(id).update(id, func(rec *SeederRecord) error {
		rec.LastHeartbeat = now
		return nil
	})
}

// UpdateExternalAddr sets the seeder's reflexive (STUN-observed) address.
// Other NetCoords fields are preserved. Returns ErrUnknownSeeder if the
// seeder is not in the registry.
func (r *Registry) UpdateExternalAddr(id ids.IdentityID, addr netip.AddrPort) error {
	return r.shardFor(id).update(id, func(rec *SeederRecord) error {
		rec.NetCoords.ExternalAddr = addr
		return nil
	})
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
go test -race ./internal/registry/...
```
Expected: PASS.

- [ ] **Step 5: Lint clean**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
golangci-lint run ./internal/registry/...
```

- [ ] **Step 6: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star
git add tracker/internal/registry/registry.go tracker/internal/registry/registry_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/registry): Heartbeat + UpdateExternalAddr

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 9: `Advertise` (capabilities + availability + headroom, atomic)

Per spec §2.1, `advertise(capabilities, available, headroom)` is a single seeder-side message. The registry must apply all three fields under one lock so the broker never sees a half-applied advertise.

**Files:**
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/registry.go`
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/registry_test.go`

- [ ] **Step 1: Append the failing tests**

```go
func TestRegistry_Advertise_AppliesAllThreeFields(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)

	id := ids.IdentityID{0x60}
	require.NoError(t, r.Register(SeederRecord{IdentityID: id}))

	caps := Capabilities{
		Models:     []string{"claude-opus-4-7", "claude-sonnet-4-6"},
		MaxContext: 200_000,
		Tiers:      []proto.PrivacyTier{proto.PrivacyTier_PRIVACY_TIER_STANDARD},
	}
	require.NoError(t, r.Advertise(id, caps, true, 0.65))

	got, _ := r.Get(id)
	assert.Equal(t, caps.Models, got.Capabilities.Models)
	assert.Equal(t, caps.MaxContext, got.Capabilities.MaxContext)
	assert.Equal(t, caps.Tiers, got.Capabilities.Tiers)
	assert.True(t, got.Available)
	assert.Equal(t, 0.65, got.HeadroomEstimate)
}

func TestRegistry_Advertise_RejectsHeadroomBelowZero(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)
	id := ids.IdentityID{0x61}
	require.NoError(t, r.Register(SeederRecord{IdentityID: id, HeadroomEstimate: 0.5}))

	err = r.Advertise(id, Capabilities{}, true, -0.1)
	assert.ErrorIs(t, err, ErrInvalidHeadroom)

	// Reject = no mutation.
	got, _ := r.Get(id)
	assert.Equal(t, 0.5, got.HeadroomEstimate)
}

func TestRegistry_Advertise_RejectsHeadroomAboveOne(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)
	id := ids.IdentityID{0x62}
	require.NoError(t, r.Register(SeederRecord{IdentityID: id}))

	err = r.Advertise(id, Capabilities{}, true, 1.0001)
	assert.ErrorIs(t, err, ErrInvalidHeadroom)
}

func TestRegistry_Advertise_HeadroomBoundsInclusive(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)
	id := ids.IdentityID{0x63}
	require.NoError(t, r.Register(SeederRecord{IdentityID: id}))

	require.NoError(t, r.Advertise(id, Capabilities{}, true, 0.0))
	require.NoError(t, r.Advertise(id, Capabilities{}, true, 1.0))
}

func TestRegistry_Advertise_UnknownReturnsErr(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)
	err = r.Advertise(ids.IdentityID{0xEE}, Capabilities{}, true, 0.5)
	assert.ErrorIs(t, err, ErrUnknownSeeder)
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
go test ./internal/registry/...
```
Expected: FAIL — `undefined: r.Advertise`.

- [ ] **Step 3: Append `Advertise` to `registry.go`**

```go
// Advertise atomically applies the seeder's reported capabilities,
// availability, and headroom. headroom must be within [0.0, 1.0] or
// ErrInvalidHeadroom is returned (and no mutation occurs).
func (r *Registry) Advertise(id ids.IdentityID, caps Capabilities, available bool, headroom float64) error {
	if headroom < 0.0 || headroom > 1.0 {
		return ErrInvalidHeadroom
	}
	return r.shardFor(id).update(id, func(rec *SeederRecord) error {
		rec.Capabilities = caps
		rec.Available = available
		rec.HeadroomEstimate = headroom
		return nil
	})
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
go test -race ./internal/registry/...
```
Expected: PASS.

- [ ] **Step 5: Lint clean**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
golangci-lint run ./internal/registry/...
```

- [ ] **Step 6: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star
git add tracker/internal/registry/registry.go tracker/internal/registry/registry_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/registry): Advertise — atomic caps + available + headroom update

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 10: `UpdateReputation`

The reputation module calls this when a seeder's score changes. The registry stores the score so the broker can sort candidates without a cross-module call on the hot path. No bounds check — the reputation module owns the score's semantics.

**Files:**
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/registry.go`
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/registry_test.go`

- [ ] **Step 1: Append the failing tests**

```go
func TestRegistry_UpdateReputation_SetsScore(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)

	id := ids.IdentityID{0x70}
	require.NoError(t, r.Register(SeederRecord{IdentityID: id, ReputationScore: 0.5}))

	require.NoError(t, r.UpdateReputation(id, 0.83))

	got, _ := r.Get(id)
	assert.Equal(t, 0.83, got.ReputationScore)
}

func TestRegistry_UpdateReputation_AcceptsAnyFloat(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)

	id := ids.IdentityID{0x71}
	require.NoError(t, r.Register(SeederRecord{IdentityID: id}))

	// Reputation module owns scaling; registry is value-neutral.
	for _, score := range []float64{-1.0, 0.0, 0.5, 1.0, 100.0} {
		require.NoError(t, r.UpdateReputation(id, score))
		got, _ := r.Get(id)
		assert.Equal(t, score, got.ReputationScore)
	}
}

func TestRegistry_UpdateReputation_UnknownReturnsErr(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)
	err = r.UpdateReputation(ids.IdentityID{0xEE}, 0.5)
	assert.ErrorIs(t, err, ErrUnknownSeeder)
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
go test ./internal/registry/...
```
Expected: FAIL — `undefined: r.UpdateReputation`.

- [ ] **Step 3: Append `UpdateReputation` to `registry.go`**

```go
// UpdateReputation sets the seeder's reputation score. The registry does not
// constrain the score range — that is the reputation subsystem's contract.
func (r *Registry) UpdateReputation(id ids.IdentityID, score float64) error {
	return r.shardFor(id).update(id, func(rec *SeederRecord) error {
		rec.ReputationScore = score
		return nil
	})
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
go test -race ./internal/registry/...
```
Expected: PASS.

- [ ] **Step 5: Lint clean**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
golangci-lint run ./internal/registry/...
```

- [ ] **Step 6: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star
git add tracker/internal/registry/registry.go tracker/internal/registry/registry_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/registry): UpdateReputation

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 11: `IncLoad` and `DecLoad`

Returns the new value so the broker can inspect post-update load without a follow-up `Get`. `DecLoad` returns `ErrLoadUnderflow` when load is already 0 (catches double-decrement bugs at the call site).

**Files:**
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/registry.go`
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/registry_test.go`

- [ ] **Step 1: Append the failing tests**

```go
func TestRegistry_IncLoad_IncrementsAndReturnsNewValue(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)
	id := ids.IdentityID{0x80}
	require.NoError(t, r.Register(SeederRecord{IdentityID: id, Load: 0}))

	n, err := r.IncLoad(id)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	n, err = r.IncLoad(id)
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	got, _ := r.Get(id)
	assert.Equal(t, 2, got.Load)
}

func TestRegistry_IncLoad_UnknownReturnsErr(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)
	_, err = r.IncLoad(ids.IdentityID{0xEE})
	assert.ErrorIs(t, err, ErrUnknownSeeder)
}

func TestRegistry_DecLoad_DecrementsAndReturnsNewValue(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)
	id := ids.IdentityID{0x81}
	require.NoError(t, r.Register(SeederRecord{IdentityID: id, Load: 3}))

	n, err := r.DecLoad(id)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
}

func TestRegistry_DecLoad_AtZero_ReturnsUnderflow(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)
	id := ids.IdentityID{0x82}
	require.NoError(t, r.Register(SeederRecord{IdentityID: id, Load: 0}))

	_, err = r.DecLoad(id)
	assert.ErrorIs(t, err, ErrLoadUnderflow)

	// Mutation must NOT be persisted.
	got, _ := r.Get(id)
	assert.Equal(t, 0, got.Load)
}

func TestRegistry_DecLoad_UnknownReturnsErr(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)
	_, err = r.DecLoad(ids.IdentityID{0xEF})
	assert.ErrorIs(t, err, ErrUnknownSeeder)
}

func TestRegistry_IncDec_Symmetric(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)
	id := ids.IdentityID{0x83}
	require.NoError(t, r.Register(SeederRecord{IdentityID: id, Load: 0}))

	for i := 0; i < 5; i++ {
		_, err := r.IncLoad(id)
		require.NoError(t, err)
	}
	for i := 0; i < 5; i++ {
		_, err := r.DecLoad(id)
		require.NoError(t, err)
	}

	got, _ := r.Get(id)
	assert.Equal(t, 0, got.Load)
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
go test ./internal/registry/...
```
Expected: FAIL — `undefined: r.IncLoad`, `undefined: r.DecLoad`.

- [ ] **Step 3: Append `IncLoad` and `DecLoad` to `registry.go`**

```go
// IncLoad increments the seeder's in-flight offer count by one. Returns the
// new load value.
func (r *Registry) IncLoad(id ids.IdentityID) (int, error) {
	var newLoad int
	err := r.shardFor(id).update(id, func(rec *SeederRecord) error {
		rec.Load++
		newLoad = rec.Load
		return nil
	})
	if err != nil {
		return 0, err
	}
	return newLoad, nil
}

// DecLoad decrements the seeder's in-flight offer count by one. Returns the
// new load value. ErrLoadUnderflow is returned (and no mutation occurs) if
// load is already 0.
func (r *Registry) DecLoad(id ids.IdentityID) (int, error) {
	var newLoad int
	err := r.shardFor(id).update(id, func(rec *SeederRecord) error {
		if rec.Load <= 0 {
			return ErrLoadUnderflow
		}
		rec.Load--
		newLoad = rec.Load
		return nil
	})
	if err != nil {
		return 0, err
	}
	return newLoad, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
go test -race ./internal/registry/...
```
Expected: PASS.

- [ ] **Step 5: Lint clean**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
golangci-lint run ./internal/registry/...
```

- [ ] **Step 6: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star
git add tracker/internal/registry/registry.go tracker/internal/registry/registry_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/registry): IncLoad + DecLoad with underflow detection

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 12: `Snapshot`

`Snapshot` returns a deep copy of every record across all shards. Order is unspecified. Used by ops/admin endpoints and as a fallback for the broker when no filtered match path is available.

**Files:**
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/registry.go`
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/registry_test.go`

- [ ] **Step 1: Append the failing tests**

```go
func TestRegistry_Snapshot_Empty(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)
	out := r.Snapshot()
	assert.Empty(t, out)
}

func TestRegistry_Snapshot_ReturnsAllRegistered(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)

	for i := 0; i < 32; i++ {
		var id ids.IdentityID
		id[0] = byte(i)
		require.NoError(t, r.Register(SeederRecord{IdentityID: id, Load: i}))
	}

	out := r.Snapshot()
	assert.Len(t, out, 32)

	// Verify every IdentityID we put in shows up exactly once.
	seen := make(map[ids.IdentityID]bool, 32)
	for _, rec := range out {
		assert.False(t, seen[rec.IdentityID], "duplicate record in snapshot")
		seen[rec.IdentityID] = true
	}
	assert.Len(t, seen, 32)
}

func TestRegistry_Snapshot_DeepCopiesSlices(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)

	id := ids.IdentityID{0xA1}
	require.NoError(t, r.Register(SeederRecord{
		IdentityID:   id,
		Capabilities: Capabilities{Models: []string{"claude-opus-4-7"}},
	}))

	out := r.Snapshot()
	require.Len(t, out, 1)
	out[0].Capabilities.Models[0] = "MUTATED"

	got, _ := r.Get(id)
	assert.Equal(t, "claude-opus-4-7", got.Capabilities.Models[0])
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
go test ./internal/registry/...
```
Expected: FAIL — `undefined: r.Snapshot`.

- [ ] **Step 3: Append `Snapshot` to `registry.go`**

```go
// Snapshot returns a deep copy of every record currently in the registry.
// Order is unspecified. Each shard is briefly RLocked in turn — for very large
// registries the result is a near-consistent (not strictly atomic) view across
// shards, which is acceptable for the broker's selection workload.
func (r *Registry) Snapshot() []SeederRecord {
	var out []SeederRecord
	for _, sh := range r.shards {
		out = append(out, sh.snapshot()...)
	}
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
go test -race ./internal/registry/...
```
Expected: PASS.

- [ ] **Step 5: Lint clean**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
golangci-lint run ./internal/registry/...
```

- [ ] **Step 6: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star
git add tracker/internal/registry/registry.go tracker/internal/registry/registry_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/registry): Snapshot — deep-copy enumeration across shards

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 13: `Filter` + `Match`

Per spec §5.1, the broker filters candidates by: availability, model membership, tier membership, headroom ≥ θ, load < θ. Reputation freezing is deliberately not part of the registry filter — that lookup is the broker's responsibility.

**Files:**
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/registry.go`
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/registry_test.go`

- [ ] **Step 1: Append the failing tests**

```go
func TestRegistry_Match_EmptyRegistry(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)
	out := r.Match(Filter{})
	assert.Empty(t, out)
}

func TestRegistry_Match_RequireAvailable(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)
	require.NoError(t, r.Register(SeederRecord{IdentityID: ids.IdentityID{0x01}, Available: true}))
	require.NoError(t, r.Register(SeederRecord{IdentityID: ids.IdentityID{0x02}, Available: false}))

	out := r.Match(Filter{RequireAvailable: true})
	require.Len(t, out, 1)
	assert.Equal(t, ids.IdentityID{0x01}, out[0].IdentityID)
}

func TestRegistry_Match_FilterByModel(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)
	require.NoError(t, r.Register(SeederRecord{
		IdentityID:   ids.IdentityID{0x01},
		Capabilities: Capabilities{Models: []string{"claude-opus-4-7"}},
	}))
	require.NoError(t, r.Register(SeederRecord{
		IdentityID:   ids.IdentityID{0x02},
		Capabilities: Capabilities{Models: []string{"claude-sonnet-4-6"}},
	}))
	require.NoError(t, r.Register(SeederRecord{
		IdentityID:   ids.IdentityID{0x03},
		Capabilities: Capabilities{Models: []string{"claude-opus-4-7", "claude-sonnet-4-6"}},
	}))

	out := r.Match(Filter{Model: "claude-opus-4-7"})
	require.Len(t, out, 2)

	got := map[ids.IdentityID]bool{}
	for _, rec := range out {
		got[rec.IdentityID] = true
	}
	assert.True(t, got[ids.IdentityID{0x01}])
	assert.True(t, got[ids.IdentityID{0x03}])
}

func TestRegistry_Match_FilterByTier(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)
	require.NoError(t, r.Register(SeederRecord{
		IdentityID:   ids.IdentityID{0x01},
		Capabilities: Capabilities{Tiers: []proto.PrivacyTier{proto.PrivacyTier_PRIVACY_TIER_STANDARD}},
	}))
	require.NoError(t, r.Register(SeederRecord{
		IdentityID:   ids.IdentityID{0x02},
		Capabilities: Capabilities{Tiers: []proto.PrivacyTier{proto.PrivacyTier_PRIVACY_TIER_TEE}},
	}))

	out := r.Match(Filter{Tier: proto.PrivacyTier_PRIVACY_TIER_TEE})
	require.Len(t, out, 1)
	assert.Equal(t, ids.IdentityID{0x02}, out[0].IdentityID)
}

func TestRegistry_Match_TierUnspecifiedSkipsTierFilter(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)
	require.NoError(t, r.Register(SeederRecord{
		IdentityID:   ids.IdentityID{0x01},
		Capabilities: Capabilities{Tiers: []proto.PrivacyTier{proto.PrivacyTier_PRIVACY_TIER_STANDARD}},
	}))
	require.NoError(t, r.Register(SeederRecord{
		IdentityID:   ids.IdentityID{0x02},
		Capabilities: Capabilities{Tiers: []proto.PrivacyTier{proto.PrivacyTier_PRIVACY_TIER_TEE}},
	}))

	out := r.Match(Filter{Tier: proto.PrivacyTier_PRIVACY_TIER_UNSPECIFIED})
	assert.Len(t, out, 2)
}

func TestRegistry_Match_FilterByMinHeadroom(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)
	require.NoError(t, r.Register(SeederRecord{IdentityID: ids.IdentityID{0x01}, HeadroomEstimate: 0.10}))
	require.NoError(t, r.Register(SeederRecord{IdentityID: ids.IdentityID{0x02}, HeadroomEstimate: 0.50}))
	require.NoError(t, r.Register(SeederRecord{IdentityID: ids.IdentityID{0x03}, HeadroomEstimate: 0.75}))

	out := r.Match(Filter{MinHeadroom: 0.5})
	require.Len(t, out, 2)
	for _, rec := range out {
		assert.GreaterOrEqual(t, rec.HeadroomEstimate, 0.5)
	}
}

func TestRegistry_Match_FilterByMaxLoad(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)
	require.NoError(t, r.Register(SeederRecord{IdentityID: ids.IdentityID{0x01}, Load: 0}))
	require.NoError(t, r.Register(SeederRecord{IdentityID: ids.IdentityID{0x02}, Load: 4}))
	require.NoError(t, r.Register(SeederRecord{IdentityID: ids.IdentityID{0x03}, Load: 5}))
	require.NoError(t, r.Register(SeederRecord{IdentityID: ids.IdentityID{0x04}, Load: 9}))

	out := r.Match(Filter{MaxLoad: 5})
	require.Len(t, out, 2, "MaxLoad is exclusive: only Load < MaxLoad passes")
	for _, rec := range out {
		assert.Less(t, rec.Load, 5)
	}
}

func TestRegistry_Match_MaxLoadZeroExcludesEveryone(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)
	require.NoError(t, r.Register(SeederRecord{IdentityID: ids.IdentityID{0x01}, Load: 0}))
	out := r.Match(Filter{MaxLoad: 0})
	assert.Empty(t, out, "MaxLoad=0 means 'load < 0' which no record satisfies")
}

func TestRegistry_Match_AllFiltersTogether(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)

	// Matches: opus, standard tier, headroom 0.6, load 2, available.
	matchID := ids.IdentityID{0x01}
	require.NoError(t, r.Register(SeederRecord{
		IdentityID: matchID,
		Available:  true,
		Capabilities: Capabilities{
			Models: []string{"claude-opus-4-7"},
			Tiers:  []proto.PrivacyTier{proto.PrivacyTier_PRIVACY_TIER_STANDARD},
		},
		HeadroomEstimate: 0.6,
		Load:             2,
	}))
	// Wrong model.
	require.NoError(t, r.Register(SeederRecord{
		IdentityID:   ids.IdentityID{0x02},
		Available:    true,
		Capabilities: Capabilities{Models: []string{"claude-sonnet-4-6"}},
	}))
	// Headroom too low.
	require.NoError(t, r.Register(SeederRecord{
		IdentityID: ids.IdentityID{0x03},
		Available:  true,
		Capabilities: Capabilities{
			Models: []string{"claude-opus-4-7"},
			Tiers:  []proto.PrivacyTier{proto.PrivacyTier_PRIVACY_TIER_STANDARD},
		},
		HeadroomEstimate: 0.05,
	}))
	// Load too high.
	require.NoError(t, r.Register(SeederRecord{
		IdentityID: ids.IdentityID{0x04},
		Available:  true,
		Capabilities: Capabilities{
			Models: []string{"claude-opus-4-7"},
			Tiers:  []proto.PrivacyTier{proto.PrivacyTier_PRIVACY_TIER_STANDARD},
		},
		HeadroomEstimate: 0.6,
		Load:             10,
	}))
	// Not available.
	require.NoError(t, r.Register(SeederRecord{
		IdentityID: ids.IdentityID{0x05},
		Available:  false,
		Capabilities: Capabilities{
			Models: []string{"claude-opus-4-7"},
			Tiers:  []proto.PrivacyTier{proto.PrivacyTier_PRIVACY_TIER_STANDARD},
		},
		HeadroomEstimate: 0.6,
	}))

	out := r.Match(Filter{
		RequireAvailable: true,
		Model:            "claude-opus-4-7",
		Tier:             proto.PrivacyTier_PRIVACY_TIER_STANDARD,
		MinHeadroom:      0.2,
		MaxLoad:          5,
	})
	require.Len(t, out, 1)
	assert.Equal(t, matchID, out[0].IdentityID)
}

func TestRegistry_Match_DeepCopies(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)

	id := ids.IdentityID{0xB1}
	require.NoError(t, r.Register(SeederRecord{
		IdentityID:   id,
		Available:    true,
		Capabilities: Capabilities{Models: []string{"claude-opus-4-7"}},
	}))

	out := r.Match(Filter{RequireAvailable: true, Model: "claude-opus-4-7"})
	require.Len(t, out, 1)
	out[0].Capabilities.Models[0] = "MUTATED"

	got, _ := r.Get(id)
	assert.Equal(t, "claude-opus-4-7", got.Capabilities.Models[0])
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
go test ./internal/registry/...
```
Expected: FAIL — `undefined: Filter`, `undefined: r.Match`.

- [ ] **Step 3: Append `Filter` and `Match` to `registry.go`**

```go
// Filter constrains the records returned by Match.
//
// Empty / zero-valued fields are interpreted as "no constraint" (Model == "",
// Tier == proto.PrivacyTier_PRIVACY_TIER_UNSPECIFIED, RequireAvailable ==
// false, MinHeadroom == 0.0). MaxLoad is interpreted as "load strictly less
// than MaxLoad" — a literal zero excludes everyone, since load is always
// non-negative.
//
// Reputation freeze is intentionally not a Filter field. The broker performs
// that lookup via the reputation subsystem after Match returns candidates.
type Filter struct {
	RequireAvailable bool
	Model            string
	Tier             proto.PrivacyTier
	MinHeadroom      float64
	MaxLoad          int
}

// Match returns every record that satisfies the filter. Returned records are
// deep copies; mutating the result does not affect the store. Order is
// unspecified.
func (r *Registry) Match(f Filter) []SeederRecord {
	all := r.Snapshot()
	out := all[:0]
	for _, rec := range all {
		if f.RequireAvailable && !rec.Available {
			continue
		}
		if f.Model != "" && !containsString(rec.Capabilities.Models, f.Model) {
			continue
		}
		if f.Tier != proto.PrivacyTier_PRIVACY_TIER_UNSPECIFIED &&
			!containsTier(rec.Capabilities.Tiers, f.Tier) {
			continue
		}
		if rec.HeadroomEstimate < f.MinHeadroom {
			continue
		}
		if rec.Load >= f.MaxLoad {
			continue
		}
		out = append(out, rec)
	}
	return out
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func containsTier(xs []proto.PrivacyTier, want proto.PrivacyTier) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
```

You will also need `"github.com/token-bay/token-bay/shared/proto"` in `registry.go`'s import block.

- [ ] **Step 4: Run test to verify it passes**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
go test -race ./internal/registry/...
```
Expected: PASS.

- [ ] **Step 5: Lint clean**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
golangci-lint run ./internal/registry/...
```

- [ ] **Step 6: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star
git add tracker/internal/registry/registry.go tracker/internal/registry/registry_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/registry): Filter + Match candidate selection

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 14: `Sweep` (stale removal)

A periodic GC the session module (or a dedicated janitor goroutine) calls to evict seeders whose heartbeats are too old. Returns the number removed for metrics.

**Files:**
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/registry.go`
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/registry_test.go`

- [ ] **Step 1: Append the failing tests**

```go
func TestRegistry_Sweep_RemovesStale(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)

	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-2 * time.Hour)
	fresh := now.Add(-1 * time.Minute)

	require.NoError(t, r.Register(SeederRecord{IdentityID: ids.IdentityID{0x01}, LastHeartbeat: stale}))
	require.NoError(t, r.Register(SeederRecord{IdentityID: ids.IdentityID{0x02}, LastHeartbeat: fresh}))
	require.NoError(t, r.Register(SeederRecord{IdentityID: ids.IdentityID{0x03}, LastHeartbeat: stale}))

	removed := r.Sweep(now.Add(-1 * time.Hour))
	assert.Equal(t, 2, removed)

	_, ok1 := r.Get(ids.IdentityID{0x01})
	_, ok2 := r.Get(ids.IdentityID{0x02})
	_, ok3 := r.Get(ids.IdentityID{0x03})
	assert.False(t, ok1)
	assert.True(t, ok2)
	assert.False(t, ok3)
}

func TestRegistry_Sweep_EmptyRegistry(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)
	assert.Equal(t, 0, r.Sweep(time.Now()))
}

func TestRegistry_Sweep_BoundaryIsInclusive(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)

	t0 := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	require.NoError(t, r.Register(SeederRecord{IdentityID: ids.IdentityID{0x01}, LastHeartbeat: t0}))

	// Sweeping at staleBefore == t0 should remove the record (inclusive).
	removed := r.Sweep(t0)
	assert.Equal(t, 1, removed)
}

func TestRegistry_Sweep_FreshRecordSurvives(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)

	t0 := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	require.NoError(t, r.Register(SeederRecord{IdentityID: ids.IdentityID{0x01}, LastHeartbeat: t0.Add(time.Second)}))

	removed := r.Sweep(t0)
	assert.Equal(t, 0, removed)

	_, ok := r.Get(ids.IdentityID{0x01})
	assert.True(t, ok)
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
go test ./internal/registry/...
```
Expected: FAIL — `undefined: r.Sweep`.

- [ ] **Step 3: Append `Sweep` to `registry.go`**

```go
// Sweep removes every seeder whose LastHeartbeat is at or before staleBefore.
// Returns the count removed. Each shard is locked in turn — sweeps do not
// hold a global lock, so concurrent reads on un-affected shards proceed.
func (r *Registry) Sweep(staleBefore time.Time) int {
	total := 0
	for _, sh := range r.shards {
		total += sh.sweepStale(staleBefore)
	}
	return total
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
go test -race ./internal/registry/...
```
Expected: PASS.

- [ ] **Step 5: Lint clean**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
golangci-lint run ./internal/registry/...
```

- [ ] **Step 6: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star
git add tracker/internal/registry/registry.go tracker/internal/registry/registry_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/registry): Sweep — stale-heartbeat eviction with count

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 15: Concurrency stress test under `-race`

A behavior test that hammers a single registry with many goroutines doing every operation. Doesn't assert outcome shape — its only job is to surface data races (the `-race` flag does the work). Per CLAUDE.md and spec §6, the registry's concurrency is a property the test suite must lock down explicitly.

**Files:**
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker/internal/registry/registry_test.go`

- [ ] **Step 1: Append the test**

```go
func TestRegistry_ConcurrentMixedOps_NoRaces(t *testing.T) {
	const (
		numIdentities = 64
		numWorkers    = 16
		opsPerWorker  = 200
	)

	r, err := New(DefaultShardCount)
	require.NoError(t, err)

	// Pre-seed every identity so update operations have something to find.
	now := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	for i := 0; i < numIdentities; i++ {
		var id ids.IdentityID
		id[0] = byte(i)
		require.NoError(t, r.Register(SeederRecord{
			IdentityID:    id,
			Available:     true,
			LastHeartbeat: now,
			Capabilities: Capabilities{
				Models: []string{"claude-opus-4-7"},
				Tiers:  []proto.PrivacyTier{proto.PrivacyTier_PRIVACY_TIER_STANDARD},
			},
			HeadroomEstimate: 0.5,
		}))
	}

	pickID := func(seed int) ids.IdentityID {
		var id ids.IdentityID
		id[0] = byte(seed % numIdentities)
		return id
	}

	var wg sync.WaitGroup
	wg.Add(numWorkers)
	for w := 0; w < numWorkers; w++ {
		go func(workerID int) {
			defer wg.Done()
			for op := 0; op < opsPerWorker; op++ {
				id := pickID(workerID*1000 + op)
				switch (workerID + op) % 8 {
				case 0:
					_, _ = r.Get(id)
				case 1:
					_ = r.Heartbeat(id, now.Add(time.Duration(op)*time.Second))
				case 2:
					_ = r.UpdateExternalAddr(id, netip.MustParseAddrPort("203.0.113.1:443"))
				case 3:
					_ = r.UpdateReputation(id, 0.7)
				case 4:
					_, _ = r.IncLoad(id)
				case 5:
					_, _ = r.DecLoad(id) // may underflow; allowed
				case 6:
					_ = r.Match(Filter{
						RequireAvailable: true,
						Model:            "claude-opus-4-7",
						Tier:             proto.PrivacyTier_PRIVACY_TIER_STANDARD,
						MinHeadroom:      0.0,
						MaxLoad:          100,
					})
				case 7:
					_ = r.Snapshot()
				}
			}
		}(w)
	}
	wg.Wait()

	// Sanity: registry still functional after the storm.
	assert.NotEmpty(t, r.Snapshot())
}
```

You will need to add `"sync"` to the test file's import block if not already present.

- [ ] **Step 2: Run with `-race` to verify it passes (the assertion is "no race detected")**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
go test -race -run TestRegistry_ConcurrentMixedOps_NoRaces ./internal/registry/...
```
Expected: PASS, no `WARNING: DATA RACE` output.

- [ ] **Step 3: Run the full registry suite under `-race`**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
go test -race ./internal/registry/...
```
Expected: every test PASS.

- [ ] **Step 4: Lint clean**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
golangci-lint run ./internal/registry/...
```

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star
git add tracker/internal/registry/registry_test.go
git commit -m "$(cat <<'EOF'
test(tracker/registry): concurrent mixed-ops race test under -race

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 16: Final coverage + verification + commit

**Files:**
- None (verification only).

- [ ] **Step 1: Run the full tracker test suite**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
make test
```
Expected: every package passes; the existing `cmd/token-bay-tracker` version test plus the new registry suite.

- [ ] **Step 2: Coverage check on registry package**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
go test -race -coverprofile=/tmp/registry.cov ./internal/registry/...
go tool cover -func=/tmp/registry.cov | tail -5
```
Expected: total coverage ≥ 90%. If under, add tests for the gap before tagging.

- [ ] **Step 3: Run full tracker `make check`**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star/tracker
make check
```
Expected: PASS for both `test` and `lint`.

- [ ] **Step 4: Run repo-root `make check`**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star
make check
```
Expected: shared, plugin, tracker — all three green. The tracker change must not have broken any cross-module behavior.

- [ ] **Step 5: Tag**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/sand-star
git tag -a tracker-registry-v0 -m "tracker/internal/registry: v0 — sharded in-memory seeder registry"
```

---

## Self-review

**Spec coverage (against `2026-04-22-tracker-design.md`):**
- §3 internal modules — `registry/` exists (Tasks 2, 6).
- §4.1 SeederRecord — every field present (Task 3): identity_id, conn_session_id, capabilities (models, max_context, tiers, attestation), availability, headroom_estimate, reputation_score, net_coords (external_addr, local_candidates), load, last_heartbeat. Sharded by `hash(identity_id) % num_shards` (Task 6, `shardIndex`).
- §5.1 broker filter — Match implements availability / model membership / tier membership / headroom ≥ θ / load < θ (Task 13). Reputation freezing intentionally excluded (broker's responsibility — explicit in Filter doc-comment).
- §5.4 STUN — `UpdateExternalAddr` lets the STUN module refresh the reflexive address on heartbeat (Task 8).
- §6 concurrency model — fine-grained per-shard `sync.RWMutex`, no global lock (Task 5). Race-detector behavior test (Task 15).
- Settlement / reservation / scoring — explicitly out of scope; broker's responsibility per spec §5.

**Placeholder scan:** None. Every test body is full Go code; every implementation step shows the exact code to add.

**Type consistency:** `Capabilities.Tiers` is `[]proto.PrivacyTier` everywhere it appears (Tasks 3, 13, 15). `LastHeartbeat` is `time.Time` everywhere (Tasks 3, 5, 8, 14, 15). `IncLoad`/`DecLoad` consistently return `(int, error)` and the new value (Task 11). `shardIndex` and `shardFor` consistent across registry.go (Tasks 6 onward). `MaxLoad` semantics ("load < MaxLoad") is consistent in the test (Task 13) and the implementation (Task 13 step 3).

**TDD discipline:** Every behavioral task is red→green→commit. Tasks 2 and 16 are docs/verification and have no red phase; that is by design.

**Out of scope (deferred to follow-on plans):**
- Persistence — registry is in-memory by spec.
- Reputation freeze checking — lives in `internal/reputation`.
- Selection scoring — lives in `internal/broker`.
- Wiring into the server lifecycle — `internal/server` plan.

---

## Execution Handoff

**Plan complete and saved to `docs/superpowers/plans/2026-04-25-tracker-registry.md`. Two execution options:**

**1. Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, review between tasks, fast iteration.

**2. Inline Execution** — Execute tasks in this session using executing-plans, batch execution with checkpoints.

**Which approach?**
