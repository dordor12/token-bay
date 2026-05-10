# Federation — Peer Health Score Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement federation §7.3 peer health-score computation: a new `*PeerHealth` value owned by the federation subsystem combines three real signals (uptime since last `KIND_ROOT_ATTESTATION`, average revocation-gossip delay for revocations issued by the peer, sticky equivocation flag) into a `0..1` score that replaces the slice-3 placeholder logic in `peerexchange.go`. The freshly-computed score is written through to `known_peers.health_score` at peer-exchange emit time, where slice-4's bootstrap-list endpoint already reads it. Latency (§7.3 signal #2) is deferred to a follow-up slice.

**Architecture:** All new state lives in a single new file `tracker/internal/federation/health.go`, guarded by one `sync.Mutex`. No goroutines. Three Observe* methods are O(1); `Score` is O(buffer-size)=O(16). Wiring touches four existing call sites (ROOT_ATTESTATION receive, revocation OnIncoming, equivocator both methods, peerexchange EmitNow) — each via a small callback so `health.go` stays the only place that imports the new type. Storage gets one new `UpdateKnownPeerHealth(ctx, trackerID, score)` method on `*storage.Store`; the `KnownPeersArchive` interface widens to match.

**Tech Stack:** Go 1.23+, `modernc.org/sqlite` (existing), `crypto/ed25519`, `prometheus/client_golang`, stdlib `sync`, `testify`.

**Spec:** `docs/superpowers/specs/federation/2026-05-10-federation-peer-health-design.md` (commits `4811605` + `c560c68` on this branch).

---

## File Structure

| Path | Action | Responsibility |
|---|---|---|
| `tracker/internal/federation/health.go` | CREATE | `PeerHealth` type, `NewPeerHealth`, `OnRootAttestation`, `OnRevocation`, `OnEquivocation`, `Score`, internal `ringBuf16` |
| `tracker/internal/federation/health_test.go` | CREATE | Unit tests for each signal + combination + concurrent-safety under `-race` |
| `tracker/internal/ledger/storage/known_peers.go` | MODIFY | Add `(*Store).UpdateKnownPeerHealth(ctx, trackerID, score)` |
| `tracker/internal/ledger/storage/known_peers_test.go` | MODIFY | Test for `UpdateKnownPeerHealth`: updates row, no-op on missing, no other column touched |
| `tracker/internal/federation/known_peers_archive.go` | MODIFY | Widen `KnownPeersArchive` interface with `UpdateKnownPeerHealth` |
| `tracker/internal/federation/peerexchange.go` | MODIFY | Replace placeholder block at lines 66-75 with `Health.Score` + `UpdateKnownPeerHealth` write-through; drop `PeerConnected` callback; new `Health *PeerHealth` field on cfg |
| `tracker/internal/federation/peerexchange_test.go` | MODIFY | Replace `PeerConnected` plumbing in tests with synthetic `*PeerHealth`; assert `UpdateKnownPeerHealth` is called per row |
| `tracker/internal/federation/revocation.go` | MODIFY | Add `OnRevocationObserved func(issuer ids.TrackerID, revokedAt, recvAt time.Time)` callback in `revocationCoordinatorCfg`; fire after sig+archive verify |
| `tracker/internal/federation/revocation_test.go` | MODIFY | Assert callback fires on archived inbound; not fired on bad sig / shape |
| `tracker/internal/federation/equivocation.go` | MODIFY | Add `onFlag func(ids.TrackerID)` parameter to `NewEquivocator`; fire from both `OnLocalConflict` and `OnIncomingEvidence`; thread through `WithSelf` |
| `tracker/internal/federation/equivocation_test.go` | MODIFY | Assert callback fires for offender on local conflict and on incoming evidence; not for self-evidence |
| `tracker/internal/federation/subsystem.go` | MODIFY | Construct `*PeerHealth`, store on `*Federation`, wire all four hooks, drop `PeerConnected` closure, pass `health.OnEquivocation` to `NewEquivocator` and `health.OnRevocation` into `revocationCoordinatorCfg` |
| `tracker/internal/federation/integration_test.go` | MODIFY | Extend two-peer scenario: assert peer B's persisted `health_score` after t=2h with no ROOT_ATTESTATION drops; revocation issued by A drops A's revgoss subscore; equivocation flips offender's score to 0 on next emit |
| `tracker/internal/federation/metrics.go` | MODIFY | New `health_score_computations_total{outcome}` counter (outcomes: `ok`, `equivocated`, `no_data`); new `Health(outcome)` accessor |
| `tracker/internal/federation/metrics_test.go` | MODIFY | Cover the new counter |
| `tracker/internal/config/config.go` | MODIFY | Add `HealthConfig` sub-struct; embed in `FederationConfig`; populate in `DefaultConfig` |
| `tracker/internal/config/apply_defaults.go` | MODIFY | Per-field zero-fill stanzas for `Health.*` |
| `tracker/internal/config/validate.go` | MODIFY | Range checks for the five `Health.*` fields including weight-sum invariant |
| `tracker/internal/config/validate_test.go` | MODIFY | Reject out-of-range Health values |
| `tracker/internal/config/testdata/full.yaml` | MODIFY | Explicit values for new keys (round-trip golden) |
| `tracker/internal/config/apply_defaults_test.go` | MODIFY | Verify defaults are applied when `Health` block is absent |

No CLI changes (`run_cmd.go`, `federation_adapters.go`) needed: `HealthConfig` is consumed entirely inside `tracker/internal/federation` and read by `subsystem.go` from the existing `FederationConfig` it already receives.

---

## Notes for the executor

- Read the spec (`docs/superpowers/specs/federation/2026-05-10-federation-peer-health-design.md`) before Task 1.
- One conventional commit per red-green cycle (`feat:`, `fix:`, `test:`, `refactor:`, `docs:`, `chore:`). Don't squash.
- Working directory: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/federation`.
- Test command pattern: `go test -race ./tracker/internal/federation/... -run <Pattern>`. `-race` is non-negotiable in this package.
- The signed `Revocation` proto field is `revoked_at` (not `signed_at`). The plugin-side error message in slice 5 uses `revoked_at` consistently.
- `NewEquivocator` currently takes `(arch, fwd, reg)`; `WithSelf` allocates a fresh struct because `atomic.Int64` is not safe to copy. The new `onFlag` field MUST be propagated by `WithSelf` — Task 10 has the full updated function.
- The new SQLite write happens once per peer per peer-exchange emit (single-digit count, low cadence). No batching needed.
- `tracker/internal/config/CLAUDE.md` requires (a) updating `testdata/full.yaml` with explicit values for every new field, (b) one `FieldError` per invariant. Task 12 follows this pattern.
- `peerexchange.go` test `TestPeerExchange_EmitNow_BuildsPeerExchangeFromArchive` currently asserts hardcoded 0.5/1.0 values. Task 8 rewrites those assertions to be driven by an injected `*PeerHealth`. The `TestPeerExchange_EmitNow_AllowlistDisconnectedScores0_5` test name and assertion both need to change in lockstep — see Task 8 step 3.

---

## Task 1: PeerHealth scaffold + uptime sub-score

**Files:**
- Create: `tracker/internal/federation/health.go`
- Test: `tracker/internal/federation/health_test.go`

- [ ] **Step 1: Write the failing test for uptime sub-score**

```go
// tracker/internal/federation/health_test.go
package federation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/token-bay/token-bay/shared/ids"
)

func TestPeerHealth_UptimeSubScore(t *testing.T) {
	now := time.Unix(10000, 0)
	clock := now
	h := NewPeerHealth(HealthConfig{
		UptimeWindow:        2 * time.Hour,
		RevGossipWindow:     600 * time.Second,
		RevGossipBufferSize: 16,
		UptimeWeight:        1.0,
		RevGossipWeight:     0.0,
	}, func() time.Time { return clock }, nil)

	var p ids.TrackerID
	p[0] = 0xAA

	// Never seen → 0.
	require.InDelta(t, 0.0, h.Score(p, now), 0.0001)

	// Just attested → 1.0 (full uptime, full weight).
	h.OnRootAttestation(p, now)
	require.InDelta(t, 1.0, h.Score(p, now), 0.0001)

	// 1h later → 0.5.
	require.InDelta(t, 0.5, h.Score(p, now.Add(1*time.Hour)), 0.0001)

	// 2h later → 0.0 (clamped at zero).
	require.InDelta(t, 0.0, h.Score(p, now.Add(2*time.Hour)), 0.0001)

	// 3h later → still 0.0 (clamped).
	require.InDelta(t, 0.0, h.Score(p, now.Add(3*time.Hour)), 0.0001)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -race ./tracker/internal/federation/... -run TestPeerHealth_UptimeSubScore -v`
Expected: FAIL with `undefined: NewPeerHealth` / `undefined: HealthConfig`.

- [ ] **Step 3: Write minimal implementation**

```go
// tracker/internal/federation/health.go
package federation

import (
	"sync"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
)

// HealthConfig parameterizes the score formula; see spec §4.1 / §8.
// Defaults are filled by tracker/internal/config; this package does not
// fall back to defaults itself.
type HealthConfig struct {
	UptimeWindow        time.Duration
	RevGossipWindow     time.Duration
	RevGossipBufferSize int
	UptimeWeight        float64
	RevGossipWeight     float64
}

// PeerHealth tracks per-peer health signals and computes a 0..1 score
// on demand. All maps are guarded by mu; no background goroutines.
type PeerHealth struct {
	cfg        HealthConfig
	now        func() time.Time
	onComputed func(outcome string)

	mu              sync.Mutex
	lastRoot        map[ids.TrackerID]time.Time
	revGossipDelays map[ids.TrackerID]*ringBuf16
	equivocated     map[ids.TrackerID]struct{}
}

// NewPeerHealth returns a fresh PeerHealth. now must not be nil.
// onComputed is fired by Score with the outcome label (one of
// "ok", "equivocated", "no_data"). May be nil; Task 12 wires it
// to the Prometheus counter.
func NewPeerHealth(cfg HealthConfig, now func() time.Time, onComputed func(outcome string)) *PeerHealth {
	if onComputed == nil {
		onComputed = func(string) {}
	}
	return &PeerHealth{
		cfg:             cfg,
		now:             now,
		onComputed:      onComputed,
		lastRoot:        make(map[ids.TrackerID]time.Time),
		revGossipDelays: make(map[ids.TrackerID]*ringBuf16),
		equivocated:     make(map[ids.TrackerID]struct{}),
	}
}

// OnRootAttestation records receipt time of the most recent successful
// KIND_ROOT_ATTESTATION from peer.
func (h *PeerHealth) OnRootAttestation(peer ids.TrackerID, recvAt time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastRoot[peer] = recvAt
}

// Score computes the current health score for peer in [0, 1].
// See spec §4.1 for the formula.
func (h *PeerHealth) Score(peer ids.TrackerID, now time.Time) float64 {
	h.mu.Lock()
	defer h.mu.Unlock()

	uptimeSub := 0.0
	if last, ok := h.lastRoot[peer]; ok {
		age := now.Sub(last)
		if age < 0 {
			age = 0
		}
		frac := 1.0 - float64(age)/float64(h.cfg.UptimeWindow)
		uptimeSub = clamp01(frac)
	}

	// Revgoss + equiv come in later tasks; for Task 1 they are zero
	// and the weight on revgoss is also zero in this test.
	return h.cfg.UptimeWeight*uptimeSub + h.cfg.RevGossipWeight*0.0
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// ringBuf16 is a fixed-capacity ring buffer used by Task 2 for
// revocation-gossip delays. Defined here because health.go owns it.
type ringBuf16 struct {
	xs   [16]time.Duration
	n    int  // count of valid samples (≤ 16)
	head int  // next write position
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -race ./tracker/internal/federation/... -run TestPeerHealth_UptimeSubScore -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/federation/health.go tracker/internal/federation/health_test.go
git commit -m "feat(federation): peer-health uptime sub-score scaffold"
```

---

## Task 2: PeerHealth revgossip sub-score

**Files:**
- Modify: `tracker/internal/federation/health.go`
- Test: `tracker/internal/federation/health_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `tracker/internal/federation/health_test.go`:

```go
func TestPeerHealth_RevGossipSubScore_Empty(t *testing.T) {
	now := time.Unix(10000, 0)
	h := NewPeerHealth(HealthConfig{
		UptimeWindow: 2 * time.Hour, RevGossipWindow: 600 * time.Second,
		RevGossipBufferSize: 16,
		UptimeWeight:        0.0, RevGossipWeight: 1.0,
	}, func() time.Time { return now }, nil)

	var p ids.TrackerID
	p[0] = 0xBB

	// No samples → revgoss=1.0 (neutral).
	require.InDelta(t, 1.0, h.Score(p, now), 0.0001)
}

func TestPeerHealth_RevGossipSubScore_Samples(t *testing.T) {
	now := time.Unix(10000, 0)
	h := NewPeerHealth(HealthConfig{
		UptimeWindow: 2 * time.Hour, RevGossipWindow: 600 * time.Second,
		RevGossipBufferSize: 16,
		UptimeWeight:        0.0, RevGossipWeight: 1.0,
	}, func() time.Time { return now }, nil)

	var p ids.TrackerID
	p[0] = 0xBB

	// One sample at 0s delay → revgoss=1.0.
	h.OnRevocation(p, now, now)
	require.InDelta(t, 1.0, h.Score(p, now), 0.0001)

	// Add a sample at 600s delay → average=300s → revgoss=0.5.
	h.OnRevocation(p, now.Add(-600*time.Second), now)
	require.InDelta(t, 0.5, h.Score(p, now), 0.0001)

	// Add a sample beyond the window → still averaged in; mean=400 → 1-400/600=0.333.
	h.OnRevocation(p, now.Add(-600*time.Second), now)
	require.InDelta(t, 1.0-400.0/600.0, h.Score(p, now), 0.0001)
}

func TestPeerHealth_RevGossipSubScore_RingWrap(t *testing.T) {
	now := time.Unix(10000, 0)
	h := NewPeerHealth(HealthConfig{
		UptimeWindow: 2 * time.Hour, RevGossipWindow: 600 * time.Second,
		RevGossipBufferSize: 16,
		UptimeWeight:        0.0, RevGossipWeight: 1.0,
	}, func() time.Time { return now }, nil)

	var p ids.TrackerID
	p[0] = 0xCC

	// Push 16 samples at 0s delay (revgoss=1.0).
	for i := 0; i < 16; i++ {
		h.OnRevocation(p, now, now)
	}
	require.InDelta(t, 1.0, h.Score(p, now), 0.0001)

	// Now push 16 samples at 600s delay; ring is full, oldest 0s
	// samples are evicted; mean=600s; revgoss=0.0.
	for i := 0; i < 16; i++ {
		h.OnRevocation(p, now.Add(-600*time.Second), now)
	}
	require.InDelta(t, 0.0, h.Score(p, now), 0.0001)
}

func TestPeerHealth_RevGossipSubScore_NegativeDelayClampedToZero(t *testing.T) {
	now := time.Unix(10000, 0)
	h := NewPeerHealth(HealthConfig{
		UptimeWindow: 2 * time.Hour, RevGossipWindow: 600 * time.Second,
		RevGossipBufferSize: 16,
		UptimeWeight:        0.0, RevGossipWeight: 1.0,
	}, func() time.Time { return now }, nil)
	var p ids.TrackerID
	p[0] = 0xDD

	// revoked_at in the future → negative delay treated as 0 → revgoss=1.0.
	h.OnRevocation(p, now.Add(60*time.Second), now)
	require.InDelta(t, 1.0, h.Score(p, now), 0.0001)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -race ./tracker/internal/federation/... -run TestPeerHealth_RevGossipSubScore -v`
Expected: FAIL with `undefined: (*PeerHealth).OnRevocation`.

- [ ] **Step 3: Add `OnRevocation` and revgoss term to `Score`**

Append `OnRevocation` to `tracker/internal/federation/health.go` and update `Score` to consume the ring buffer.

```go
// OnRevocation records a revocation issued by peer with the given
// revoked_at, observed at recvAt. Negative deltas are clamped to zero.
func (h *PeerHealth) OnRevocation(peer ids.TrackerID, revokedAt, recvAt time.Time) {
	delay := recvAt.Sub(revokedAt)
	if delay < 0 {
		delay = 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	rb, ok := h.revGossipDelays[peer]
	if !ok {
		rb = &ringBuf16{}
		h.revGossipDelays[peer] = rb
	}
	rb.push(delay)
}

func (rb *ringBuf16) push(d time.Duration) {
	rb.xs[rb.head] = d
	rb.head = (rb.head + 1) % len(rb.xs)
	if rb.n < len(rb.xs) {
		rb.n++
	}
}

func (rb *ringBuf16) mean() time.Duration {
	if rb == nil || rb.n == 0 {
		return 0
	}
	var sum time.Duration
	for i := 0; i < rb.n; i++ {
		sum += rb.xs[i]
	}
	return sum / time.Duration(rb.n)
}
```

Replace the body of `Score` (in the same file) with:

```go
func (h *PeerHealth) Score(peer ids.TrackerID, now time.Time) float64 {
	h.mu.Lock()
	defer h.mu.Unlock()

	uptimeSub := 0.0
	if last, ok := h.lastRoot[peer]; ok {
		age := now.Sub(last)
		if age < 0 {
			age = 0
		}
		frac := 1.0 - float64(age)/float64(h.cfg.UptimeWindow)
		uptimeSub = clamp01(frac)
	}

	revgossSub := 1.0 // empty ring → neutral
	if rb, ok := h.revGossipDelays[peer]; ok && rb.n > 0 {
		mean := rb.mean()
		frac := 1.0 - float64(mean)/float64(h.cfg.RevGossipWindow)
		revgossSub = clamp01(frac)
	}

	return h.cfg.UptimeWeight*uptimeSub + h.cfg.RevGossipWeight*revgossSub
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./tracker/internal/federation/... -run TestPeerHealth_ -v`
Expected: PASS (uptime + revgoss tests).

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/federation/health.go tracker/internal/federation/health_test.go
git commit -m "feat(federation): peer-health revocation-gossip sub-score"
```

---

## Task 3: PeerHealth equivocation gate

**Files:**
- Modify: `tracker/internal/federation/health.go`
- Test: `tracker/internal/federation/health_test.go`

- [ ] **Step 1: Write the failing test**

Append:

```go
func TestPeerHealth_EquivocationGate(t *testing.T) {
	now := time.Unix(10000, 0)
	h := NewPeerHealth(HealthConfig{
		UptimeWindow: 2 * time.Hour, RevGossipWindow: 600 * time.Second,
		RevGossipBufferSize: 16,
		UptimeWeight:        0.7, RevGossipWeight: 0.3,
	}, func() time.Time { return now }, nil)

	var p ids.TrackerID
	p[0] = 0xEE

	// Max signals + no equiv → 1.0.
	h.OnRootAttestation(p, now)
	require.InDelta(t, 0.7+0.3, h.Score(p, now), 0.0001)

	// Flag equivocation → score forced to 0 even with max signals.
	h.OnEquivocation(p)
	require.InDelta(t, 0.0, h.Score(p, now), 0.0001)

	// Idempotent: re-flag is fine.
	h.OnEquivocation(p)
	require.InDelta(t, 0.0, h.Score(p, now), 0.0001)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -race ./tracker/internal/federation/... -run TestPeerHealth_EquivocationGate -v`
Expected: FAIL with `undefined: (*PeerHealth).OnEquivocation`.

- [ ] **Step 3: Implement equivocation gate**

Append to `tracker/internal/federation/health.go`:

```go
// OnEquivocation flags peer as equivocating. Idempotent. The flag is
// sticky for the life of the process — there is no admin clear path
// in this slice (spec §3, §4.2).
func (h *PeerHealth) OnEquivocation(peer ids.TrackerID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.equivocated[peer] = struct{}{}
}
```

Update `Score` to short-circuit on the flag:

```go
func (h *PeerHealth) Score(peer ids.TrackerID, now time.Time) float64 {
	h.mu.Lock()
	defer h.mu.Unlock()

	if _, equiv := h.equivocated[peer]; equiv {
		return 0
	}

	uptimeSub := 0.0
	if last, ok := h.lastRoot[peer]; ok {
		age := now.Sub(last)
		if age < 0 {
			age = 0
		}
		frac := 1.0 - float64(age)/float64(h.cfg.UptimeWindow)
		uptimeSub = clamp01(frac)
	}

	revgossSub := 1.0
	if rb, ok := h.revGossipDelays[peer]; ok && rb.n > 0 {
		mean := rb.mean()
		frac := 1.0 - float64(mean)/float64(h.cfg.RevGossipWindow)
		revgossSub = clamp01(frac)
	}

	return h.cfg.UptimeWeight*uptimeSub + h.cfg.RevGossipWeight*revgossSub
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./tracker/internal/federation/... -run TestPeerHealth_ -v`
Expected: PASS (all three Task 1-3 tests).

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/federation/health.go tracker/internal/federation/health_test.go
git commit -m "feat(federation): peer-health sticky equivocation gate"
```

---

## Task 4: PeerHealth concurrent safety

**Files:**
- Test: `tracker/internal/federation/health_test.go`

- [ ] **Step 1: Write the failing test (must run under -race to catch issues)**

Append:

```go
func TestPeerHealth_Concurrent(t *testing.T) {
	now := time.Unix(10000, 0)
	h := NewPeerHealth(HealthConfig{
		UptimeWindow: 2 * time.Hour, RevGossipWindow: 600 * time.Second,
		RevGossipBufferSize: 16,
		UptimeWeight:        0.7, RevGossipWeight: 0.3,
	}, func() time.Time { return now }, nil)

	const peers = 8
	const iters = 200

	var wg sync.WaitGroup
	for w := 0; w < peers; w++ {
		var p ids.TrackerID
		p[0] = byte(w)
		wg.Add(3)
		// Observe ROOT_ATTESTATION concurrently.
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				h.OnRootAttestation(p, now)
			}
		}()
		// Observe revocations concurrently.
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				h.OnRevocation(p, now.Add(-30*time.Second), now)
			}
		}()
		// Read concurrently.
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				_ = h.Score(p, now)
			}
		}()
	}
	wg.Wait()
}
```

Add `"sync"` to the test file's import list if not already present.

- [ ] **Step 2: Run with race detector**

Run: `go test -race ./tracker/internal/federation/... -run TestPeerHealth_Concurrent -v`
Expected: PASS, no race warnings.

- [ ] **Step 3: Commit**

```bash
git add tracker/internal/federation/health_test.go
git commit -m "test(federation): peer-health concurrent observe + score"
```

---

## Task 5: storage.UpdateKnownPeerHealth

**Files:**
- Modify: `tracker/internal/ledger/storage/known_peers.go`
- Test: `tracker/internal/ledger/storage/known_peers_test.go`

- [ ] **Step 1: Write the failing test**

Append to `tracker/internal/ledger/storage/known_peers_test.go`:

```go
func TestUpdateKnownPeerHealth_UpdatesOnlyHealthScore(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	id := bytes.Repeat([]byte{0xAA}, 32)
	require.NoError(t, s.UpsertKnownPeer(ctx, KnownPeer{
		TrackerID: id, Addr: "wss://a:443", LastSeen: time.Unix(100, 0),
		RegionHint: "eu", HealthScore: 0.5, Source: "allowlist",
	}))

	require.NoError(t, s.UpdateKnownPeerHealth(ctx, id, 0.9))

	got, ok, err := s.GetKnownPeer(ctx, id)
	require.NoError(t, err)
	require.True(t, ok)
	require.InDelta(t, 0.9, got.HealthScore, 0.0001)
	require.Equal(t, "wss://a:443", got.Addr)
	require.Equal(t, "eu", got.RegionHint)
	require.Equal(t, "allowlist", got.Source)
	require.Equal(t, time.Unix(100, 0), got.LastSeen)
}

func TestUpdateKnownPeerHealth_NoOpOnMissingRow(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	require.NoError(t, s.UpdateKnownPeerHealth(ctx, bytes.Repeat([]byte{0xFF}, 32), 0.42))
	_, ok, err := s.GetKnownPeer(ctx, bytes.Repeat([]byte{0xFF}, 32))
	require.NoError(t, err)
	require.False(t, ok)
}
```

If `newTestStore` doesn't exist in this test file, copy the helper used by the existing `TestUpsertKnownPeer*` tests (it should be a `t.TempDir()` + `Open` pattern).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -race ./tracker/internal/ledger/storage/... -run TestUpdateKnownPeerHealth -v`
Expected: FAIL with `undefined: (*Store).UpdateKnownPeerHealth`.

- [ ] **Step 3: Implement the method**

Append to `tracker/internal/ledger/storage/known_peers.go`:

```go
// UpdateKnownPeerHealth updates only the health_score column for the
// given tracker_id. No-op (returns nil) if the row does not exist.
// Does not touch last_seen, region_hint, or source. Used by federation
// peer-exchange to write back a freshly computed score without the
// "I just observed this peer" semantics of UpsertKnownPeer.
func (s *Store) UpdateKnownPeerHealth(ctx context.Context, trackerID []byte, score float64) error {
	if len(trackerID) == 0 {
		return errors.New("storage: UpdateKnownPeerHealth requires non-empty tracker_id")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.db.ExecContext(ctx,
		`UPDATE known_peers SET health_score = ? WHERE tracker_id = ?`,
		score, trackerID,
	); err != nil {
		return fmt.Errorf("storage: UpdateKnownPeerHealth: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./tracker/internal/ledger/storage/... -run TestUpdateKnownPeerHealth -v`
Expected: PASS (both tests).

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/ledger/storage/known_peers.go tracker/internal/ledger/storage/known_peers_test.go
git commit -m "feat(storage): UpdateKnownPeerHealth for peer-health write-through"
```

---

## Task 6: Widen KnownPeersArchive interface

**Files:**
- Modify: `tracker/internal/federation/known_peers_archive.go`
- Modify (compile-only): `tracker/internal/federation/peerexchange_test.go` (the `fakeKnownPeersArchive` must add the new method to keep satisfying the interface)

- [ ] **Step 1: Add the method to the interface**

```go
// tracker/internal/federation/known_peers_archive.go
package federation

import (
	"context"

	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
)

// KnownPeersArchive is the slice of *storage.Store federation needs for
// peer-exchange gossip and slice-5 peer-health write-through.
// *storage.Store satisfies it via Go structural typing.
type KnownPeersArchive interface {
	UpsertKnownPeer(ctx context.Context, p storage.KnownPeer) error
	GetKnownPeer(ctx context.Context, trackerID []byte) (storage.KnownPeer, bool, error)
	ListKnownPeers(ctx context.Context, limit int, byHealthDesc bool) ([]storage.KnownPeer, error)
	UpdateKnownPeerHealth(ctx context.Context, trackerID []byte, score float64) error
}
```

- [ ] **Step 2: Stub the method on the test fake (compile-only update)**

Append to `tracker/internal/federation/peerexchange_test.go` immediately after the existing `fakeKnownPeersArchive.ListKnownPeers` method:

```go
func (f *fakeKnownPeersArchive) UpdateKnownPeerHealth(_ context.Context, trackerID []byte, score float64) error {
	for i, r := range f.rows {
		if bytes.Equal(r.TrackerID, trackerID) {
			f.rows[i].HealthScore = score
			return nil
		}
	}
	return nil // no-op on missing row
}
```

- [ ] **Step 3: Verify compilation**

Run: `go build ./tracker/...`
Expected: build succeeds. `*storage.Store` and `*fakeKnownPeersArchive` both satisfy the widened interface.

- [ ] **Step 4: Commit**

```bash
git add tracker/internal/federation/known_peers_archive.go tracker/internal/federation/peerexchange_test.go
git commit -m "refactor(federation): widen KnownPeersArchive with UpdateKnownPeerHealth"
```

---

## Task 7: HealthConfig in tracker/internal/config

**Files:**
- Modify: `tracker/internal/config/config.go`
- Modify: `tracker/internal/config/apply_defaults.go`
- Modify: `tracker/internal/config/validate.go`
- Modify: `tracker/internal/config/validate_test.go`
- Modify: `tracker/internal/config/apply_defaults_test.go`
- Modify: `tracker/internal/config/testdata/full.yaml`

- [ ] **Step 1: Write the failing validate tests**

Append to `tracker/internal/config/validate_test.go`:

```go
func TestValidate_FederationHealth_RangeChecks(t *testing.T) {
	cases := []struct {
		name string
		mut  func(c *Config)
		err  string
	}{
		{"uptime_window_zero", func(c *Config) { c.Federation.Health.UptimeWindowS = 0 }, "must be > 0"},
		{"revgoss_window_zero", func(c *Config) { c.Federation.Health.RevGossipWindowS = 0 }, "must be > 0"},
		{"buffer_zero", func(c *Config) { c.Federation.Health.RevGossipBufferSize = 0 }, "must be 1..256"},
		{"buffer_too_big", func(c *Config) { c.Federation.Health.RevGossipBufferSize = 257 }, "must be 1..256"},
		{"uptime_weight_negative", func(c *Config) { c.Federation.Health.UptimeWeight = -0.1 }, "must be in [0,1]"},
		{"revgoss_weight_above_one", func(c *Config) { c.Federation.Health.RevGossipWeight = 1.1 }, "must be in [0,1]"},
		{"weights_dont_sum_to_one", func(c *Config) {
			c.Federation.Health.UptimeWeight = 0.5
			c.Federation.Health.RevGossipWeight = 0.4
		}, "must sum to 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig(t)
			tc.mut(c)
			err := Validate(c)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.err)
		})
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./tracker/internal/config/... -run TestValidate_FederationHealth -v`
Expected: FAIL with `Health undefined` or similar.

- [ ] **Step 3: Add the type and the embed**

Edit `tracker/internal/config/config.go`. Append a new sub-struct after `FederationBootstrapConfig`:

```go
// FederationHealthConfig governs the §7.3 peer-health-score
// computation: signal windows, ring-buffer cap, and the two weights
// (which must sum to 1.0 with float-epsilon tolerance).
type FederationHealthConfig struct {
	UptimeWindowS       int     `yaml:"uptime_window_s"`        // > 0; default 7200 (2h)
	RevGossipWindowS    int     `yaml:"rev_gossip_window_s"`    // > 0; default 600 (10min)
	RevGossipBufferSize int     `yaml:"rev_gossip_buffer_size"` // [1, 256]; default 16
	UptimeWeight        float64 `yaml:"uptime_weight"`          // [0,1]; default 0.7
	RevGossipWeight     float64 `yaml:"rev_gossip_weight"`      // [0,1]; default 0.3
}
```

Edit the `FederationConfig` struct, adding the new field at the end (just after `Bootstrap`):

```go
type FederationConfig struct {
	// ...existing fields up to Bootstrap unchanged...
	Bootstrap FederationBootstrapConfig `yaml:"bootstrap"`
	Health    FederationHealthConfig    `yaml:"health"`
}
```

Edit `DefaultConfig` to populate the new sub-struct:

```go
Federation: FederationConfig{
	// ...existing fields unchanged...
	Bootstrap: FederationBootstrapConfig{
		MaxPeers:   50,
		TTLSeconds: 600,
	},
	Health: FederationHealthConfig{
		UptimeWindowS:       7200,
		RevGossipWindowS:    600,
		RevGossipBufferSize: 16,
		UptimeWeight:        0.7,
		RevGossipWeight:     0.3,
	},
},
```

- [ ] **Step 4: Add per-field default-fill stanzas**

Edit `tracker/internal/config/apply_defaults.go`. After the `Bootstrap.TTLSeconds` block (line 131-133 area):

```go
if c.Federation.Health.UptimeWindowS == 0 {
	c.Federation.Health.UptimeWindowS = d.Federation.Health.UptimeWindowS
}
if c.Federation.Health.RevGossipWindowS == 0 {
	c.Federation.Health.RevGossipWindowS = d.Federation.Health.RevGossipWindowS
}
if c.Federation.Health.RevGossipBufferSize == 0 {
	c.Federation.Health.RevGossipBufferSize = d.Federation.Health.RevGossipBufferSize
}
// Weights: a zero-zero pair means "no operator override" → fill from
// defaults. A partial override is left alone so Validate's sum check
// fires loudly. Same idiom as BrokerScoreWeights.
if c.Federation.Health.UptimeWeight == 0 && c.Federation.Health.RevGossipWeight == 0 {
	c.Federation.Health.UptimeWeight = d.Federation.Health.UptimeWeight
	c.Federation.Health.RevGossipWeight = d.Federation.Health.RevGossipWeight
}
```

- [ ] **Step 5: Add range checks**

Edit `tracker/internal/config/validate.go`. After the existing `Federation.Bootstrap.TTLSeconds` check (line ~282), append a new section:

```go
if c.Federation.Health.UptimeWindowS <= 0 {
	v.add("federation.health.uptime_window_s",
		"must be > 0, got "+strconv.Itoa(c.Federation.Health.UptimeWindowS))
}
if c.Federation.Health.RevGossipWindowS <= 0 {
	v.add("federation.health.rev_gossip_window_s",
		"must be > 0, got "+strconv.Itoa(c.Federation.Health.RevGossipWindowS))
}
if c.Federation.Health.RevGossipBufferSize < 1 || c.Federation.Health.RevGossipBufferSize > 256 {
	v.add("federation.health.rev_gossip_buffer_size",
		"must be 1..256, got "+strconv.Itoa(c.Federation.Health.RevGossipBufferSize))
}
if c.Federation.Health.UptimeWeight < 0 || c.Federation.Health.UptimeWeight > 1 {
	v.add("federation.health.uptime_weight",
		"must be in [0,1]")
}
if c.Federation.Health.RevGossipWeight < 0 || c.Federation.Health.RevGossipWeight > 1 {
	v.add("federation.health.rev_gossip_weight",
		"must be in [0,1]")
}
sum := c.Federation.Health.UptimeWeight + c.Federation.Health.RevGossipWeight
if math.Abs(sum-1.0) > 1e-9 {
	v.add("federation.health",
		"uptime_weight + rev_gossip_weight must sum to 1.0")
}
```

Add `"math"` to the imports of `validate.go` if it isn't already there.

- [ ] **Step 6: Update testdata/full.yaml**

Add to `tracker/internal/config/testdata/full.yaml` under the `federation:` section, after the existing `bootstrap:` block:

```yaml
  health:
    uptime_window_s: 7200
    rev_gossip_window_s: 600
    rev_gossip_buffer_size: 16
    uptime_weight: 0.7
    rev_gossip_weight: 0.3
```

- [ ] **Step 7: Add a defaults test**

Append to `tracker/internal/config/apply_defaults_test.go`:

```go
func TestApplyDefaults_FederationHealth_Empty(t *testing.T) {
	c := &Config{}
	ApplyDefaults(c)
	require.Equal(t, 7200, c.Federation.Health.UptimeWindowS)
	require.Equal(t, 600, c.Federation.Health.RevGossipWindowS)
	require.Equal(t, 16, c.Federation.Health.RevGossipBufferSize)
	require.InDelta(t, 0.7, c.Federation.Health.UptimeWeight, 1e-9)
	require.InDelta(t, 0.3, c.Federation.Health.RevGossipWeight, 1e-9)
}
```

- [ ] **Step 8: Run all config tests**

Run: `go test -race ./tracker/internal/config/...`
Expected: PASS, including the new range checks and defaults test.

- [ ] **Step 9: Commit**

```bash
git add tracker/internal/config/config.go \
        tracker/internal/config/apply_defaults.go \
        tracker/internal/config/apply_defaults_test.go \
        tracker/internal/config/validate.go \
        tracker/internal/config/validate_test.go \
        tracker/internal/config/testdata/full.yaml
git commit -m "feat(config): FederationHealthConfig with defaults + range checks"
```

---

## Task 8: PeerExchangeCoordinator integrates Health.Score

**Files:**
- Modify: `tracker/internal/federation/peerexchange.go`
- Modify: `tracker/internal/federation/peerexchange_test.go`

- [ ] **Step 1: Update existing tests to drive scoring via injected *PeerHealth**

Edit `tracker/internal/federation/peerexchange_test.go`. Rewrite `TestPeerExchange_EmitNow_BuildsPeerExchangeFromArchive` so the `peerExchangeCoordinatorCfg` it constructs no longer carries `PeerConnected` and instead carries a fresh `*PeerHealth` configured with `UptimeWeight=1.0`, `RevGossipWeight=0.0`. Pre-populate the health for `bID` so its score will be 1.0; leave `cID` unobserved so its score will be 0.0. The assertions become "bID score = 1.0, cID score = 0.0":

```go
func TestPeerExchange_EmitNow_BuildsPeerExchangeFromArchive(t *testing.T) {
	_, myPriv, myID := mustGenKeypair(t)

	bID := bytes.Repeat([]byte{0xBB}, 32)
	cID := bytes.Repeat([]byte{0xCC}, 32)
	arch := &fakeKnownPeersArchive{rows: []storage.KnownPeer{
		{TrackerID: bID, Addr: "wss://b:443", LastSeen: time.Unix(100, 0), RegionHint: "eu", HealthScore: 0.0, Source: "allowlist"},
		{TrackerID: cID, Addr: "wss://c:443", LastSeen: time.Unix(50, 0), RegionHint: "us", HealthScore: 0.7, Source: "gossip"},
	}}

	now := time.Unix(1000, 0)
	health := NewPeerHealth(HealthConfig{
		UptimeWindow: 2 * time.Hour, RevGossipWindow: 600 * time.Second,
		RevGossipBufferSize: 16, UptimeWeight: 1.0, RevGossipWeight: 0.0,
	}, func() time.Time { return now }, nil)

	var bTID ids.TrackerID
	copy(bTID[:], bID)
	health.OnRootAttestation(bTID, now) // peer b just attested → uptime=1.0

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
		MyTrackerID: myID,
		MyPriv:      myPriv,
		Archive:     arch,
		Forward:     forward,
		Health:      health,
		Now:         func() time.Time { return now },
		EmitCap:     100,
		Invalid:     func(string) {},
		OnEmit:      func() { emitted++ },
		OnReceived:  func(string) {},
	})

	require.NoError(t, pc.EmitNow(context.Background()))
	require.Equal(t, 1, emitted, "OnEmit fired")
	require.Equal(t, fed.Kind_KIND_PEER_EXCHANGE, gotKind)

	var msg fed.PeerExchange
	require.NoError(t, proto.Unmarshal(gotPayload, &msg))
	require.Len(t, msg.Peers, 2)

	byID := map[string]*fed.KnownPeer{}
	for _, p := range msg.Peers {
		byID[string(p.TrackerId)] = p
	}
	bEntry := byID[string(bID)]
	require.NotNil(t, bEntry)
	require.InDelta(t, 1.0, bEntry.HealthScore, 0.0001, "peer b just attested → 1.0")

	cEntry := byID[string(cID)]
	require.NotNil(t, cEntry)
	require.InDelta(t, 0.0, cEntry.HealthScore, 0.0001, "peer c never attested → 0.0")

	// Write-through: the archive row's health_score has been replaced.
	bRow, _, _ := arch.GetKnownPeer(context.Background(), bID)
	require.InDelta(t, 1.0, bRow.HealthScore, 0.0001)
	cRow, _, _ := arch.GetKnownPeer(context.Background(), cID)
	require.InDelta(t, 0.0, cRow.HealthScore, 0.0001)
}
```

- [ ] **Step 2: Delete the slice-3 placeholder tests that no longer apply**

Delete the entire body of `TestPeerExchange_EmitNow_AllowlistDisconnectedScores0_5` from `peerexchange_test.go`. Slice 5 has no notion of "allowlist disconnected scores 0.5"; the placeholder is gone. The two remaining tests (`OnIncoming_UpsertsAllAndForwards`, `OnIncoming_SkipsSelfEntry`) keep working as-is *except* their `peerExchangeCoordinatorCfg` literals must drop `PeerConnected:` and add `Health: NewPeerHealth(...)` (any values; OnIncoming doesn't read them). For minimum churn, factor a tiny helper at the top of the test file:

```go
func newTestHealth(t *testing.T) *PeerHealth {
	t.Helper()
	return NewPeerHealth(HealthConfig{
		UptimeWindow: 2 * time.Hour, RevGossipWindow: 600 * time.Second,
		RevGossipBufferSize: 16, UptimeWeight: 0.7, RevGossipWeight: 0.3,
	}, time.Now, nil)
}
```

Then in each remaining test, replace `PeerConnected: ...` with `Health: newTestHealth(t)`.

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test -race ./tracker/internal/federation/... -run TestPeerExchange_ -v`
Expected: FAIL with `unknown field Health in struct literal` (or similar).

- [ ] **Step 4: Update peerexchange.go to consume Health and write through**

Edit `tracker/internal/federation/peerexchange.go`. Update the cfg struct, dropping `PeerConnected` and adding `Health`:

```go
type peerExchangeCoordinatorCfg struct {
	MyTrackerID ids.TrackerID
	MyPriv      ed25519.PrivateKey
	Archive     KnownPeersArchive
	Forward     Forwarder
	Health      *PeerHealth
	Now         func() time.Time
	EmitCap     int
	Invalid     func(reason string)
	OnEmit      func()
	OnReceived  func(outcome string)
}
```

Replace the `EmitNow` body's allowlist-rewrite block (currently lines 65-83) with:

```go
out := make([]*fed.KnownPeer, 0, len(rows))
for _, r := range rows {
	var tid ids.TrackerID
	copy(tid[:], r.TrackerID)
	score := pc.cfg.Health.Score(tid, pc.cfg.Now())
	if err := pc.cfg.Archive.UpdateKnownPeerHealth(ctx, r.TrackerID, score); err != nil {
		pc.cfg.Invalid("health_persist")
	}
	out = append(out, &fed.KnownPeer{
		TrackerId:   append([]byte(nil), r.TrackerID...),
		Addr:        r.Addr,
		LastSeen:    uint64(r.LastSeen.Unix()), //nolint:gosec // G115
		RegionHint:  r.RegionHint,
		HealthScore: score,
	})
}
```

Update the `peerexchange.go` doc-comment at the top of the file to reflect the new behavior:

```go
// peerExchangeCoordinator implements §7.1 PEER_EXCHANGE: hourly-cadence
// (on-demand in v1) gossip of a KnownPeer table, plus inbound merge.
//
// Outbound: EmitNow(ctx) lists the top-EmitCap rows from the archive,
// computes a fresh health_score per row via *PeerHealth (slice 5),
// writes the freshly-computed score back to the archive row, packs into
// a PeerExchange proto, marshals, and Forwards.
//
// Inbound: OnIncoming(ctx, env, msg) upserts every entry (skipping
// self-entries by tracker_id) and forwards the envelope onward via the
// slice-0 dedupe-and-forward gossip core.
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test -race ./tracker/internal/federation/... -run TestPeerExchange_ -v`
Expected: PASS (all three tests).

- [ ] **Step 6: Commit**

```bash
git add tracker/internal/federation/peerexchange.go tracker/internal/federation/peerexchange_test.go
git commit -m "feat(federation): peerexchange writes computed health-score through to archive"
```

---

## Task 9: revocationCoordinator OnRevocationObserved callback

**Files:**
- Modify: `tracker/internal/federation/revocation.go`
- Modify: `tracker/internal/federation/revocation_test.go`

- [ ] **Step 1: Write the failing test**

Append to `tracker/internal/federation/revocation_test.go`. (If the file uses helpers like `newTestRevocationCoordinator`, follow the same pattern; otherwise build the cfg inline like the existing tests do.)

```go
func TestRevocationCoordinator_OnIncoming_FiresOnRevocationObserved(t *testing.T) {
	// Build a valid signed Revocation from issuer; confirm the
	// observed callback fires with the right args.
	issuerPub, issuerPriv, _ := ed25519.GenerateKey(rand.Reader)
	var issuerID ids.TrackerID
	copy(issuerID[:], issuerPub) // shape; sig-verify uses the explicit pub

	rev := &fed.Revocation{
		TrackerId:  issuerID[:],
		IdentityId: bytes.Repeat([]byte{0x42}, 32),
		Reason:     fed.RevocationReason_REVOCATION_REASON_ABUSE,
		RevokedAt:  9000,
	}
	canonical, err := fed.CanonicalRevocationPreSig(rev)
	require.NoError(t, err)
	rev.TrackerSig = ed25519.Sign(issuerPriv, canonical)
	payload, err := proto.Marshal(rev)
	require.NoError(t, err)

	var (
		gotIssuer    ids.TrackerID
		gotRevokedAt time.Time
		gotRecvAt    time.Time
		callCount    int
	)
	rc := newRevocationCoordinator(revocationCoordinatorCfg{
		Archive:    &fakeRevArchive{}, // existing test fake; or stub it inline
		Forward:    func(context.Context, fed.Kind, []byte) {},
		PeerPubKey: func(id ids.TrackerID) (ed25519.PublicKey, bool) { return issuerPub, true },
		Now:        func() time.Time { return time.Unix(9100, 0) },
		Invalid:    func(string) {},
		OnEmit:     func() {},
		OnReceived: func(string) {},
		OnRevocationObserved: func(issuer ids.TrackerID, revokedAt, recvAt time.Time) {
			callCount++
			gotIssuer = issuer
			gotRevokedAt = revokedAt
			gotRecvAt = recvAt
		},
	})
	rc.OnIncoming(context.Background(), &fed.Envelope{Kind: fed.Kind_KIND_REVOCATION, Payload: payload}, ids.TrackerID{})

	require.Equal(t, 1, callCount)
	require.Equal(t, issuerID, gotIssuer)
	require.Equal(t, time.Unix(9000, 0), gotRevokedAt)
	require.Equal(t, time.Unix(9100, 0), gotRecvAt)
}

func TestRevocationCoordinator_OnIncoming_DoesNotFireOnBadSig(t *testing.T) {
	issuerPub, _, _ := ed25519.GenerateKey(rand.Reader)
	var issuerID ids.TrackerID
	copy(issuerID[:], issuerPub)
	rev := &fed.Revocation{
		TrackerId:  issuerID[:],
		IdentityId: bytes.Repeat([]byte{0x42}, 32),
		Reason:     fed.RevocationReason_REVOCATION_REASON_ABUSE,
		RevokedAt:  9000,
		TrackerSig: bytes.Repeat([]byte{0x00}, 64), // garbage sig
	}
	payload, _ := proto.Marshal(rev)

	called := false
	rc := newRevocationCoordinator(revocationCoordinatorCfg{
		Archive:    &fakeRevArchive{},
		Forward:    func(context.Context, fed.Kind, []byte) {},
		PeerPubKey: func(id ids.TrackerID) (ed25519.PublicKey, bool) { return issuerPub, true },
		Now:        func() time.Time { return time.Unix(9100, 0) },
		Invalid:    func(string) {},
		OnEmit:     func() {},
		OnReceived: func(string) {},
		OnRevocationObserved: func(ids.TrackerID, time.Time, time.Time) { called = true },
	})
	rc.OnIncoming(context.Background(), &fed.Envelope{Kind: fed.Kind_KIND_REVOCATION, Payload: payload}, ids.TrackerID{})
	require.False(t, called, "callback must not fire when sig fails verify")
}
```

If `fakeRevArchive` doesn't exist in the existing test file, model it on `fakeKnownPeersArchive`: a struct that records `PutPeerRevocation` calls and never returns errors.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -race ./tracker/internal/federation/... -run TestRevocationCoordinator_OnIncoming_FiresOnRevocationObserved -v`
Expected: FAIL with `unknown field OnRevocationObserved` (compile error).

- [ ] **Step 3: Add the callback to the cfg and fire it**

Edit `tracker/internal/federation/revocation.go`. Add the field to `revocationCoordinatorCfg`:

```go
type revocationCoordinatorCfg struct {
	// ...existing fields unchanged...
	OnReceived func(outcome string)
	// OnRevocationObserved is fired after an incoming revocation passes
	// shape + issuer + sig + archive checks. The peer-health subsystem
	// uses it to update the issuer's revocation-gossip-delay sample.
	// May be nil.
	OnRevocationObserved func(issuer ids.TrackerID, revokedAt, recvAt time.Time)
}
```

In `newRevocationCoordinator`, default the callback to a no-op so cfg.OnRevocationObserved is always safe to call:

```go
if cfg.OnRevocationObserved == nil {
	cfg.OnRevocationObserved = func(ids.TrackerID, time.Time, time.Time) {}
}
```

Edit `OnIncoming`. After the successful archive write (existing `rc.cfg.OnReceived("archived")` line), insert the callback fire just before the existing `Forward` and `OnReceived("archived")` lines:

```go
var issuer ids.TrackerID
copy(issuer[:], rev.TrackerId)
rc.cfg.OnRevocationObserved(issuer,
	time.Unix(int64(rev.RevokedAt), 0), //nolint:gosec
	rc.cfg.Now(),
)
rc.cfg.Forward(ctx, fed.Kind_KIND_REVOCATION, env.Payload)
rc.cfg.OnReceived("archived")
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./tracker/internal/federation/... -run TestRevocationCoordinator_OnIncoming -v`
Expected: PASS for the two new tests + all pre-existing revocation tests.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/federation/revocation.go tracker/internal/federation/revocation_test.go
git commit -m "feat(federation): revocation coordinator OnRevocationObserved callback"
```

---

## Task 10: Equivocator onFlag callback

**Files:**
- Modify: `tracker/internal/federation/equivocation.go`
- Modify: `tracker/internal/federation/equivocation_test.go`
- Modify (compile-only): `tracker/internal/federation/subsystem.go` (call site of `NewEquivocator`)

- [ ] **Step 1: Write the failing tests**

Append to `tracker/internal/federation/equivocation_test.go`:

```go
func TestEquivocator_OnLocalConflict_FiresOnFlag(t *testing.T) {
	arch := &fakePeerRootArchive{}
	// Pre-populate an existing peer-root row for the offender so
	// OnLocalConflict has something to compare against.
	offender := bytes.Repeat([]byte{0xAB}, 32)
	arch.rows = append(arch.rows, fakePeerRoot{
		TrackerID: offender, Hour: 100,
		Root: bytes.Repeat([]byte{0x01}, 32),
		Sig:  bytes.Repeat([]byte{0x02}, 64),
	})

	var flagged ids.TrackerID
	calls := 0
	e := NewEquivocator(arch, func(context.Context, fed.Kind, []byte) {}, &Registry{}, func(id ids.TrackerID) {
		calls++
		flagged = id
	})

	incoming := &fed.RootAttestation{
		TrackerId:  offender,
		Hour:       100,
		MerkleRoot: bytes.Repeat([]byte{0x10}, 32),
		TrackerSig: bytes.Repeat([]byte{0x20}, 64),
	}
	e.OnLocalConflict(context.Background(), incoming, &fed.Envelope{})

	require.Equal(t, 1, calls)
	var want ids.TrackerID
	copy(want[:], offender)
	require.Equal(t, want, flagged)
}

func TestEquivocator_OnIncomingEvidence_FiresOnFlag(t *testing.T) {
	arch := &fakePeerRootArchive{}
	offender := bytes.Repeat([]byte{0xCD}, 32)

	var flagged ids.TrackerID
	calls := 0
	e := NewEquivocator(arch, func(context.Context, fed.Kind, []byte) {}, &Registry{}, func(id ids.TrackerID) {
		calls++
		flagged = id
	})

	evi := &fed.EquivocationEvidence{
		TrackerId: offender,
		Hour:      100,
		RootA:     bytes.Repeat([]byte{0x01}, 32),
		SigA:      bytes.Repeat([]byte{0x02}, 64),
		RootB:     bytes.Repeat([]byte{0x10}, 32),
		SigB:      bytes.Repeat([]byte{0x20}, 64),
	}
	payload, _ := proto.Marshal(evi)
	e.OnIncomingEvidence(context.Background(), &fed.Envelope{Kind: fed.Kind_KIND_EQUIVOCATION_EVIDENCE, Payload: payload}, ids.TrackerID{})

	require.Equal(t, 1, calls)
	var want ids.TrackerID
	copy(want[:], offender)
	require.Equal(t, want, flagged)
}

func TestEquivocator_OnIncomingEvidence_AboutSelf_DoesNotFireOnFlag(t *testing.T) {
	arch := &fakePeerRootArchive{}
	var self ids.TrackerID
	self[0] = 0x99

	calls := 0
	e := NewEquivocator(arch, func(context.Context, fed.Kind, []byte) {}, &Registry{}, func(ids.TrackerID) {
		calls++
	}).WithSelf(self)

	evi := &fed.EquivocationEvidence{
		TrackerId: self[:],
		Hour:      100,
		RootA:     bytes.Repeat([]byte{0x01}, 32),
		SigA:      bytes.Repeat([]byte{0x02}, 64),
		RootB:     bytes.Repeat([]byte{0x10}, 32),
		SigB:      bytes.Repeat([]byte{0x20}, 64),
	}
	payload, _ := proto.Marshal(evi)
	e.OnIncomingEvidence(context.Background(), &fed.Envelope{Kind: fed.Kind_KIND_EQUIVOCATION_EVIDENCE, Payload: payload}, ids.TrackerID{})

	require.Equal(t, 0, calls, "evidence about self does not flag self")
}
```

If `fakePeerRootArchive` (or `fakePeerRoot`) doesn't exist in this file, build a minimal one:

```go
type fakePeerRoot struct {
	TrackerID []byte
	Hour      uint64
	Root      []byte
	Sig       []byte
}

type fakePeerRootArchive struct {
	rows []fakePeerRoot
}

func (f *fakePeerRootArchive) GetPeerRoot(_ context.Context, trackerID []byte, hour uint64) (storage.PeerRoot, bool, error) {
	for _, r := range f.rows {
		if bytes.Equal(r.TrackerID, trackerID) && r.Hour == hour {
			return storage.PeerRoot{TrackerID: r.TrackerID, Hour: r.Hour, Root: r.Root, Sig: r.Sig}, true, nil
		}
	}
	return storage.PeerRoot{}, false, nil
}
```

(Adjust the return type to whatever `PeerRootArchive.GetPeerRoot` actually returns; check the existing interface in `tracker/internal/federation` — it's likely `storage.PeerRoot`.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -race ./tracker/internal/federation/... -run TestEquivocator_ -v`
Expected: FAIL with "too many arguments to NewEquivocator" or similar.

- [ ] **Step 3: Add the parameter and fire it from both methods**

Edit `tracker/internal/federation/equivocation.go`:

```go
type Equivocator struct {
	archive PeerRootArchive
	forward Forwarder
	reg     *Registry
	self    *ids.TrackerID
	onFlag  func(ids.TrackerID)

	selfEquiv atomic.Int64
}

// NewEquivocator. onFlag may be nil; it is fired with the offender's
// TrackerID after a Depeer call, on both locally-detected conflicts and
// peer-broadcast evidence about a third party. Self-evidence does NOT
// fire onFlag (the local equivocation is already metric-tracked via
// SelfEquivocations).
func NewEquivocator(arch PeerRootArchive, fwd Forwarder, reg *Registry, onFlag func(ids.TrackerID)) *Equivocator {
	if onFlag == nil {
		onFlag = func(ids.TrackerID) {}
	}
	return &Equivocator{archive: arch, forward: fwd, reg: reg, onFlag: onFlag}
}

// WithSelf preserves all fields including onFlag.
func (e *Equivocator) WithSelf(selfID ids.TrackerID) *Equivocator {
	id := selfID
	return &Equivocator{
		archive: e.archive,
		forward: e.forward,
		reg:     e.reg,
		self:    &id,
		onFlag:  e.onFlag,
	}
}
```

In `OnLocalConflict`, immediately before `_ = e.reg.Depeer(...)`, fire onFlag:

```go
var offender ids.TrackerID
copy(offender[:], incoming.TrackerId)
_ = e.reg.Depeer(offender, ReasonEquivocation)
e.onFlag(offender)
```

Same for `OnIncomingEvidence`, immediately before its `Depeer`:

```go
var offender ids.TrackerID
copy(offender[:], evi.TrackerId)
_ = e.reg.Depeer(offender, ReasonEquivocation)
e.onFlag(offender)
```

- [ ] **Step 4: Update the existing call site in subsystem.go (compile-only)**

Edit `tracker/internal/federation/subsystem.go`. Find the existing `equiv := NewEquivocator(dep.Archive, forward, reg).WithSelf(...)` line (around line 84). For now, pass `nil` as the new fourth argument; Task 11 will replace the `nil` with `health.OnEquivocation`:

```go
equiv := NewEquivocator(dep.Archive, forward, reg, nil).WithSelf(cfg.MyTrackerID)
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test -race ./tracker/internal/federation/...`
Expected: PASS, including pre-existing equivocator tests (which should not assert anything about the new onFlag).

- [ ] **Step 6: Commit**

```bash
git add tracker/internal/federation/equivocation.go tracker/internal/federation/equivocation_test.go tracker/internal/federation/subsystem.go
git commit -m "feat(federation): equivocator onFlag callback for peer-health hook"
```

---

## Task 11: Subsystem wires PeerHealth + drops PeerConnected

**Files:**
- Modify: `tracker/internal/federation/subsystem.go`

- [ ] **Step 1: Add Health to the federation-package Config struct**

Edit `tracker/internal/federation/config.go`. The federation package's `Config` is at line 24. Append a new field at the end of the struct (just after `Peers`):

```go
type Config struct {
	// ...existing fields up through Peers unchanged...
	Peers  []AllowlistedPeer
	Health HealthConfig
}
```

`Config.withDefaults()` does not need a stanza for Health: the tracker-level config already supplies non-zero defaults (Task 7), and operator-overridden zero values are caught by `tracker/internal/config/validate.go` (Task 7).

- [ ] **Step 2: Construct *PeerHealth in NewFederation**

Edit `tracker/internal/federation/subsystem.go`. Near the top of `NewFederation`, after `cfg` and `dep.Now` are in scope, build the health type. Place it just above the `equiv := NewEquivocator(...)` call (around line 84):

```go
health := NewPeerHealth(cfg.Health, dep.Now, nil)
```

(The `nil` third arg is replaced with `dep.Metrics.HealthScoreComputed` in Task 12. Leaving it `nil` here keeps Task 11 self-contained.)

- [ ] **Step 3: Wire run_cmd.go to populate cfg.Federation → federation.Config.Health**

Edit `tracker/cmd/token-bay-tracker/run_cmd.go`. Find the `federation.Config{...}` literal where the tracker-level config is translated into the federation-package config. Add the new field:

```go
federation.Config{
	// ...existing fields unchanged...
	Health: federation.HealthConfig{
		UptimeWindow:        time.Duration(cfg.Federation.Health.UptimeWindowS) * time.Second,
		RevGossipWindow:     time.Duration(cfg.Federation.Health.RevGossipWindowS) * time.Second,
		RevGossipBufferSize: cfg.Federation.Health.RevGossipBufferSize,
		UptimeWeight:        cfg.Federation.Health.UptimeWeight,
		RevGossipWeight:     cfg.Federation.Health.RevGossipWeight,
	},
}
```

If the literal is multi-line and dispersed across helpers, search for `federation.Config{` and add the `Health:` field in the matching place.

- [ ] **Step 4: Update the NewEquivocator call to pass health.OnEquivocation**

Replace the slice-3 `equiv := NewEquivocator(dep.Archive, forward, reg, nil).WithSelf(cfg.MyTrackerID)` line with:

```go
equiv := NewEquivocator(dep.Archive, forward, reg, health.OnEquivocation).WithSelf(cfg.MyTrackerID)
```

- [ ] **Step 5: Wire OnRevocationObserved into the revocation cfg**

Find `revocationCoordinatorCfg{ ... }` literal in `subsystem.go`. Add the new field:

```go
OnRevocationObserved: health.OnRevocation,
```

- [ ] **Step 6: Wire ROOT_ATTESTATION receive**

In the dispatch switch (currently around line 435), in the `case fed.Kind_KIND_ROOT_ATTESTATION:` arm, after the existing success-path metric `f.dep.Metrics.RootAttestationsReceived("archived")`, append:

```go
f.health.OnRootAttestation(peerID, f.dep.Now())
```

`peerID` is already in scope from the outer recv loop (the function signature has it).

- [ ] **Step 7: Drop the PeerConnected closure and pass Health into peerExchangeCoordinator**

Locate the existing block that sets `peerExchange.cfg.PeerConnected = func(id ids.TrackerID) bool { ... }`. Delete that block entirely.

Locate the `peerExchangeCoordinatorCfg{ ... }` literal where it is constructed; remove the `PeerConnected` field (Task 8 already removed it from the struct definition; this just removes any remaining call-site value) and add:

```go
Health: health,
```

- [ ] **Step 8: Store the *PeerHealth on the Federation struct**

Find the `type Federation struct { ... }` definition. Add a new field:

```go
health *PeerHealth
```

Find the `f := &Federation{...}` literal and set `health: health,`.

- [ ] **Step 9: Run all federation tests**

Run: `go test -race ./tracker/internal/federation/... ./tracker/cmd/...`
Expected: PASS for everything except possibly `integration_test.go` (Task 13 covers integration changes); if integration tests fail because they construct a `federation.Config` without `Health`, fall back to defaults inline in those tests (`Health: HealthConfig{UptimeWindow: 2*time.Hour, RevGossipWindow: 600*time.Second, RevGossipBufferSize: 16, UptimeWeight: 0.7, RevGossipWeight: 0.3}`). Patch every failing integration cfg literal to include this default block in this task.

- [ ] **Step 10: Commit**

```bash
git add tracker/internal/federation/subsystem.go tracker/internal/federation/config.go tracker/cmd/token-bay-tracker/run_cmd.go tracker/internal/federation/integration_test.go
git commit -m "feat(federation): subsystem wires PeerHealth across all four hooks"
```

(Stage only the files actually changed in this task. The integration_test.go change here is the cfg-literal patch only — Task 13 adds new assertions.)

---

## Task 12: health_score_computations_total counter

**Files:**
- Modify: `tracker/internal/federation/metrics.go`
- Modify: `tracker/internal/federation/metrics_test.go`
- Modify: `tracker/internal/federation/health.go` (Score must call into the metric)
- Modify: `tracker/internal/federation/health_test.go` (cover the metric)

- [ ] **Step 1: Write the failing test for the counter**

Append to `tracker/internal/federation/metrics_test.go`:

```go
func TestMetrics_HealthScoreComputations(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())

	m.HealthScoreComputed("ok")
	m.HealthScoreComputed("ok")
	m.HealthScoreComputed("equivocated")
	m.HealthScoreComputed("no_data")

	require.Equal(t, float64(2), testutil.ToFloat64(m.HealthScoreComputationsCounter().WithLabelValues("ok")))
	require.Equal(t, float64(1), testutil.ToFloat64(m.HealthScoreComputationsCounter().WithLabelValues("equivocated")))
	require.Equal(t, float64(1), testutil.ToFloat64(m.HealthScoreComputationsCounter().WithLabelValues("no_data")))
}
```

(Use the same `testutil` import the existing `metrics_test.go` already imports.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -race ./tracker/internal/federation/... -run TestMetrics_HealthScoreComputations -v`
Expected: FAIL with `undefined: (*Metrics).HealthScoreComputed`.

- [ ] **Step 3: Add the metric**

Edit `tracker/internal/federation/metrics.go`. Add a new field to `Metrics`:

```go
healthScoreComputations *prometheus.CounterVec
```

In `NewMetrics(reg)`, add:

```go
healthScoreComputations: prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "tokenbay_federation_health_score_computations_total",
	Help: "Number of peer-health-score computations, by outcome.",
}, []string{"outcome"}),
```

Add it to the `for _, c := range []prometheus.Collector{...}` registration list (alongside the other counters).

Add accessors:

```go
func (m *Metrics) HealthScoreComputed(outcome string) {
	m.healthScoreComputations.WithLabelValues(outcome).Inc()
}

func (m *Metrics) HealthScoreComputationsCounter() *prometheus.CounterVec {
	return m.healthScoreComputations
}
```

- [ ] **Step 4: Wire the existing onComputed callback in Score**

`PeerHealth.onComputed` was added in Task 1 but never invoked. Update `Score` in `tracker/internal/federation/health.go` to fire it at exit:

```go
func (h *PeerHealth) Score(peer ids.TrackerID, now time.Time) float64 {
	h.mu.Lock()
	defer h.mu.Unlock()

	if _, equiv := h.equivocated[peer]; equiv {
		h.onComputed("equivocated")
		return 0
	}

	_, hasRoot := h.lastRoot[peer]
	rb, hasRing := h.revGossipDelays[peer]
	if !hasRoot && (!hasRing || rb.n == 0) {
		h.onComputed("no_data")
	} else {
		h.onComputed("ok")
	}

	uptimeSub := 0.0
	if last, ok := h.lastRoot[peer]; ok {
		age := now.Sub(last)
		if age < 0 {
			age = 0
		}
		frac := 1.0 - float64(age)/float64(h.cfg.UptimeWindow)
		uptimeSub = clamp01(frac)
	}

	revgossSub := 1.0
	if rb, ok := h.revGossipDelays[peer]; ok && rb.n > 0 {
		mean := rb.mean()
		frac := 1.0 - float64(mean)/float64(h.cfg.RevGossipWindow)
		revgossSub = clamp01(frac)
	}

	return h.cfg.UptimeWeight*uptimeSub + h.cfg.RevGossipWeight*revgossSub
}
```

- [ ] **Step 5: Wire `HealthScoreComputed` into the subsystem call site**

In `tracker/internal/federation/subsystem.go`, locate the `health := NewPeerHealth(...)` call from Task 11. Replace its third argument (currently `nil`) with `dep.Metrics.HealthScoreComputed`:

```go
health := NewPeerHealth(HealthConfig{
	// ...same config fields as Task 11...
}, dep.Now, dep.Metrics.HealthScoreComputed)
```

Add a small test in `health_test.go` to confirm onComputed fires with the right outcome:

```go
func TestPeerHealth_OnComputedFires(t *testing.T) {
	now := time.Unix(10000, 0)
	var outcomes []string
	h := NewPeerHealth(HealthConfig{
		UptimeWindow: 2 * time.Hour, RevGossipWindow: 600 * time.Second,
		RevGossipBufferSize: 16, UptimeWeight: 0.7, RevGossipWeight: 0.3,
	}, func() time.Time { return now }, func(o string) { outcomes = append(outcomes, o) })

	var p ids.TrackerID
	p[0] = 0x42

	_ = h.Score(p, now)
	h.OnRootAttestation(p, now)
	_ = h.Score(p, now)
	h.OnEquivocation(p)
	_ = h.Score(p, now)

	require.Equal(t, []string{"no_data", "ok", "equivocated"}, outcomes)
}
```

- [ ] **Step 6: Run all federation tests**

Run: `go test -race ./tracker/internal/federation/...`
Expected: PASS — the new metric test, the new onComputed test, and every pre-existing test (since the third arg is `nil`-tolerant).

- [ ] **Step 7: Commit**

```bash
git add tracker/internal/federation/metrics.go \
        tracker/internal/federation/metrics_test.go \
        tracker/internal/federation/health.go \
        tracker/internal/federation/health_test.go \
        tracker/internal/federation/subsystem.go
git commit -m "feat(federation): health_score_computations_total counter"
```

---

## Task 13: Integration test — two-peer health scenario

**Files:**
- Modify: `tracker/internal/federation/integration_test.go`

- [ ] **Step 1: Write the failing integration assertions**

Append the new test to `tracker/internal/federation/integration_test.go`. Use the existing `newTwoTrackerWithKnownPeers(t)` helper (line 148) — it sets up two in-process trackers A and B with `KnownPeersArchive` wired. Reuse the same handshake-wait pattern as `TestIntegration_PeerExchange_AB` (line 787) and the same publish helpers (`flipReadyA`, `PublishHour`, `PublishPeerExchange`). The test uses real wall-clock `time.Now`; with `UptimeWindow=2h`, scores immediately after publish are still within 0.01 of the expected limit values. The 2h-decay scenario is covered by unit tests in Task 1, not here.

```go
func TestIntegration_PeerHealth_RootAttestationLiftsScore(t *testing.T) {
	t.Parallel()
	tt := newTwoTrackerWithKnownPeers(t)

	// Wait for handshake (steady).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got := false
		for _, p := range tt.a.Peers() {
			if p.State == federation.PeerStateSteady {
				got = true
				break
			}
		}
		if got {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// B publishes a ROOT_ATTESTATION. A archives it AND fires
	// health.OnRootAttestation under the slice-5 wiring.
	tt.flipReadyB(b(32, 7), b(64, 8)) // b32(7) -> root, b64(8) -> sig
	require.NoError(t, tt.b.PublishHour(context.Background(), 100))

	// Wait for A to archive B's root.
	bIDBytes := tt.bID.Bytes()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok, _ := tt.archA.GetPeerRoot(context.Background(), bIDBytes[:], 100); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Trigger peer-exchange emit on A: this calls Health.Score(B, now)
	// and writes the freshly-computed score back to kpA's B row.
	require.NoError(t, tt.a.PublishPeerExchange(context.Background()))

	// Health for B should be ~1.0 (uptime full, revgoss neutral).
	row, ok, err := tt.kpA.GetKnownPeer(context.Background(), bIDBytes[:])
	require.NoError(t, err)
	require.True(t, ok)
	require.InDelta(t, 1.0, row.HealthScore, 0.05, "B just attested → score ≈ 1.0")
}

func TestIntegration_PeerHealth_EquivocationZerosScore(t *testing.T) {
	t.Parallel()
	tt := newTwoTrackerWithKnownPeers(t)

	// Wait for handshake.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got := false
		for _, p := range tt.a.Peers() {
			if p.State == federation.PeerStateSteady {
				got = true
				break
			}
		}
		if got {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Pre-populate kpA with a row for B at score 0.5 so we can
	// observe the score being written down to 0 after equivocation.
	bIDBytes := tt.bID.Bytes()
	require.NoError(t, tt.kpA.UpsertKnownPeer(context.Background(), storage.KnownPeer{
		TrackerID: bIDBytes[:], Addr: "B", LastSeen: time.Now(),
		HealthScore: 0.5, Source: "allowlist",
	}))

	// Drive a local equivocation detection on A by publishing two
	// conflicting ROOT_ATTESTATIONs from B for the same hour. The
	// first archives; the second triggers OnLocalConflict, which
	// fires onFlag → health.OnEquivocation(B).
	tt.flipReadyB(b(32, 1), b(64, 1))
	require.NoError(t, tt.b.PublishHour(context.Background(), 200))
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok, _ := tt.archA.GetPeerRoot(context.Background(), bIDBytes[:], 200); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Now publish a different root for the same hour — triggers conflict.
	tt.flipReadyB(b(32, 2), b(64, 2))
	require.NoError(t, tt.b.PublishHour(context.Background(), 200))

	// Wait for A's equivocation counter to bump (existing pattern in
	// TestIntegration_Equivocation_LocalDetection).
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		// emit + assert score is now 0
		require.NoError(t, tt.a.PublishPeerExchange(context.Background()))
		row, ok, _ := tt.kpA.GetKnownPeer(context.Background(), bIDBytes[:])
		if ok && row.HealthScore == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("B's health_score never reached 0 after equivocation")
}
```

If `flipReadyB` doesn't exist in this file (only `flipReadyA` does, at line 34), add a mirror helper:

```go
func (tt *twoTracker) flipReadyB(root, sig []byte) {
	tt.srcB.mu.Lock()
	defer tt.srcB.mu.Unlock()
	tt.srcB.ok = true
	tt.srcB.root = root
	tt.srcB.sig = sig
}
```

The `b(32, n)` / `b(64, n)` helpers are existing helpers in this test file (they fill a byte slice with `n` repeated to length 32 or 64).

- [ ] **Step 2: Run the integration test**

Run: `go test -race ./tracker/internal/federation/... -run TestPeerHealth_Integration_TwoPeers -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add tracker/internal/federation/integration_test.go
git commit -m "test(federation): two-peer integration scenario for health-score"
```

---

## Task 14: Final check — `make check` from repo root

**Files:** *(no code changes; verification only)*

- [ ] **Step 1: Run the full repo check**

Run from the repo root: `make check`
Expected: every module's `go test -race ./...` and `golangci-lint run ./...` pass.

- [ ] **Step 2: If lint complains about unused PeerConnected references or stale doc comments, fix in place**

Common cleanup items:
- The line `peerExchange.cfg.PeerConnected = func(id ids.TrackerID) bool { ... }` in `subsystem.go` (slice-3 wiring) should be gone. If lint flags any leftover unused symbol, delete it.
- `peerexchange.go` doc comment must mention slice-5 health, not the placeholder.
- Any orphan import of `ids` or other packages in `peerexchange.go` should be removed if no longer needed.

- [ ] **Step 3: Inspect the diff for the whole branch and re-run check if anything changed**

Run: `git diff origin/main..HEAD --stat` and confirm nothing surprising is touched. If the slice5 spec doc is the only docs/ change, that's expected.

- [ ] **Step 4: Commit any final cleanup**

```bash
git add <files>
git commit -m "chore(federation): peer-health cleanup"
```

(Skip this step if no cleanup was needed.)
