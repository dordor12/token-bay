# Federation — Revocation Gossip Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement federation §6 revocation gossip: when local reputation freezes an identity, sign and broadcast a `REVOCATION` to peers; archive inbound revocations and forward via the slice-0 dedupe-and-forward gossip core.

**Architecture:** A new `revocationCoordinator` lives inside `tracker/internal/federation` alongside the slice-1 `transferCoordinator`. The reputation subsystem gains a single `FreezeListener` Option pointing at `*Federation`; reputation calls `OnFreeze(id, reason, t)` synchronously at the existing `evaluator.go` freeze site. Inbound revocations are validated, issuer-sig-verified against the operator-managed peer pubkey allowlist, persisted to a new additive `peer_revocations` SQLite table, and forwarded via `gossip.Forward`. Federation satisfies reputation's `FreezeListener` interface via Go structural typing — no `federation→reputation` import, no `reputation→federation` import.

**Tech Stack:** Go 1.23+, `google.golang.org/protobuf`, stdlib `crypto/ed25519`, `modernc.org/sqlite`, `prometheus/client_golang`, `testify`.

**Spec:** `docs/superpowers/specs/federation/2026-05-10-federation-revocation-gossip-design.md` (commit `6d1709a` on this branch).

---

## File Structure

| Path | Action | Responsibility |
|---|---|---|
| `shared/federation/federation.proto` | MODIFY | Add `KIND_REVOCATION = 12`, `Revocation` message, `RevocationReason` enum |
| `shared/federation/federation.pb.go` | REGEN | `make -C shared proto` (or `protoc`); regenerated, not hand-edited |
| `shared/federation/validate.go` | MODIFY | Add `ValidateRevocation`; lift envelope ceiling to `KIND_REVOCATION` |
| `shared/federation/validate_test.go` | MODIFY | Tests for `ValidateRevocation` happy + bad-shape table |
| `shared/federation/signing_revocation.go` | CREATE | `CanonicalRevocationPreSig` (zeros `tracker_sig`) |
| `shared/federation/signing_revocation_test.go` | CREATE | Determinism + tracker_sig zeroing + tamper-detect |
| `tracker/internal/ledger/storage/schema_v1.sql` | MODIFY | Append `CREATE TABLE IF NOT EXISTS peer_revocations` + index |
| `tracker/internal/ledger/storage/peer_revocations.go` | CREATE | `PeerRevocation` struct, `PutPeerRevocation`, `GetPeerRevocation` |
| `tracker/internal/ledger/storage/peer_revocations_test.go` | CREATE | Round-trip, idempotent duplicate, missing returns ok=false |
| `tracker/internal/federation/revocation_archive.go` | CREATE | `PeerRevocationArchive` interface (storage slice federation needs) |
| `tracker/internal/federation/revocation.go` | CREATE | `revocationCoordinator`, `OnFreeze`, `OnIncoming`, reason mapper |
| `tracker/internal/federation/revocation_test.go` | CREATE | Unit tests for coordinator (happy + drop branches) |
| `tracker/internal/federation/config.go` | MODIFY | Add `Deps.RevocationArchive PeerRevocationArchive` |
| `tracker/internal/federation/subsystem.go` | MODIFY | Wire `revocationCoordinator`; dispatch `KIND_REVOCATION`; add `Federation.OnFreeze` |
| `tracker/internal/federation/integration_test.go` | MODIFY | Two- and three-tracker revocation propagation tests |
| `tracker/internal/federation/metrics.go` | MODIFY | `RevocationsEmitted`, `RevocationsReceived(outcome)` |
| `tracker/internal/federation/metrics_test.go` | MODIFY | Test the new counters |
| `tracker/internal/reputation/listeners.go` | CREATE | `FreezeListener` interface + `WithFreezeListener` Option |
| `tracker/internal/reputation/reputation.go` | MODIFY | Hold optional listener; emit at freeze site |
| `tracker/internal/reputation/listeners_test.go` | CREATE | Listener fires on AUDIT→FROZEN transition |
| `tracker/cmd/token-bay-tracker/run_cmd.go` | MODIFY | Reorder so federation opens first; pass `WithFreezeListener(fed)` to reputation |

---

## Notes for the executor

- Read the spec (`docs/superpowers/specs/federation/2026-05-10-federation-revocation-gossip-design.md`) before Task 1; it contains the wire-format and dataflow contracts you'll be encoding.
- Read `tracker/internal/federation/transfer.go` and `tracker/internal/federation/rootattest.go` first — slice 2 mirrors their patterns (proto-marshal, canonical-sig, dispatcher hook, in-memory state, gossip forward).
- The reputation package is a leaf module per `tracker/internal/reputation/CLAUDE.md` — it cannot import `tracker/internal/federation`. Federation satisfies `FreezeListener` via Go structural typing; no compile-time assertion is needed.
- All federation tests must pass `go test -race`. The package is on the always-`-race` list.
- One conventional commit per red-green cycle (`feat:`, `fix:`, `test:`, `refactor:`, `docs:`, `chore:`). Don't squash multiple TDD cycles into one commit.
- Working directory throughout: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/federation`. The `go.work` file links the three modules; `make check` at the repo root runs all of them.
- Use `go test ./tracker/internal/federation/...`, `go test ./shared/federation/...`, etc. — not `cd` first; absolute paths or repo-root invocations are fine.

---

### Task 1: Add KIND_REVOCATION + Revocation proto message

**Files:**
- Modify: `shared/federation/federation.proto:18-21`

- [ ] **Step 1: Edit the proto.** Add `KIND_REVOCATION = 12;` to the `Kind` enum (right after `KIND_TRANSFER_APPLIED = 11`). Append the new enum and message at end of file.

```proto
  KIND_REVOCATION             = 12;
```

Append at end of file:

```proto
enum RevocationReason {
  REVOCATION_REASON_UNSPECIFIED = 0;
  REVOCATION_REASON_ABUSE       = 1;  // reputation-driven freeze
  REVOCATION_REASON_MANUAL      = 2;  // operator-issued
  REVOCATION_REASON_EXPIRED     = 3;  // identity TTL elapsed
}

message Revocation {
  bytes            tracker_id  = 1;  // 32 bytes — issuer
  bytes            identity_id = 2;  // 32 bytes — revoked identity
  RevocationReason reason      = 3;
  uint64           revoked_at  = 4;  // unix seconds
  bytes            tracker_sig = 5;  // 64 bytes — Ed25519(canonical) by issuer
}
```

- [ ] **Step 2: Regenerate.** Run `make -C shared proto-gen` from the repo root. Expect `shared/federation/federation.pb.go` to update.

- [ ] **Step 3: Compile-check.** Run `go build ./shared/federation/...`. Expected: success. (No callers reference the new types yet.)

- [ ] **Step 4: Commit.**

```bash
git add shared/federation/federation.proto shared/federation/federation.pb.go
git commit -m "feat(shared/federation): KIND_REVOCATION + Revocation + RevocationReason proto"
```

---

### Task 2: ValidateRevocation + ceiling lift (red)

**Files:**
- Modify: `shared/federation/validate.go`
- Modify: `shared/federation/validate_test.go`

- [ ] **Step 1: Write the failing test.** Append to `shared/federation/validate_test.go`:

```go
func validRevocation() *Revocation {
	return &Revocation{
		TrackerId:  bytes.Repeat([]byte{1}, TrackerIDLen),
		IdentityId: bytes.Repeat([]byte{2}, TrackerIDLen),
		Reason:     RevocationReason_REVOCATION_REASON_ABUSE,
		RevokedAt:  1714000000,
		TrackerSig: bytes.Repeat([]byte{3}, SigLen),
	}
}

func TestValidateRevocation_HappyPath(t *testing.T) {
	require.NoError(t, ValidateRevocation(validRevocation()))
}

func TestValidateRevocation_BadShape(t *testing.T) {
	cases := []struct {
		name  string
		mutate func(*Revocation)
	}{
		{"nil", func(_ *Revocation) {}},
		{"tracker_id wrong len", func(r *Revocation) { r.TrackerId = bytes.Repeat([]byte{1}, 16) }},
		{"tracker_id all zero", func(r *Revocation) { r.TrackerId = bytes.Repeat([]byte{0}, TrackerIDLen) }},
		{"identity_id wrong len", func(r *Revocation) { r.IdentityId = bytes.Repeat([]byte{2}, 16) }},
		{"identity_id all zero", func(r *Revocation) { r.IdentityId = bytes.Repeat([]byte{0}, TrackerIDLen) }},
		{"tracker_sig wrong len", func(r *Revocation) { r.TrackerSig = bytes.Repeat([]byte{3}, 32) }},
		{"revoked_at zero", func(r *Revocation) { r.RevokedAt = 0 }},
		{"reason unspecified", func(r *Revocation) { r.Reason = RevocationReason_REVOCATION_REASON_UNSPECIFIED }},
		{"reason out of range", func(r *Revocation) { r.Reason = RevocationReason(99) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var m *Revocation
			if tc.name != "nil" {
				m = validRevocation()
				tc.mutate(m)
			}
			require.Error(t, ValidateRevocation(m))
		})
	}
}

func TestValidateEnvelope_AcceptsKindRevocation(t *testing.T) {
	env := &Envelope{
		SenderId:  bytes.Repeat([]byte{1}, TrackerIDLen),
		Kind:      Kind_KIND_REVOCATION,
		Payload:   []byte{0xAA},
		SenderSig: bytes.Repeat([]byte{3}, SigLen),
	}
	require.NoError(t, ValidateEnvelope(env))
}
```

- [ ] **Step 2: Run the test, expect failure.**

Run: `go test ./shared/federation/ -run 'TestValidateRevocation|TestValidateEnvelope_AcceptsKindRevocation' -v`
Expected: FAIL — `ValidateRevocation` undefined; `ValidateEnvelope` rejects `KIND_REVOCATION` (out of range).

---

### Task 3: ValidateRevocation + ceiling lift (green)

**Files:**
- Modify: `shared/federation/validate.go:27` (envelope kind ceiling)
- Modify: `shared/federation/validate.go` (append `ValidateRevocation`)

- [ ] **Step 1: Lift envelope ceiling.** In `ValidateEnvelope`, change the upper-bound check:

```go
	if e.Kind <= Kind_KIND_UNSPECIFIED || e.Kind > Kind_KIND_REVOCATION {
```

- [ ] **Step 2: Append `ValidateRevocation` at end of file:**

```go
// ValidateRevocation enforces shape invariants on a Revocation. Callers
// MUST verify the issuer's tracker_sig separately (validation only checks
// shape, not signatures).
func ValidateRevocation(m *Revocation) error {
	if m == nil {
		return errors.New("federation: nil Revocation")
	}
	if len(m.TrackerId) != TrackerIDLen {
		return fmt.Errorf("federation: revocation.tracker_id len %d != %d", len(m.TrackerId), TrackerIDLen)
	}
	if allZero(m.TrackerId) {
		return errors.New("federation: revocation.tracker_id is all zero")
	}
	if len(m.IdentityId) != TrackerIDLen {
		return fmt.Errorf("federation: revocation.identity_id len %d != %d", len(m.IdentityId), TrackerIDLen)
	}
	if allZero(m.IdentityId) {
		return errors.New("federation: revocation.identity_id is all zero")
	}
	if len(m.TrackerSig) != SigLen {
		return fmt.Errorf("federation: revocation.tracker_sig len %d != %d", len(m.TrackerSig), SigLen)
	}
	if m.RevokedAt == 0 {
		return errors.New("federation: revocation.revoked_at must be > 0")
	}
	if m.Reason <= RevocationReason_REVOCATION_REASON_UNSPECIFIED || m.Reason > RevocationReason_REVOCATION_REASON_EXPIRED {
		return fmt.Errorf("federation: revocation.reason %d out of range", int32(m.Reason))
	}
	return nil
}
```

- [ ] **Step 3: Run the test, expect pass.**

Run: `go test ./shared/federation/ -run 'TestValidateRevocation|TestValidateEnvelope_AcceptsKindRevocation' -v`
Expected: PASS.

- [ ] **Step 4: Run full validate suite.**

Run: `go test ./shared/federation/ -v`
Expected: PASS — existing transfer + rootattest tests still pass with the higher ceiling.

- [ ] **Step 5: Commit.**

```bash
git add shared/federation/validate.go shared/federation/validate_test.go
git commit -m "feat(shared/federation): ValidateRevocation + KIND_REVOCATION envelope acceptance"
```

---

### Task 4: CanonicalRevocationPreSig (red)

**Files:**
- Create: `shared/federation/signing_revocation_test.go`

- [ ] **Step 1: Write the failing test.**

```go
package federation

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCanonicalRevocationPreSig_ZeroesTrackerSig(t *testing.T) {
	m := validRevocation()
	m.TrackerSig = bytes.Repeat([]byte{0xAB}, SigLen)
	a, err := CanonicalRevocationPreSig(m)
	require.NoError(t, err)

	m2 := validRevocation()
	m2.TrackerSig = bytes.Repeat([]byte{0xCD}, SigLen)
	b, err := CanonicalRevocationPreSig(m2)
	require.NoError(t, err)

	assert.Equal(t, a, b, "tracker_sig must be zeroed before canonical marshal")
}

func TestCanonicalRevocationPreSig_DoesNotMutate(t *testing.T) {
	m := validRevocation()
	want := bytes.Repeat([]byte{0xAB}, SigLen)
	m.TrackerSig = append([]byte(nil), want...)
	_, err := CanonicalRevocationPreSig(m)
	require.NoError(t, err)
	assert.Equal(t, want, m.TrackerSig, "input must not be mutated")
}

func TestCanonicalRevocationPreSig_TamperDetect(t *testing.T) {
	a, err := CanonicalRevocationPreSig(validRevocation())
	require.NoError(t, err)

	tampered := validRevocation()
	tampered.RevokedAt = 9999999999
	b, err := CanonicalRevocationPreSig(tampered)
	require.NoError(t, err)
	assert.NotEqual(t, a, b)
}

func TestCanonicalRevocationPreSig_NilErrors(t *testing.T) {
	_, err := CanonicalRevocationPreSig(nil)
	require.Error(t, err)
}
```

- [ ] **Step 2: Run the test, expect failure.**

Run: `go test ./shared/federation/ -run TestCanonicalRevocationPreSig -v`
Expected: FAIL — `CanonicalRevocationPreSig` undefined.

---

### Task 5: CanonicalRevocationPreSig (green)

**Files:**
- Create: `shared/federation/signing_revocation.go`

- [ ] **Step 1: Create the helper.**

```go
// Package federation: canonical pre-sig byte builder for Revocation. Like
// signing_transfer.go, this routes through shared/signing.DeterministicMarshal
// (CLAUDE.md §6: every signed proto goes through that single determinism
// choke point).
package federation

import (
	"errors"

	"github.com/token-bay/token-bay/shared/signing"
	"google.golang.org/protobuf/proto"
)

// CanonicalRevocationPreSig returns the deterministic byte representation
// of m with tracker_sig cleared. The issuer signs the returned bytes; the
// verifier reconstructs identically.
func CanonicalRevocationPreSig(m *Revocation) ([]byte, error) {
	if m == nil {
		return nil, errors.New("federation: nil Revocation")
	}
	clone, ok := proto.Clone(m).(*Revocation)
	if !ok {
		return nil, errors.New("federation: clone Revocation")
	}
	clone.TrackerSig = nil
	return signing.DeterministicMarshal(clone)
}
```

- [ ] **Step 2: Run the test, expect pass.**

Run: `go test ./shared/federation/ -run TestCanonicalRevocationPreSig -v`
Expected: PASS.

- [ ] **Step 3: Commit.**

```bash
git add shared/federation/signing_revocation.go shared/federation/signing_revocation_test.go
git commit -m "feat(shared/federation): CanonicalRevocationPreSig"
```

---

### Task 6: peer_revocations table + Put/Get (red)

**Files:**
- Modify: `tracker/internal/ledger/storage/schema_v1.sql`
- Create: `tracker/internal/ledger/storage/peer_revocations_test.go`

- [ ] **Step 1: Add the table.** Append to `tracker/internal/ledger/storage/schema_v1.sql`:

```sql

CREATE TABLE IF NOT EXISTS peer_revocations (
    tracker_id  BLOB NOT NULL,         -- issuer
    identity_id BLOB NOT NULL,         -- revoked identity
    reason      INTEGER NOT NULL,      -- RevocationReason enum value
    revoked_at  INTEGER NOT NULL,      -- unix seconds (issuer's clock)
    tracker_sig BLOB NOT NULL,         -- 64 bytes
    received_at INTEGER NOT NULL,      -- unix seconds (local clock)
    PRIMARY KEY (tracker_id, identity_id)
);

CREATE INDEX IF NOT EXISTS idx_peer_revocations_identity
    ON peer_revocations (identity_id);
```

- [ ] **Step 2: Write the failing test.** Create `tracker/internal/ledger/storage/peer_revocations_test.go`:

```go
package storage

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func samplePeerRevocation(trackerByte, identityByte byte) PeerRevocation {
	return PeerRevocation{
		TrackerID:  bytes.Repeat([]byte{trackerByte}, 32),
		IdentityID: bytes.Repeat([]byte{identityByte}, 32),
		Reason:     1, // ABUSE
		RevokedAt:  1714000000,
		TrackerSig: bytes.Repeat([]byte{trackerByte ^ 0xAA}, 64),
		ReceivedAt: 1714000005,
	}
}

func TestPutPeerRevocation_RoundTrip(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	r := samplePeerRevocation(0x01, 0x02)
	require.NoError(t, s.PutPeerRevocation(ctx, r))

	got, ok, err := s.GetPeerRevocation(ctx, r.TrackerID, r.IdentityID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, r.TrackerID, got.TrackerID)
	assert.Equal(t, r.IdentityID, got.IdentityID)
	assert.Equal(t, r.Reason, got.Reason)
	assert.Equal(t, r.RevokedAt, got.RevokedAt)
	assert.Equal(t, r.TrackerSig, got.TrackerSig)
	assert.Equal(t, r.ReceivedAt, got.ReceivedAt)
}

func TestPutPeerRevocation_DuplicateIsNoop(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	r := samplePeerRevocation(0x01, 0x02)
	require.NoError(t, s.PutPeerRevocation(ctx, r))

	// Second put with different reason — INSERT OR IGNORE, first wins.
	r2 := r
	r2.Reason = 2 // MANUAL
	r2.ReceivedAt = 9999999999
	require.NoError(t, s.PutPeerRevocation(ctx, r2))

	got, ok, err := s.GetPeerRevocation(ctx, r.TrackerID, r.IdentityID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, uint32(1), got.Reason, "first writer wins")
	assert.Equal(t, uint64(1714000005), got.ReceivedAt, "first writer wins")
}

func TestGetPeerRevocation_Missing(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	_, ok, err := s.GetPeerRevocation(ctx,
		bytes.Repeat([]byte{0x99}, 32),
		bytes.Repeat([]byte{0x88}, 32))
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestPutPeerRevocation_RejectsEmptyFields(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	bad := samplePeerRevocation(0x01, 0x02)
	bad.TrackerID = nil
	require.Error(t, s.PutPeerRevocation(ctx, bad))
}
```

- [ ] **Step 3: Run the test, expect failure.**

Run: `go test ./tracker/internal/ledger/storage/ -run TestPutPeerRevocation -v`
Expected: FAIL — `PeerRevocation`, `PutPeerRevocation`, `GetPeerRevocation` undefined.

---

### Task 7: peer_revocations table + Put/Get (green)

**Files:**
- Create: `tracker/internal/ledger/storage/peer_revocations.go`

- [ ] **Step 1: Implement.** Create `tracker/internal/ledger/storage/peer_revocations.go`:

```go
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// PeerRevocation is one row from peer_revocations.
type PeerRevocation struct {
	TrackerID  []byte // 32 — issuer
	IdentityID []byte // 32 — revoked identity
	Reason     uint32 // RevocationReason enum
	RevokedAt  uint64 // unix seconds (issuer's clock)
	TrackerSig []byte // 64 bytes
	ReceivedAt uint64 // unix seconds (local clock)
}

// PutPeerRevocation persists r idempotently. INSERT OR IGNORE on the
// composite (tracker_id, identity_id) primary key: a duplicate revocation
// from gossip-echoed paths is a silent no-op, preserving the first writer.
func (s *Store) PutPeerRevocation(ctx context.Context, r PeerRevocation) error {
	if len(r.TrackerID) == 0 || len(r.IdentityID) == 0 || len(r.TrackerSig) == 0 {
		return errors.New("storage: PutPeerRevocation requires non-empty tracker_id, identity_id, tracker_sig")
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if _, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO peer_revocations
		    (tracker_id, identity_id, reason, revoked_at, tracker_sig, received_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		r.TrackerID, r.IdentityID, r.Reason, r.RevokedAt, r.TrackerSig, r.ReceivedAt,
	); err != nil {
		return fmt.Errorf("storage: PutPeerRevocation insert: %w", err)
	}
	return nil
}

// GetPeerRevocation returns the row for (trackerID, identityID), or
// ok=false on miss.
func (s *Store) GetPeerRevocation(ctx context.Context, trackerID, identityID []byte) (PeerRevocation, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT tracker_id, identity_id, reason, revoked_at, tracker_sig, received_at
		FROM peer_revocations WHERE tracker_id = ? AND identity_id = ?`,
		trackerID, identityID,
	)
	var r PeerRevocation
	err := row.Scan(&r.TrackerID, &r.IdentityID, &r.Reason, &r.RevokedAt, &r.TrackerSig, &r.ReceivedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PeerRevocation{}, false, nil
	}
	if err != nil {
		return PeerRevocation{}, false, fmt.Errorf("storage: GetPeerRevocation: %w", err)
	}
	return r, true, nil
}
```

- [ ] **Step 2: Run the test, expect pass.**

Run: `go test ./tracker/internal/ledger/storage/ -run TestPutPeerRevocation -v && go test ./tracker/internal/ledger/storage/ -run TestGetPeerRevocation -v`
Expected: PASS.

- [ ] **Step 3: Run the whole storage suite to confirm schema additions are clean.**

Run: `go test ./tracker/internal/ledger/storage/ -race`
Expected: PASS.

- [ ] **Step 4: Commit.**

```bash
git add tracker/internal/ledger/storage/schema_v1.sql tracker/internal/ledger/storage/peer_revocations.go tracker/internal/ledger/storage/peer_revocations_test.go
git commit -m "feat(tracker/storage): peer_revocations table + idempotent Put/Get"
```

---

### Task 8: PeerRevocationArchive interface in federation (red)

**Files:**
- Create: `tracker/internal/federation/revocation_archive.go`

- [ ] **Step 1: Write the file (interface only).**

```go
package federation

import (
	"context"

	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
)

// PeerRevocationArchive is the slice of *storage.Store the federation
// revocation path needs. Backed by *ledger/storage.Store in production;
// tests substitute an in-memory fake.
type PeerRevocationArchive interface {
	PutPeerRevocation(ctx context.Context, r storage.PeerRevocation) error
	GetPeerRevocation(ctx context.Context, trackerID, identityID []byte) (storage.PeerRevocation, bool, error)
}
```

- [ ] **Step 2: Compile-check.**

Run: `go build ./tracker/internal/federation/...`
Expected: success — `*storage.Store` already satisfies this interface (Task 7 added the methods with matching signatures).

- [ ] **Step 3: Commit.**

```bash
git add tracker/internal/federation/revocation_archive.go
git commit -m "feat(federation): PeerRevocationArchive interface"
```

---

### Task 9: Add Deps.RevocationArchive (red+green together — config slot only)

**Files:**
- Modify: `tracker/internal/federation/config.go`

- [ ] **Step 1: Add the Deps field.** In `tracker/internal/federation/config.go`, append to the `Deps` struct (after `Ledger LedgerHooks`):

```go
	// RevocationArchive is the federation→storage hook for peer
	// revocations. May be nil; when nil, Federation.OnFreeze is a
	// no-op and inbound KIND_REVOCATION is rejected with metric
	// reason "revocation_disabled".
	RevocationArchive PeerRevocationArchive
```

- [ ] **Step 2: Compile-check.**

Run: `go build ./tracker/internal/federation/...`
Expected: success — Deps struct accepts the new optional field; existing Open() callers (which leave it zero) still compile.

- [ ] **Step 3: Run the federation suite to confirm no regressions.**

Run: `go test ./tracker/internal/federation/... -race`
Expected: PASS.

- [ ] **Step 4: Commit.**

```bash
git add tracker/internal/federation/config.go
git commit -m "feat(federation): Deps.RevocationArchive optional slot"
```

---

### Task 10: revocationCoordinator + reason mapping helper (red)

**Files:**
- Create: `tracker/internal/federation/revocation_test.go`

- [ ] **Step 1: Write the failing tests.**

```go
package federation

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
	"google.golang.org/protobuf/proto"
)

// fakeRevocationArchive is a thread-safe in-memory PeerRevocationArchive.
type fakeRevocationArchive struct {
	mu   sync.Mutex
	rows map[[64]byte]storage.PeerRevocation
}

func newFakeRevocationArchive() *fakeRevocationArchive {
	return &fakeRevocationArchive{rows: map[[64]byte]storage.PeerRevocation{}}
}

func (f *fakeRevocationArchive) key(tr, id []byte) [64]byte {
	var k [64]byte
	copy(k[0:32], tr)
	copy(k[32:64], id)
	return k
}

func (f *fakeRevocationArchive) PutPeerRevocation(_ context.Context, r storage.PeerRevocation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := f.key(r.TrackerID, r.IdentityID)
	if _, exists := f.rows[k]; exists {
		return nil // INSERT OR IGNORE semantics
	}
	f.rows[k] = r
	return nil
}

func (f *fakeRevocationArchive) GetPeerRevocation(_ context.Context, tr, id []byte) (storage.PeerRevocation, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[f.key(tr, id)]
	return r, ok, nil
}

// captureForward records every (kind, payload) sent.
type captureForward struct {
	mu    sync.Mutex
	calls []capturedForward
}

type capturedForward struct {
	kind    fed.Kind
	payload []byte
	exclude *ids.TrackerID
}

func (c *captureForward) fn(_ context.Context, kind fed.Kind, payload []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, capturedForward{kind: kind, payload: append([]byte(nil), payload...)})
}

func (c *captureForward) snapshot() []capturedForward {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]capturedForward, len(c.calls))
	copy(out, c.calls)
	return out
}

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return pub, priv
}

func newTestCoordinator(t *testing.T, arch PeerRevocationArchive, fwd Forwarder) (*revocationCoordinator, ed25519.PublicKey, ed25519.PrivateKey, ids.TrackerID) {
	t.Helper()
	pub, priv := newKey(t)
	tid := ids.TrackerID(sha256.Sum256(pub))
	rc := newRevocationCoordinator(revocationCoordinatorCfg{
		MyTrackerID: tid,
		MyPriv:      priv,
		Archive:     arch,
		Forward:     fwd,
		PeerPubKey: func(id ids.TrackerID) (ed25519.PublicKey, bool) {
			if id == tid {
				return pub, true
			}
			return nil, false
		},
		Now:     func() time.Time { return time.Unix(1714000123, 0) },
		Metrics: func(string) {},
	})
	return rc, pub, priv, tid
}

func TestRevocationReasonString_KnownAndUnknown(t *testing.T) {
	assert.Equal(t, fed.RevocationReason_REVOCATION_REASON_ABUSE, mapReputationReasonToProto("freeze_repeat"))
	assert.Equal(t, fed.RevocationReason_REVOCATION_REASON_MANUAL, mapReputationReasonToProto("operator"))
	// Unknown reputation reason defaults to ABUSE (reputation-emitted strings are abuse-class).
	assert.Equal(t, fed.RevocationReason_REVOCATION_REASON_ABUSE, mapReputationReasonToProto("some_unknown_reason"))
}

func TestRevocationCoordinator_OnFreeze_EmitsAndArchives(t *testing.T) {
	arch := newFakeRevocationArchive()
	cap := &captureForward{}
	rc, _, priv, tid := newTestCoordinator(t, arch, cap.fn)

	identity := ids.IdentityID(bytes.Repeat([]byte{0x55}, 32))
	revokedAt := time.Unix(1714000100, 0)
	rc.OnFreeze(context.Background(), identity, "freeze_repeat", revokedAt)

	calls := cap.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, fed.Kind_KIND_REVOCATION, calls[0].kind)

	rev := &fed.Revocation{}
	require.NoError(t, proto.Unmarshal(calls[0].payload, rev))
	assert.Equal(t, tid.Bytes()[:], rev.TrackerId)
	idArr := identity
	assert.Equal(t, idArr[:], rev.IdentityId)
	assert.Equal(t, fed.RevocationReason_REVOCATION_REASON_ABUSE, rev.Reason)
	assert.Equal(t, uint64(1714000100), rev.RevokedAt)
	require.Len(t, rev.TrackerSig, fed.SigLen)

	canonical, err := fed.CanonicalRevocationPreSig(rev)
	require.NoError(t, err)
	assert.True(t, ed25519.Verify(priv.Public().(ed25519.PublicKey), canonical, rev.TrackerSig))

	got, ok, err := arch.GetPeerRevocation(context.Background(), tid.Bytes()[:], identity[:])
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, uint32(fed.RevocationReason_REVOCATION_REASON_ABUSE), got.Reason)
	assert.Equal(t, uint64(1714000100), got.RevokedAt)
	assert.Equal(t, uint64(1714000123), got.ReceivedAt, "ReceivedAt = coordinator.Now()")
}

func TestRevocationCoordinator_OnFreeze_NilArchive_NoOp(t *testing.T) {
	cap := &captureForward{}
	pub, priv := newKey(t)
	tid := ids.TrackerID(sha256.Sum256(pub))
	rc := newRevocationCoordinator(revocationCoordinatorCfg{
		MyTrackerID: tid,
		MyPriv:      priv,
		Archive:     nil, // disabled
		Forward:     cap.fn,
		PeerPubKey:  func(ids.TrackerID) (ed25519.PublicKey, bool) { return nil, false },
		Now:         time.Now,
		Metrics:     func(string) {},
	})
	rc.OnFreeze(context.Background(), ids.IdentityID(bytes.Repeat([]byte{0x55}, 32)), "freeze_repeat", time.Now())
	assert.Empty(t, cap.snapshot(), "nil archive disables emit")
}

func TestRevocationCoordinator_OnIncoming_HappyPath(t *testing.T) {
	arch := newFakeRevocationArchive()
	cap := &captureForward{}

	// Issuer identity (a remote peer).
	issuerPub, issuerPriv := newKey(t)
	issuerID := ids.TrackerID(sha256.Sum256(issuerPub))

	// Build a properly-signed Revocation from the issuer.
	identity := bytes.Repeat([]byte{0x77}, 32)
	rev := &fed.Revocation{
		TrackerId:  issuerID.Bytes()[:32],
		IdentityId: identity,
		Reason:     fed.RevocationReason_REVOCATION_REASON_ABUSE,
		RevokedAt:  1714000100,
	}
	canonical, err := fed.CanonicalRevocationPreSig(rev)
	require.NoError(t, err)
	rev.TrackerSig = ed25519.Sign(issuerPriv, canonical)
	payload, err := proto.Marshal(rev)
	require.NoError(t, err)

	// Coordinator that knows issuerPub. Use an explicit cfg here so we
	// can set MyTrackerID independently of the issuer.
	myPub, myPriv := newKey(t)
	myID := ids.TrackerID(sha256.Sum256(myPub))
	rc := newRevocationCoordinator(revocationCoordinatorCfg{
		MyTrackerID: myID,
		MyPriv:      myPriv,
		Archive:     arch,
		Forward:     cap.fn,
		PeerPubKey: func(id ids.TrackerID) (ed25519.PublicKey, bool) {
			if id == issuerID {
				return issuerPub, true
			}
			return nil, false
		},
		Now:     func() time.Time { return time.Unix(1714000200, 0) },
		Metrics: func(string) {},
	})

	env := &fed.Envelope{
		SenderId:  issuerID.Bytes()[:32],
		Kind:      fed.Kind_KIND_REVOCATION,
		Payload:   payload,
		SenderSig: bytes.Repeat([]byte{0xFF}, fed.SigLen), // dispatcher would have verified
	}
	rc.OnIncoming(context.Background(), env, issuerID)

	got, ok, err := arch.GetPeerRevocation(context.Background(), issuerID.Bytes()[:32], identity)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, uint64(1714000100), got.RevokedAt)
	assert.Equal(t, uint64(1714000200), got.ReceivedAt)

	// Forwarded onward.
	calls := cap.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, fed.Kind_KIND_REVOCATION, calls[0].kind)
	assert.Equal(t, payload, calls[0].payload)
}

func TestRevocationCoordinator_OnIncoming_BadSig_Drops(t *testing.T) {
	arch := newFakeRevocationArchive()
	cap := &captureForward{}

	issuerPub, issuerPriv := newKey(t)
	issuerID := ids.TrackerID(sha256.Sum256(issuerPub))

	rev := &fed.Revocation{
		TrackerId:  issuerID.Bytes()[:32],
		IdentityId: bytes.Repeat([]byte{0x77}, 32),
		Reason:     fed.RevocationReason_REVOCATION_REASON_ABUSE,
		RevokedAt:  1714000100,
	}
	canonical, err := fed.CanonicalRevocationPreSig(rev)
	require.NoError(t, err)
	rev.TrackerSig = ed25519.Sign(issuerPriv, canonical)
	rev.TrackerSig[0] ^= 0xFF // tamper
	payload, err := proto.Marshal(rev)
	require.NoError(t, err)

	myPub, myPriv := newKey(t)
	myID := ids.TrackerID(sha256.Sum256(myPub))
	rc := newRevocationCoordinator(revocationCoordinatorCfg{
		MyTrackerID: myID, MyPriv: myPriv, Archive: arch, Forward: cap.fn,
		PeerPubKey: func(id ids.TrackerID) (ed25519.PublicKey, bool) {
			if id == issuerID {
				return issuerPub, true
			}
			return nil, false
		},
		Now: func() time.Time { return time.Unix(1714000200, 0) }, Metrics: func(string) {},
	})

	env := &fed.Envelope{
		SenderId: issuerID.Bytes()[:32], Kind: fed.Kind_KIND_REVOCATION,
		Payload: payload, SenderSig: bytes.Repeat([]byte{0xFF}, fed.SigLen),
	}
	rc.OnIncoming(context.Background(), env, issuerID)

	_, ok, err := arch.GetPeerRevocation(context.Background(), issuerID.Bytes()[:32], rev.IdentityId)
	require.NoError(t, err)
	assert.False(t, ok, "bad sig must not be archived")
	assert.Empty(t, cap.snapshot(), "bad sig must not be forwarded")
}

func TestRevocationCoordinator_OnIncoming_UnknownIssuer_Drops(t *testing.T) {
	arch := newFakeRevocationArchive()
	cap := &captureForward{}

	issuerPub, issuerPriv := newKey(t)
	issuerID := ids.TrackerID(sha256.Sum256(issuerPub))

	rev := &fed.Revocation{
		TrackerId:  issuerID.Bytes()[:32],
		IdentityId: bytes.Repeat([]byte{0x77}, 32),
		Reason:     fed.RevocationReason_REVOCATION_REASON_ABUSE,
		RevokedAt:  1714000100,
	}
	canonical, err := fed.CanonicalRevocationPreSig(rev)
	require.NoError(t, err)
	rev.TrackerSig = ed25519.Sign(issuerPriv, canonical)
	payload, err := proto.Marshal(rev)
	require.NoError(t, err)

	myPub, myPriv := newKey(t)
	myID := ids.TrackerID(sha256.Sum256(myPub))
	rc := newRevocationCoordinator(revocationCoordinatorCfg{
		MyTrackerID: myID, MyPriv: myPriv, Archive: arch, Forward: cap.fn,
		PeerPubKey: func(ids.TrackerID) (ed25519.PublicKey, bool) { return nil, false }, // unknown
		Now:        func() time.Time { return time.Unix(1714000200, 0) },
		Metrics:    func(string) {},
	})

	env := &fed.Envelope{
		SenderId: issuerID.Bytes()[:32], Kind: fed.Kind_KIND_REVOCATION,
		Payload: payload, SenderSig: bytes.Repeat([]byte{0xFF}, fed.SigLen),
	}
	rc.OnIncoming(context.Background(), env, issuerID)

	_, ok, err := arch.GetPeerRevocation(context.Background(), issuerID.Bytes()[:32], rev.IdentityId)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, cap.snapshot())
}
```

- [ ] **Step 2: Run the test, expect failure.**

Run: `go test ./tracker/internal/federation/ -run TestRevocation -v`
Expected: FAIL — `revocationCoordinator`, `newRevocationCoordinator`, `mapReputationReasonToProto` undefined.

---

### Task 11: revocationCoordinator + reason mapping helper (green)

**Files:**
- Create: `tracker/internal/federation/revocation.go`

- [ ] **Step 1: Implement.**

```go
// Package federation: revocation gossip coordinator.
//
// revocationCoordinator owns:
//   - the outbound emit path: reputation freezes -> signed Revocation
//     -> forward via gossip + persist locally
//   - the inbound apply path: validate -> issuer-sig-verify -> archive
//     (idempotent INSERT OR IGNORE) -> forward onward
//
// Concurrency: stateless; each call is one synchronous flow. Storage
// idempotency comes from the SQLite primary-key INSERT OR IGNORE in
// PeerRevocationArchive.PutPeerRevocation. Gossip-echo suppression
// comes from the slice-0 dedupe core (the dispatcher Seen-checks
// before invoking OnIncoming).
package federation

import (
	"context"
	"crypto/ed25519"
	"time"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
	"google.golang.org/protobuf/proto"
)

type revocationCoordinatorCfg struct {
	MyTrackerID ids.TrackerID
	MyPriv      ed25519.PrivateKey
	Archive     PeerRevocationArchive
	Forward     Forwarder
	PeerPubKey  func(ids.TrackerID) (ed25519.PublicKey, bool)
	Now         func() time.Time
	Metrics     func(name string) // increment a named counter
}

type revocationCoordinator struct {
	cfg revocationCoordinatorCfg
}

func newRevocationCoordinator(cfg revocationCoordinatorCfg) *revocationCoordinator {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Metrics == nil {
		cfg.Metrics = func(string) {}
	}
	return &revocationCoordinator{cfg: cfg}
}

// mapReputationReasonToProto translates a reputation-subsystem reason
// string (Signal in ReasonRecord.Kind=="transition") into the wire enum.
// Unknown reputation reasons default to ABUSE: reputation only emits
// freeze-class transitions in v1, all of which are abuse-driven.
func mapReputationReasonToProto(s string) fed.RevocationReason {
	switch s {
	case "freeze_repeat":
		return fed.RevocationReason_REVOCATION_REASON_ABUSE
	case "operator":
		return fed.RevocationReason_REVOCATION_REASON_MANUAL
	default:
		return fed.RevocationReason_REVOCATION_REASON_ABUSE
	}
}

// OnFreeze emits a signed REVOCATION for identity, broadcasts via the
// gossip Forward closure, and persists locally so a later round-trip
// echo from peers is a no-op at the archive layer (and dedupe at the
// envelope layer).
func (rc *revocationCoordinator) OnFreeze(ctx context.Context, identity ids.IdentityID, reason string, revokedAt time.Time) {
	if rc == nil || rc.cfg.Archive == nil {
		rc.cfg.Metrics("revocation_emit_disabled")
		return
	}
	myID := rc.cfg.MyTrackerID.Bytes()
	idArr := identity
	rev := &fed.Revocation{
		TrackerId:  myID[:],
		IdentityId: idArr[:],
		Reason:     mapReputationReasonToProto(reason),
		RevokedAt:  uint64(revokedAt.Unix()), //nolint:gosec // G115 — Unix() ≥ 0 for any post-epoch timestamp
	}
	canonical, err := fed.CanonicalRevocationPreSig(rev)
	if err != nil {
		rc.cfg.Metrics("revocation_canonical")
		return
	}
	rev.TrackerSig = ed25519.Sign(rc.cfg.MyPriv, canonical)
	if err := fed.ValidateRevocation(rev); err != nil {
		rc.cfg.Metrics("revocation_self_shape")
		return
	}
	payload, err := proto.Marshal(rev)
	if err != nil {
		rc.cfg.Metrics("revocation_marshal")
		return
	}

	if err := rc.cfg.Archive.PutPeerRevocation(ctx, storage.PeerRevocation{
		TrackerID:  append([]byte(nil), rev.TrackerId...),
		IdentityID: append([]byte(nil), rev.IdentityId...),
		Reason:     uint32(rev.Reason),
		RevokedAt:  rev.RevokedAt,
		TrackerSig: append([]byte(nil), rev.TrackerSig...),
		ReceivedAt: uint64(rc.cfg.Now().Unix()), //nolint:gosec // G115 — Unix() ≥ 0
	}); err != nil {
		rc.cfg.Metrics("revocation_archive_self_err")
		// Continue forwarding — peers should still learn even if our
		// own archive write hit a transient error.
	}

	rc.cfg.Forward(ctx, fed.Kind_KIND_REVOCATION, payload)
	rc.cfg.Metrics("revocations_emitted")
}

// OnIncoming validates a received KIND_REVOCATION envelope, verifies the
// issuer's signature against the operator-allowlisted pubkey, archives
// idempotently, and forwards to all active peers via the slice-0 forward
// closure. The dedupe core suppresses round-trip echoes.
func (rc *revocationCoordinator) OnIncoming(ctx context.Context, env *fed.Envelope, _ ids.TrackerID) {
	if rc == nil || rc.cfg.Archive == nil {
		rc.cfg.Metrics("revocation_disabled")
		return
	}
	rev := &fed.Revocation{}
	if err := proto.Unmarshal(env.Payload, rev); err != nil {
		rc.cfg.Metrics("revocation_shape")
		return
	}
	if err := fed.ValidateRevocation(rev); err != nil {
		rc.cfg.Metrics("revocation_shape")
		return
	}
	var issuerID ids.TrackerID
	copy(issuerID[:], rev.TrackerId)
	issuerPub, ok := rc.cfg.PeerPubKey(issuerID)
	if !ok {
		rc.cfg.Metrics("revocation_unknown_issuer")
		return
	}
	canonical, err := fed.CanonicalRevocationPreSig(rev)
	if err != nil {
		rc.cfg.Metrics("revocation_canonical")
		return
	}
	if !ed25519.Verify(issuerPub, canonical, rev.TrackerSig) {
		rc.cfg.Metrics("revocation_sig")
		return
	}
	if err := rc.cfg.Archive.PutPeerRevocation(ctx, storage.PeerRevocation{
		TrackerID:  append([]byte(nil), rev.TrackerId...),
		IdentityID: append([]byte(nil), rev.IdentityId...),
		Reason:     uint32(rev.Reason),
		RevokedAt:  rev.RevokedAt,
		TrackerSig: append([]byte(nil), rev.TrackerSig...),
		ReceivedAt: uint64(rc.cfg.Now().Unix()), //nolint:gosec // G115 — Unix() ≥ 0
	}); err != nil {
		// Cannot vouch for durability — do not forward.
		rc.cfg.Metrics("revocation_archive_err")
		return
	}
	rc.cfg.Forward(ctx, fed.Kind_KIND_REVOCATION, env.Payload)
	rc.cfg.Metrics("revocations_archived")
}
```

- [ ] **Step 2: Run the test, expect pass.**

Run: `go test ./tracker/internal/federation/ -run TestRevocation -race -v`
Expected: PASS.

- [ ] **Step 3: Run the full federation suite.**

Run: `go test ./tracker/internal/federation/... -race`
Expected: PASS.

- [ ] **Step 4: Commit.**

```bash
git add tracker/internal/federation/revocation.go tracker/internal/federation/revocation_test.go
git commit -m "feat(federation): revocationCoordinator (OnFreeze + OnIncoming)"
```

---

### Task 12: Reputation FreezeListener interface + Option (red)

**Files:**
- Create: `tracker/internal/reputation/listeners_test.go`

- [ ] **Step 1: Write the failing test.**

```go
package reputation

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/config"
)

type capturedFreeze struct {
	id        ids.IdentityID
	reason    string
	revokedAt time.Time
}

type fakeFreezeListener struct {
	mu    sync.Mutex
	calls []capturedFreeze
}

func (f *fakeFreezeListener) OnFreeze(_ context.Context, id ids.IdentityID, reason string, revokedAt time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, capturedFreeze{id, reason, revokedAt})
}

func (f *fakeFreezeListener) snapshot() []capturedFreeze {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]capturedFreeze, len(f.calls))
	copy(out, f.calls)
	return out
}

func TestFreezeListener_FiresOnAuditToFrozen(t *testing.T) {
	dir := t.TempDir()
	listener := &fakeFreezeListener{}
	now := time.Unix(1714000000, 0)

	s, err := Open(context.Background(), config.ReputationConfig{
		StoragePath:            dir + "/rep.db",
		EvaluationIntervalS:    3600, // ensure no background tick during this test
		MinPopulationForZScore: 1,
		ZScoreThreshold:        2.5,
		DefaultScore:           0.5,
	}, WithClock(func() time.Time { return now }), WithFreezeListener(listener))
	require.NoError(t, err)
	defer s.Close()

	id := ids.IdentityID{1, 2, 3}
	require.NoError(t, s.store.ensureState(context.Background(), id, now))

	// Three audit-class reasons within 7 days drives the freeze_repeat
	// transition path (evaluator.go).
	for i := 0; i < 3; i++ {
		require.NoError(t, s.store.transition(context.Background(), id, StateAudit,
			ReasonRecord{Kind: "zscore", Signal: "network_requests_per_h", Z: 5.0, Window: "1h", At: now.Unix() - int64(i*100)},
			now))
	}

	// Drive one cycle synchronously.
	require.NoError(t, s.runOneCycle(context.Background()))

	calls := listener.snapshot()
	require.Len(t, calls, 1, "listener should fire once on AUDIT->FROZEN")
	assert.Equal(t, id, calls[0].id)
	assert.Equal(t, "freeze_repeat", calls[0].reason)
	assert.Equal(t, now, calls[0].revokedAt)
}

func TestFreezeListener_NotConfigured_NoCalls(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1714000000, 0)
	s, err := Open(context.Background(), config.ReputationConfig{
		StoragePath:            dir + "/rep.db",
		EvaluationIntervalS:    3600,
		MinPopulationForZScore: 1,
		ZScoreThreshold:        2.5,
		DefaultScore:           0.5,
	}, WithClock(func() time.Time { return now }))
	require.NoError(t, err)
	defer s.Close()

	id := ids.IdentityID{1, 2, 3}
	require.NoError(t, s.store.ensureState(context.Background(), id, now))
	for i := 0; i < 3; i++ {
		require.NoError(t, s.store.transition(context.Background(), id, StateAudit,
			ReasonRecord{Kind: "zscore", At: now.Unix() - int64(i*100)}, now))
	}
	// Simply must not panic without a listener.
	require.NoError(t, s.runOneCycle(context.Background()))
}
```

- [ ] **Step 2: Run the test, expect failure.**

Run: `go test ./tracker/internal/reputation/ -run TestFreezeListener -v`
Expected: FAIL — `WithFreezeListener` undefined; `FreezeListener` interface undefined.

---

### Task 13: Reputation FreezeListener interface + Option (green)

**Files:**
- Create: `tracker/internal/reputation/listeners.go`
- Modify: `tracker/internal/reputation/reputation.go` (add field on Subsystem, add Option)
- Modify: `tracker/internal/reputation/evaluator.go:178` (call listener at freeze emit)

- [ ] **Step 1: Create the interface file.**

```go
package reputation

import (
	"context"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
)

// FreezeListener is the reputation→outside notification hook. The
// evaluator calls OnFreeze synchronously when an identity transitions
// to FROZEN. Implementations MUST NOT block — federation's impl signs
// and enqueues the gossip via per-peer send queues, returning before
// the network round-trip completes.
//
// federation.*Federation satisfies this interface via Go structural
// typing; no compile-time assertion or import is needed (per the
// reputation-leaf-module rule in CLAUDE.md).
type FreezeListener interface {
	OnFreeze(ctx context.Context, id ids.IdentityID, reason string, revokedAt time.Time)
}

// WithFreezeListener registers a FreezeListener that the evaluator
// calls on every AUDIT->FROZEN transition. Optional; nil disables
// the hook.
func WithFreezeListener(l FreezeListener) Option {
	return func(s *Subsystem) { s.freezeListener = l }
}
```

- [ ] **Step 2: Add `freezeListener` field to `Subsystem`.** In `tracker/internal/reputation/reputation.go`, inside the `Subsystem` struct, append after `metrics *metrics`:

```go
	freezeListener FreezeListener
```

- [ ] **Step 3: Call the listener at the freeze site.** In `tracker/internal/reputation/evaluator.go`, replace lines 174-180 (the `freeze_repeat` block) with:

```go
				if count >= 3 {
					reason := ReasonRecord{
						Kind: "transition", Signal: "freeze_repeat",
						At: now.Unix(),
					}
					if err := s.store.transition(ctx, id, StateFrozen, reason, now); err == nil {
						s.metrics.transitions.WithLabelValues("AUDIT", "FROZEN", "freeze_repeat").Inc()
						s.notifyFreeze(ctx, id, "freeze_repeat", now)
					}
				} else if !lastAudit.IsZero() && lastAudit.Before(fortyEightHoursAgo) &&
```

- [ ] **Step 4: Add the `notifyFreeze` helper.** Append to `tracker/internal/reputation/reputation.go`:

```go
// notifyFreeze invokes the configured FreezeListener with panic
// recovery so a misbehaving listener cannot crash the evaluator
// goroutine. No-op when no listener is configured.
func (s *Subsystem) notifyFreeze(ctx context.Context, id ids.IdentityID, reason string, revokedAt time.Time) {
	if s.freezeListener == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			s.metrics.evaluatorPanics.Inc()
		}
	}()
	s.freezeListener.OnFreeze(ctx, id, reason, revokedAt)
}
```

- [ ] **Step 5: Run the test, expect pass.**

Run: `go test ./tracker/internal/reputation/ -run TestFreezeListener -v`
Expected: PASS.

- [ ] **Step 6: Run the full reputation suite.**

Run: `go test ./tracker/internal/reputation/ -race`
Expected: PASS — existing evaluator tests remain green.

- [ ] **Step 7: Commit.**

```bash
git add tracker/internal/reputation/listeners.go tracker/internal/reputation/listeners_test.go tracker/internal/reputation/reputation.go tracker/internal/reputation/evaluator.go
git commit -m "feat(tracker/reputation): FreezeListener Option + emit at AUDIT->FROZEN"
```

---

### Task 14: Wire revocationCoordinator into Federation subsystem (red)

**Files:**
- Modify: `tracker/internal/federation/subsystem_test.go` (add a unit test)

- [ ] **Step 1: Add a failing test.** Append to `tracker/internal/federation/subsystem_test.go`:

```go
func TestFederation_OnFreeze_NoArchive_IsNoOp(t *testing.T) {
	hub := NewInprocHub()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	tid := ids.TrackerID(sha256.Sum256(pub))
	tr := NewInprocTransport(hub, "X", pub, priv)

	f, err := Open(Config{MyTrackerID: tid, MyPriv: priv}, Deps{
		Transport: tr,
		RootSrc:   &fakeRootSrc{ok: false},
		Archive:   newFakeArchive(),
		Metrics:   NewMetrics(prometheus.NewRegistry()),
		Logger:    zerolog.Nop(),
		Now:       time.Now,
		// RevocationArchive intentionally nil.
	})
	require.NoError(t, err)
	defer f.Close()

	// Should not panic; no archive, no listeners — silent no-op.
	f.OnFreeze(context.Background(), ids.IdentityID(bytes.Repeat([]byte{1}, 32)), "freeze_repeat", time.Unix(1714000000, 0))
}
```

(If `subsystem_test.go` doesn't already import `bytes`, `crypto/ed25519`, `crypto/rand`, `crypto/sha256`, `context`, `time`, `prometheus`, `zerolog`, add them. Use existing imports as a guide.)

- [ ] **Step 2: Run the test, expect failure.**

Run: `go test ./tracker/internal/federation/ -run TestFederation_OnFreeze_NoArchive_IsNoOp -v`
Expected: FAIL — `*Federation` has no `OnFreeze` method.

---

### Task 15: Wire revocationCoordinator into Federation subsystem (green)

**Files:**
- Modify: `tracker/internal/federation/subsystem.go` (build coordinator; add `OnFreeze`; dispatch `KIND_REVOCATION`)

- [ ] **Step 1: Add `revocation *revocationCoordinator` field on `Federation` (right after the existing `transfer` field).**

```go
	transfer   *transferCoordinator
	revocation *revocationCoordinator
```

- [ ] **Step 2: Build the coordinator inside `Open` (right after `transfer := newTransferCoordinator(...)`).**

```go
	revocation := newRevocationCoordinator(revocationCoordinatorCfg{
		MyTrackerID: cfg.MyTrackerID,
		MyPriv:      cfg.MyPriv,
		Archive:     dep.RevocationArchive,
		Forward:     forward,
		PeerPubKey: func(id ids.TrackerID) (ed25519.PublicKey, bool) {
			info, ok := reg.Get(id)
			if !ok {
				return nil, false
			}
			return info.PubKey, true
		},
		Now:     dep.Now,
		Metrics: func(name string) { dep.Metrics.InvalidFrames(name) },
	})
```

- [ ] **Step 3: Pass it into the Federation struct.** Update the struct literal:

```go
	f := &Federation{
		cfg: cfg, dep: dep,
		reg: reg, dedupe: dedupe, gossip: gossip,
		apply: apply, equiv: equiv, pub: pub, transfer: transfer, revocation: revocation,
		peers: make(map[ids.TrackerID]*Peer),
	}
```

- [ ] **Step 4: Add a `KIND_REVOCATION` case in `makeDispatcher`.** In the switch in `subsystem.go:326`, after the three transfer cases:

```go
		case fed.Kind_KIND_REVOCATION:
			if f.revocation == nil {
				f.dep.Metrics.InvalidFrames("revocation_disabled")
				return
			}
			f.revocation.OnIncoming(context.Background(), env, peerID)
```

- [ ] **Step 5: Add `Federation.OnFreeze` method (after `StartTransfer`).**

```go
// OnFreeze implements reputation.FreezeListener via Go structural
// typing. The reputation evaluator calls this synchronously on every
// AUDIT->FROZEN transition; the call returns once the signed Revocation
// is on per-peer send queues. With no RevocationArchive Dep, the call
// is a no-op (revocation gossip is disabled at subsystem-open time).
func (f *Federation) OnFreeze(ctx context.Context, id ids.IdentityID, reason string, revokedAt time.Time) {
	if f == nil || f.revocation == nil {
		return
	}
	f.revocation.OnFreeze(ctx, id, reason, revokedAt)
}
```

- [ ] **Step 6: Add `time` and `ids.IdentityID` imports as needed.** Verify the `import` block has `"time"` and `"github.com/token-bay/token-bay/shared/ids"` (already present per existing source).

- [ ] **Step 7: Run the test, expect pass.**

Run: `go test ./tracker/internal/federation/ -run TestFederation_OnFreeze_NoArchive_IsNoOp -v`
Expected: PASS.

- [ ] **Step 8: Run the full federation suite.**

Run: `go test ./tracker/internal/federation/ -race`
Expected: PASS.

- [ ] **Step 9: Commit.**

```bash
git add tracker/internal/federation/subsystem.go tracker/internal/federation/subsystem_test.go
git commit -m "feat(federation): wire revocationCoordinator + Federation.OnFreeze"
```

---

### Task 16: Two-tracker integration test (A freezes -> B archives) (red)

**Files:**
- Modify: `tracker/internal/federation/integration_test.go`

- [ ] **Step 1: Add `RevocationArchive` plumbing to `newTwoTracker`** (the existing helper at line 38). Replace the existing helper's two `federation.Open` calls so each gets a `RevocationArchive`. Specifically: add `revArchA, revArchB := newFakeRevocationArchive(), newFakeRevocationArchive()` and pass `RevocationArchive: revArchA` / `RevocationArchive: revArchB` to each Deps. Add the field to `twoTracker`:

```go
type twoTracker struct {
	hub          *federation.InprocHub
	a, b         *federation.Federation
	archA, archB *fakeArchive
	revArchA, revArchB *fakeRevocationArchive
	srcA, srcB   *fakeRootSrc
	aID, bID     ids.TrackerID
}
```

(`fakeRevocationArchive` is the test fixture already added in Task 10's `revocation_test.go`. It lives in `package federation` while `integration_test.go` is `package federation_test`. Move the fixture to a shared internal-test file, or duplicate the small implementation in `integration_test.go` under `package federation_test`. Prefer **duplication** — the fixture is ~20 lines and avoids package-boundary churn.)

- [ ] **Step 2: Add `fakeRevocationArchive` fixture to `integration_test.go`.** Append to integration_test.go (kept in the test file, not exported):

```go
// fakeRevocationArchiveExt is the package_test mirror of revocation_test.go's
// fakeRevocationArchive. Duplicated to avoid moving an internal-package
// fixture into an integration test file.
type fakeRevocationArchiveExt struct {
	mu   sync.Mutex
	rows map[[64]byte]storage.PeerRevocation
}

func newFakeRevocationArchiveExt() *fakeRevocationArchiveExt {
	return &fakeRevocationArchiveExt{rows: map[[64]byte]storage.PeerRevocation{}}
}

func (f *fakeRevocationArchiveExt) PutPeerRevocation(_ context.Context, r storage.PeerRevocation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var k [64]byte
	copy(k[0:32], r.TrackerID)
	copy(k[32:64], r.IdentityID)
	if _, ok := f.rows[k]; ok {
		return nil
	}
	f.rows[k] = r
	return nil
}

func (f *fakeRevocationArchiveExt) GetPeerRevocation(_ context.Context, tr, id []byte) (storage.PeerRevocation, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var k [64]byte
	copy(k[0:32], tr)
	copy(k[32:64], id)
	r, ok := f.rows[k]
	return r, ok, nil
}
```

- [ ] **Step 3: Update `newTwoTracker`** to use the external fixture and plumb it through:

```go
	revArchA, revArchB := newFakeRevocationArchiveExt(), newFakeRevocationArchiveExt()
	// ...
	aFed, err := federation.Open(federation.Config{
		MyTrackerID: aID, MyPriv: a.priv,
		Peers: []federation.AllowlistedPeer{{TrackerID: bID, PubKey: b.pub, Addr: "B"}},
	}, federation.Deps{
		Transport: trA, RootSrc: srcA, Archive: archA,
		RevocationArchive: revArchA,
		Metrics:           federation.NewMetrics(prometheus.NewRegistry()),
		Logger:            zerolog.Nop(), Now: time.Now,
	})
```

(Same for B. Add `revArchA: revArchA, revArchB: revArchB` to the `twoTracker{}` literal at the end.)

Switch the `twoTracker` field type to the external fixture:

```go
type twoTracker struct {
	// ...
	revArchA, revArchB *fakeRevocationArchiveExt
	// ...
}
```

- [ ] **Step 4: Add the failing integration test.**

```go
func TestIntegration_Revocation_AB(t *testing.T) {
	t.Parallel()
	tt := newTwoTracker(t)

	// Wait for steady-state peering A<->B.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		gotSteady := false
		for _, p := range tt.a.Peers() {
			if p.State == federation.PeerStateSteady {
				gotSteady = true
				break
			}
		}
		if gotSteady {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	identity := ids.IdentityID(bytes.Repeat([]byte{0x44}, 32))
	tt.a.OnFreeze(context.Background(), identity, "freeze_repeat", time.Unix(1714000100, 0))

	aIDBytes := tt.aID.Bytes()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, ok, _ := tt.revArchB.GetPeerRevocation(context.Background(), aIDBytes[:], identity[:])
		if ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("B never archived A's revocation")
}
```

(Add `bytes` to imports if not already present.)

- [ ] **Step 5: Run the test, expect failure.**

Run: `go test ./tracker/internal/federation/ -run TestIntegration_Revocation_AB -race -v`
Expected: FAIL — RevocationArchive plumbing is new; B may not yet archive correctly. (If it incidentally passes, the test still validates the wiring; mark as green and proceed.)

---

### Task 17: Two-tracker integration test (green)

**Files:**
- (No new files — the implementation is already complete from Tasks 11/15. Only the integration plumbing needs verification.)

- [ ] **Step 1: Run the test.**

Run: `go test ./tracker/internal/federation/ -run TestIntegration_Revocation_AB -race -v`
Expected: PASS.

- [ ] **Step 2: Run the full federation integration suite.**

Run: `go test ./tracker/internal/federation/ -race`
Expected: PASS — slice 1's `TestIntegration_CrossRegionTransfer_HappyPath` and slice 0's `TestIntegration_RootAttestation_AB` still pass.

- [ ] **Step 3: Commit.**

```bash
git add tracker/internal/federation/integration_test.go
git commit -m "test(federation): two-tracker A->B revocation propagation"
```

---

### Task 18: Three-tracker forwarding test (red+green)

**Files:**
- Modify: `tracker/internal/federation/integration_test.go`

- [ ] **Step 1: Add the failing test.** Append to `integration_test.go`:

```go
func TestIntegration_Revocation_ThreeTracker_LineGraph(t *testing.T) {
	t.Parallel()
	// Topology: A <-> B <-> C. C does not directly peer with A.
	// A emits a revocation; B forwards to C; C archives.

	hub := federation.NewInprocHub()
	a := newPeerCfg(t)
	b := newPeerCfg(t)
	c := newPeerCfg(t)

	aID := ids.TrackerID(sha256.Sum256(a.pub))
	bID := ids.TrackerID(sha256.Sum256(b.pub))
	cID := ids.TrackerID(sha256.Sum256(c.pub))

	trA := federation.NewInprocTransport(hub, "A", a.pub, a.priv)
	trB := federation.NewInprocTransport(hub, "B", b.pub, b.priv)
	trC := federation.NewInprocTransport(hub, "C", c.pub, c.priv)

	revArchA := newFakeRevocationArchiveExt()
	revArchB := newFakeRevocationArchiveExt()
	revArchC := newFakeRevocationArchiveExt()

	open := func(myID ids.TrackerID, myPriv ed25519.PrivateKey, tr federation.Transport, peers []federation.AllowlistedPeer, revArch *fakeRevocationArchiveExt) *federation.Federation {
		f, err := federation.Open(federation.Config{MyTrackerID: myID, MyPriv: myPriv, Peers: peers},
			federation.Deps{
				Transport: tr, RootSrc: &fakeRootSrc{ok: false}, Archive: newFakeArchive(),
				RevocationArchive: revArch,
				Metrics:           federation.NewMetrics(prometheus.NewRegistry()),
				Logger:            zerolog.Nop(), Now: time.Now,
			})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return f
	}

	aFed := open(aID, a.priv, trA, []federation.AllowlistedPeer{{TrackerID: bID, PubKey: b.pub, Addr: "B"}}, revArchA)
	bFed := open(bID, b.priv, trB, []federation.AllowlistedPeer{
		{TrackerID: aID, PubKey: a.pub, Addr: "A"},
		{TrackerID: cID, PubKey: c.pub, Addr: "C"},
	}, revArchB)
	_ = open(cID, c.priv, trC, []federation.AllowlistedPeer{{TrackerID: bID, PubKey: b.pub, Addr: "B"}}, revArchC)

	// Wait for B to be steady with both A and C.
	waitSteadyPeers := func(f *federation.Federation, want int) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			n := 0
			for _, p := range f.Peers() {
				if p.State == federation.PeerStateSteady {
					n++
				}
			}
			if n >= want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("federation never reached %d steady peers", want)
	}
	waitSteadyPeers(bFed, 2)

	identity := ids.IdentityID(bytes.Repeat([]byte{0x99}, 32))
	aFed.OnFreeze(context.Background(), identity, "freeze_repeat", time.Unix(1714000100, 0))

	aIDBytes := aID.Bytes()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, ok, _ := revArchC.GetPeerRevocation(context.Background(), aIDBytes[:], identity[:])
		if ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("C never archived A's revocation through B")
}
```

- [ ] **Step 2: Run the test, expect pass on the first try.**

Run: `go test ./tracker/internal/federation/ -run TestIntegration_Revocation_ThreeTracker_LineGraph -race -v`
Expected: PASS — three-tracker forwarding works because the slice-0 dedupe-and-forward gossip core handles fan-out for free.

- [ ] **Step 3: Commit.**

```bash
git add tracker/internal/federation/integration_test.go
git commit -m "test(federation): three-tracker A->B->C revocation forwarding"
```

---

### Task 19: Metrics — emit + receive counters

**Files:**
- Modify: `tracker/internal/federation/metrics.go`
- Modify: `tracker/internal/federation/metrics_test.go`
- Modify: `tracker/internal/federation/revocation.go` (replace string-bucket metric calls with typed callbacks)
- Modify: `tracker/internal/federation/revocation_test.go` (new test fixture passes the new callbacks)
- Modify: `tracker/internal/federation/subsystem.go` (wire typed callbacks into coordinator cfg)

- [ ] **Step 1: Add Prometheus counters to `Metrics`.** In `metrics.go`, append fields to the struct:

```go
	revocationsEmitted  prometheus.Counter
	revocationsReceived *prometheus.CounterVec
```

In `NewMetrics`, register them. Update the existing `MustRegister` slice to include the two new collectors:

```go
	m.revocationsEmitted = prometheus.NewCounter(prometheus.CounterOpts{Name: "tokenbay_federation_revocations_emitted_total"})
	m.revocationsReceived = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "tokenbay_federation_revocations_received_total"}, []string{"outcome"})
	for _, c := range []prometheus.Collector{m.framesIn, m.framesOut, m.invalidFrames, m.dedupeSize, m.peers, m.rootAttestationsPublished, m.rootAttestationsReceived, m.equivocationsDetected, m.equivocationsAboutSelf, m.revocationsEmitted, m.revocationsReceived} {
		reg.MustRegister(c)
	}
```

- [ ] **Step 2: Add accessor methods.**

```go
func (m *Metrics) RevocationsEmitted()                { m.revocationsEmitted.Inc() }
func (m *Metrics) RevocationsReceived(outcome string) { m.revocationsReceived.WithLabelValues(outcome).Inc() }
```

- [ ] **Step 3: Test the counters.** Append to `metrics_test.go`:

```go
func TestMetrics_RevocationsEmitted_Counts(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.RevocationsEmitted()
	m.RevocationsEmitted()
	require.InDelta(t, 2, testutil.ToFloat64(m.revocationsEmitted), 1e-9)
}

func TestMetrics_RevocationsReceived_LabelsAreLive(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.RevocationsReceived("archived")
	m.RevocationsReceived("sig")
	m.RevocationsReceived("archived")
	require.InDelta(t, 2, testutil.ToFloat64(m.revocationsReceived.WithLabelValues("archived")), 1e-9)
	require.InDelta(t, 1, testutil.ToFloat64(m.revocationsReceived.WithLabelValues("sig")), 1e-9)
}
```

- [ ] **Step 4: Replace the coordinator's single-bucket `Metrics func(string)` with a typed pair.** In `tracker/internal/federation/revocation.go`, change the cfg struct + use sites.

Replace the existing `revocationCoordinatorCfg` with:

```go
type revocationCoordinatorCfg struct {
	MyTrackerID ids.TrackerID
	MyPriv      ed25519.PrivateKey
	Archive     PeerRevocationArchive
	Forward     Forwarder
	PeerPubKey  func(ids.TrackerID) (ed25519.PublicKey, bool)
	Now         func() time.Time
	// Invalid is bumped on drop branches and routes to invalid_frames_total{reason=...}
	// per spec §12. Reason names match those listed in spec §10.
	Invalid func(reason string)
	// OnEmit is bumped on a successful local OnFreeze emit (after forward + archive).
	OnEmit func()
	// OnReceived is bumped on every inbound resolution. outcome ∈
	// {archived, sig, shape, unknown_issuer, disabled, storage_err, canonical}.
	OnReceived func(outcome string)
}
```

Update the constructor's nil-callback defaults:

```go
func newRevocationCoordinator(cfg revocationCoordinatorCfg) *revocationCoordinator {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Invalid == nil {
		cfg.Invalid = func(string) {}
	}
	if cfg.OnEmit == nil {
		cfg.OnEmit = func() {}
	}
	if cfg.OnReceived == nil {
		cfg.OnReceived = func(string) {}
	}
	return &revocationCoordinator{cfg: cfg}
}
```

In `OnFreeze`, replace each `rc.cfg.Metrics(...)` call:
- `"revocation_emit_disabled"` → `rc.cfg.Invalid("revocation_emit_disabled")`
- `"revocation_canonical"` → `rc.cfg.Invalid("revocation_canonical")`
- `"revocation_self_shape"` → `rc.cfg.Invalid("revocation_self_shape")`
- `"revocation_marshal"` → `rc.cfg.Invalid("revocation_marshal")`
- `"revocation_archive_self_err"` → `rc.cfg.Invalid("revocation_archive_self_err")`
- `"revocations_emitted"` (success) → `rc.cfg.OnEmit()`

In `OnIncoming`, replace each `rc.cfg.Metrics(...)` call:
- `"revocation_disabled"` → `rc.cfg.Invalid("revocation_disabled"); rc.cfg.OnReceived("disabled")`
- `"revocation_shape"` → `rc.cfg.Invalid("revocation_shape"); rc.cfg.OnReceived("shape")`
- `"revocation_unknown_issuer"` → `rc.cfg.Invalid("revocation_unknown_issuer"); rc.cfg.OnReceived("unknown_issuer")`
- `"revocation_canonical"` → `rc.cfg.Invalid("revocation_canonical"); rc.cfg.OnReceived("canonical")`
- `"revocation_sig"` → `rc.cfg.Invalid("revocation_sig"); rc.cfg.OnReceived("sig")`
- `"revocation_archive_err"` → `rc.cfg.Invalid("revocation_archive_err"); rc.cfg.OnReceived("storage_err")`
- `"revocations_archived"` (success) → `rc.cfg.OnReceived("archived")`

- [ ] **Step 5: Update `revocation_test.go` test fixture (`newTestCoordinator`).**

Replace the existing `Metrics: func(string) {}` line with:

```go
		Invalid:    func(string) {},
		OnEmit:     func() {},
		OnReceived: func(string) {},
```

Apply the same change to all three other test sites in `revocation_test.go` that build `revocationCoordinatorCfg` directly (the `_NilArchive_NoOp`, `_OnIncoming_HappyPath`, `_OnIncoming_BadSig_Drops`, `_OnIncoming_UnknownIssuer_Drops` tests). Each currently has `Metrics: func(string) {}` — replace with the three-callback set.

- [ ] **Step 6: Update subsystem wiring** in `tracker/internal/federation/subsystem.go` Task 15's coordinator construction. Change:

```go
		Metrics: func(name string) { dep.Metrics.InvalidFrames(name) },
```

to:

```go
		Invalid:    func(name string) { dep.Metrics.InvalidFrames(name) },
		OnEmit:     dep.Metrics.RevocationsEmitted,
		OnReceived: dep.Metrics.RevocationsReceived,
```

- [ ] **Step 7: Run the metrics tests.**

Run: `go test ./tracker/internal/federation/ -run 'TestMetrics_Revocations|TestRevocation' -race -v`
Expected: PASS.

- [ ] **Step 8: Run the full federation suite to confirm no regressions.**

Run: `go test ./tracker/internal/federation/ -race`
Expected: PASS.

- [ ] **Step 9: Commit.**

```bash
git add tracker/internal/federation/metrics.go tracker/internal/federation/metrics_test.go tracker/internal/federation/revocation.go tracker/internal/federation/revocation_test.go tracker/internal/federation/subsystem.go
git commit -m "feat(federation): revocations_emitted/received Prometheus counters wired into coordinator"
```

---

### Task 20: run_cmd wiring (federation→reputation FreezeListener + RevocationArchive)

**Files:**
- Modify: `tracker/cmd/token-bay-tracker/run_cmd.go`

- [ ] **Step 1: Reorder so federation opens before reputation.** Move the federation block (currently at lines ~132-194) to before the `reputation.Open` call (currently at line 97). The new order:

  1. `admission.Open`
  2. `federation.Open` (with `RevocationArchive: store`)
  3. `reputation.Open` (with `WithFreezeListener(fed)`)
  4. `broker.Open` (existing — uses `rep`)

- [ ] **Step 2: Set `RevocationArchive` in federation Deps.** In the federation Deps literal, after `Archive: storeAsArchive{store: store}`, add:

```go
				RevocationArchive: store, // *storage.Store satisfies PeerRevocationArchive
```

- [ ] **Step 3: Pass the FreezeListener to reputation.** Change the `reputation.Open` call to:

```go
				rep, err := reputation.Open(cmd.Context(), cfg.Reputation, reputation.WithFreezeListener(fed))
```

- [ ] **Step 4: Build.**

Run: `go build ./tracker/cmd/...`
Expected: success.

- [ ] **Step 5: Run the run_cmd tests.**

Run: `go test ./tracker/cmd/...`
Expected: PASS.

- [ ] **Step 6: Commit.**

```bash
git add tracker/cmd/token-bay-tracker/run_cmd.go
git commit -m "feat(tracker/cmd): wire federation as reputation.FreezeListener + RevocationArchive"
```

---

### Task 21: Final make-check + slice acceptance

**Files:** none

- [ ] **Step 1: Repo-root make check.**

Run (from repo root `/Users/dor.amid/git/token-bay`): `make check`
Expected: PASS — all three modules' tests + lints pass.

- [ ] **Step 2: Run `-race` on the federation package one more time** (it's on the always-`-race` list).

Run: `go test ./tracker/internal/federation/ -race -count=1`
Expected: PASS.

- [ ] **Step 3: Verify acceptance criteria from the spec §15:**
   - Two trackers in-process: A freezes X, B archives within 100ms — `TestIntegration_Revocation_AB`.
   - Three-tracker A↔B↔C: A emits, C archives — `TestIntegration_Revocation_ThreeTracker_LineGraph`.
   - Bad-issuer-sig + unknown-issuer drops — `TestRevocationCoordinator_OnIncoming_BadSig_Drops`, `TestRevocationCoordinator_OnIncoming_UnknownIssuer_Drops`.
   - `dep.RevocationArchive == nil` deployment is undisturbed — `TestFederation_OnFreeze_NoArchive_IsNoOp` + slice-0/slice-1 integration tests still passing.
   - Federation passes `-race`.

- [ ] **Step 4: Push the branch and open the PR per CLAUDE.md.**

```bash
git push -u origin tracker/federation-revocation
gh pr create --base main --title "feat(federation): revocation gossip subsystem" --body "$(cat <<'EOF'
## Summary
- Adds federation §6 revocation gossip: KIND_REVOCATION wire format, signed Revocation message, RevocationCoordinator (OnFreeze + OnIncoming), peer_revocations storage table, FreezeListener Option on reputation.
- Federation satisfies reputation.FreezeListener via Go structural typing — no reputation→federation import (preserves the leaf-module rule).
- Spec: docs/superpowers/specs/federation/2026-05-10-federation-revocation-gossip-design.md

## Test plan
- [x] make check at repo root green
- [x] `go test ./tracker/internal/federation/ -race`
- [x] `go test ./shared/federation/`
- [x] `go test ./tracker/internal/ledger/storage/`
- [x] `go test ./tracker/internal/reputation/`
- [x] Two-tracker A->B in-process propagation
- [x] Three-tracker A->B->C forwarding (dedupe of echo)
- [x] Bad-issuer-sig + unknown-issuer drops
EOF
)"
```

- [ ] **Step 5: Watch CI.**

Run: `gh pr checks --watch`
Expected: all checks PASS.

- [ ] **Step 6: Merge** (only after all checks pass).

```bash
gh pr merge --squash --delete-branch
```
