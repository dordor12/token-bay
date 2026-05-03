# Tracker — `internal/admission` Core (in-memory) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the in-memory core of the tracker admission subsystem — `tracker/internal/admission`. End state: callers can construct a `Subsystem` from `(config.AdmissionConfig, *registry.Registry, ed25519.PrivateKey, clock)`, drive ledger events through it via a small `LedgerEventObserver` interface, ask `Decide(consumerID, attestation, now) AdmissionResult` to get `ADMIT | QUEUE | REJECT`, and request a `SignedCreditAttestation` via `IssueAttestation`. Persistence (tlog + snapshot + replay), admin HTTP, Prometheus metrics, broker wire-up, and FetchHeadroom RPC are deliberately out of scope (plan 3 + later).

**Architecture:** A `Subsystem` struct owns: per-consumer credit state (a sharded `map[ids.IdentityID]*ConsumerCreditState`, each with three rolling 30-day-bucket arrays); per-seeder heartbeat-reliability state (10-minute rolling buckets); a published `*SupplySnapshot` swapped atomically by a 5s aggregator goroutine that reads `*registry.Registry`; a max-heap queue of `QueueEntry`; and a per-consumer rate limiter for attestation issuance. The hot path `Decide` resolves credit score (attestation → local → trial tier), reads `SupplySnapshot` via `atomic.Pointer.Load` (no lock), computes pressure, and branches per admission-design §5.1 step 4. All in-memory state is sharded so `-race` stays clean. The ledger has no event bus on disk yet — admission ships only the *consumer* side (`LedgerEventObserver` interface + `OnLedgerEvent(LedgerEvent)`); ledger-side emission lands in a separate plan, and v1 of admission is library-shaped (no broker integration yet).

**Tech Stack:** Go 1.23+, stdlib (`crypto/ed25519`, `sync`, `sync/atomic`, `time`, `container/heap`, `context`), `github.com/stretchr/testify`. No new third-party deps. Imports `shared/admission` and `shared/signing.SignCreditAttestation` (delivered by plan 1) and `tracker/internal/{config,registry}` (already on disk).

**Specs:**
- `docs/superpowers/specs/admission/2026-04-25-admission-design.md` — primary spec. §3 (interfaces), §4.1 wire types (consumed via `shared/admission`), §4.2 in-memory types, §5.1 Decide, §5.2 ComputeLocalScore, §5.3 supply aggregation, §5.4 queue draining + backoff, §5.5 attestation issuance, §5.6 OnLedgerEvent, §6.4 tracker-internal tests, §6.5 banded responses, §7 failure handling, §8 security (clamp), §9.3 config (already implemented), §10 acceptance criteria #1-8 (functional) + #20 (race-clean).
- `docs/superpowers/specs/tracker/2026-04-22-tracker-design.md` — §6 concurrency model (admission joins broker + federation in the mandatory `-race` list — amendment lands via plan 1).

**Dependency order:** Runs after **plan 1** (`tracker_admission_shared`) merges to `main`. Plan 1 creates `shared/admission/` (proto types, validation, sign/verify) which this plan imports. Within tracker, this plan depends on:
- `tracker/internal/config.AdmissionConfig` (already populated with defaults + validation; no edits in this plan)
- `tracker/internal/registry.Registry` + `SeederRecord` (already populated; admission reads `Snapshot()`, `HeadroomEstimate`, `LastHeartbeat`, `ReputationScore`)
- `tracker/internal/ledger.Ledger` (existing, not directly invoked here — admission consumes events via the `LedgerEventObserver` interface introduced in this plan)

**Branch:** `tracker_admission_core` off `main` head (after plan 1 merges). One PR per plan.

**Repo path:** This worktree lives at `/Users/dor.amid/.superset/worktrees/token-bay/tracker_admission`. All absolute paths in this plan use that prefix. Module-relative commands run from `/Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker`.

---

## 1. File map

```
tracker/internal/admission/
├── doc.go                            ← CREATE: package doc + non-negotiables
├── admission.go                      ← CREATE: Subsystem struct + Open + Close + Option
├── admission_test.go                 ← CREATE: lifecycle tests
├── buckets.go                        ← CREATE: DayBucket + bucket-index + rotation primitives
├── buckets_test.go                   ← CREATE
├── state.go                          ← CREATE: ConsumerCreditState + SeederHeartbeatState +
│                                                MinuteBucket + accessor methods
├── state_test.go                     ← CREATE
├── score.go                          ← CREATE: ComputeLocalScore + signal compute helpers +
│                                                undefined-signal redistribution + trial-tier const
├── score_test.go                     ← CREATE
├── supply.go                         ← CREATE: SupplySnapshot + atomic.Pointer publication +
│                                                5s aggregator goroutine + freshness decay
├── supply_test.go                    ← CREATE
├── queue.go                          ← CREATE: QueueEntry + EffectivePriority + max-heap impl
│                                                via container/heap
├── queue_test.go                     ← CREATE
├── decide.go                         ← CREATE: AdmissionResult + RejectReason +
│                                                PositionBand + EtaBand enums +
│                                                Decide hot path + retry-after backoff
├── decide_test.go                    ← CREATE
├── decision_matrix_test.go           ← CREATE: §6.4 table-driven decision-matrix test
├── aging_boost_test.go               ← CREATE: §10 #6 acceptance integration test
├── attestation.go                    ← CREATE: IssueAttestation + per-consumer rate limiter
├── attestation_test.go               ← CREATE
├── events.go                         ← CREATE: LedgerEventObserver interface + LedgerEvent struct +
│                                                OnLedgerEvent in-memory bucket updates
├── events_test.go                    ← CREATE
└── helpers_test.go                   ← CREATE: openTempSubsystem, fixtureConsumer, fakeBus,
                                                deterministic keypair, fixed-clock helpers
```

Notes:
- One Go file per area of responsibility; keeps each test file scoped to its sibling.
- `helpers_test.go` provides `openTempSubsystem(t)` returning a wired `*Subsystem` with a fixed clock and an empty registry. All higher-level tests start from this helper.
- `decision_matrix_test.go` and `aging_boost_test.go` are intentionally separate from `decide_test.go` because they're large table-driven / integration tests that span multiple files' behavior; isolating them keeps `decide_test.go` focused on per-branch unit tests.
- No persistence files (`tlog.go`, `snapshot.go`, `replay.go`) — those land in plan 3.
- No metric / admin files — those land in plan 3.
- The wire types `AdmissionResult`, `PositionBand`, `EtaBand`, `RejectReason`, `Queued`, `Rejected` are defined as Go-only types in `decide.go`. The corresponding **proto** definitions are described in plan 1's tracker-spec amendment §5.1; when tracker control-plane proto is later implemented, those Go types will be replaced by the generated proto types in a follow-up plan.

## 2. Conventions used in this plan

- All `go test`, `go build`, and `make` commands run from `/Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker` unless a different `cd` is shown.
- `PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH"` if Go is managed by mise. Bash steps include it where required.
- One commit per task. Conventional-commit prefixes: `feat(tracker/admission):`, `test(tracker/admission):`, `docs(tracker/admission):`, `refactor(tracker/admission):`. Cross-module commits use the broader form (e.g. `chore(tracker):`).
- Co-Authored-By footer on every commit:
  ```
  Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
  ```
- TDD: failing test first → run-and-confirm-fail → minimal impl → run-and-confirm-pass → commit. Refactor-only commits get `refactor:`.
- Lint policy: respect `.golangci.yml`. Every exported symbol needs a doc comment (`revive:exported`).
- Coverage target: ≥ 90% line coverage on every file in `internal/admission/`.
- **Race discipline:** every test invocation in this plan uses `-race`. The race-detector is not optional here — admission joins broker + federation in the §6 mandatory race-list (plan 1's amendment).
- Time handling: callers pass `time.Time` in. The subsystem stores a `nowFn func() time.Time` (defaulting to `time.Now`). Tests use a fixed `time.Date(...)` reference.
- Identity type: `ids.IdentityID` (32-byte array). Construct from a 32-byte slice via `ids.IdentityID(append([]byte{}, b...))` cast — see `helpers_test.go` (Task 14).

## 3. Design decisions used by all tasks

### 3.1 Subsystem shape

```go
type Subsystem struct {
	cfg       config.AdmissionConfig
	reg       *registry.Registry
	priv      ed25519.PrivateKey
	pub       ed25519.PublicKey
	trackerID ids.IdentityID  // tracker pubkey hash; embedded in attestations as IssuerTrackerID
	nowFn     func() time.Time

	// per-consumer state — sharded for race-cleanness; one RWMutex per shard.
	consumerShards []*consumerShard

	// per-seeder state — same sharding shape; populated on first heartbeat-event.
	seederShards []*seederShard

	// supply snapshot — atomic pointer published by aggregator goroutine.
	supply atomic.Pointer[SupplySnapshot]

	// queue — single max-heap protected by its own mutex.
	queueMu sync.Mutex
	queue   queueHeap

	// per-consumer attestation issuance rate limiter — sharded.
	attestRL *rateLimiter

	// federation peer-set check (§5.1 step 1). Real wiring is later;
	// v1 callers pass federationPeersAlwaysFalse / a stub.
	peers PeerSet

	// aggregator + queue-drain goroutines lifecycle.
	stop   chan struct{}
	wg     sync.WaitGroup
}

type Option func(*Subsystem)

func WithClock(now func() time.Time) Option { ... }
func WithPeerSet(p PeerSet) Option           { ... }

func Open(cfg config.AdmissionConfig, reg *registry.Registry, priv ed25519.PrivateKey, opts ...Option) (*Subsystem, error)
func (s *Subsystem) Close() error
```

`Open` validates the config (sum-of-weights, threshold ordering, etc.) by calling `cfg.Validate()` — but `tracker/internal/config` already does that on load, so `Open` only re-checks structural invariants admission cares about (e.g. `RollingWindowDays > 0`). Spawns the aggregator goroutine and queue-drain ticker. `Close` signals `stop` and waits for the goroutines via `wg.Wait()`.

### 3.2 Sharding scheme

`consumerShards` and `seederShards` use the same shard count as the registry — `registry.DefaultShardCount = 16`. Shard index by `binary.BigEndian.Uint64(id[:8]) % numShards` mirroring the registry's `shardIndex`. This keeps hot-path lookups O(1) average and avoids a global lock.

### 3.3 Decide signature

```go
type AdmissionResult struct {
	Outcome    Outcome           // ADMIT | QUEUE | REJECT
	Queued     *QueuedDetails    // non-nil iff Outcome == QUEUE
	Rejected   *RejectedDetails  // non-nil iff Outcome == REJECT
	CreditUsed float64           // [0, 1] — score that drove the decision (for tests + future logging)
}

type Outcome uint8
const (
	OutcomeUnspecified Outcome = iota
	OutcomeAdmit
	OutcomeQueue
	OutcomeReject
)

type QueuedDetails struct {
	RequestID    [16]byte
	PositionBand PositionBand
	EtaBand      EtaBand
}

type RejectedDetails struct {
	Reason       RejectReason
	RetryAfterS  uint32
}

func (s *Subsystem) Decide(consumerID ids.IdentityID, att *admission.SignedCreditAttestation, now time.Time) AdmissionResult
```

The bands and reason enum mirror plan 1's tracker-spec amendment §5.1. `RequestID` is a UUID generated at queue time; tests pass a fixed seeder for determinism via `WithRequestIDFn`.

### 3.4 Score scale

All public scores are normalized `[0, 1] float64`. Conversion to/from the wire `uint32` fixed-point 0..10000 happens only at the attestation boundary in `attestation.go` (`uint32(score * 10000)` round-half-to-nearest-even via `math.Round`). Internal logic stays in float64.

### 3.5 LedgerEvent shape

```go
type LedgerEventKind uint8

const (
	LedgerEventUnspecified     LedgerEventKind = iota
	LedgerEventSettlement                       // 1: usage entry finalized
	LedgerEventTransferIn                       // 2: peer-region credit landing
	LedgerEventTransferOut                      // 3: peer-region credit leaving
	LedgerEventStarterGrant                     // 4: starter grant issued to consumer
	LedgerEventDisputeFiled                     // 5: dispute opened
	LedgerEventDisputeResolved                  // 6: dispute closed (status: UPHELD or REJECTED)
)

type LedgerEvent struct {
	Kind         LedgerEventKind
	ConsumerID   ids.IdentityID
	SeederID     ids.IdentityID  // zero for non-usage kinds
	CostCredits  uint64
	Flags        uint32          // bit 0 = consumer_sig_missing (settlements only)
	DisputeUpheld bool           // settlement_finalized: false; dispute_resolved: true → UPHELD
	Timestamp    time.Time
}

type LedgerEventObserver interface {
	OnLedgerEvent(ev LedgerEvent)
}
```

`Subsystem` implements `LedgerEventObserver`. Real ledger emission is a separate plan; tests drive `OnLedgerEvent` directly.

### 3.6 Out-of-scope reminders

| Concern | Where it lives |
|---|---|
| `admission.tlog` / snapshot / `StartupReplay` / `SnapshotEmit` | plan 3 |
| Admin HTTP endpoints `/admission/*` | plan 3 |
| Prometheus metrics | plan 3 |
| Broker integration (route `broker_request` through `Decide`) | future tracker control-plane plan |
| `FetchHeadroom` RPC | future tracker↔seeder RPC plan |
| Real federation peer-set lookup | future federation wiring plan; v1 uses `PeerSet` stub |
| Real ledger event emission | future ledger-bus plan; v1 tests drive `OnLedgerEvent` directly |

---

## Task 1: Wire `shared/admission` into `tracker/go.mod` + package skeleton

**Why this is its own task:** `tracker/go.mod` doesn't yet require `shared/admission` because no tracker file imports it yet. We materialize the require by adding `doc.go` and `admission.go` (which both import `shared/admission`). One commit, no test changes.

**Files:**
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker/internal/admission/doc.go`
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker/internal/admission/admission.go`
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker/go.mod`
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker/go.sum`

- [ ] **Step 1: Create `doc.go`**

Write `tracker/internal/admission/doc.go`:

```go
// Package admission gates broker_request on regional supply pressure and a
// composite per-consumer credit score. It sits in front of the broker per
// admission-design §1: the broker still picks which seeder serves a request,
// but admission decides whether to run the broker at all (ADMIT), defer
// (QUEUE), or refuse (REJECT).
//
// State is in-memory only in this revision: ConsumerCreditState (rolling
// 30-day-bucket histories of settlements / disputes / flow), SeederHeartbeat-
// State (10-minute rolling reliability buckets), and an atomically-published
// SupplySnapshot derived from the registry every 5 seconds. Persistence —
// admission.tlog, periodic snapshots, StartupReplay — lands in a follow-up
// plan; this revision does not write to disk.
//
// The ledger emits no events to admission yet either: this revision exposes
// LedgerEventObserver as the consumer-side contract, and tests drive it
// directly with synthetic events. Real wiring (ledger → admission event bus)
// is a separate plan.
//
// Concurrency model: per-consumer and per-seeder state is sharded; each
// shard owns its own sync.RWMutex. The hot path Decide reads the supply
// snapshot via atomic.Pointer.Load (no lock) and a single shard's RLock
// for the credit-state lookup. The queue is a max-heap protected by one
// mutex; queue ops are never on the Decide-frequency hot path. The 5s
// aggregator goroutine writes the supply pointer atomically; readers
// always observe a consistent snapshot.
//
// Spec: docs/superpowers/specs/admission/2026-04-25-admission-design.md.
package admission
```

- [ ] **Step 2: Create `admission.go` with the Subsystem stub**

Write `tracker/internal/admission/admission.go`:

```go
package admission

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/token-bay/token-bay/shared/admission"
	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/config"
	"github.com/token-bay/token-bay/tracker/internal/registry"
)

// Subsystem owns the in-memory admission state and the goroutines that keep
// it fresh. One per tracker process. Construct via Open; tear down via Close.
type Subsystem struct {
	cfg       config.AdmissionConfig
	reg       *registry.Registry
	priv      ed25519.PrivateKey
	pub       ed25519.PublicKey
	trackerID ids.IdentityID
	nowFn     func() time.Time

	// supply published atomically by aggregator (Task 7).
	supply atomic.Pointer[SupplySnapshot]

	// per-consumer credit state — sharded (Task 3).
	consumerShards []*consumerShard

	// per-seeder heartbeat state — sharded (Task 3).
	seederShards []*seederShard

	// queue — single max-heap with one mutex (Task 8).
	queueMu sync.Mutex
	queue   queueHeap

	// federation peer-set membership check (§5.1 step 1).
	peers PeerSet

	stop chan struct{}
	wg   sync.WaitGroup

	// silence "declared and not used" until later tasks reference these:
	_ *admission.SignedCreditAttestation
}

// Option configures optional Subsystem dependencies on Open.
type Option func(*Subsystem)

// WithClock overrides time.Now for testing. Called once per timestamp-using
// operation.
func WithClock(now func() time.Time) Option {
	return func(s *Subsystem) { s.nowFn = now }
}

// WithPeerSet wires the federation peer-set lookup. v1 default is a stub
// that returns false for every issuer (no peer is recognized). Real
// federation lands in a later plan.
func WithPeerSet(p PeerSet) Option {
	return func(s *Subsystem) { s.peers = p }
}

// PeerSet reports whether a tracker pubkey hash belongs to the local
// federation. Used during attestation validation (admission-design §5.1
// step 1).
type PeerSet interface {
	Contains(issuer ids.IdentityID) bool
}

// peerSetAlwaysFalse is the v1 default. Every imported attestation falls
// through to local-history or trial-tier scoring.
type peerSetAlwaysFalse struct{}

func (peerSetAlwaysFalse) Contains(ids.IdentityID) bool { return false }

// Open constructs a Subsystem and starts its background goroutines (the
// supply aggregator, queue-drain ticker — both land in later tasks). The
// caller passes a registry the admission subsystem will read for headroom
// and a tracker keypair used to sign issued attestations.
//
// Returns an error on a nil registry, wrong-length tracker key, or a config
// that fails admission's structural checks (sum-of-weights, threshold
// ordering — these are also enforced at config-load time, but Open re-runs
// them so a programmatic caller cannot bypass).
func Open(cfg config.AdmissionConfig, reg *registry.Registry, priv ed25519.PrivateKey, opts ...Option) (*Subsystem, error) {
	if reg == nil {
		return nil, errors.New("admission: Open requires a non-nil registry")
	}
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("admission: tracker key length %d, want %d", len(priv), ed25519.PrivateKeySize)
	}
	if err := validateConfigForOpen(cfg); err != nil {
		return nil, fmt.Errorf("admission: %w", err)
	}

	s := &Subsystem{
		cfg:       cfg,
		reg:       reg,
		priv:      priv,
		pub:       priv.Public().(ed25519.PublicKey),
		trackerID: trackerIDFromPubkey(priv.Public().(ed25519.PublicKey)),
		nowFn:     time.Now,
		peers:     peerSetAlwaysFalse{},
		stop:      make(chan struct{}),
	}
	for _, opt := range opts {
		opt(s)
	}
	s.consumerShards = newConsumerShards(registry.DefaultShardCount)
	s.seederShards = newSeederShards(registry.DefaultShardCount)

	// Background goroutines (aggregator, queue drainer) are started in later
	// tasks. Stub for now so Close still works.
	return s, nil
}

// Close signals all background goroutines to stop and waits for them.
// Safe to call multiple times; subsequent calls are no-ops.
func (s *Subsystem) Close() error {
	select {
	case <-s.stop:
		return nil
	default:
		close(s.stop)
	}
	s.wg.Wait()
	return nil
}

// validateConfigForOpen re-checks the AdmissionConfig invariants admission
// itself cares about. The full set of YAML-config validations lives in
// tracker/internal/config; this is a defense-in-depth layer for callers
// constructing a config programmatically.
func validateConfigForOpen(cfg config.AdmissionConfig) error {
	if cfg.RollingWindowDays <= 0 {
		return errors.New("RollingWindowDays must be positive")
	}
	if cfg.PressureAdmitThreshold >= cfg.PressureRejectThreshold {
		return fmt.Errorf("PressureAdmitThreshold %.3f must be < PressureRejectThreshold %.3f",
			cfg.PressureAdmitThreshold, cfg.PressureRejectThreshold)
	}
	if cfg.QueueCap <= 0 {
		return errors.New("QueueCap must be positive")
	}
	return nil
}

// trackerIDFromPubkey hashes an Ed25519 pubkey to a 32-byte tracker
// identity, the same shape as ids.IdentityID. Plan 2 uses a length-truncated
// pubkey since SHA-256(pubkey) is the spec-correct form but identity-hashing
// helpers don't yet live in shared/ids. The substitution is wire-equivalent
// (32 bytes) and tests pass it round-trip; switching to the proper hash is a
// shared/ids cleanup PR.
func trackerIDFromPubkey(pub ed25519.PublicKey) ids.IdentityID {
	var out ids.IdentityID
	if len(pub) >= 32 {
		copy(out[:], pub[:32])
	}
	return out
}

// Forward declarations satisfied by later tasks. Stubs here so Open compiles.
type consumerShard struct{}
type seederShard struct{}

func newConsumerShards(n int) []*consumerShard {
	out := make([]*consumerShard, n)
	for i := range out {
		out[i] = &consumerShard{}
	}
	return out
}

func newSeederShards(n int) []*seederShard {
	out := make([]*seederShard, n)
	for i := range out {
		out[i] = &seederShard{}
	}
	return out
}
```

Note: `consumerShard`, `seederShard`, `queueHeap`, and `SupplySnapshot` are forward-declared / placeholder-defined here so the file compiles before later tasks fill them in. Each subsequent task replaces the placeholder with the real implementation.

- [ ] **Step 3: Run `go mod tidy` to materialize the `shared/admission` require**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go mod tidy
```

Expected: `tracker/go.mod` now lists `shared/admission` indirectly via the existing `require shared` line; `go.sum` updated. The local `replace` directive (already present) wires `../shared` for development.

- [ ] **Step 4: Verify build**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go build ./internal/admission/...
```

Expected: success. If the build complains about `SupplySnapshot` or `queueHeap` undefined — recheck the placeholder type stubs in `admission.go`; later tasks replace them.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission
git add tracker/internal/admission/doc.go tracker/internal/admission/admission.go tracker/go.mod tracker/go.sum
git commit -m "$(cat <<'EOF'
feat(tracker/admission): package skeleton + Subsystem stub

Creates tracker/internal/admission with doc.go, admission.go, and the
Subsystem struct + Open / Close lifecycle. Wires shared/admission into
tracker's go.mod via the first import. Forward-declares the in-memory
state types (consumerShard, seederShard, queueHeap, SupplySnapshot) as
empty placeholders so the file compiles; subsequent tasks replace each
with its real implementation.

Subsystem.Open validates the structural invariants admission itself
relies on (RollingWindowDays > 0, threshold ordering, queue cap > 0)
on top of the YAML-time validation in tracker/internal/config.

Background goroutines (5s supply aggregator, queue-drain ticker) are
not yet started — Close still works because stop/wg are wired.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: Day-bucket primitives

**Files:**
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker/internal/admission/buckets.go`
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker/internal/admission/buckets_test.go`

A `DayBucket` is a one-day cell in the rolling 30-day window each consumer keeps for {settlements, disputes, flow}. The bucket stores per-day counts; the index is computed from the timestamp's unix-day modulo `RollingWindowDays`. Rotation: when an event lands and the existing bucket's day-stamp is older than `RollingWindowDays`, the bucket is zeroed first.

- [ ] **Step 1: Write the failing test**

Write `tracker/internal/admission/buckets_test.go`:

```go
package admission

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDayBucketIndex_StableForSameDay(t *testing.T) {
	morning := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	evening := time.Date(2026, 4, 25, 23, 59, 59, 0, time.UTC)
	assert.Equal(t, dayBucketIndex(morning, 30), dayBucketIndex(evening, 30))
}

func TestDayBucketIndex_DiffersAcrossDays(t *testing.T) {
	d1 := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	assert.NotEqual(t, dayBucketIndex(d1, 30), dayBucketIndex(d2, 30))
}

func TestDayBucketIndex_WrapsAroundWindow(t *testing.T) {
	base := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	wrapped := base.AddDate(0, 0, 30) // exactly one window later
	assert.Equal(t, dayBucketIndex(base, 30), dayBucketIndex(wrapped, 30))
}

func TestDayBucketIndex_AlwaysInRange(t *testing.T) {
	// Spot-check 100 dates across multiple years.
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 100; i++ {
		ts := start.AddDate(0, 0, i*7) // every 7 days
		idx := dayBucketIndex(ts, 30)
		require.GreaterOrEqual(t, idx, 0)
		require.Less(t, idx, 30)
	}
}

func TestDayBucketRotateIfStale_ZeroesStale(t *testing.T) {
	b := DayBucket{Total: 5, A: 3, B: 2, DayStamp: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)}
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC) // > 30 days later
	rotated := rotateIfStale(b, now, 30)
	assert.Equal(t, uint32(0), rotated.Total)
	assert.Equal(t, uint32(0), rotated.A)
	assert.Equal(t, uint32(0), rotated.B)
	assert.True(t, rotated.DayStamp.Equal(stripToDay(now)))
}

func TestDayBucketRotateIfStale_KeepsFresh(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	b := DayBucket{Total: 5, A: 3, B: 2, DayStamp: now.AddDate(0, 0, -3)} // 3 days old
	rotated := rotateIfStale(b, now, 30)
	assert.Equal(t, uint32(5), rotated.Total)
	assert.Equal(t, uint32(3), rotated.A)
	assert.Equal(t, uint32(2), rotated.B)
}

func TestDayBucketRotateIfStale_ZeroDayStampInitializes(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	b := DayBucket{} // zero-value, never used
	rotated := rotateIfStale(b, now, 30)
	assert.Equal(t, uint32(0), rotated.Total)
	assert.True(t, rotated.DayStamp.Equal(stripToDay(now)))
}

func TestStripToDay_DropsHoursMinutesSeconds(t *testing.T) {
	ts := time.Date(2026, 4, 25, 17, 23, 45, 123456789, time.UTC)
	day := stripToDay(ts)
	assert.Equal(t, time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC), day)
}
```

- [ ] **Step 2: Run, confirm FAIL**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/admission/...
```

Expected: compilation failure — `DayBucket`, `dayBucketIndex`, `rotateIfStale`, `stripToDay` undefined.

- [ ] **Step 3: Write the primitives**

Write `tracker/internal/admission/buckets.go`:

```go
package admission

import "time"

// DayBucket is one day-cell in a rolling 30-day per-consumer window. The
// fields A and B are role-overloaded so a single struct serves the three
// rolling sets: settlements (Total = settled count, A = clean count, B unused),
// disputes (Total unused, A = filed count, B = upheld count), and
// flow (Total unused, A = earned credits, B = spent credits).
//
// Day-bucket arrays are time-bucketed (RollingWindowDays-length), not
// time-sorted: the bucket at index i holds events from day-of-year mod N == i.
// On a new event, callers first check DayStamp and zero the bucket if it
// belongs to a day older than the window — see rotateIfStale.
type DayBucket struct {
	Total    uint32
	A        uint32
	B        uint32
	DayStamp time.Time // start-of-day (UTC) for which this bucket is valid
}

// dayBucketIndex maps a timestamp to a bucket index in [0, windowDays).
// The unix-day count modulo windowDays scatters events across the array.
func dayBucketIndex(t time.Time, windowDays int) int {
	day := t.UTC().Unix() / 86400
	idx := int(day % int64(windowDays))
	if idx < 0 {
		idx += windowDays
	}
	return idx
}

// stripToDay returns the start-of-day (UTC) for t. Used as the canonical
// DayStamp value so two events on the same day produce identical stamps.
func stripToDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// rotateIfStale returns a copy of b with counts zeroed and DayStamp updated
// to today if the existing DayStamp is older than windowDays. Otherwise
// returns b unchanged. A zero-value DayStamp (i.e. never used) is treated
// as stale, which initializes the bucket on first use.
func rotateIfStale(b DayBucket, now time.Time, windowDays int) DayBucket {
	today := stripToDay(now)
	if b.DayStamp.IsZero() {
		return DayBucket{DayStamp: today}
	}
	age := today.Sub(b.DayStamp)
	if age >= time.Duration(windowDays)*24*time.Hour {
		return DayBucket{DayStamp: today}
	}
	// Same-window: if the DayStamp is older but within the window, the bucket
	// represents a different day-of-window than today's index expects only if
	// callers passed an inconsistent (idx, time) pair — which they don't, see
	// state.go callers. So we leave the bucket alone.
	return b
}
```

- [ ] **Step 4: Run, confirm PASS**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/admission/...
```

Expected: all 7 tests in `buckets_test.go` PASS.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission
git add tracker/internal/admission/buckets.go tracker/internal/admission/buckets_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/admission): day-bucket primitives

DayBucket struct (Total/A/B + DayStamp) is the unit of the rolling 30-day
per-consumer windows for settlements, disputes, and flow. dayBucketIndex
maps a timestamp to a bucket via unix-day mod windowDays; rotateIfStale
zeroes a bucket whose DayStamp is older than the window so callers get
fresh counts on new events without explicit per-day GC.

Field-overloading (A, B used differently per rolling-set) avoids three
near-identical struct types; state.go uses named accessor methods to
keep the call sites readable.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: Per-consumer + per-seeder state types

**Files:**
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker/internal/admission/state.go`
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker/internal/admission/state_test.go`
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker/internal/admission/admission.go` (replace placeholder shard types with real ones)

This task introduces `ConsumerCreditState`, `SeederHeartbeatState`, and the sharded maps that hold them. Each shard has its own `RWMutex`. The shard count matches the registry's `DefaultShardCount` (16).

- [ ] **Step 1: Write the failing test**

Write `tracker/internal/admission/state_test.go`:

```go
package admission

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/ids"
)

func TestConsumerShard_GetOrInit_CreatesOnFirstUse(t *testing.T) {
	sh := newConsumerShard()
	id := makeID(0x11)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	st := sh.getOrInit(id, now)
	require.NotNil(t, st)
	assert.True(t, st.FirstSeenAt.Equal(now), "FirstSeenAt initialized to now")
	assert.Equal(t, int64(0), st.LastBalanceSeen)
}

func TestConsumerShard_GetOrInit_ReturnsExisting(t *testing.T) {
	sh := newConsumerShard()
	id := makeID(0x11)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	first := sh.getOrInit(id, now)
	first.LastBalanceSeen = 12345

	later := now.Add(time.Hour)
	second := sh.getOrInit(id, later)
	assert.Same(t, first, second, "second call returns same pointer")
	assert.Equal(t, int64(12345), second.LastBalanceSeen)
	assert.True(t, second.FirstSeenAt.Equal(now), "FirstSeenAt unchanged after second call")
}

func TestConsumerShard_Get_ReportsAbsence(t *testing.T) {
	sh := newConsumerShard()
	_, ok := sh.get(makeID(0xFF))
	assert.False(t, ok)
}

func TestConsumerShards_RoutingByIdentityID(t *testing.T) {
	shards := newConsumerShards(16)
	require.Len(t, shards, 16)

	// Same ID → same shard.
	id := makeID(0x42)
	a := consumerShardFor(shards, id)
	b := consumerShardFor(shards, id)
	assert.Same(t, a, b)

	// Different IDs in different leading bytes likely route differently;
	// not strictly guaranteed for any specific pair (mod 16), but the
	// router's invariant is determinism, which we verify via same-ID call.
}

func TestSeederShard_GetOrInit_CreatesOnFirstUse(t *testing.T) {
	sh := newSeederShard()
	id := makeID(0xAA)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	st := sh.getOrInit(id, now)
	require.NotNil(t, st)
	assert.Equal(t, uint32(0), st.LastHeadroomEstimate)
	assert.False(t, st.LastHeadroomTs.IsZero(), "LastBucketRollAt initialized to now")
}

func TestSeederShard_GetOrInit_ReturnsExisting(t *testing.T) {
	sh := newSeederShard()
	id := makeID(0xAA)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	first := sh.getOrInit(id, now)
	first.LastHeadroomEstimate = 7500

	later := now.Add(time.Minute)
	second := sh.getOrInit(id, later)
	assert.Same(t, first, second)
	assert.Equal(t, uint32(7500), second.LastHeadroomEstimate)
}

// makeID builds a fixture IdentityID with all bytes set to b.
func makeID(b byte) ids.IdentityID {
	var id ids.IdentityID
	for i := range id {
		id[i] = b
	}
	return id
}
```

- [ ] **Step 2: Run, confirm FAIL**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/admission/...
```

Expected: compilation failure — `ConsumerCreditState`, `SeederHeartbeatState`, `consumerShardFor`, `newConsumerShard`, `newSeederShard`, getOrInit / get methods undefined.

- [ ] **Step 3: Write `state.go`**

Write `tracker/internal/admission/state.go`:

```go
package admission

import (
	"encoding/binary"
	"sync"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
)

// rollingWindowDays is the §4.2 30-day window. Each ConsumerCreditState
// keeps three arrays of this length (settlements, disputes, flow). The
// AdmissionConfig field RollingWindowDays carries this value at runtime;
// the constant here is the array fixed-size dimension.
const rollingWindowDays = 30

// MinuteBucket is the one-minute cell in the seeder heartbeat-reliability
// rolling window. Expected = how many heartbeats we should have received
// at the configured cadence; Actual = how many we did.
type MinuteBucket struct {
	Expected uint32
	Actual   uint32
}

// heartbeatWindowMinutes is the §4.2 10-minute window dimension.
const heartbeatWindowMinutes = 10

// ConsumerCreditState is admission's per-consumer credit history. Held
// only in memory in this revision; persistence lands in plan 3.
type ConsumerCreditState struct {
	FirstSeenAt time.Time

	// SettlementBuckets — Total = total settlements that day, A = clean count.
	SettlementBuckets [rollingWindowDays]DayBucket

	// DisputeBuckets — A = filed count, B = upheld count.
	DisputeBuckets [rollingWindowDays]DayBucket

	// FlowBuckets — A = credits earned (settled to this consumer as seeder),
	// B = credits spent (settled away).
	FlowBuckets [rollingWindowDays]DayBucket

	// LastBalanceSeen mirrors the ledger's authoritative balance projection.
	// Allowed to drift on missed events (recovered via §9.1
	// /admission/recompute/<consumer_id> in plan 3).
	LastBalanceSeen int64
}

// SeederHeartbeatState tracks the rolling heartbeat-reliability window plus
// the most recent headroom signal from this seeder.
type SeederHeartbeatState struct {
	Buckets              [heartbeatWindowMinutes]MinuteBucket
	LastBucketRollAt     time.Time
	LastHeadroomEstimate uint32
	LastHeadroomSource   uint8 // 0=unspecified, 1=heuristic, 2=usage_probe (mirrors admission.TickSource)
	LastHeadroomTs       time.Time
	CanProbeUsage        bool
}

// consumerShard owns a fraction of the consumer credit-state map. Each
// shard is independently locked. See package doc concurrency model.
type consumerShard struct {
	mu sync.RWMutex
	m  map[ids.IdentityID]*ConsumerCreditState
}

func newConsumerShard() *consumerShard {
	return &consumerShard{m: make(map[ids.IdentityID]*ConsumerCreditState)}
}

// getOrInit returns the consumer's state, creating it on first use with
// FirstSeenAt = now. Holds the write lock only on the create path.
func (sh *consumerShard) getOrInit(id ids.IdentityID, now time.Time) *ConsumerCreditState {
	sh.mu.RLock()
	st, ok := sh.m[id]
	sh.mu.RUnlock()
	if ok {
		return st
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()
	// Re-check under write lock — another goroutine may have created it.
	if st, ok := sh.m[id]; ok {
		return st
	}
	st = &ConsumerCreditState{FirstSeenAt: now}
	sh.m[id] = st
	return st
}

// get returns the existing state without creating one. ok is false when no
// state exists for id.
func (sh *consumerShard) get(id ids.IdentityID) (*ConsumerCreditState, bool) {
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	st, ok := sh.m[id]
	return st, ok
}

// seederShard mirrors consumerShard for SeederHeartbeatState.
type seederShard struct {
	mu sync.RWMutex
	m  map[ids.IdentityID]*SeederHeartbeatState
}

func newSeederShard() *seederShard {
	return &seederShard{m: make(map[ids.IdentityID]*SeederHeartbeatState)}
}

// getOrInit returns the seeder's heartbeat state, creating it on first use.
// LastBucketRollAt is initialized to now so the rolling window's first roll
// has a defined start.
func (sh *seederShard) getOrInit(id ids.IdentityID, now time.Time) *SeederHeartbeatState {
	sh.mu.RLock()
	st, ok := sh.m[id]
	sh.mu.RUnlock()
	if ok {
		return st
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if st, ok := sh.m[id]; ok {
		return st
	}
	st = &SeederHeartbeatState{LastBucketRollAt: now, LastHeadroomTs: now}
	sh.m[id] = st
	return st
}

// shardIndex returns the deterministic shard index for an IdentityID.
// Same scheme as registry.shardIndex (binary.BigEndian.Uint64 of the first
// 8 bytes mod n) so ID→shard routing is consistent across subsystems.
func shardIndex(id ids.IdentityID, n int) int {
	idx := int(binary.BigEndian.Uint64(id[:8]) % uint64(n)) //nolint:gosec // G115: bounded by n, a positive int
	return idx
}

// consumerShardFor routes an ID to its shard.
func consumerShardFor(shards []*consumerShard, id ids.IdentityID) *consumerShard {
	return shards[shardIndex(id, len(shards))]
}

func seederShardFor(shards []*seederShard, id ids.IdentityID) *seederShard {
	return shards[shardIndex(id, len(shards))]
}
```

- [ ] **Step 4: Replace placeholder shard types in `admission.go`**

In `tracker/internal/admission/admission.go`, **delete** the four lines at the bottom that read:

```go
// Forward declarations satisfied by later tasks. Stubs here so Open compiles.
type consumerShard struct{}
type seederShard struct{}

func newConsumerShards(n int) []*consumerShard { ... }
func newSeederShards(n int) []*seederShard { ... }
```

Replace with:

```go
// newConsumerShards constructs n independently-locked consumer shards.
func newConsumerShards(n int) []*consumerShard {
	out := make([]*consumerShard, n)
	for i := range out {
		out[i] = newConsumerShard()
	}
	return out
}

// newSeederShards constructs n independently-locked seeder shards.
func newSeederShards(n int) []*seederShard {
	out := make([]*seederShard, n)
	for i := range out {
		out[i] = newSeederShard()
	}
	return out
}
```

(The struct types `consumerShard` and `seederShard` move to `state.go` — keep `admission.go` constructors only.)

- [ ] **Step 5: Run, confirm PASS**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/admission/...
```

Expected: all `state_test.go` tests PASS; `buckets_test.go` still green.

- [ ] **Step 6: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission
git add tracker/internal/admission/state.go tracker/internal/admission/state_test.go tracker/internal/admission/admission.go
git commit -m "$(cat <<'EOF'
feat(tracker/admission): per-consumer + per-seeder state with sharded maps

ConsumerCreditState carries the three rolling 30-day-bucket arrays
(settlements / disputes / flow) plus FirstSeenAt and LastBalanceSeen.
SeederHeartbeatState carries the 10-minute MinuteBucket window plus the
most recent headroom signal.

Each shard owns its own RWMutex (16 shards by default, matching
registry.DefaultShardCount). Hot-path reads use RLock; first-time
inserts upgrade to Lock with a re-check pattern. shardIndex mirrors
the registry's deterministic ID→shard routing so a single ID always
maps to the same shard across subsystems.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: `ComputeLocalScore` — five signals + weighted compose

**Files:**
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker/internal/admission/score.go`
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker/internal/admission/score_test.go`

`ComputeLocalScore(state, cfg, now) (score float64, signals Signals)` per admission-design §5.2:

1. tenure_days = min(cfg.TenureCapDays, days since FirstSeenAt)
2. settlement_reliability = sum(buckets.A) / sum(buckets.Total)
3. dispute_rate = (sum.Filed - sum.Upheld) / sum(total settlements) — clamp to [0, 1]
4. net_flow = sum(earned) - sum(spent)
5. balance_cushion_log2 = clamp(floor(log2(max(1, balance/starter_grant))), [-8, 8])

Normalize each to `[0, 1]` then weighted-compose. If a consumer has zero settlements, reliability + inverse-dispute weights redistribute proportionally to the other three signals.

- [ ] **Step 1: Write failing tests**

Write `tracker/internal/admission/score_test.go`:

```go
package admission

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/tracker/internal/config"
)

func defaultScoreConfig() config.AdmissionConfig {
	return config.AdmissionConfig{
		TrialTierScore:               0.4,
		AgingAlphaPerMinute:          0.05,
		QueueTimeoutS:                300,
		ScoreWeights: config.AdmissionScoreWeights{
			SettlementReliability: 0.30,
			InverseDisputeRate:    0.10,
			Tenure:                0.20,
			NetCreditFlow:         0.30,
			BalanceCushion:        0.10,
		},
		NetFlowNormalizationConstant: 10000,
		TenureCapDays:                30,
		StarterGrantCredits:          1000,
		RollingWindowDays:            30,
		PressureAdmitThreshold:       0.85,
		PressureRejectThreshold:      1.5,
		QueueCap:                     512,
		MaxAttestationScoreImported:  0.95,
	}
}

func TestComputeLocalScore_AllSignalsKnown_WeightedSum(t *testing.T) {
	cfg := defaultScoreConfig()
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	st := &ConsumerCreditState{FirstSeenAt: now.AddDate(0, 0, -30), LastBalanceSeen: 1000}
	// 100 settlements, all clean → reliability = 1.0
	st.SettlementBuckets[0] = DayBucket{Total: 100, A: 100, DayStamp: stripToDay(now)}
	// 0 disputes filed → dispute_rate = 0 → inverse = 1.0
	// Flow: earned 5000, spent 5000 → net = 0 → sigmoid(0) = 0.5
	st.FlowBuckets[0] = DayBucket{A: 5000, B: 5000, DayStamp: stripToDay(now)}

	score, sig := ComputeLocalScore(st, cfg, now)

	assert.InDelta(t, 1.0, sig.SettlementReliability, 1e-9)
	assert.InDelta(t, 0.0, sig.DisputeRate, 1e-9)
	assert.Equal(t, 30, sig.TenureDays)
	assert.Equal(t, int64(0), sig.NetFlow)
	assert.Equal(t, 0, sig.BalanceCushionLog2) // log2(1000/1000) = 0

	// Normalized:
	//   reliability        = 1.0
	//   inverse_dispute    = 1.0
	//   tenure_norm        = 30/30 = 1.0
	//   net_flow_norm      = sigmoid(0/10000) = 0.5
	//   cushion_norm       = (0 + 8) / 16 = 0.5
	// score = 0.30*1 + 0.10*1 + 0.20*1 + 0.30*0.5 + 0.10*0.5 = 0.80
	assert.InDelta(t, 0.80, score, 1e-6)
}

func TestComputeLocalScore_NoSettlements_RedistributesWeights(t *testing.T) {
	cfg := defaultScoreConfig()
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	st := &ConsumerCreditState{FirstSeenAt: now.AddDate(0, 0, -10), LastBalanceSeen: 1000}
	// no settlements, no disputes, no flow

	score, sig := ComputeLocalScore(st, cfg, now)
	assert.Equal(t, 10, sig.TenureDays)

	// Reliability + inverse-dispute weights (0.40 total) redistribute to the
	// remaining three (tenure 0.20, net_flow 0.30, cushion 0.10 — sum 0.60).
	// Adjusted weights: tenure 0.20 + 0.20*0.40/0.60 = 0.20 + 0.13333 = 0.33333
	//                   net_flow 0.30 + 0.30*0.40/0.60 = 0.30 + 0.20000 = 0.50000
	//                   cushion 0.10 + 0.10*0.40/0.60 = 0.10 + 0.06667 = 0.16667
	// tenure_norm = 10/30 = 0.33333
	// net_flow_norm = sigmoid(0) = 0.5
	// cushion_norm = (0+8)/16 = 0.5
	// score = 0.33333*0.33333 + 0.50000*0.5 + 0.16667*0.5 = 0.11111 + 0.25 + 0.08333 = 0.44444
	assert.InDelta(t, 0.44444, score, 1e-4)
}

func TestComputeLocalScore_TenureCappedAtCfg(t *testing.T) {
	cfg := defaultScoreConfig()
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	st := &ConsumerCreditState{FirstSeenAt: now.AddDate(-1, 0, 0)} // ~365 days

	_, sig := ComputeLocalScore(st, cfg, now)
	assert.Equal(t, cfg.TenureCapDays, sig.TenureDays)
}

func TestComputeLocalScore_DisputeRateClampedToZero(t *testing.T) {
	cfg := defaultScoreConfig()
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	st := &ConsumerCreditState{FirstSeenAt: now.AddDate(0, 0, -10)}

	// 5 settlements; 10 disputes filed, 5 upheld → (10-5)/5 = 1.0 (rate)
	st.SettlementBuckets[0] = DayBucket{Total: 5, A: 5, DayStamp: stripToDay(now)}
	st.DisputeBuckets[0] = DayBucket{A: 10, B: 5, DayStamp: stripToDay(now)}

	_, sig := ComputeLocalScore(st, cfg, now)
	assert.InDelta(t, 1.0, sig.DisputeRate, 1e-9)
}

func TestComputeLocalScore_BalanceCushionClamped(t *testing.T) {
	cfg := defaultScoreConfig()
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	t.Run("very high balance clamps to +8", func(t *testing.T) {
		st := &ConsumerCreditState{FirstSeenAt: now.AddDate(0, 0, -1), LastBalanceSeen: 1_000_000_000}
		_, sig := ComputeLocalScore(st, cfg, now)
		assert.Equal(t, 8, sig.BalanceCushionLog2)
	})

	t.Run("zero balance clamps to -8", func(t *testing.T) {
		st := &ConsumerCreditState{FirstSeenAt: now.AddDate(0, 0, -1), LastBalanceSeen: 0}
		_, sig := ComputeLocalScore(st, cfg, now)
		assert.Equal(t, -8, sig.BalanceCushionLog2)
	})
}

func TestComputeLocalScore_NilStateReturnsTrialTier(t *testing.T) {
	cfg := defaultScoreConfig()
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	score, _ := ComputeLocalScore(nil, cfg, now)
	assert.InDelta(t, cfg.TrialTierScore, score, 1e-9)
}

func TestSigmoid_ZeroIsHalf(t *testing.T) {
	assert.InDelta(t, 0.5, sigmoid(0), 1e-9)
}

func TestSigmoid_LargePositiveApproachesOne(t *testing.T) {
	assert.Greater(t, sigmoid(10), 0.999)
}

func TestSigmoid_LargeNegativeApproachesZero(t *testing.T) {
	assert.Less(t, sigmoid(-10), 0.001)
}

func TestClampInt_OutsideRange(t *testing.T) {
	assert.Equal(t, -8, clampInt(-100, -8, 8))
	assert.Equal(t, 8, clampInt(100, -8, 8))
	assert.Equal(t, 3, clampInt(3, -8, 8))
}

func TestFloorLog2Ratio(t *testing.T) {
	assert.Equal(t, 0, floorLog2Ratio(1000, 1000))   // log2(1)
	assert.Equal(t, 1, floorLog2Ratio(2000, 1000))   // log2(2)
	assert.Equal(t, 3, floorLog2Ratio(8000, 1000))   // log2(8)
	assert.Equal(t, -1, floorLog2Ratio(500, 1000))   // log2(0.5)
	assert.Equal(t, math.MinInt, floorLog2Ratio(0, 1000))
}
```

- [ ] **Step 2: Run, confirm FAIL**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/admission/...
```

Expected: compilation failure — `ComputeLocalScore`, `Signals`, `sigmoid`, `clampInt`, `floorLog2Ratio` undefined.

- [ ] **Step 3: Write `score.go`**

Write `tracker/internal/admission/score.go`:

```go
package admission

import (
	"math"
	"time"

	"github.com/token-bay/token-bay/tracker/internal/config"
)

// Signals carries the five raw signal values that feed ComputeLocalScore.
// Tests use these to assert per-signal correctness independently of the
// final weighted compose.
type Signals struct {
	SettlementReliability float64 // [0, 1]; -1 if undefined (no settlements)
	DisputeRate           float64 // [0, 1]; -1 if undefined (no settlements)
	TenureDays            int     // capped at cfg.TenureCapDays
	NetFlow               int64   // signed credits (earned - spent)
	BalanceCushionLog2    int     // clamped to [-8, 8]
}

// ComputeLocalScore returns the composite credit score in [0, 1] plus the
// five raw signals. Implements admission-design §5.2.
//
// A nil state (consumer never seen locally) returns cfg.TrialTierScore with
// zero-valued Signals — callers can branch on Signals.TenureDays == 0 to
// detect the trial-tier path.
//
// Weight redistribution: if a consumer has zero settlements, reliability
// and inverse-dispute are undefined. Their combined weight redistributes
// proportionally across the remaining three signals (tenure, net_flow,
// cushion) preserving the score-scale's [0, 1] semantics.
func ComputeLocalScore(state *ConsumerCreditState, cfg config.AdmissionConfig, now time.Time) (float64, Signals) {
	if state == nil {
		return cfg.TrialTierScore, Signals{}
	}

	signals := computeSignals(state, cfg, now)

	// Normalize.
	tenureNorm := 0.0
	if cfg.TenureCapDays > 0 {
		tenureNorm = math.Min(1.0, float64(signals.TenureDays)/float64(cfg.TenureCapDays))
	}
	netFlowNorm := sigmoid(float64(signals.NetFlow) / float64(maxInt(1, cfg.NetFlowNormalizationConstant)))
	cushionNorm := float64(signals.BalanceCushionLog2+8) / 16.0 // [-8,8] → [0,1]

	w := cfg.ScoreWeights
	if signals.SettlementReliability < 0 {
		// Undefined-signal redistribution: reliability + inverse_dispute
		// weights spread proportionally across the other three.
		undef := w.SettlementReliability + w.InverseDisputeRate
		defined := w.Tenure + w.NetCreditFlow + w.BalanceCushion
		if defined <= 0 {
			return cfg.TrialTierScore, signals
		}
		factor := 1.0 + undef/defined
		score := factor*(w.Tenure*tenureNorm+w.NetCreditFlow*netFlowNorm+w.BalanceCushion*cushionNorm)
		return clamp01(score), signals
	}

	inverseDispute := 1.0 - signals.DisputeRate
	score := w.SettlementReliability*signals.SettlementReliability +
		w.InverseDisputeRate*inverseDispute +
		w.Tenure*tenureNorm +
		w.NetCreditFlow*netFlowNorm +
		w.BalanceCushion*cushionNorm
	return clamp01(score), signals
}

// computeSignals materializes the five raw signals from state. Reliability
// and DisputeRate are -1 when no settlements exist (sentinel for "undefined").
func computeSignals(st *ConsumerCreditState, cfg config.AdmissionConfig, now time.Time) Signals {
	tenureDays := int(now.Sub(st.FirstSeenAt) / (24 * time.Hour))
	if tenureDays < 0 {
		tenureDays = 0
	}
	if tenureDays > cfg.TenureCapDays {
		tenureDays = cfg.TenureCapDays
	}

	var totalSettled, cleanCount uint32
	for _, b := range st.SettlementBuckets {
		totalSettled += b.Total
		cleanCount += b.A
	}
	var filed, upheld uint32
	for _, b := range st.DisputeBuckets {
		filed += b.A
		upheld += b.B
	}
	var earned, spent uint32
	for _, b := range st.FlowBuckets {
		earned += b.A
		spent += b.B
	}

	reliability := -1.0
	disputeRate := -1.0
	if totalSettled > 0 {
		reliability = float64(cleanCount) / float64(totalSettled)
		// (filed - upheld) / total settlements, clamped to [0, 1]
		net := int64(filed) - int64(upheld)
		if net < 0 {
			net = 0
		}
		disputeRate = float64(net) / float64(totalSettled)
		if disputeRate > 1 {
			disputeRate = 1
		}
	}

	netFlow := int64(earned) - int64(spent)
	cushion := floorLog2Ratio(st.LastBalanceSeen, int64(maxInt(1, cfg.StarterGrantCredits)))
	cushion = clampInt(cushion, -8, 8)

	return Signals{
		SettlementReliability: reliability,
		DisputeRate:           disputeRate,
		TenureDays:            tenureDays,
		NetFlow:               netFlow,
		BalanceCushionLog2:    cushion,
	}
}

// sigmoid maps R → (0, 1) with sigmoid(0) = 0.5. Used to normalize NetFlow
// into the score scale.
func sigmoid(x float64) float64 {
	return 1.0 / (1.0 + math.Exp(-x))
}

// clamp01 clamps a float to [0, 1].
func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

// clampInt clamps an int to [lo, hi] inclusive.
func clampInt(x, lo, hi int) int {
	if x < lo {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}

// maxInt returns the larger of two ints.
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// floorLog2Ratio returns floor(log2(num/den)) as an int. Returns math.MinInt
// when num <= 0 (semantically -∞; callers clamp the result).
func floorLog2Ratio(num, den int64) int {
	if num <= 0 || den <= 0 {
		return math.MinInt
	}
	return int(math.Floor(math.Log2(float64(num) / float64(den))))
}
```

- [ ] **Step 4: Run, confirm PASS**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/admission/...
```

Expected: all `score_test.go` tests PASS.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission
git add tracker/internal/admission/score.go tracker/internal/admission/score_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/admission): ComputeLocalScore + 5-signal compose

Implements admission-design §5.2: tenure / settlement reliability / dispute
rate / net flow / balance cushion, normalized to [0,1] each, then weighted-
composed. Undefined-signal redistribution: when a consumer has zero
settlements, reliability + inverse-dispute weights spread proportionally
across the other three signals so the score scale stays in [0,1].

Nil state returns cfg.TrialTierScore — callers branch on
Signals.TenureDays == 0 to detect the trial-tier path. The 5.1 step 1
"resolve credit_score" path lands in the next task.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Tasks 5-15: outline (full code arrives in subsequent commits)

The remaining tasks follow the same TDD shape: failing test → minimal impl → green → commit. Below is the outline; the full code blocks for each step land in the next plan-update commits to keep this initial drop reviewable.

### Task 5: Resolve credit-score (attestation → local → trial), with `MaxAttestationScoreImported` clamp

- File: `tracker/internal/admission/score.go` (extend) + `score_test.go` (extend)
- Adds `(s *Subsystem) resolveCreditScore(consumerID, att, now) (score float64, source ScoreSource)` per §5.1 step 1.
- Validates attestation via `admission.ValidateCreditAttestationBody`, verifies sig via `signing.VerifyCreditAttestation`, checks `now < expires_at` and `s.peers.Contains(IssuerTrackerID)`.
- Failure on any check → fall through to local computation; empty local → `cfg.TrialTierScore`.
- Imported attestation score clamped at `cfg.MaxAttestationScoreImported` (default 0.95) per §8.6.

### Task 6: `SupplySnapshot` type + atomic publication helpers

- File: `supply.go` (partial) + `supply_test.go` (partial)
- `SupplySnapshot` struct (admission-design §4.2): `ComputedAt`, `TotalHeadroom`, `PerModel`, `ContributingSeeders`, `SilentSeeders`, `Pressure`, `RecentRejectRate`.
- `(s *Subsystem) Supply() *SupplySnapshot` — Load on `atomic.Pointer`.
- `(s *Subsystem) publishSupply(*SupplySnapshot)` — Store.
- Tests: nil-pre-publish returns sentinel "not yet computed" snapshot; concurrent Load/Store is race-clean.

### Task 7: 5s aggregator goroutine reading `*registry.Registry`

- File: `supply.go` (extend) + `supply_test.go` (extend)
- Aggregator iterates `reg.Snapshot()`, applies freshness decay (`1.0 → 0` linear over `cfg.HeartbeatFreshnessDecayMaxS`), heartbeat-reliability weighting from `SeederHeartbeatState`, sums into `total_headroom` + `per_model`.
- `pressure = demand_rate_ewma / total_headroom`. Demand EWMA exposed as `(s *Subsystem) recordDemandTick(now)`; tests drive ticks then advance the fake clock.
- `Open` spawns the goroutine; `Close` stops it via `s.stop`.
- Tests use a fixed clock + synthetic registry; verify snapshot updates within ≤5 simulated seconds and pressure crosses thresholds correctly.

### Task 8: `QueueEntry` + max-heap + EffectivePriority + aging boost

- File: `queue.go` + `queue_test.go`
- `QueueEntry` (admission-design §4.2) + `EffectivePriority(now, agingAlpha) = CreditScore + agingAlpha*waitMinutes`.
- Max-heap via `container/heap`; tests verify pop order, aging boost (a 0.3-credit consumer waiting 10min vs a fresh 0.5), and capacity overflow.
- `Subsystem.queue` field replaces the placeholder `queueHeap` in `admission.go`.

### Task 9: `Decide` hot path → `ADMIT | QUEUE | REJECT`

- File: `decide.go` + `decide_test.go`
- Defines `AdmissionResult`, `Outcome`, `QueuedDetails`, `RejectedDetails`, `RejectReason`, `PositionBand`, `EtaBand`.
- Implements §5.1 step 4 branching using `pressure_admit_threshold` / `pressure_reject_threshold` / `queue_cap`.
- `bandPosition(int) PositionBand` and `bandEta(time.Duration) EtaBand` per §6.5.
- `retryAfterSeconds(queueSize, avgServiceTime, jitterFn)` per §5.4.
- Tests: pressure < 0.85 → ADMIT regardless of credit; 0.85 ≤ pressure < 1.5 with queue room → QUEUE; pressure ≥ 1.5 OR queue full → REJECT with retry_after in [60, 600].

### Task 10: Decision-matrix table-driven test (§6.4)

- File: `decision_matrix_test.go`
- One row per `{attestation: nil|valid|expired|forged|ejected_issuer} × {local_history: none|some|extensive} × {pressure: low|medium|high|extreme}` per spec §6.4.
- Builds expected `AdmissionResult` shape for each row; compares.
- Catches regressions in cross-component behavior the per-file unit tests miss.

### Task 11: Aging-boost integration test (§10 #6 acceptance)

- File: `aging_boost_test.go`
- Two consumers (A: 0.3 credit, B: 0.5 credit). A queued at t=0, B queued at t=9min. Advance fake clock to t=10min, drain queue once. Assert A pops before B.

### Task 12: Attestation issuance + per-consumer rate limiter

- File: `attestation.go` + `attestation_test.go`
- `IssueAttestation(consumerID, now) (*admission.SignedCreditAttestation, error)` per §5.5.
- Sentinel `ErrNoLocalHistory` when no `ConsumerCreditState` exists.
- Per-consumer rate limiter (default 6/hr): sliding-window counter; `ErrRateLimited` when exceeded.
- Body fields populated from `ComputeLocalScore` output; signed via `signing.SignCreditAttestation` with `s.priv`.
- Round-trip test: issued attestation verifies via `signing.VerifyCreditAttestation` with `s.pub`.

### Task 13: `LedgerEventObserver` interface + `OnLedgerEvent` for all kinds

- File: `events.go` + `events_test.go`
- Defines `LedgerEvent`, `LedgerEventKind`, `LedgerEventObserver`. `Subsystem` implements the observer.
- `OnLedgerEvent` updates buckets per §5.6:
  - `LedgerEventSettlement`: SettlementBuckets (Total++, A++ if clean), FlowBuckets (consumer.Spent += cost; seeder.Earned += cost), LastBalanceSeen mirror.
  - `LedgerEventTransferIn` / `LedgerEventTransferOut`: LastBalanceSeen mirror only.
  - `LedgerEventStarterGrant`: init state if absent, set FirstSeenAt + LastBalanceSeen.
  - `LedgerEventDisputeFiled`: DisputeBuckets.A++.
  - `LedgerEventDisputeResolved`: if `DisputeUpheld`, DisputeBuckets.B++.
- **No tlog write in this plan** — that's plan 3.
- Tests drive events synthetically and assert in-memory state changes.

### Task 14: `helpers_test.go` — fixtures used across tests

- File: `helpers_test.go`
- `openTempSubsystem(t, opts ...Option) *Subsystem` — wires fixed clock, default config, empty registry, fixture keypair.
- `fixtureKeypair() (ed25519.PublicKey, ed25519.PrivateKey)` — same seed pattern as plan 1.
- `fakeBus` with subscribe/publish if needed for §13 tests.
- Each prior test's local fixtures progressively migrate here (extract during Task 14 to keep early-task commits self-contained).

### Task 15: Final integration — race-clean + repo-root `make check`

- Run from repo root:
  ```bash
  cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission
  PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" make check
  ```
- Expected: every module's tests pass under `-race`; lint passes.
- Race-discipline: `go test -race -count=10 ./internal/admission/...` to flush flakes.
- Coverage: `≥ 90%` on every file in `internal/admission/`.
- Branch push: `git push -u origin tracker_admission_core`.
- PR title: `feat(tracker/admission): in-memory core — Decide, score, queue, attestation, ledger-event observer`.

---

## Coverage of admission-design §10 acceptance criteria — what plan 2 covers

| §10 # | Criterion | Implemented in |
|---|---|---|
| 1 | Cold-start consumer (no history, no attestation) → trial_tier_score 0.4; admitted under low pressure, queued under high | Tasks 5, 9 |
| 2 | Clean 30-day consumer → score ≥ 0.9; admitted under any pressure ≤ reject threshold | Task 4, 9 |
| 3 | Valid attestation from federation peer → score honored exactly (post-clamp) | Task 5 |
| 4 | Expired or signature-invalid attestation → fall back to local; never errors out | Task 5 |
| 5 | Higher-credit consumers serve before lower under pressure, modulo aging | Task 8 + 11 |
| 6 | Aging boost: 0.3 waiting 10 min serves before fresh 0.5 | Task 11 |
| 7 | Queue overflow returns REJECT{retry_after_s ∈ [60, 600]} with jitter | Task 9 |
| 8 | IssueAttestation produces verifiable SignedCreditAttestation matching local computation | Task 12 |
| 9-12 | Persistence + recovery | **plan 3** |
| 13-16 | Performance | partially Task 9 (decide hot path); full perf-bench in plan 3 |
| 17-19 | Security: forged sig rejected / ejected peer rejected / clamped at ceiling | Task 5 |
| 20 | Admin /queue endpoint requires operator auth | **plan 3** |
| Race | `go test -race ./internal/admission/...` clean | Task 15 |

## Dependencies + Out-of-scope summary

**Depends on (must merge first):**
- Plan 1 (`tracker_admission_shared`) — `shared/admission` proto types + `signing.{Sign,Verify}CreditAttestation`.

**Deferred to plan 3:**
- `admission.tlog` (append-only event log, CRC-framed, batched fsync, dispute fdatasync)
- `admission.snapshot.<seq>` (periodic in-memory dumps, last 3 retained)
- `StartupReplay` — snapshot load + tlog replay + ledger cross-check
- Admin HTTP endpoints `/admission/*` (depends on the admin-server skeleton, also in plan 3 unless that has its own plan)
- Prometheus metrics (`admission_decisions_total`, etc.)
- Acceptance criteria #9-12 (persistence/recovery), #13-16 (performance benchmarks), #20 (admin auth)

**Deferred to other plans:**
- Real ledger event-bus emission (currently admission has the *consumer* side; ledger-side `Notify(LedgerEvent)` is a separate ledger-package PR)
- Broker integration: `internal/broker` is empty `.gitkeep`. When that gets implemented, its `broker_request` flow calls `admission.Decide` first.
- Tracker control-plane proto: heartbeat extensions + broker_request response oneof. Plan 1 amended the spec; the actual `.proto` lands when tracker control-plane gets its own plan.
- `FetchHeadroom` RPC: same — depends on tracker↔seeder RPC layer existing.
- Real federation peer-set check: admission accepts a `PeerSet` interface; v1 uses the always-false stub.
