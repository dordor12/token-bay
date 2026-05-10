# Federation — Peer Exchange Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement federation §7.1 peer exchange: hourly-cadence (on-demand in v1) gossip of a `KnownPeer` table, durable persistence in a new `known_peers` table, and dedupe-and-forward propagation across the federation graph. Plugin distribution (§7.2) and real `health_score` (§7.3) are deferred to slice 4 and slice 5.

**Architecture:** A new `peerExchangeCoordinator` lives inside `tracker/internal/federation` alongside the slice-1 `transferCoordinator` and slice-2 `revocationCoordinator`. The subsystem grows an on-demand `Federation.PublishPeerExchange(ctx)` method (mirroring slice 0's `Federation.PublishHour`). Inbound `KIND_PEER_EXCHANGE` envelopes are validated, every entry is upserted into a new additive `known_peers` SQLite table (last-write-wins by `last_seen`, allowlist rows pinned via SQL `CASE`), and the envelope is forwarded via `gossip.Forward`. There is no inner-message cryptographic binding — the envelope's `sender_sig` is the only crypto path; entries are advisory hints.

**Tech Stack:** Go 1.23+, `google.golang.org/protobuf`, stdlib `crypto/ed25519`, `modernc.org/sqlite`, `prometheus/client_golang`, `testify`.

**Spec:** `docs/superpowers/specs/federation/2026-05-10-federation-peer-exchange-design.md` (commit `7e3aa1b` on this branch).

---

## File Structure

| Path | Action | Responsibility |
|---|---|---|
| `shared/federation/federation.proto` | MODIFY | Add `KIND_PEER_EXCHANGE = 13`, `KnownPeer` message, `PeerExchange` message |
| `shared/federation/federation.pb.go` | REGEN | `make -C shared proto-gen`; regenerated, not hand-edited |
| `shared/federation/validate.go` | MODIFY | Add `ValidatePeerExchange`; lift envelope ceiling to `KIND_PEER_EXCHANGE` |
| `shared/federation/validate_test.go` | MODIFY | Tests for `ValidatePeerExchange` happy + bad-shape table |
| `tracker/internal/ledger/storage/schema_v1.sql` | MODIFY | Append `CREATE TABLE IF NOT EXISTS known_peers` + index |
| `tracker/internal/ledger/storage/known_peers.go` | CREATE | `KnownPeer` struct, `UpsertKnownPeer`, `GetKnownPeer`, `ListKnownPeers` |
| `tracker/internal/ledger/storage/known_peers_test.go` | CREATE | Round-trip, last-write-wins, allowlist pinning, list ordering, miss returns ok=false |
| `tracker/internal/federation/known_peers_archive.go` | CREATE | `KnownPeersArchive` interface (storage slice federation needs) |
| `tracker/internal/federation/peerexchange.go` | CREATE | `peerExchangeCoordinator`, `EmitNow`, `OnIncoming` |
| `tracker/internal/federation/peerexchange_test.go` | CREATE | Unit tests for coordinator (emit content, ingest, self-skip, nil-archive disabled) |
| `tracker/internal/federation/errors.go` | MODIFY | Add `ErrPeerExchangeDisabled` |
| `tracker/internal/federation/config.go` | MODIFY | Add `Deps.KnownPeers KnownPeersArchive` |
| `tracker/internal/federation/subsystem.go` | MODIFY | Build coordinator; allowlist seeding; dispatcher case; `PublishPeerExchange` method |
| `tracker/internal/federation/subsystem_test.go` | MODIFY | `TestFederation_PublishPeerExchange_*` (happy + nil-archive disabled) |
| `tracker/internal/federation/integration_test.go` | MODIFY | Two- and three-tracker peer-exchange propagation tests |
| `tracker/internal/federation/metrics.go` | MODIFY | `PeerExchangeEmitted`, `PeerExchangeReceived(outcome)`, `SetKnownPeersSize` |
| `tracker/internal/federation/metrics_test.go` | MODIFY | Test the new counters/gauge |
| `tracker/cmd/token-bay-tracker/run_cmd.go` | MODIFY | Pass `KnownPeers: store` in federation `Deps` |

---

## Notes for the executor

- Read the spec (`docs/superpowers/specs/federation/2026-05-10-federation-peer-exchange-design.md`) before Task 1; it contains the wire-format and dataflow contracts you'll be encoding.
- Read `tracker/internal/federation/revocation.go` and `tracker/internal/federation/publisher.go` first — slice 3 mirrors slice 2's coordinator pattern (no goroutine, on-demand emit, dispatcher hook) and slice 0's on-demand publisher pattern.
- The federation subsystem is on the always-`-race` list. All tests must pass `go test -race`.
- One conventional commit per red-green cycle (`feat:`, `fix:`, `test:`, `refactor:`, `docs:`, `chore:`). Don't squash multiple TDD cycles into one commit.
- Working directory throughout: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/federation`. The `go.work` file links the three modules; `make check` at the repo root runs all of them.
- Use `go test ./tracker/internal/federation/...`, `go test ./shared/federation/...`, etc. — not `cd` first; absolute paths or repo-root invocations are fine.
- The peer-exchange wire format has **no inner cryptographic signature** on `KnownPeer` or `PeerExchange`. The envelope's `sender_sig` is the only crypto binding. Do **not** add a `signing_peerexchange.go` — it's intentionally absent.

---

### Task 1: Add KIND_PEER_EXCHANGE + KnownPeer + PeerExchange proto

**Files:**
- Modify: `shared/federation/federation.proto`

- [ ] **Step 1: Edit the proto.** Add `KIND_PEER_EXCHANGE = 13;` to the `Kind` enum (right after `KIND_REVOCATION = 12`). Append the new messages at end of file.

```proto
  KIND_PEER_EXCHANGE          = 13;
```

Append at end of file:

```proto
message KnownPeer {
  bytes  tracker_id   = 1;  // 32 bytes
  string addr         = 2;  // e.g. "wss://tracker.example.org:443"; len ≤ 256
  uint64 last_seen    = 3;  // unix seconds; 0 = never
  string region_hint  = 4;  // human-friendly; len ≤ 64
  double health_score = 5;  // 0..1 (clamped on validate)
}

message PeerExchange {
  repeated KnownPeer peers = 1;  // ≤ 1024 entries (validator)
}
```

- [ ] **Step 2: Regenerate.** Run `make -C shared proto-gen`. Expect `shared/federation/federation.pb.go` to update with `KnownPeer`, `PeerExchange`, and `Kind_KIND_PEER_EXCHANGE`.

- [ ] **Step 3: Compile-check.** Run `go build ./shared/federation/...`. Expected: success. (No callers reference the new types yet.)

- [ ] **Step 4: Commit.**

```bash
git add shared/federation/federation.proto shared/federation/federation.pb.go
git commit -m "feat(shared/federation): KIND_PEER_EXCHANGE + KnownPeer + PeerExchange proto"
```

---

### Task 2: ValidatePeerExchange + ceiling lift (red)

**Files:**
- Modify: `shared/federation/validate_test.go`

- [ ] **Step 1: Write the failing test.** Append to `shared/federation/validate_test.go`:

```go
func validKnownPeer() *KnownPeer {
	return &KnownPeer{
		TrackerId:   bytes.Repeat([]byte{1}, TrackerIDLen),
		Addr:        "wss://tracker.example.org:443",
		LastSeen:    1714000000,
		RegionHint:  "eu-central-1",
		HealthScore: 0.9,
	}
}

func validPeerExchange() *PeerExchange {
	return &PeerExchange{Peers: []*KnownPeer{validKnownPeer()}}
}

func TestValidatePeerExchange_HappyPath(t *testing.T) {
	require.NoError(t, ValidatePeerExchange(validPeerExchange()))
}

func TestValidatePeerExchange_EmptyPeersOK(t *testing.T) {
	require.NoError(t, ValidatePeerExchange(&PeerExchange{}))
}

func TestValidatePeerExchange_BadShape(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*PeerExchange)
	}{
		{"nil msg", func(_ *PeerExchange) {}},
		{"too many entries", func(m *PeerExchange) {
			m.Peers = make([]*KnownPeer, 1025)
			for i := range m.Peers {
				m.Peers[i] = validKnownPeer()
			}
		}},
		{"nil entry", func(m *PeerExchange) { m.Peers = []*KnownPeer{nil} }},
		{"tracker_id wrong len", func(m *PeerExchange) {
			m.Peers[0].TrackerId = bytes.Repeat([]byte{1}, 16)
		}},
		{"tracker_id all zero", func(m *PeerExchange) {
			m.Peers[0].TrackerId = bytes.Repeat([]byte{0}, TrackerIDLen)
		}},
		{"addr empty", func(m *PeerExchange) { m.Peers[0].Addr = "" }},
		{"addr too long", func(m *PeerExchange) {
			m.Peers[0].Addr = string(bytes.Repeat([]byte("a"), 257))
		}},
		{"addr invalid utf8", func(m *PeerExchange) { m.Peers[0].Addr = "\xff\xfe" }},
		{"region_hint too long", func(m *PeerExchange) {
			m.Peers[0].RegionHint = string(bytes.Repeat([]byte("r"), 65))
		}},
		{"region_hint invalid utf8", func(m *PeerExchange) { m.Peers[0].RegionHint = "\xff" }},
		{"health_score negative", func(m *PeerExchange) { m.Peers[0].HealthScore = -0.1 }},
		{"health_score above one", func(m *PeerExchange) { m.Peers[0].HealthScore = 1.1 }},
		{"health_score nan", func(m *PeerExchange) { m.Peers[0].HealthScore = math.NaN() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var m *PeerExchange
			if tc.name != "nil msg" {
				m = validPeerExchange()
				tc.mutate(m)
			}
			require.Error(t, ValidatePeerExchange(m))
		})
	}
}

func TestValidateEnvelope_AcceptsKindPeerExchange(t *testing.T) {
	env := &Envelope{
		SenderId:  bytes.Repeat([]byte{1}, TrackerIDLen),
		Kind:      Kind_KIND_PEER_EXCHANGE,
		Payload:   []byte{0xAA},
		SenderSig: bytes.Repeat([]byte{3}, SigLen),
	}
	require.NoError(t, ValidateEnvelope(env))
}
```

The new test imports `math` — add `"math"` to the imports if not already present (search the file's import block).

- [ ] **Step 2: Run the test, expect failure.**

Run: `go test ./shared/federation/ -run 'TestValidatePeerExchange|TestValidateEnvelope_AcceptsKindPeerExchange' -v`
Expected: FAIL — `ValidatePeerExchange` undefined; `ValidateEnvelope` rejects `KIND_PEER_EXCHANGE` (out of range, ceiling is `KIND_REVOCATION`).

---

### Task 3: ValidatePeerExchange + ceiling lift (green)

**Files:**
- Modify: `shared/federation/validate.go`

- [ ] **Step 1: Lift envelope ceiling.** In `ValidateEnvelope`, change the upper-bound check at `shared/federation/validate.go:27`:

```go
	if e.Kind <= Kind_KIND_UNSPECIFIED || e.Kind > Kind_KIND_PEER_EXCHANGE {
```

- [ ] **Step 2: Append `ValidatePeerExchange` at end of file.** First ensure the file imports `"math"` and `"unicode/utf8"` (add to the existing import block; if either is already imported, don't double-add).

```go
// MaxPeerExchangeEntries caps the number of KnownPeer entries inside a
// single PeerExchange message at the validator boundary. The emitter
// caps itself lower (defaultPeerExchangeEmitCap = 256 in
// tracker/internal/federation); the validator allows headroom for
// future growth without DoS.
const MaxPeerExchangeEntries = 1024

// MaxKnownPeerAddrLen and MaxKnownPeerRegionHintLen are byte-length
// caps on the corresponding string fields.
const (
	MaxKnownPeerAddrLen       = 256
	MaxKnownPeerRegionHintLen = 64
)

// ValidatePeerExchange enforces shape invariants on a PeerExchange.
// Entries are advisory hints; this only checks shape, never trust.
func ValidatePeerExchange(m *PeerExchange) error {
	if m == nil {
		return errors.New("federation: nil PeerExchange")
	}
	if len(m.Peers) > MaxPeerExchangeEntries {
		return fmt.Errorf("federation: peer_exchange.peers len %d > %d", len(m.Peers), MaxPeerExchangeEntries)
	}
	for i, p := range m.Peers {
		if p == nil {
			return fmt.Errorf("federation: peer_exchange.peers[%d] is nil", i)
		}
		if len(p.TrackerId) != TrackerIDLen {
			return fmt.Errorf("federation: peer_exchange.peers[%d].tracker_id len %d != %d", i, len(p.TrackerId), TrackerIDLen)
		}
		if allZero(p.TrackerId) {
			return fmt.Errorf("federation: peer_exchange.peers[%d].tracker_id is all zero", i)
		}
		if len(p.Addr) == 0 {
			return fmt.Errorf("federation: peer_exchange.peers[%d].addr empty", i)
		}
		if len(p.Addr) > MaxKnownPeerAddrLen {
			return fmt.Errorf("federation: peer_exchange.peers[%d].addr len %d > %d", i, len(p.Addr), MaxKnownPeerAddrLen)
		}
		if !utf8.ValidString(p.Addr) {
			return fmt.Errorf("federation: peer_exchange.peers[%d].addr invalid utf-8", i)
		}
		if len(p.RegionHint) > MaxKnownPeerRegionHintLen {
			return fmt.Errorf("federation: peer_exchange.peers[%d].region_hint len %d > %d", i, len(p.RegionHint), MaxKnownPeerRegionHintLen)
		}
		if !utf8.ValidString(p.RegionHint) {
			return fmt.Errorf("federation: peer_exchange.peers[%d].region_hint invalid utf-8", i)
		}
		if math.IsNaN(p.HealthScore) || p.HealthScore < 0.0 || p.HealthScore > 1.0 {
			return fmt.Errorf("federation: peer_exchange.peers[%d].health_score %f out of [0,1]", i, p.HealthScore)
		}
	}
	return nil
}
```

- [ ] **Step 3: Run the test, expect pass.**

Run: `go test ./shared/federation/ -run 'TestValidatePeerExchange|TestValidateEnvelope_AcceptsKindPeerExchange' -v`
Expected: PASS.

- [ ] **Step 4: Run full validate suite.**

Run: `go test ./shared/federation/ -v`
Expected: PASS — existing tests still pass with the higher ceiling.

- [ ] **Step 5: Commit.**

```bash
git add shared/federation/validate.go shared/federation/validate_test.go
git commit -m "feat(shared/federation): ValidatePeerExchange + KIND_PEER_EXCHANGE envelope acceptance"
```

---

### Task 4: known_peers schema (additive migration)

**Files:**
- Modify: `tracker/internal/ledger/storage/schema_v1.sql`

- [ ] **Step 1: Append to schema.** At end of `schema_v1.sql`, add:

```sql
CREATE TABLE IF NOT EXISTS known_peers (
    tracker_id   BLOB    NOT NULL PRIMARY KEY,
    addr         TEXT    NOT NULL,
    last_seen    INTEGER NOT NULL,           -- unix seconds
    region_hint  TEXT    NOT NULL DEFAULT '',
    health_score REAL    NOT NULL DEFAULT 0.0,
    source       TEXT    NOT NULL CHECK (source IN ('allowlist','gossip'))
);

CREATE INDEX IF NOT EXISTS idx_known_peers_health
    ON known_peers (health_score DESC);
```

- [ ] **Step 2: Schema-load smoke check.** Run the storage test suite — schema is loaded by `Open` so this verifies the SQL parses on a fresh DB.

Run: `go test ./tracker/internal/ledger/storage/... -run 'TestStore' -v`
Expected: PASS — no migration syntax errors.

- [ ] **Step 3: Commit.**

```bash
git add tracker/internal/ledger/storage/schema_v1.sql
git commit -m "feat(tracker/storage): known_peers schema (additive)"
```

---

### Task 5: KnownPeer storage type + UpsertKnownPeer (red)

**Files:**
- Create: `tracker/internal/ledger/storage/known_peers_test.go`

- [ ] **Step 1: Write the failing test.** Create `tracker/internal/ledger/storage/known_peers_test.go`:

```go
package storage

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestKnownPeers_UpsertAndGet(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id := bytes.Repeat([]byte{1}, 32)
	row := KnownPeer{
		TrackerID:   id,
		Addr:        "wss://a.example:443",
		LastSeen:    time.Unix(1714000000, 0),
		RegionHint:  "eu-central-1",
		HealthScore: 0.9,
		Source:      "allowlist",
	}
	require.NoError(t, s.UpsertKnownPeer(ctx, row))

	got, ok, err := s.GetKnownPeer(ctx, id)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "wss://a.example:443", got.Addr)
	require.Equal(t, "eu-central-1", got.RegionHint)
	require.InDelta(t, 0.9, got.HealthScore, 0.0001)
	require.Equal(t, "allowlist", got.Source)
	require.Equal(t, int64(1714000000), got.LastSeen.Unix())
}

func TestKnownPeers_GetMissReturnsFalse(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	_, ok, err := s.GetKnownPeer(ctx, bytes.Repeat([]byte{9}, 32))
	require.NoError(t, err)
	require.False(t, ok)
}
```

- [ ] **Step 2: Run, expect failure.**

Run: `go test ./tracker/internal/ledger/storage/ -run 'TestKnownPeers_' -v`
Expected: FAIL — `KnownPeer` undefined, `UpsertKnownPeer`/`GetKnownPeer` undefined.

---

### Task 6: KnownPeer storage type + UpsertKnownPeer (green)

**Files:**
- Create: `tracker/internal/ledger/storage/known_peers.go`

- [ ] **Step 1: Create the file.**

```go
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// KnownPeer is one row from known_peers. Source is "allowlist" for
// operator-configured peers and "gossip" for entries learned via
// PEER_EXCHANGE.
type KnownPeer struct {
	TrackerID   []byte
	Addr        string
	LastSeen    time.Time
	RegionHint  string
	HealthScore float64
	Source      string
}

// UpsertKnownPeer inserts or updates one known_peers row.
//
// SQL invariants enforced in this statement:
//   - Last-write-wins by last_seen: a stale upsert (excluded.last_seen <
//     known_peers.last_seen) is a no-op for an existing 'gossip' row.
//   - Allowlist rows are pinned: addr and region_hint of a 'allowlist'
//     row cannot be mutated by a 'gossip' write. health_score and
//     last_seen of a 'allowlist' row may still update so slice-5 metrics
//     can flow in.
//   - source is never changed by an upsert.
func (s *Store) UpsertKnownPeer(ctx context.Context, p KnownPeer) error {
	if len(p.TrackerID) == 0 {
		return errors.New("storage: UpsertKnownPeer requires non-empty tracker_id")
	}
	if p.Addr == "" {
		return errors.New("storage: UpsertKnownPeer requires non-empty addr")
	}
	if p.Source != "allowlist" && p.Source != "gossip" {
		return fmt.Errorf("storage: UpsertKnownPeer invalid source %q", p.Source)
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO known_peers (tracker_id, addr, last_seen, region_hint, health_score, source)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(tracker_id) DO UPDATE SET
		    addr         = CASE WHEN known_peers.source = 'allowlist' AND excluded.source = 'gossip'
		                        THEN known_peers.addr ELSE excluded.addr END,
		    last_seen    = MAX(known_peers.last_seen, excluded.last_seen),
		    region_hint  = CASE WHEN known_peers.source = 'allowlist' AND excluded.source = 'gossip'
		                        THEN known_peers.region_hint ELSE excluded.region_hint END,
		    health_score = CASE WHEN excluded.last_seen >= known_peers.last_seen
		                        THEN excluded.health_score ELSE known_peers.health_score END,
		    source       = known_peers.source
		WHERE excluded.last_seen >= known_peers.last_seen
		   OR known_peers.source  = 'allowlist'`,
		p.TrackerID, p.Addr, p.LastSeen.Unix(), p.RegionHint, p.HealthScore, p.Source,
	); err != nil {
		return fmt.Errorf("storage: UpsertKnownPeer: %w", err)
	}
	return nil
}

// GetKnownPeer returns the row for trackerID, or ok=false on miss.
func (s *Store) GetKnownPeer(ctx context.Context, trackerID []byte) (KnownPeer, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT tracker_id, addr, last_seen, region_hint, health_score, source
		FROM known_peers WHERE tracker_id = ?`,
		trackerID,
	)
	var (
		p        KnownPeer
		lastSeen int64
	)
	err := row.Scan(&p.TrackerID, &p.Addr, &lastSeen, &p.RegionHint, &p.HealthScore, &p.Source)
	if errors.Is(err, sql.ErrNoRows) {
		return KnownPeer{}, false, nil
	}
	if err != nil {
		return KnownPeer{}, false, fmt.Errorf("storage: GetKnownPeer: %w", err)
	}
	p.LastSeen = time.Unix(lastSeen, 0)
	return p, true, nil
}

// ListKnownPeers returns up to limit rows, optionally sorted by health
// descending (then tracker_id ascending for deterministic tiebreak).
func (s *Store) ListKnownPeers(ctx context.Context, limit int, byHealthDesc bool) ([]KnownPeer, error) {
	if limit <= 0 {
		return nil, nil
	}
	order := "tracker_id ASC"
	if byHealthDesc {
		order = "health_score DESC, tracker_id ASC"
	}
	q := fmt.Sprintf(`
		SELECT tracker_id, addr, last_seen, region_hint, health_score, source
		FROM known_peers
		ORDER BY %s
		LIMIT ?`, order)
	rows, err := s.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("storage: ListKnownPeers: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var out []KnownPeer
	for rows.Next() {
		var (
			p        KnownPeer
			lastSeen int64
		)
		if err := rows.Scan(&p.TrackerID, &p.Addr, &lastSeen, &p.RegionHint, &p.HealthScore, &p.Source); err != nil {
			return nil, fmt.Errorf("storage: ListKnownPeers scan: %w", err)
		}
		p.LastSeen = time.Unix(lastSeen, 0)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: ListKnownPeers rows: %w", err)
	}
	return out, nil
}
```

- [ ] **Step 2: Run, expect pass.**

Run: `go test ./tracker/internal/ledger/storage/ -run 'TestKnownPeers_' -v`
Expected: PASS.

- [ ] **Step 3: Commit.**

```bash
git add tracker/internal/ledger/storage/known_peers.go tracker/internal/ledger/storage/known_peers_test.go
git commit -m "feat(tracker/storage): KnownPeer + UpsertKnownPeer/GetKnownPeer/ListKnownPeers"
```

---

### Task 7: known_peers SQL invariants (last-write-wins, allowlist pinning, list ordering)

**Files:**
- Modify: `tracker/internal/ledger/storage/known_peers_test.go`

- [ ] **Step 1: Write the failing tests.** Append to `known_peers_test.go`:

```go
func TestKnownPeers_LastWriteWins(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := bytes.Repeat([]byte{1}, 32)

	// Insert at t=200.
	require.NoError(t, s.UpsertKnownPeer(ctx, KnownPeer{
		TrackerID: id, Addr: "wss://a:1", LastSeen: time.Unix(200, 0),
		HealthScore: 0.9, Source: "gossip",
	}))
	// Stale upsert at t=100 with different addr — must NOT overwrite.
	require.NoError(t, s.UpsertKnownPeer(ctx, KnownPeer{
		TrackerID: id, Addr: "wss://b:2", LastSeen: time.Unix(100, 0),
		HealthScore: 0.1, Source: "gossip",
	}))
	got, _, err := s.GetKnownPeer(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "wss://a:1", got.Addr)
	require.InDelta(t, 0.9, got.HealthScore, 0.0001)
	require.Equal(t, int64(200), got.LastSeen.Unix())

	// Newer upsert at t=300 — wins.
	require.NoError(t, s.UpsertKnownPeer(ctx, KnownPeer{
		TrackerID: id, Addr: "wss://c:3", LastSeen: time.Unix(300, 0),
		HealthScore: 0.5, Source: "gossip",
	}))
	got, _, err = s.GetKnownPeer(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "wss://c:3", got.Addr)
	require.InDelta(t, 0.5, got.HealthScore, 0.0001)
}

func TestKnownPeers_AllowlistPinning(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := bytes.Repeat([]byte{1}, 32)

	// Seed with allowlist row.
	require.NoError(t, s.UpsertKnownPeer(ctx, KnownPeer{
		TrackerID: id, Addr: "wss://config:443", LastSeen: time.Unix(100, 0),
		RegionHint: "operator-config-region", HealthScore: 0.5, Source: "allowlist",
	}))

	// Gossip says different addr/region/health at later time — addr +
	// region_hint must NOT change; health_score MAY update; source stays
	// 'allowlist'.
	require.NoError(t, s.UpsertKnownPeer(ctx, KnownPeer{
		TrackerID: id, Addr: "wss://gossip-lie:1", LastSeen: time.Unix(200, 0),
		RegionHint: "lie", HealthScore: 0.99, Source: "gossip",
	}))
	got, _, err := s.GetKnownPeer(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "wss://config:443", got.Addr, "allowlist addr must be pinned")
	require.Equal(t, "operator-config-region", got.RegionHint, "allowlist region_hint must be pinned")
	require.Equal(t, "allowlist", got.Source, "source must not change to gossip")
	require.InDelta(t, 0.99, got.HealthScore, 0.0001, "health_score may update")
}

func TestKnownPeers_AllowlistRefreshFromConfig(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := bytes.Repeat([]byte{1}, 32)

	// Seed.
	require.NoError(t, s.UpsertKnownPeer(ctx, KnownPeer{
		TrackerID: id, Addr: "wss://old:443", LastSeen: time.Unix(100, 0),
		RegionHint: "old-region", HealthScore: 0.5, Source: "allowlist",
	}))
	// Operator changed YAML config: allowlist→allowlist write must take effect.
	require.NoError(t, s.UpsertKnownPeer(ctx, KnownPeer{
		TrackerID: id, Addr: "wss://new:443", LastSeen: time.Unix(200, 0),
		RegionHint: "new-region", HealthScore: 0.5, Source: "allowlist",
	}))
	got, _, err := s.GetKnownPeer(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "wss://new:443", got.Addr)
	require.Equal(t, "new-region", got.RegionHint)
}

func TestKnownPeers_ListByHealthDescDeterministic(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Two rows with same health, different tracker_id — tracker_id
	// ascending tiebreak.
	idA := bytes.Repeat([]byte{0xAA}, 32)
	idB := bytes.Repeat([]byte{0xBB}, 32)
	idC := bytes.Repeat([]byte{0xCC}, 32)
	require.NoError(t, s.UpsertKnownPeer(ctx, KnownPeer{TrackerID: idC, Addr: "x", LastSeen: time.Unix(1, 0), HealthScore: 0.5, Source: "gossip"}))
	require.NoError(t, s.UpsertKnownPeer(ctx, KnownPeer{TrackerID: idA, Addr: "x", LastSeen: time.Unix(1, 0), HealthScore: 0.9, Source: "gossip"}))
	require.NoError(t, s.UpsertKnownPeer(ctx, KnownPeer{TrackerID: idB, Addr: "x", LastSeen: time.Unix(1, 0), HealthScore: 0.5, Source: "gossip"}))

	got, err := s.ListKnownPeers(ctx, 10, true)
	require.NoError(t, err)
	require.Len(t, got, 3)
	require.Equal(t, idA, got[0].TrackerID, "highest health first")
	require.Equal(t, idB, got[1].TrackerID, "tie broken by tracker_id ASC")
	require.Equal(t, idC, got[2].TrackerID)
}
```

- [ ] **Step 2: Run the new tests, expect pass.** The SQL invariants are already in place from Task 6; these tests verify them.

Run: `go test ./tracker/internal/ledger/storage/ -run 'TestKnownPeers_' -v`
Expected: PASS.

- [ ] **Step 3: Commit.**

```bash
git add tracker/internal/ledger/storage/known_peers_test.go
git commit -m "test(tracker/storage): known_peers last-write-wins + allowlist pinning + list ordering"
```

---

### Task 8: KnownPeersArchive interface (federation→storage hook)

**Files:**
- Create: `tracker/internal/federation/known_peers_archive.go`

- [ ] **Step 1: Create the file.**

```go
package federation

import (
	"context"

	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
)

// KnownPeersArchive is the slice of *storage.Store federation needs for
// peer-exchange gossip. *storage.Store satisfies it via Go structural
// typing; tests pass a fake. May be nil in Deps; when nil, peer-exchange
// is disabled (Federation.PublishPeerExchange returns
// ErrPeerExchangeDisabled and inbound KIND_PEER_EXCHANGE is rejected
// with metric reason "peer_exchange_disabled").
type KnownPeersArchive interface {
	UpsertKnownPeer(ctx context.Context, p storage.KnownPeer) error
	GetKnownPeer(ctx context.Context, trackerID []byte) (storage.KnownPeer, bool, error)
	ListKnownPeers(ctx context.Context, limit int, byHealthDesc bool) ([]storage.KnownPeer, error)
}
```

- [ ] **Step 2: Compile-check.**

Run: `go build ./tracker/internal/federation/...`
Expected: success.

- [ ] **Step 3: Commit.**

```bash
git add tracker/internal/federation/known_peers_archive.go
git commit -m "feat(federation): KnownPeersArchive interface"
```

---

### Task 9: Deps.KnownPeers slot + ErrPeerExchangeDisabled

**Files:**
- Modify: `tracker/internal/federation/config.go`
- Modify: `tracker/internal/federation/errors.go`

- [ ] **Step 1: Add the Deps slot.** In `config.go`'s `Deps` struct, append after `RevocationArchive`:

```go
	// KnownPeers is the federation→storage hook for peer-exchange
	// (slice 3). May be nil; when nil, Federation.PublishPeerExchange
	// returns ErrPeerExchangeDisabled and inbound KIND_PEER_EXCHANGE is
	// rejected with metric reason "peer_exchange_disabled".
	KnownPeers KnownPeersArchive
```

- [ ] **Step 2: Add the error sentinel.** Append to `tracker/internal/federation/errors.go`:

```go
// ErrPeerExchangeDisabled is returned by Federation.PublishPeerExchange
// when the subsystem was opened without a KnownPeersArchive Dep.
var ErrPeerExchangeDisabled = errors.New("federation: peer exchange disabled")
```

If the `errors` package is not yet imported in `errors.go`, add it.

- [ ] **Step 3: Compile-check.**

Run: `go build ./tracker/internal/federation/...`
Expected: success.

- [ ] **Step 4: Commit.**

```bash
git add tracker/internal/federation/config.go tracker/internal/federation/errors.go
git commit -m "feat(federation): Deps.KnownPeers + ErrPeerExchangeDisabled"
```

---

### Task 10: peerExchangeCoordinator (red)

**Files:**
- Create: `tracker/internal/federation/peerexchange_test.go`

- [ ] **Step 1: Write the failing tests.**

```go
package federation

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"testing"
	"time"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// fakeKnownPeersArchive is an in-memory KnownPeersArchive for unit tests.
type fakeKnownPeersArchive struct {
	rows []storage.KnownPeer
}

func (f *fakeKnownPeersArchive) UpsertKnownPeer(_ context.Context, p storage.KnownPeer) error {
	for i, r := range f.rows {
		if bytes.Equal(r.TrackerID, p.TrackerID) {
			// Last-write-wins simulation.
			if p.LastSeen.After(r.LastSeen) || p.LastSeen.Equal(r.LastSeen) {
				if r.Source == "allowlist" && p.Source == "gossip" {
					p.Addr = r.Addr
					p.RegionHint = r.RegionHint
					p.Source = "allowlist"
				}
				f.rows[i] = p
			}
			return nil
		}
	}
	f.rows = append(f.rows, p)
	return nil
}

func (f *fakeKnownPeersArchive) GetKnownPeer(_ context.Context, trackerID []byte) (storage.KnownPeer, bool, error) {
	for _, r := range f.rows {
		if bytes.Equal(r.TrackerID, trackerID) {
			return r, true, nil
		}
	}
	return storage.KnownPeer{}, false, nil
}

func (f *fakeKnownPeersArchive) ListKnownPeers(_ context.Context, limit int, _ bool) ([]storage.KnownPeer, error) {
	if limit > len(f.rows) {
		limit = len(f.rows)
	}
	return append([]storage.KnownPeer(nil), f.rows[:limit]...), nil
}

func mustGenKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey, ids.TrackerID) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return pub, priv, ids.TrackerID(sha256.Sum256(pub))
}

func TestPeerExchange_EmitNow_BuildsPeerExchangeFromArchive(t *testing.T) {
	_, myPriv, myID := mustGenKeypair(t)

	bID := bytes.Repeat([]byte{0xBB}, 32)
	cID := bytes.Repeat([]byte{0xCC}, 32)
	arch := &fakeKnownPeersArchive{rows: []storage.KnownPeer{
		{TrackerID: bID, Addr: "wss://b:443", LastSeen: time.Unix(100, 0), RegionHint: "eu", HealthScore: 0.0, Source: "allowlist"},
		{TrackerID: cID, Addr: "wss://c:443", LastSeen: time.Unix(50, 0), RegionHint: "us", HealthScore: 0.7, Source: "gossip"},
	}}

	var (
		gotKind    fed.Kind
		gotPayload []byte
	)
	forward := func(_ context.Context, kind fed.Kind, payload []byte) {
		gotKind = kind
		gotPayload = payload
	}

	emitted := 0
	pc := newPeerExchangeCoordinator(peerExchangeCoordinatorCfg{
		MyTrackerID:   myID,
		MyPriv:        myPriv,
		Archive:       arch,
		Forward:       forward,
		PeerConnected: func(id ids.TrackerID) bool { return bytes.Equal(id.Bytes(), bID) },
		Now:           func() time.Time { return time.Unix(1000, 0) },
		EmitCap:       100,
		Invalid:       func(string) {},
		OnEmit:        func() { emitted++ },
		OnReceived:    func(string) {},
	})

	require.NoError(t, pc.EmitNow(context.Background()))
	require.Equal(t, 1, emitted, "OnEmit fired")
	require.Equal(t, fed.Kind_KIND_PEER_EXCHANGE, gotKind)

	var msg fed.PeerExchange
	require.NoError(t, proto.Unmarshal(gotPayload, &msg))
	require.Len(t, msg.Peers, 2)

	// Find each entry by tracker_id and check fields.
	byID := map[string]*fed.KnownPeer{}
	for _, p := range msg.Peers {
		byID[string(p.TrackerId)] = p
	}
	bEntry := byID[string(bID)]
	require.NotNil(t, bEntry)
	require.Equal(t, "wss://b:443", bEntry.Addr)
	require.InDelta(t, 1.0, bEntry.HealthScore, 0.0001, "allowlist + connected → 1.0")

	cEntry := byID[string(cID)]
	require.NotNil(t, cEntry)
	require.Equal(t, "wss://c:443", cEntry.Addr)
	require.InDelta(t, 0.7, cEntry.HealthScore, 0.0001, "gossip → verbatim")
}

func TestPeerExchange_EmitNow_AllowlistDisconnectedScores0_5(t *testing.T) {
	_, myPriv, myID := mustGenKeypair(t)
	bID := bytes.Repeat([]byte{0xBB}, 32)
	arch := &fakeKnownPeersArchive{rows: []storage.KnownPeer{
		{TrackerID: bID, Addr: "wss://b:443", LastSeen: time.Unix(100, 0), HealthScore: 0.0, Source: "allowlist"},
	}}
	var gotPayload []byte
	pc := newPeerExchangeCoordinator(peerExchangeCoordinatorCfg{
		MyTrackerID:   myID,
		MyPriv:        myPriv,
		Archive:       arch,
		Forward:       func(_ context.Context, _ fed.Kind, p []byte) { gotPayload = p },
		PeerConnected: func(_ ids.TrackerID) bool { return false },
		Now:           func() time.Time { return time.Unix(1000, 0) },
		EmitCap:       100,
		Invalid:       func(string) {}, OnEmit: func() {}, OnReceived: func(string) {},
	})
	require.NoError(t, pc.EmitNow(context.Background()))
	var msg fed.PeerExchange
	require.NoError(t, proto.Unmarshal(gotPayload, &msg))
	require.InDelta(t, 0.5, msg.Peers[0].HealthScore, 0.0001)
}

func TestPeerExchange_OnIncoming_UpsertsAllAndForwards(t *testing.T) {
	_, myPriv, myID := mustGenKeypair(t)
	arch := &fakeKnownPeersArchive{}

	bID := bytes.Repeat([]byte{0xBB}, 32)
	cID := bytes.Repeat([]byte{0xCC}, 32)
	msg := &fed.PeerExchange{
		Peers: []*fed.KnownPeer{
			{TrackerId: bID, Addr: "wss://b:443", LastSeen: 100, RegionHint: "eu", HealthScore: 0.6},
			{TrackerId: cID, Addr: "wss://c:443", LastSeen: 200, RegionHint: "us", HealthScore: 0.8},
		},
	}
	payload, err := proto.Marshal(msg)
	require.NoError(t, err)

	forwardCalls := 0
	pc := newPeerExchangeCoordinator(peerExchangeCoordinatorCfg{
		MyTrackerID:   myID,
		MyPriv:        myPriv,
		Archive:       arch,
		Forward:       func(_ context.Context, _ fed.Kind, _ []byte) { forwardCalls++ },
		PeerConnected: func(_ ids.TrackerID) bool { return false },
		Now:           func() time.Time { return time.Unix(1000, 0) },
		EmitCap:       100,
		Invalid:       func(string) {},
		OnEmit:        func() {},
		OnReceived:    func(_ string) {},
	})

	env := &fed.Envelope{Kind: fed.Kind_KIND_PEER_EXCHANGE, Payload: payload}
	pc.OnIncoming(context.Background(), env, msg)

	require.Len(t, arch.rows, 2)
	for _, r := range arch.rows {
		require.Equal(t, "gossip", r.Source)
	}
	require.Equal(t, 1, forwardCalls)
}

func TestPeerExchange_OnIncoming_SkipsSelfEntry(t *testing.T) {
	_, myPriv, myID := mustGenKeypair(t)
	arch := &fakeKnownPeersArchive{}
	myBytes := myID.Bytes()

	msg := &fed.PeerExchange{
		Peers: []*fed.KnownPeer{
			{TrackerId: myBytes[:], Addr: "wss://self:443", LastSeen: 100, HealthScore: 0.5},
			{TrackerId: bytes.Repeat([]byte{0xBB}, 32), Addr: "wss://b:443", LastSeen: 100, HealthScore: 0.6},
		},
	}
	payload, _ := proto.Marshal(msg)
	pc := newPeerExchangeCoordinator(peerExchangeCoordinatorCfg{
		MyTrackerID:   myID,
		MyPriv:        myPriv,
		Archive:       arch,
		Forward:       func(_ context.Context, _ fed.Kind, _ []byte) {},
		PeerConnected: func(_ ids.TrackerID) bool { return false },
		Now:           func() time.Time { return time.Unix(1000, 0) },
		EmitCap:       100,
		Invalid:       func(string) {}, OnEmit: func() {}, OnReceived: func(string) {},
	})
	pc.OnIncoming(context.Background(), &fed.Envelope{Payload: payload}, msg)

	require.Len(t, arch.rows, 1, "self entry skipped")
	require.NotEqual(t, myBytes[:], arch.rows[0].TrackerID)
}
```

- [ ] **Step 2: Run, expect failure.**

Run: `go test ./tracker/internal/federation/ -run 'TestPeerExchange_' -v`
Expected: FAIL — `peerExchangeCoordinator`, `peerExchangeCoordinatorCfg`, `newPeerExchangeCoordinator` undefined.

---

### Task 11: peerExchangeCoordinator (green)

**Files:**
- Create: `tracker/internal/federation/peerexchange.go`

- [ ] **Step 1: Create the file.**

```go
package federation

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"time"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
	"google.golang.org/protobuf/proto"
)

const defaultPeerExchangeEmitCap = 256

// peerExchangeCoordinator implements §7.1 PEER_EXCHANGE: hourly-cadence
// (on-demand in v1) gossip of a KnownPeer table, plus inbound merge.
//
// Outbound: EmitNow(ctx) lists the top-EmitCap rows from the archive,
// refreshes health_score for source='allowlist' rows using
// PeerConnected (slice-3 placeholder; slice 5 replaces with real
// metrics), packs into a PeerExchange proto, marshals, and Forwards.
//
// Inbound: OnIncoming(ctx, env, msg) upserts every entry (skipping
// self-entries by tracker_id) and forwards the envelope onward via the
// slice-0 dedupe-and-forward gossip core.
type peerExchangeCoordinator struct {
	cfg peerExchangeCoordinatorCfg
}

type peerExchangeCoordinatorCfg struct {
	MyTrackerID   ids.TrackerID
	MyPriv        ed25519.PrivateKey
	Archive       KnownPeersArchive
	Forward       Forwarder
	PeerConnected func(ids.TrackerID) bool
	Now           func() time.Time
	EmitCap       int
	Invalid       func(reason string)
	OnEmit        func()
	OnReceived    func(outcome string)
}

func newPeerExchangeCoordinator(cfg peerExchangeCoordinatorCfg) *peerExchangeCoordinator {
	if cfg.EmitCap <= 0 {
		cfg.EmitCap = defaultPeerExchangeEmitCap
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &peerExchangeCoordinator{cfg: cfg}
}

// EmitNow snapshots the archive and broadcasts a PeerExchange envelope
// to all active peers via Forward. Returns the archive's error verbatim
// if the snapshot fails; otherwise nil.
func (pc *peerExchangeCoordinator) EmitNow(ctx context.Context) error {
	rows, err := pc.cfg.Archive.ListKnownPeers(ctx, pc.cfg.EmitCap, true)
	if err != nil {
		return err
	}

	out := make([]*fed.KnownPeer, 0, len(rows))
	for _, r := range rows {
		health := r.HealthScore
		if r.Source == "allowlist" && len(r.TrackerID) == 32 {
			var tid ids.TrackerID
			copy(tid[:], r.TrackerID)
			if pc.cfg.PeerConnected(tid) {
				health = 1.0
			} else {
				health = 0.5
			}
		}
		out = append(out, &fed.KnownPeer{
			TrackerId:   append([]byte(nil), r.TrackerID...),
			Addr:        r.Addr,
			LastSeen:    uint64(r.LastSeen.Unix()), //nolint:gosec // G115 — Unix() is non-negative for any sane wall clock
			RegionHint:  r.RegionHint,
			HealthScore: health,
		})
	}

	payload, err := proto.Marshal(&fed.PeerExchange{Peers: out})
	if err != nil {
		return err
	}
	pc.cfg.Forward(ctx, fed.Kind_KIND_PEER_EXCHANGE, payload)
	pc.cfg.OnEmit()
	return nil
}

// OnIncoming merges a received PeerExchange into the archive (skipping
// self-entries) and forwards the envelope onward.
func (pc *peerExchangeCoordinator) OnIncoming(ctx context.Context, env *fed.Envelope, msg *fed.PeerExchange) {
	myBytes := pc.cfg.MyTrackerID.Bytes()
	for _, p := range msg.Peers {
		if bytes.Equal(p.TrackerId, myBytes[:]) {
			continue
		}
		_ = pc.cfg.Archive.UpsertKnownPeer(ctx, storage.KnownPeer{
			TrackerID:   append([]byte(nil), p.TrackerId...),
			Addr:        p.Addr,
			LastSeen:    time.Unix(int64(p.LastSeen), 0), //nolint:gosec // G115 — validator capped above
			RegionHint:  p.RegionHint,
			HealthScore: p.HealthScore,
			Source:      "gossip",
		})
	}
	pc.cfg.Forward(ctx, fed.Kind_KIND_PEER_EXCHANGE, env.Payload)
	pc.cfg.OnReceived("merged")
}
```

- [ ] **Step 2: Run, expect pass.**

Run: `go test ./tracker/internal/federation/ -run 'TestPeerExchange_' -v`
Expected: PASS.

- [ ] **Step 3: Commit.**

```bash
git add tracker/internal/federation/peerexchange.go tracker/internal/federation/peerexchange_test.go
git commit -m "feat(federation): peerExchangeCoordinator (EmitNow + OnIncoming)"
```

---

### Task 12: Metrics — peer_exchange_emitted/received + known_peers_size (red)

**Files:**
- Modify: `tracker/internal/federation/metrics_test.go`

- [ ] **Step 1: Write the failing tests.** Append to `metrics_test.go`:

```go
func TestMetrics_PeerExchangeEmittedCounts(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.PeerExchangeEmitted()
	m.PeerExchangeEmitted()
	require.InDelta(t, 2.0, testutil.ToFloat64(m.PeerExchangeEmittedCounter()), 0.0001)
}

func TestMetrics_PeerExchangeReceivedLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.PeerExchangeReceived("merged")
	m.PeerExchangeReceived("merged")
	m.PeerExchangeReceived("peer_exchange_disabled")
	v := m.PeerExchangeReceivedVec()
	require.InDelta(t, 2.0, testutil.ToFloat64(v.WithLabelValues("merged")), 0.0001)
	require.InDelta(t, 1.0, testutil.ToFloat64(v.WithLabelValues("peer_exchange_disabled")), 0.0001)
}

func TestMetrics_KnownPeersSize(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.SetKnownPeersSize(7)
	require.InDelta(t, 7.0, testutil.ToFloat64(m.KnownPeersSizeGauge()), 0.0001)
}
```

- [ ] **Step 2: Run, expect failure.**

Run: `go test ./tracker/internal/federation/ -run 'TestMetrics_PeerExchange|TestMetrics_KnownPeersSize' -v`
Expected: FAIL — `PeerExchangeEmitted`, `PeerExchangeReceived`, `SetKnownPeersSize`, etc. undefined.

---

### Task 13: Metrics — peer_exchange_emitted/received + known_peers_size (green)

**Files:**
- Modify: `tracker/internal/federation/metrics.go`

- [ ] **Step 1: Add fields to `Metrics` struct.** Add three lines in the `Metrics` struct, right after `revocationsReceived`:

```go
	peerExchangeEmitted  prometheus.Counter
	peerExchangeReceived *prometheus.CounterVec
	knownPeersSize       prometheus.Gauge
```

- [ ] **Step 2: Initialize in `NewMetrics`.** Inside `NewMetrics`, append to the `m := &Metrics{...}` literal three new fields:

```go
		peerExchangeEmitted:  prometheus.NewCounter(prometheus.CounterOpts{Name: "tokenbay_federation_peer_exchange_emitted_total"}),
		peerExchangeReceived: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "tokenbay_federation_peer_exchange_received_total"}, []string{"outcome"}),
		knownPeersSize:       prometheus.NewGauge(prometheus.GaugeOpts{Name: "tokenbay_federation_known_peers_size"}),
```

Add them to the `MustRegister` slice that follows.

- [ ] **Step 3: Add accessor methods.** Append to `metrics.go`:

```go
func (m *Metrics) PeerExchangeEmitted()                     { m.peerExchangeEmitted.Inc() }
func (m *Metrics) PeerExchangeReceived(outcome string)      { m.peerExchangeReceived.WithLabelValues(outcome).Inc() }
func (m *Metrics) SetKnownPeersSize(n int)                  { m.knownPeersSize.Set(float64(n)) }
func (m *Metrics) PeerExchangeEmittedCounter() prometheus.Counter { return m.peerExchangeEmitted }
func (m *Metrics) PeerExchangeReceivedVec() *prometheus.CounterVec { return m.peerExchangeReceived }
func (m *Metrics) KnownPeersSizeGauge() prometheus.Gauge    { return m.knownPeersSize }
```

- [ ] **Step 4: Run, expect pass.**

Run: `go test ./tracker/internal/federation/ -run 'TestMetrics_PeerExchange|TestMetrics_KnownPeersSize' -v`
Expected: PASS.

- [ ] **Step 5: Run full metrics suite.**

Run: `go test ./tracker/internal/federation/ -run 'TestMetrics' -v`
Expected: PASS — existing slice-0/-1/-2 metrics tests still pass.

- [ ] **Step 6: Commit.**

```bash
git add tracker/internal/federation/metrics.go tracker/internal/federation/metrics_test.go
git commit -m "feat(federation): peer_exchange_emitted/received + known_peers_size metrics"
```

---

### Task 14: Subsystem wiring — coordinator + dispatcher case + PublishPeerExchange (red)

**Files:**
- Modify: `tracker/internal/federation/subsystem_test.go`

- [ ] **Step 1: Write the failing tests.** Append:

```go
func TestFederation_PublishPeerExchange_NoArchive_ReturnsErr(t *testing.T) {
	f := newTestFederation(t, withoutKnownPeers)
	t.Cleanup(func() { _ = f.Close() })

	err := f.PublishPeerExchange(context.Background())
	require.ErrorIs(t, err, ErrPeerExchangeDisabled)
}

func TestFederation_PublishPeerExchange_WithArchive_BumpsMetricAndForwards(t *testing.T) {
	f := newTestFederation(t, withKnownPeers)
	t.Cleanup(func() { _ = f.Close() })

	require.NoError(t, f.PublishPeerExchange(context.Background()))
	require.InDelta(t, 1.0, testutil.ToFloat64(f.dep.Metrics.PeerExchangeEmittedCounter()), 0.0001)
}
```

`newTestFederation`, `withoutKnownPeers`, and `withKnownPeers` are helpers. If they don't exist already in `subsystem_test.go`, add them as part of the same task — examine the existing `subsystem_test.go` for the existing test-federation construction pattern (look for `Open(...)` calls and the inproc transport setup) and parameterize it so the new option toggles `Deps.KnownPeers`.

A reasonable shape (adapt to whatever the file already does):

```go
type testFedOpt func(*Deps)

func withKnownPeers(d *Deps)    { d.KnownPeers = &fakeKnownPeersArchive{} }
func withoutKnownPeers(d *Deps) { d.KnownPeers = nil }

func newTestFederation(t *testing.T, opts ...testFedOpt) *Federation {
	t.Helper()
	hub := NewInprocHub()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	tid := ids.TrackerIDFromPubKey(pub)
	deps := Deps{
		Transport: NewInprocTransport(hub, "self", pub, priv),
		RootSrc:   stubRootSource{},
		Archive:   &stubPeerRootArchive{},
		Metrics:   NewMetrics(prometheus.NewRegistry()),
		Now:       time.Now,
	}
	for _, o := range opts {
		o(&deps)
	}
	f, err := Open(Config{MyTrackerID: tid, MyPriv: priv}, deps)
	require.NoError(t, err)
	return f
}
```

If the file already has its own helper, reuse it and add a `KnownPeers` option to whatever pattern is established. Keep the change scoped — don't refactor unrelated tests.

- [ ] **Step 2: Run, expect failure.**

Run: `go test ./tracker/internal/federation/ -run 'TestFederation_PublishPeerExchange' -v`
Expected: FAIL — `Federation.PublishPeerExchange` undefined; the dispatcher does not yet route `KIND_PEER_EXCHANGE`.

---

### Task 15: Subsystem wiring — coordinator + dispatcher case + PublishPeerExchange (green)

**Files:**
- Modify: `tracker/internal/federation/subsystem.go`

- [ ] **Step 1: Add the field.** Add `peerExchange *peerExchangeCoordinator` to the `Federation` struct, right after `revocation`:

```go
	revocation   *revocationCoordinator
	peerExchange *peerExchangeCoordinator
```

- [ ] **Step 2: Construct the coordinator in `Open`.** Right after the `revocation := newRevocationCoordinator(...)` block, add:

```go
	var peerExchange *peerExchangeCoordinator
	if dep.KnownPeers != nil {
		peerExchange = newPeerExchangeCoordinator(peerExchangeCoordinatorCfg{
			MyTrackerID: cfg.MyTrackerID,
			MyPriv:      cfg.MyPriv,
			Archive:     dep.KnownPeers,
			Forward:     forward,
			Now:         dep.Now,
			EmitCap:     defaultPeerExchangeEmitCap,
			Invalid:     func(name string) { dep.Metrics.InvalidFrames(name) },
			OnEmit:      dep.Metrics.PeerExchangeEmitted,
			OnReceived:  dep.Metrics.PeerExchangeReceived,
			// PeerConnected is set on f below — registry is owned by f.
		})
	}
```

- [ ] **Step 3: Add the field to the `f := &Federation{…}` literal.**

```go
	f := &Federation{
		cfg: cfg, dep: dep,
		reg: reg, dedupe: dedupe, gossip: gossip,
		apply: apply, equiv: equiv, pub: pub,
		transfer: transfer, revocation: revocation, peerExchange: peerExchange,
		peers: make(map[ids.TrackerID]*Peer),
	}
```

- [ ] **Step 4: Patch PeerConnected after `f` is built.** Right after the existing `transfer.cfg.Send = func(...)` patch line:

```go
	if peerExchange != nil {
		peerExchange.cfg.PeerConnected = func(id ids.TrackerID) bool {
			return f.reg.IsActive(id)
		}
	}
```

- [ ] **Step 5: Allowlist seed.** Right after the existing `for _, p := range cfg.Peers { _ = reg.Add(...) }` loop, add:

```go
	if dep.KnownPeers != nil {
		seedCtx := context.Background()
		seenAt := dep.Now()
		for _, p := range cfg.Peers {
			tid := p.TrackerID.Bytes()
			_ = dep.KnownPeers.UpsertKnownPeer(seedCtx, storage.KnownPeer{
				TrackerID:   tid[:],
				Addr:        p.Addr,
				LastSeen:    seenAt,
				RegionHint:  p.Region,
				HealthScore: 0.5,
				Source:      "allowlist",
			})
		}
	}
```

Add the `"github.com/token-bay/token-bay/tracker/internal/ledger/storage"` import to `subsystem.go` if not present.

- [ ] **Step 6: Add the dispatcher case.** Inside `makeDispatcher`'s `switch env.Kind`, append after the `case fed.Kind_KIND_REVOCATION` block:

```go
		case fed.Kind_KIND_PEER_EXCHANGE:
			if f.peerExchange == nil {
				f.dep.Metrics.InvalidFrames("peer_exchange_disabled")
				f.dep.Metrics.PeerExchangeReceived("peer_exchange_disabled")
				return
			}
			var msg fed.PeerExchange
			if err := proto.Unmarshal(env.Payload, &msg); err != nil {
				f.dep.Metrics.InvalidFrames("peer_exchange_unmarshal")
				return
			}
			if err := fed.ValidatePeerExchange(&msg); err != nil {
				f.dep.Metrics.InvalidFrames("peer_exchange_shape")
				return
			}
			f.peerExchange.OnIncoming(context.Background(), env, &msg)
```

- [ ] **Step 7: Add the public method.** Append to `subsystem.go` (next to `PublishHour`):

```go
// PublishPeerExchange snapshots the local known_peers archive and
// broadcasts a KIND_PEER_EXCHANGE envelope to all active peers. v1
// ships the on-demand entry point only; production cadence is operator-
// driven (cron, admin endpoint, or future ticker driver in
// tracker/cmd/). Returns ErrPeerExchangeDisabled if the subsystem was
// opened without a KnownPeersArchive Dep.
func (f *Federation) PublishPeerExchange(ctx context.Context) error {
	if f == nil || f.peerExchange == nil {
		return ErrPeerExchangeDisabled
	}
	return f.peerExchange.EmitNow(ctx)
}
```

- [ ] **Step 8: Run the failing tests, expect pass.**

Run: `go test ./tracker/internal/federation/ -run 'TestFederation_PublishPeerExchange|TestPeerExchange_' -v`
Expected: PASS.

- [ ] **Step 9: Run full federation suite.**

Run: `go test -race ./tracker/internal/federation/...`
Expected: PASS.

- [ ] **Step 10: Commit.**

```bash
git add tracker/internal/federation/subsystem.go tracker/internal/federation/subsystem_test.go
git commit -m "feat(federation): wire peerExchangeCoordinator + Federation.PublishPeerExchange"
```

---

### Task 16: Two-tracker integration test (peer-exchange propagation)

**Files:**
- Modify: `tracker/internal/federation/integration_test.go`

- [ ] **Step 1: Write the failing test.** Append to `integration_test.go`:

```go
func TestIntegration_PeerExchange_AB(t *testing.T) {
	// A and B peer on the in-process transport. A's allowlist contains B
	// and a non-live entry T (no transport for T). After
	// aFed.PublishPeerExchange(ctx), B's known_peers table contains a
	// gossip-sourced row for T, and B's row for A remains 'allowlist'.

	tt := newTwoTracker(t, withKnownPeersStores(t))
	t.Cleanup(tt.Close)

	tID := bytes.Repeat([]byte{0xCC}, 32)
	require.NoError(t, tt.aStore.UpsertKnownPeer(context.Background(), storage.KnownPeer{
		TrackerID: tID, Addr: "wss://t-not-dialed:443",
		LastSeen: time.Unix(1714000000, 0), RegionHint: "asia",
		HealthScore: 0.5, Source: "allowlist",
	}))

	tt.WaitSteady(t)
	require.NoError(t, tt.a.PublishPeerExchange(context.Background()))

	// B should eventually have a 'gossip' row for T.
	require.Eventually(t, func() bool {
		got, ok, err := tt.bStore.GetKnownPeer(context.Background(), tID)
		if err != nil || !ok {
			return false
		}
		return got.Source == "gossip" && got.Addr == "wss://t-not-dialed:443"
	}, 2*time.Second, 25*time.Millisecond)

	// And B's row for A is allowlist-pinned.
	aBytes := tt.aID.Bytes()
	got, ok, err := tt.bStore.GetKnownPeer(context.Background(), aBytes[:])
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "allowlist", got.Source)
}
```

`newTwoTracker` and `withKnownPeersStores` are integration-test helpers. The existing `newTwoTracker` (used by slice-1/slice-2 integration tests) constructs A↔B with a real transport and exposes `tt.a`, `tt.b`, `tt.aID`, `tt.bID`, `tt.WaitSteady`, `tt.Close`. Extend it with `tt.aStore` and `tt.bStore` `*storage.Store` fields plumbed into `Deps.KnownPeers` when `withKnownPeersStores(t)` is supplied. Read the existing helper first; the slice-2 integration tests added a similar `withRevocationArchive` option — the slice-3 option is the same shape.

- [ ] **Step 2: Run, expect failure.**

Run: `go test ./tracker/internal/federation/ -run 'TestIntegration_PeerExchange_AB' -v`
Expected: FAIL until the helper is extended.

- [ ] **Step 3: Extend `newTwoTracker` with `withKnownPeersStores`.** In whatever file `newTwoTracker` lives (commonly `integration_test.go` itself), add:

```go
type twoTrackerOpt func(*twoTrackerCfg)

func withKnownPeersStores(t *testing.T) twoTrackerOpt {
	return func(c *twoTrackerCfg) {
		aS := openTestStorageStore(t)
		bS := openTestStorageStore(t)
		c.aKnownPeers = aS
		c.bKnownPeers = bS
		c.aStoreOut = aS
		c.bStoreOut = bS
	}
}
```

`openTestStorageStore(t)` opens a `*storage.Store` against a `t.TempDir()`-backed SQLite file (use the existing storage test helper if there is one, otherwise inline `storage.Open(ctx, filepath.Join(t.TempDir(), "ts.db"))`). Plumb `aKnownPeers`/`bKnownPeers` into the federation `Deps` for A and B respectively, and set `tt.aStore = aStoreOut`, `tt.bStore = bStoreOut`.

(Adapt to whatever construction pattern `newTwoTracker` already uses — keep changes minimal.)

- [ ] **Step 4: Re-run, expect pass.**

Run: `go test -race ./tracker/internal/federation/ -run 'TestIntegration_PeerExchange_AB' -v`
Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add tracker/internal/federation/integration_test.go
git commit -m "test(federation): two-tracker peer-exchange propagation"
```

---

### Task 17: Three-tracker integration test (line-graph forwarding)

**Files:**
- Modify: `tracker/internal/federation/integration_test.go`

- [ ] **Step 1: Write the failing test.** Append:

```go
func TestIntegration_PeerExchange_ThreeTracker_LineGraph(t *testing.T) {
	// A↔B↔C line topology, no direct A-C link.
	// A's allowlist contains B and a non-live entry D (no transport for D).
	// aFed.PublishPeerExchange propagates the entry for D through B to C.

	tt := newThreeTrackerLine(t, withThreeKnownPeersStores(t))
	t.Cleanup(tt.Close)

	dID := bytes.Repeat([]byte{0xDD}, 32)
	require.NoError(t, tt.aStore.UpsertKnownPeer(context.Background(), storage.KnownPeer{
		TrackerID: dID, Addr: "wss://d-not-dialed:443",
		LastSeen: time.Unix(1714000000, 0), RegionHint: "south",
		HealthScore: 0.5, Source: "allowlist",
	}))

	tt.WaitSteady(t)
	require.NoError(t, tt.a.PublishPeerExchange(context.Background()))
	require.NoError(t, tt.b.PublishPeerExchange(context.Background())) // forward to C

	require.Eventually(t, func() bool {
		got, ok, err := tt.cStore.GetKnownPeer(context.Background(), dID)
		if err != nil || !ok {
			return false
		}
		return got.Source == "gossip"
	}, 2*time.Second, 25*time.Millisecond)
}
```

`newThreeTrackerLine` is a sibling of slice-2's `newThreeTrackerLine` (the slice-2 integration suite added it). `withThreeKnownPeersStores` mirrors slice 3's two-tracker version: opens three `*storage.Store` instances and plumbs them into A, B, C `Deps.KnownPeers`. Read the slice-2 helper to crib the shape.

If the line-topology helper does NOT already exist (slice-2 may have inlined it inside one test rather than extracting), extract it. Keep the extraction tight — no unrelated refactoring.

- [ ] **Step 2: Run, expect failure (or pass if helper already extracted).**

Run: `go test ./tracker/internal/federation/ -run 'TestIntegration_PeerExchange_ThreeTracker_LineGraph' -v`

- [ ] **Step 3: Extend the helper if needed.** Same shape as Task 16's `withKnownPeersStores`, applied to A, B, C.

- [ ] **Step 4: Re-run, expect pass.**

Run: `go test -race ./tracker/internal/federation/ -run 'TestIntegration_PeerExchange_ThreeTracker_LineGraph' -v`
Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add tracker/internal/federation/integration_test.go
git commit -m "test(federation): three-tracker line-graph peer-exchange forwarding"
```

---

### Task 18: Wire `KnownPeers: store` in run_cmd.go

**Files:**
- Modify: `tracker/cmd/token-bay-tracker/run_cmd.go`

- [ ] **Step 1: Add the field to federation `Deps`.** In the `federation.Open(...)` call where `RevocationArchive: store` is set, append:

```go
				KnownPeers:        store, // *storage.Store satisfies KnownPeersArchive
```

- [ ] **Step 2: Build the cmd.**

Run: `go build ./tracker/cmd/token-bay-tracker/...`
Expected: success.

- [ ] **Step 3: Compile-check the whole tracker module.**

Run: `go build ./tracker/...`
Expected: success.

- [ ] **Step 4: Commit.**

```bash
git add tracker/cmd/token-bay-tracker/run_cmd.go
git commit -m "feat(tracker/cmd): wire federation Deps.KnownPeers from storage"
```

---

### Task 19: Final make check

**Files:** none (verification only)

- [ ] **Step 1: Race-clean federation suite.**

Run: `go test -race ./tracker/internal/federation/...`
Expected: PASS.

- [ ] **Step 2: Race-clean storage suite.**

Run: `go test -race ./tracker/internal/ledger/storage/...`
Expected: PASS.

- [ ] **Step 3: Repo-root make check.**

Run: `make check` from `/Users/dor.amid/.superset/worktrees/token-bay/tracker/federation`
Expected: PASS — both `make test` (race-clean across plugin/, tracker/, shared/) and `make lint` (golangci-lint clean) green.

- [ ] **Step 4: Hand off to `superpowers:finishing-a-development-branch`.** Per the executing-plans skill, after all tasks are completed and verified, that skill walks through tests-pass-verification → present 4 options → user picks (push and create a PR, etc.).

No commit on this task; verification only.

---

## End of plan

Plan saved to `docs/superpowers/plans/2026-05-10-federation-peer-exchange.md`.
