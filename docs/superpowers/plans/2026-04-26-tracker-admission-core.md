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

## Task 5: Resolve credit-score (attestation → local → trial), with `MaxAttestationScoreImported` clamp

**Files:**
- Modify: `tracker/internal/admission/score.go`
- Modify: `tracker/internal/admission/score_test.go`

Implements admission-design §5.1 step 1 ("Resolve `credit_score`"). Five fall-through branches: validate attestation body → verify sig → check expiry → check federation-peer membership → on any miss, use local; on empty local, use `cfg.TrialTierScore`. Imported attestation score clamped to `cfg.MaxAttestationScoreImported` (default 0.95, §8.6).

- [ ] **Step 1: Append failing tests to `score_test.go`**

Append at end of `tracker/internal/admission/score_test.go`:

```go
func TestResolveCreditScore_NilAttestation_NoLocal_ReturnsTrialTier(t *testing.T) {
	s, _ := openTempSubsystem(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	score, source := s.resolveCreditScore(makeID(0x11), nil, now)
	assert.InDelta(t, s.cfg.TrialTierScore, score, 1e-9)
	assert.Equal(t, ScoreSourceTrialTier, source)
}

func TestResolveCreditScore_NilAttestation_LocalHistory_UsesLocal(t *testing.T) {
	s, _ := openTempSubsystem(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	id := makeID(0x11)

	// Seed local history.
	st := consumerShardFor(s.consumerShards, id).getOrInit(id, now.AddDate(0, 0, -10))
	st.SettlementBuckets[0] = DayBucket{Total: 50, A: 50, DayStamp: stripToDay(now)}
	st.LastBalanceSeen = 1000

	score, source := s.resolveCreditScore(id, nil, now)
	assert.Greater(t, score, s.cfg.TrialTierScore, "local should outrank trial-tier")
	assert.Equal(t, ScoreSourceLocal, source)
}

func TestResolveCreditScore_ValidAttestationFromPeer_UsedAndClamped(t *testing.T) {
	peerPriv, peerID := fixturePeerKeypair(t)
	s, _ := openTempSubsystem(t, WithPeerSet(staticPeers{peerID: true}))
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	body := &admission.CreditAttestationBody{
		IdentityId:            makeID(0x11).Bytes(),
		IssuerTrackerId:       peerID.Bytes(),
		Score:                 9800,           // 0.98 — exceeds 0.95 default cap
		TenureDays:            120,
		SettlementReliability: 9700,
		DisputeRate:           50,
		NetCreditFlow_30D:     250000,
		BalanceCushionLog2:    3,
		ComputedAt:            uint64(now.Add(-time.Hour).Unix()),
		ExpiresAt:             uint64(now.Add(23 * time.Hour).Unix()),
	}
	att := signFixtureAttestation(t, peerPriv, body)

	score, source := s.resolveCreditScore(makeID(0x11), att, now)
	assert.InDelta(t, s.cfg.MaxAttestationScoreImported, score, 1e-9, "clamp at MaxAttestationScoreImported")
	assert.Equal(t, ScoreSourceAttestation, source)
}

func TestResolveCreditScore_ValidAttestationBelowClamp_PassThrough(t *testing.T) {
	peerPriv, peerID := fixturePeerKeypair(t)
	s, _ := openTempSubsystem(t, WithPeerSet(staticPeers{peerID: true}))
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	body := validAttestationBody(now, peerID, 0x11)
	body.Score = 7000 // 0.70 — below clamp
	att := signFixtureAttestation(t, peerPriv, body)

	score, _ := s.resolveCreditScore(makeID(0x11), att, now)
	assert.InDelta(t, 0.70, score, 1e-9)
}

func TestResolveCreditScore_AttestationFromUnknownPeer_FallsThrough(t *testing.T) {
	otherPriv, otherID := fixturePeerKeypair(t)
	s, _ := openTempSubsystem(t) // default peer-set is always-false
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	body := validAttestationBody(now, otherID, 0x11)
	body.Score = 9000
	att := signFixtureAttestation(t, otherPriv, body)

	score, source := s.resolveCreditScore(makeID(0x11), att, now)
	// no local history → trial tier
	assert.InDelta(t, s.cfg.TrialTierScore, score, 1e-9)
	assert.Equal(t, ScoreSourceTrialTier, source)
}

func TestResolveCreditScore_ExpiredAttestation_FallsThrough(t *testing.T) {
	peerPriv, peerID := fixturePeerKeypair(t)
	s, _ := openTempSubsystem(t, WithPeerSet(staticPeers{peerID: true}))
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	body := validAttestationBody(now.Add(-48*time.Hour), peerID, 0x11)
	body.ExpiresAt = uint64(now.Add(-1 * time.Hour).Unix()) // expired 1h ago
	att := signFixtureAttestation(t, peerPriv, body)

	score, source := s.resolveCreditScore(makeID(0x11), att, now)
	assert.InDelta(t, s.cfg.TrialTierScore, score, 1e-9)
	assert.Equal(t, ScoreSourceTrialTier, source)
}

func TestResolveCreditScore_TamperedAttestation_FallsThrough(t *testing.T) {
	peerPriv, peerID := fixturePeerKeypair(t)
	s, _ := openTempSubsystem(t, WithPeerSet(staticPeers{peerID: true}))
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	body := validAttestationBody(now, peerID, 0x11)
	att := signFixtureAttestation(t, peerPriv, body)
	att.Body.Score = 9999 // post-sign mutation invalidates sig

	score, source := s.resolveCreditScore(makeID(0x11), att, now)
	assert.InDelta(t, s.cfg.TrialTierScore, score, 1e-9)
	assert.Equal(t, ScoreSourceTrialTier, source)
}

func TestResolveCreditScore_MalformedAttestationBody_FallsThrough(t *testing.T) {
	peerPriv, peerID := fixturePeerKeypair(t)
	s, _ := openTempSubsystem(t, WithPeerSet(staticPeers{peerID: true}))
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	body := validAttestationBody(now, peerID, 0x11)
	body.IdentityId = []byte{1, 2, 3} // wrong length — ValidateCreditAttestationBody rejects
	att := signFixtureAttestation(t, peerPriv, body)

	score, source := s.resolveCreditScore(makeID(0x11), att, now)
	assert.InDelta(t, s.cfg.TrialTierScore, score, 1e-9)
	assert.Equal(t, ScoreSourceTrialTier, source)
}
```

The helpers `validAttestationBody`, `signFixtureAttestation`, `fixturePeerKeypair`, `staticPeers`, `openTempSubsystem` are defined in `helpers_test.go` (Task 14). Until that lands, **append minimal versions inline at the bottom of `score_test.go`** so this task is self-contained:

```go
func validAttestationBody(now time.Time, peerID ids.IdentityID, consumerByte byte) *admission.CreditAttestationBody {
	return &admission.CreditAttestationBody{
		IdentityId:            makeID(consumerByte).Bytes(),
		IssuerTrackerId:       peerID.Bytes(),
		Score:                 7000,
		TenureDays:            60,
		SettlementReliability: 9000,
		DisputeRate:           100,
		NetCreditFlow_30D:     50000,
		BalanceCushionLog2:    1,
		ComputedAt:            uint64(now.Add(-time.Hour).Unix()),
		ExpiresAt:             uint64(now.Add(23 * time.Hour).Unix()),
	}
}

func signFixtureAttestation(t *testing.T, priv ed25519.PrivateKey, body *admission.CreditAttestationBody) *admission.SignedCreditAttestation {
	t.Helper()
	sig, err := signing.SignCreditAttestation(priv, body)
	require.NoError(t, err)
	return &admission.SignedCreditAttestation{Body: body, TrackerSig: sig}
}

func fixturePeerKeypair(t *testing.T) (ed25519.PrivateKey, ids.IdentityID) {
	t.Helper()
	seed := []byte("admission-fixture-peer-seed-v1-x") // 32 bytes
	require.Len(t, seed, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	var id ids.IdentityID
	copy(id[:], pub[:32])
	return priv, id
}

type staticPeers map[ids.IdentityID]bool

func (s staticPeers) Contains(id ids.IdentityID) bool { return s[id] }

func openTempSubsystem(t *testing.T, opts ...Option) (*Subsystem, ids.IdentityID) {
	t.Helper()
	seed := []byte("admission-fixture-tracker-seed-1") // 32 bytes
	require.Len(t, seed, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(seed)

	cfg := defaultScoreConfig()
	reg, err := registry.New(16)
	require.NoError(t, err)

	s, err := Open(cfg, reg, priv, opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s, s.trackerID
}
```

These are extracted into `helpers_test.go` in Task 14. Add the imports (`ed25519`, `signing`, `registry`, `ids`, `admission`) to `score_test.go`'s import block — group `admission "github.com/token-bay/token-bay/shared/admission"` and `signing "github.com/token-bay/token-bay/shared/signing"` and `registry "github.com/token-bay/token-bay/tracker/internal/registry"` and `ids "github.com/token-bay/token-bay/shared/ids"` alongside the existing `config` import.

- [ ] **Step 2: Run, confirm FAIL**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/admission/...
```

Expected: compile failure — `resolveCreditScore`, `ScoreSource`, `ScoreSourceTrialTier`, `ScoreSourceLocal`, `ScoreSourceAttestation` undefined.

- [ ] **Step 3: Append the resolver to `score.go`**

Add to `score.go`'s import block: `"time"` (already there), `admission "github.com/token-bay/token-bay/shared/admission"`, `signing "github.com/token-bay/token-bay/shared/signing"`. Then append:

```go
// ScoreSource indicates which path resolveCreditScore took. Returned to
// callers (Decide) for logging / metrics; not part of the admission
// decision itself.
type ScoreSource uint8

const (
	ScoreSourceUnspecified ScoreSource = iota
	ScoreSourceAttestation             // valid peer attestation, used as-is (post-clamp)
	ScoreSourceLocal                   // local ConsumerCreditState present
	ScoreSourceTrialTier               // no attestation, no local — cfg.TrialTierScore
)

// resolveCreditScore implements admission-design §5.1 step 1. Five
// fall-through checks on the attestation; on any miss falls through to
// local-history compute; on empty local returns cfg.TrialTierScore.
//
// The MaxAttestationScoreImported clamp (§8.6) bounds peer-issued scores
// so a malicious-but-undetected peer cannot inflate above the regional
// ceiling for an arbitrary consumer.
func (s *Subsystem) resolveCreditScore(consumerID ids.IdentityID, att *admission.SignedCreditAttestation, now time.Time) (float64, ScoreSource) {
	if att != nil {
		if score, ok := s.tryAttestation(att, now); ok {
			return score, ScoreSourceAttestation
		}
	}
	// Local fall-through.
	if st, ok := consumerShardFor(s.consumerShards, consumerID).get(consumerID); ok {
		score, sigs := ComputeLocalScore(st, s.cfg, now)
		// "≥ 1 settled entry" gate per §5.1 step 1: only honor local if
		// the consumer has done real work.
		if hasAnySettlement(sigs) {
			return score, ScoreSourceLocal
		}
	}
	return s.cfg.TrialTierScore, ScoreSourceTrialTier
}

// tryAttestation runs the four §5.1 step-1 checks. Returns (score, true)
// on success and (0, false) on any miss — caller treats the latter as
// "fall through to local".
func (s *Subsystem) tryAttestation(att *admission.SignedCreditAttestation, now time.Time) (float64, bool) {
	if att.Body == nil {
		return 0, false
	}
	if err := admission.ValidateCreditAttestationBody(att.Body); err != nil {
		return 0, false
	}
	// Federation peer-set check.
	var issuer ids.IdentityID
	if len(att.Body.IssuerTrackerId) != len(issuer) {
		return 0, false
	}
	copy(issuer[:], att.Body.IssuerTrackerId)
	if !s.peers.Contains(issuer) {
		return 0, false
	}
	// Expiry. Wall-clock comparison; the validator already enforced
	// expires_at > computed_at and ttl ≤ 7d.
	if uint64(now.Unix()) >= att.Body.ExpiresAt {
		return 0, false
	}
	// Signature verification — pubkey reconstructed from issuer ID. v1
	// uses the raw IdentityID bytes as the pubkey; tracker spec §5.2 will
	// resolve a proper "issuer pubkey from federation roster" lookup once
	// federation is wired (see admission.go.trackerIDFromPubkey for the
	// matching v1 substitution on the issuer side).
	pub := ed25519.PublicKey(issuer[:])
	if !signing.VerifyCreditAttestation(pub, att) {
		return 0, false
	}
	score := float64(att.Body.Score) / 10000.0
	if score > s.cfg.MaxAttestationScoreImported {
		score = s.cfg.MaxAttestationScoreImported
	}
	return score, true
}

// hasAnySettlement reports whether ComputeLocalScore's signal output came
// from a non-trivial state (≥ 1 settlement) — used to gate the local
// score path. Reliability is the canonical "I have settlement history"
// indicator (set to ≥ 0 only when totalSettled > 0).
func hasAnySettlement(s Signals) bool {
	return s.SettlementReliability >= 0
}
```

Add `"crypto/ed25519"` to score.go's import block.

- [ ] **Step 4: Run, confirm PASS**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/admission/...
```

Expected: all 8 new tests PASS plus prior tests still green.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission
git add tracker/internal/admission/score.go tracker/internal/admission/score_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/admission): resolveCreditScore — attestation → local → trial

Implements admission-design §5.1 step 1 with the four-check fall-through
chain on the attestation: ValidateCreditAttestationBody → federation
peer-set membership → expiry → Ed25519 signature. Any miss falls through
to local-history compute; empty local returns cfg.TrialTierScore.

Imported attestation scores clamp at cfg.MaxAttestationScoreImported
(default 0.95, §8.6) so a malicious-but-undetected peer cannot inflate
above the regional ceiling.

ScoreSource enum returned to callers for future metrics/logging.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: `SupplySnapshot` + atomic publication

**Files:**
- Create: `tracker/internal/admission/supply.go`
- Create: `tracker/internal/admission/supply_test.go`
- Modify: `tracker/internal/admission/admission.go` (replace placeholder `SupplySnapshot` reference if any; the field `supply atomic.Pointer[SupplySnapshot]` already exists from Task 1)

`SupplySnapshot` is the published view of regional supply pressure. The 5s aggregator (Task 7) writes it; `Decide` (Task 9) reads it via `atomic.Pointer.Load`. This task lands the type + Load/Store helpers; the goroutine itself is Task 7.

- [ ] **Step 1: Write failing tests**

Write `tracker/internal/admission/supply_test.go`:

```go
package admission

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSupply_PrePublish_ReturnsZeroSnapshotMarker(t *testing.T) {
	s, _ := openTempSubsystem(t)
	snap := s.Supply()
	require.NotNil(t, snap, "Supply must never return nil — callers always get a snapshot")
	assert.True(t, snap.ComputedAt.IsZero(), "pre-publish snapshot has zero ComputedAt as 'never computed' marker")
	assert.Equal(t, 0.0, snap.TotalHeadroom)
}

func TestSupply_PublishThenLoad(t *testing.T) {
	s, _ := openTempSubsystem(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	want := &SupplySnapshot{
		ComputedAt:    now,
		TotalHeadroom: 5.0,
		Pressure:      0.6,
	}
	s.publishSupply(want)

	got := s.Supply()
	assert.Equal(t, want, got)
}

func TestSupply_ConcurrentLoadStore_RaceClean(t *testing.T) {
	s, _ := openTempSubsystem(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					s.publishSupply(&SupplySnapshot{ComputedAt: now, TotalHeadroom: float64(i)})
				}
			}
		}(i)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = s.Supply()
				}
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
	// Race detector catches violations; pass = no race reported.
}
```

- [ ] **Step 2: Run, confirm FAIL**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/admission/...
```

Expected: compile failure — `SupplySnapshot` (real type), `Supply()`, `publishSupply()` undefined.

- [ ] **Step 3: Write `supply.go`**

Write `tracker/internal/admission/supply.go`:

```go
package admission

import "time"

// SupplySnapshot is the published view of regional supply pressure that
// Decide reads on the hot path. Aggregator (Task 7) writes via
// publishSupply; Decide reads via Supply. Reads never block — the field
// is an atomic.Pointer[SupplySnapshot] on the Subsystem.
//
// Spec: admission-design §4.2.
type SupplySnapshot struct {
	ComputedAt          time.Time          // zero value indicates "never computed yet"
	TotalHeadroom       float64            // sum of weighted seeder contributions
	PerModel            map[string]float64 // per-model breakdown (admission-design §5.3 step 2)
	ContributingSeeders uint32             // count of seeders with weight > 0
	SilentSeeders       uint32             // count of seeders dropped by freshness decay
	Pressure            float64            // demand_rate_ewma / TotalHeadroom; 0 if TotalHeadroom == 0
	RecentRejectRate    float64            // OVER_CAPACITY rejects per minute, EWMA
}

// emptySupplySnapshot is returned by Supply() before the aggregator has
// run for the first time. Decide treats this as "no supply data yet" and
// admits unconditionally — at boot the network has no rate limit until
// the 5s aggregator first publishes.
var emptySupplySnapshot = &SupplySnapshot{}

// Supply returns the most-recently-published SupplySnapshot. Never nil.
// Pre-publish callers receive emptySupplySnapshot (ComputedAt zero).
func (s *Subsystem) Supply() *SupplySnapshot {
	if v := s.supply.Load(); v != nil {
		return v
	}
	return emptySupplySnapshot
}

// publishSupply atomically swaps the current snapshot. Aggregator
// (Task 7) is the only caller in production; tests use it directly.
func (s *Subsystem) publishSupply(snap *SupplySnapshot) {
	s.supply.Store(snap)
}
```

- [ ] **Step 4: Run, confirm PASS**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/admission/...
```

Expected: 3 new tests PASS, race-detector clean.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission
git add tracker/internal/admission/supply.go tracker/internal/admission/supply_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/admission): SupplySnapshot + atomic publish/load

Decide hot-path needs a wait-free read of the published supply view;
atomic.Pointer.Store/Load is sufficient for race-clean publication.
Supply() never returns nil — pre-publish callers see emptySupplySnapshot
(ComputedAt zero) and Decide treats that as "no supply data yet".

The 5s aggregator goroutine that produces real snapshots lands in the
next task.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

## Task 7: 5s aggregator goroutine + freshness decay + demand EWMA

**Files:**
- Modify: `tracker/internal/admission/supply.go`
- Modify: `tracker/internal/admission/supply_test.go`
- Modify: `tracker/internal/admission/admission.go` (spawn aggregator on Open)

Implements admission-design §5.3. Aggregator runs on a 5s ticker; iterates `reg.Snapshot()`; per seeder computes `contribution = headroom · freshness_decay(age) · heartbeat_reliability(seeder)`; sums into `TotalHeadroom` + `PerModel`; computes `Pressure = demand_rate_ewma / TotalHeadroom`; publishes via `publishSupply`. The aggregator is driven by a *tick channel* in tests so timing is deterministic.

- [ ] **Step 1: Append failing tests to `supply_test.go`**

```go
func TestAggregator_TickProducesSnapshot(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	clk := newFixedClock(now)
	s, _ := openTempSubsystem(t, WithClock(clk.Now))

	// Seed the registry with two seeders, both reachable.
	registerSeeder(t, s.reg, makeID(0xAA), 0.7 /*headroom*/, now /*last hb*/)
	registerSeeder(t, s.reg, makeID(0xBB), 0.5, now)

	// Drive one aggregator tick.
	s.runAggregatorOnce(now)

	snap := s.Supply()
	assert.Equal(t, now, snap.ComputedAt)
	assert.Greater(t, snap.TotalHeadroom, 0.0)
	assert.Equal(t, uint32(2), snap.ContributingSeeders)
	assert.Equal(t, uint32(0), snap.SilentSeeders)
}

func TestAggregator_FreshnessDecay_DropsStaleSeeders(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	clk := newFixedClock(now)
	s, _ := openTempSubsystem(t, WithClock(clk.Now))

	// One fresh seeder, one stale (heartbeat older than freshness window).
	registerSeeder(t, s.reg, makeID(0xAA), 0.8, now)
	registerSeeder(t, s.reg, makeID(0xBB), 0.8, now.Add(-time.Duration(s.cfg.HeartbeatFreshnessDecayMaxS+10)*time.Second))

	s.runAggregatorOnce(now)

	snap := s.Supply()
	assert.Equal(t, uint32(1), snap.ContributingSeeders)
	assert.Equal(t, uint32(1), snap.SilentSeeders)
}

func TestAggregator_PressureZeroWhenNoSupply(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	clk := newFixedClock(now)
	s, _ := openTempSubsystem(t, WithClock(clk.Now))

	s.runAggregatorOnce(now)
	snap := s.Supply()
	assert.Equal(t, 0.0, snap.TotalHeadroom)
	assert.Equal(t, 0.0, snap.Pressure, "pressure must be 0 (not NaN/Inf) when supply is zero")
}

func TestAggregator_DemandEWMA_AffectsPressure(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	clk := newFixedClock(now)
	s, _ := openTempSubsystem(t, WithClock(clk.Now))
	registerSeeder(t, s.reg, makeID(0xAA), 1.0, now)

	// Drive demand: 5 ticks back-to-back.
	for i := 0; i < 5; i++ {
		s.recordDemandTick(now)
	}
	s.runAggregatorOnce(now)
	snap1 := s.Supply()
	assert.Greater(t, snap1.Pressure, 0.0)

	// More demand → higher pressure.
	for i := 0; i < 50; i++ {
		s.recordDemandTick(now)
	}
	s.runAggregatorOnce(now)
	snap2 := s.Supply()
	assert.Greater(t, snap2.Pressure, snap1.Pressure)
}

func TestAggregator_BackgroundLoop_RunsOnTicker(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	clk := newFixedClock(now)
	s, _ := openTempSubsystem(t, WithClock(clk.Now))
	registerSeeder(t, s.reg, makeID(0xAA), 0.6, now)

	// Manually fire the aggregator tick channel.
	s.aggregatorTick <- now
	// Wait briefly for the goroutine to process — bounded loop with timeout.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.Supply().ComputedAt.Equal(now) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	assert.Equal(t, now, s.Supply().ComputedAt, "aggregator goroutine consumed the tick")
}

// Helpers — extracted to helpers_test.go in Task 14. Inline here for
// task self-containment.

type fixedClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFixedClock(t time.Time) *fixedClock { return &fixedClock{now: t} }

func (c *fixedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fixedClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func registerSeeder(t *testing.T, reg *registry.Registry, id ids.IdentityID, headroom float64, lastHB time.Time) {
	t.Helper()
	reg.Register(registry.SeederRecord{
		IdentityID:       id,
		HeadroomEstimate: headroom,
		LastHeartbeat:    lastHB,
		Available:        true,
		ReputationScore:  1.0,
	})
}
```

- [ ] **Step 2: Run, confirm FAIL**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/admission/...
```

Expected: compile failure — `runAggregatorOnce`, `recordDemandTick`, `aggregatorTick` undefined.

- [ ] **Step 3: Append aggregator to `supply.go`**

```go
import (
	"math"
	"sync"
	"time"

	"github.com/token-bay/token-bay/tracker/internal/registry"
)
```

(Replace existing import block with the above; adds math/sync/registry.)

Append at end of `supply.go`:

```go
// aggregator state. Demand EWMA tracks broker_request arrival rate;
// every recordDemandTick increments a running counter that decays each
// time the aggregator runs. Half-life ≈ 5s (admission-design §5.1 step 3).
type demandTracker struct {
	mu       sync.Mutex
	rateEWMA float64   // events per second
	lastTick time.Time
}

func (d *demandTracker) record(now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rateEWMA += 1.0 // raw count; decayed when read
	d.lastTick = now
}

// readAndDecay returns the current rate and decays it by exp(-Δt/halfLife).
// Half-life 5s.
func (d *demandTracker) readAndDecay(now time.Time) float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lastTick.IsZero() {
		return 0
	}
	dt := now.Sub(d.lastTick).Seconds()
	if dt < 0 {
		dt = 0
	}
	d.rateEWMA *= math.Exp(-dt / 5.0)
	d.lastTick = now
	return d.rateEWMA
}

// recordDemandTick is called from Decide every time a broker_request
// arrives. Cheap; aggregator drains the EWMA.
func (s *Subsystem) recordDemandTick(now time.Time) {
	s.demand.record(now)
}

// runAggregatorOnce executes one aggregation pass. Exposed for tests
// (the production path drives it via aggregatorTick).
func (s *Subsystem) runAggregatorOnce(now time.Time) {
	seeders := s.reg.Snapshot()
	var (
		total       float64
		perModel    = make(map[string]float64)
		contributing uint32
		silent      uint32
	)
	for _, sr := range seeders {
		w := s.seederWeight(sr, now)
		if w <= 0 {
			silent++
			continue
		}
		contribution := sr.HeadroomEstimate * w
		total += contribution
		contributing++
		for _, model := range sr.Capabilities.Models {
			perModel[model] += contribution
		}
	}

	demand := s.demand.readAndDecay(now)
	pressure := 0.0
	if total > 0 {
		pressure = demand / total
	}
	s.publishSupply(&SupplySnapshot{
		ComputedAt:          now,
		TotalHeadroom:       total,
		PerModel:            perModel,
		ContributingSeeders: contributing,
		SilentSeeders:       silent,
		Pressure:            pressure,
	})
}

// seederWeight combines freshness decay and heartbeat-reliability
// weighting per admission-design §5.3. Result is in [0, 1].
//
// freshness_decay: 1.0 if last_heartbeat < 60s ago; linear decay to 0
// at HeartbeatFreshnessDecayMaxS; 0 thereafter.
// heartbeat_reliability: from SeederHeartbeatState; v1 returns 1.0 when
// no state exists yet (§5.5: the heartbeat-reliability signal becomes
// meaningful only after the rolling window has data).
func (s *Subsystem) seederWeight(sr registry.SeederRecord, now time.Time) float64 {
	age := now.Sub(sr.LastHeartbeat).Seconds()
	if age < 0 {
		age = 0
	}
	maxAge := float64(s.cfg.HeartbeatFreshnessDecayMaxS)
	const freshThresholdS = 60.0
	freshness := 1.0
	switch {
	case age <= freshThresholdS:
		freshness = 1.0
	case age >= maxAge:
		freshness = 0
	default:
		freshness = (maxAge - age) / (maxAge - freshThresholdS)
	}
	return freshness * sr.ReputationScore
}

// runAggregatorLoop is the goroutine body. Consumes tick events from
// aggregatorTick and runs runAggregatorOnce per tick. Stops on s.stop.
func (s *Subsystem) runAggregatorLoop() {
	defer s.wg.Done()
	for {
		select {
		case <-s.stop:
			return
		case now := <-s.aggregatorTick:
			s.runAggregatorOnce(now)
		}
	}
}

// startAggregator spawns the aggregator goroutine and the 5s ticker
// that feeds it. Tests can pre-fill aggregatorTick to drive deterministic
// runs without involving real time.
func (s *Subsystem) startAggregator() {
	s.aggregatorTick = make(chan time.Time, 1)
	s.wg.Add(1)
	go s.runAggregatorLoop()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case now := <-t.C:
				select {
				case s.aggregatorTick <- now:
				default:
					// drop tick if loop is busy — next tick will catch up
				}
			}
		}
	}()
}
```

In `admission.go`, add fields to Subsystem:

```go
demand         demandTracker
aggregatorTick chan time.Time
```

And in `Open`, after the shard initialization, spawn the aggregator:

```go
s.startAggregator()
```

- [ ] **Step 4: Run, confirm PASS**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/admission/...
```

Expected: all 5 new tests PASS; existing tests still green; race-detector clean.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission
git add tracker/internal/admission/supply.go tracker/internal/admission/supply_test.go tracker/internal/admission/admission.go
git commit -m "$(cat <<'EOF'
feat(tracker/admission): supply aggregator + freshness decay + demand EWMA

5s aggregator goroutine reads registry.Snapshot(), applies linear-decay
freshness weighting (1.0 below 60s, → 0 at HeartbeatFreshnessDecayMaxS),
multiplies by seeder reputation, sums per-model and total headroom, and
publishes a fresh SupplySnapshot via atomic.Pointer.

Demand EWMA: every Decide call (Task 9) records a tick; aggregator
decays it on each pass with a 5s half-life. Pressure = demand / supply,
0 (not NaN) when supply is empty.

Aggregator goroutine is exposed via runAggregatorOnce for deterministic
tests; production path drives it from a 5s time.Ticker.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

## Task 8: `QueueEntry` + max-heap + `EffectivePriority` + aging boost

**Files:**
- Create: `tracker/internal/admission/queue.go`
- Create: `tracker/internal/admission/queue_test.go`

`QueueEntry` is the in-flight queued-request record. The queue is a max-heap keyed on `EffectivePriority(now, agingAlpha) = CreditScore + agingAlpha*waitMinutes`. `Subsystem.queue` field replaces the placeholder `queueHeap` from Task 1.

- [ ] **Step 1: Write failing tests**

Write `tracker/internal/admission/queue_test.go`:

```go
package admission

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQueueEntry_EffectivePriority_NoWaitEqualsCredit(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	e := QueueEntry{CreditScore: 0.7, EnqueuedAt: now}
	assert.InDelta(t, 0.7, e.EffectivePriority(now, 0.05), 1e-9)
}

func TestQueueEntry_EffectivePriority_AgingBoost(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	e := QueueEntry{CreditScore: 0.3, EnqueuedAt: now.Add(-10 * time.Minute)}
	// 0.3 + 0.05 * 10 = 0.8
	assert.InDelta(t, 0.8, e.EffectivePriority(now, 0.05), 1e-9)
}

func TestQueueHeap_PopReturnsHighestEffectivePriority(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	h := newQueueHeap(now, 0.05)

	a := QueueEntry{ConsumerID: makeID(0xAA), CreditScore: 0.3, EnqueuedAt: now} // low credit, no wait
	b := QueueEntry{ConsumerID: makeID(0xBB), CreditScore: 0.7, EnqueuedAt: now} // high credit, no wait
	c := QueueEntry{ConsumerID: makeID(0xCC), CreditScore: 0.5, EnqueuedAt: now} // mid

	h.Push(a)
	h.Push(b)
	h.Push(c)

	got := []QueueEntry{h.Pop(), h.Pop(), h.Pop()}
	assert.Equal(t, b.ConsumerID, got[0].ConsumerID, "highest credit pops first")
	assert.Equal(t, c.ConsumerID, got[1].ConsumerID)
	assert.Equal(t, a.ConsumerID, got[2].ConsumerID)
}

func TestQueueHeap_AgingBoostOverridesStaticOrder(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	h := newQueueHeap(now, 0.05)

	// A: 0.3 credit, waited 10 min → effective 0.8
	// B: 0.5 credit, just enqueued → effective 0.5
	a := QueueEntry{ConsumerID: makeID(0xAA), CreditScore: 0.3, EnqueuedAt: now.Add(-10 * time.Minute)}
	b := QueueEntry{ConsumerID: makeID(0xBB), CreditScore: 0.5, EnqueuedAt: now}
	h.Push(a)
	h.Push(b)

	first := h.Pop()
	assert.Equal(t, a.ConsumerID, first.ConsumerID, "aged 0.3 outranks fresh 0.5 — admission-design §10 #6")
}

func TestQueueHeap_LenAndCap(t *testing.T) {
	h := newQueueHeap(time.Now(), 0.05)
	assert.Equal(t, 0, h.Len())
	h.Push(QueueEntry{ConsumerID: makeID(0xAA)})
	assert.Equal(t, 1, h.Len())
	h.Push(QueueEntry{ConsumerID: makeID(0xBB)})
	assert.Equal(t, 2, h.Len())
	h.Pop()
	assert.Equal(t, 1, h.Len())
}

func TestQueueHeap_PopFromEmpty_ReturnsZero(t *testing.T) {
	h := newQueueHeap(time.Now(), 0.05)
	z := h.Pop()
	assert.Equal(t, QueueEntry{}, z)
}

func TestQueueHeap_PeekDoesNotMutate(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	h := newQueueHeap(now, 0.05)
	h.Push(QueueEntry{ConsumerID: makeID(0xAA), CreditScore: 0.5, EnqueuedAt: now})
	h.Push(QueueEntry{ConsumerID: makeID(0xBB), CreditScore: 0.7, EnqueuedAt: now})

	first, ok := h.Peek()
	require.True(t, ok)
	assert.Equal(t, makeID(0xBB), first.ConsumerID)
	assert.Equal(t, 2, h.Len(), "Peek must not mutate")
}
```

- [ ] **Step 2: Run, confirm FAIL**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/admission/...
```

Expected: compile failure — `QueueEntry`, `EffectivePriority`, `newQueueHeap`, `queueHeap.Push/Pop/Peek/Len` undefined.

- [ ] **Step 3: Write `queue.go`**

```go
package admission

import (
	"container/heap"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
)

// QueueEntry is one in-flight queued admission decision. Spec
// admission-design §4.2.
type QueueEntry struct {
	RequestID    [16]byte
	ConsumerID   ids.IdentityID
	EnvelopeHash [32]byte
	CreditScore  float64
	EnqueuedAt   time.Time
}

// EffectivePriority returns the heap-ordering key. CreditScore plus an
// aging boost proportional to wait time. Aging boost is what lets a low-
// credit consumer eventually serve under sustained pressure
// (admission-design §5.4).
func (e *QueueEntry) EffectivePriority(now time.Time, agingAlpha float64) float64 {
	wait := now.Sub(e.EnqueuedAt)
	if wait < 0 {
		wait = 0
	}
	waitMinutes := wait.Minutes()
	return e.CreditScore + agingAlpha*waitMinutes
}

// queueHeap is a max-heap of QueueEntry by EffectivePriority. The heap
// captures (now, agingAlpha) at construction time; the Less function
// uses those for the ordering. Push/Pop/Peek are typed wrappers over
// container/heap.
type queueHeap struct {
	entries     []QueueEntry
	now         time.Time
	agingAlpha  float64
}

func newQueueHeap(now time.Time, agingAlpha float64) *queueHeap {
	return &queueHeap{now: now, agingAlpha: agingAlpha}
}

func (h *queueHeap) Len() int { return len(h.entries) }
func (h *queueHeap) Less(i, j int) bool {
	pi := h.entries[i].EffectivePriority(h.now, h.agingAlpha)
	pj := h.entries[j].EffectivePriority(h.now, h.agingAlpha)
	return pi > pj // max-heap
}
func (h *queueHeap) Swap(i, j int)      { h.entries[i], h.entries[j] = h.entries[j], h.entries[i] }
func (h *queueHeap) heapPush(x any)     { h.entries = append(h.entries, x.(QueueEntry)) }
func (h *queueHeap) heapPop() any {
	n := len(h.entries) - 1
	x := h.entries[n]
	h.entries = h.entries[:n]
	return x
}

// Push adds an entry to the heap.
func (h *queueHeap) Push(e QueueEntry) {
	heap.Push((*queueHeapAdapter)(h), e)
}

// Pop returns the top entry. Returns the zero value when the heap is
// empty (callers MUST check Len() == 0 before calling if they need
// distinguishability).
func (h *queueHeap) Pop() QueueEntry {
	if len(h.entries) == 0 {
		return QueueEntry{}
	}
	return heap.Pop((*queueHeapAdapter)(h)).(QueueEntry)
}

// Peek returns the top entry without removing it. ok = false on an
// empty heap.
func (h *queueHeap) Peek() (QueueEntry, bool) {
	if len(h.entries) == 0 {
		return QueueEntry{}, false
	}
	return h.entries[0], true
}

// AdvanceTime updates the "now" cached on the heap. Re-orders is NOT
// done automatically — callers needing strict ordering after a clock
// jump should call Reheapify().
func (h *queueHeap) AdvanceTime(now time.Time) { h.now = now }

// Reheapify reorders the heap. Useful after AdvanceTime when the aging
// boost has shifted some entries' effective priorities relative to others.
func (h *queueHeap) Reheapify() {
	heap.Init((*queueHeapAdapter)(h))
}

// queueHeapAdapter exposes queueHeap as a heap.Interface implementation
// without forcing queueHeap itself to expose Push/Pop in the heap.Interface
// shape (which would conflict with our Push/Pop methods).
type queueHeapAdapter queueHeap

func (a *queueHeapAdapter) Len() int            { return (*queueHeap)(a).Len() }
func (a *queueHeapAdapter) Less(i, j int) bool  { return (*queueHeap)(a).Less(i, j) }
func (a *queueHeapAdapter) Swap(i, j int)       { (*queueHeap)(a).Swap(i, j) }
func (a *queueHeapAdapter) Push(x any)          { (*queueHeap)(a).heapPush(x) }
func (a *queueHeapAdapter) Pop() any            { return (*queueHeap)(a).heapPop() }
```

In `admission.go`, replace the placeholder `queueHeap` field type. The Subsystem struct already has `queue queueHeap` from Task 1; change to `queue *queueHeap` and initialize in `Open` after the shard setup:

```go
s.queue = newQueueHeap(time.Time{}, cfg.AgingAlphaPerMinute)
```

- [ ] **Step 4: Run, confirm PASS**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/admission/...
```

Expected: all 7 new tests PASS.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission
git add tracker/internal/admission/queue.go tracker/internal/admission/queue_test.go tracker/internal/admission/admission.go
git commit -m "$(cat <<'EOF'
feat(tracker/admission): QueueEntry + max-heap with aging boost

QueueEntry.EffectivePriority(now, agingAlpha) = CreditScore +
agingAlpha*waitMinutes. Max-heap built on container/heap via an adapter
type that keeps the QueueEntry-typed Push/Pop API on queueHeap distinct
from the heap.Interface shape that container/heap requires.

AdvanceTime + Reheapify let queue-drain step the cached "now" between
tick batches without rebuilding the heap from scratch.

Aging-boost test (admission-design §10 #6 acceptance pre-condition):
0.3-credit waiting 10min outranks fresh 0.5.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

## Task 9: `Decide` hot path — ADMIT | QUEUE | REJECT branching

**Files:**
- Create: `tracker/internal/admission/decide.go`
- Create: `tracker/internal/admission/decide_test.go`

Implements admission-design §5.1 step 4. Reads supply via `Supply()`, computes pressure (already in the snapshot from Task 7), branches:
- `pressure < pressure_admit_threshold` → `ADMIT`.
- `pressure < pressure_reject_threshold` AND `queue.Len() < queue_cap` → `QUEUE`.
- otherwise → `REJECT{REGION_OVERLOADED, retry_after_s}`.

Banded position/eta per §6.5; retry_after with jitter per §5.4.

- [ ] **Step 1: Write failing tests**

Write `tracker/internal/admission/decide_test.go`:

```go
package admission

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestDecide_LowPressure_Admits(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)))
	s.publishSupply(&SupplySnapshot{ComputedAt: now, TotalHeadroom: 10, Pressure: 0.5})

	res := s.Decide(makeID(0x11), nil, now)
	assert.Equal(t, OutcomeAdmit, res.Outcome)
	assert.Nil(t, res.Queued)
	assert.Nil(t, res.Rejected)
	assert.InDelta(t, s.cfg.TrialTierScore, res.CreditUsed, 1e-9)
}

func TestDecide_MediumPressure_Queues(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)))
	// 0.85 ≤ p < 1.5 → QUEUE
	s.publishSupply(&SupplySnapshot{ComputedAt: now, TotalHeadroom: 10, Pressure: 1.0})

	res := s.Decide(makeID(0x11), nil, now)
	assert.Equal(t, OutcomeQueue, res.Outcome)
	assert.NotNil(t, res.Queued)
	assert.Equal(t, PositionBand_1To10, res.Queued.PositionBand, "first queued entry → band 1-10")
}

func TestDecide_HighPressure_Rejects(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)))
	s.publishSupply(&SupplySnapshot{ComputedAt: now, TotalHeadroom: 10, Pressure: 2.0})

	res := s.Decide(makeID(0x11), nil, now)
	assert.Equal(t, OutcomeReject, res.Outcome)
	assert.NotNil(t, res.Rejected)
	assert.Equal(t, RejectReason_RegionOverloaded, res.Rejected.Reason)
	assert.GreaterOrEqual(t, res.Rejected.RetryAfterS, uint32(60))
	assert.LessOrEqual(t, res.Rejected.RetryAfterS, uint32(600))
}

func TestDecide_QueueOverflow_Rejects(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)))
	s.publishSupply(&SupplySnapshot{ComputedAt: now, TotalHeadroom: 10, Pressure: 1.0})

	// Fill the queue to cap.
	for i := 0; i < s.cfg.QueueCap; i++ {
		_ = s.Decide(makeIDi(i), nil, now)
	}
	// Next request must REJECT.
	res := s.Decide(makeID(0xFE), nil, now)
	assert.Equal(t, OutcomeReject, res.Outcome)
	assert.Equal(t, RejectReason_RegionOverloaded, res.Rejected.Reason)
}

func TestDecide_PrePublishSnapshot_Admits(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)))
	// Don't publish — Supply() returns emptySupplySnapshot
	res := s.Decide(makeID(0x11), nil, now)
	assert.Equal(t, OutcomeAdmit, res.Outcome,
		"boot-time admit until aggregator publishes")
}

func TestBandPosition_Boundaries(t *testing.T) {
	cases := []struct {
		size int
		want PositionBand
	}{
		{1, PositionBand_1To10},
		{10, PositionBand_1To10},
		{11, PositionBand_11To50},
		{50, PositionBand_11To50},
		{51, PositionBand_51To200},
		{200, PositionBand_51To200},
		{201, PositionBand_200Plus},
		{10000, PositionBand_200Plus},
	}
	for _, c := range cases {
		t.Run("", func(t *testing.T) {
			assert.Equal(t, c.want, bandPosition(c.size))
		})
	}
}

func TestBandEta_Boundaries(t *testing.T) {
	cases := []struct {
		eta  time.Duration
		want EtaBand
	}{
		{15 * time.Second, EtaBand_LessThan30s},
		{30 * time.Second, EtaBand_30sTo2m},
		{2 * time.Minute, EtaBand_30sTo2m},
		{3 * time.Minute, EtaBand_2mTo5m},
		{5 * time.Minute, EtaBand_2mTo5m},
		{10 * time.Minute, EtaBand_5mPlus},
	}
	for _, c := range cases {
		t.Run("", func(t *testing.T) {
			assert.Equal(t, c.want, bandEta(c.eta))
		})
	}
}

func TestRetryAfter_RangeAndJitter(t *testing.T) {
	// Drive deterministic jitter via an injected RNG.
	got := make(map[uint32]struct{})
	for i := 0; i < 100; i++ {
		retry := computeRetryAfterS(50 /*queue*/, 200*time.Millisecond /*svc*/, fixedRand(int64(i)))
		assert.GreaterOrEqual(t, retry, uint32(60))
		assert.LessOrEqual(t, retry, uint32(600))
		got[retry] = struct{}{}
	}
	assert.Greater(t, len(got), 5, "jitter should produce a spread of values")
}

// helpers — extracted into helpers_test.go in Task 14.
func staticClockFn(t time.Time) func() time.Time { return func() time.Time { return t } }
func makeIDi(i int) ids.IdentityID {
	var id ids.IdentityID
	id[0] = byte(i & 0xff)
	id[1] = byte((i >> 8) & 0xff)
	return id
}
func fixedRand(seed int64) *rand.Rand { return rand.New(rand.NewSource(seed)) }
```

Add to imports: `"math/rand"`, `ids "github.com/token-bay/token-bay/shared/ids"`.

- [ ] **Step 2: Run, confirm FAIL**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/admission/...
```

Expected: compile failure — `Decide`, `Outcome*`, `PositionBand_*`, `EtaBand_*`, `RejectReason_*`, `bandPosition`, `bandEta`, `computeRetryAfterS` undefined.

- [ ] **Step 3: Write `decide.go`**

```go
package admission

import (
	"crypto/rand"
	"encoding/binary"
	mrand "math/rand"
	"time"

	"github.com/token-bay/token-bay/shared/admission"
	"github.com/token-bay/token-bay/shared/ids"
)

// Outcome enumerates Decide's three result shapes.
type Outcome uint8

const (
	OutcomeUnspecified Outcome = iota
	OutcomeAdmit
	OutcomeQueue
	OutcomeReject
)

// PositionBand maps queue position to a banded value (admission-design §6.5).
type PositionBand uint8

const (
	PositionBand_Unspecified PositionBand = iota
	PositionBand_1To10
	PositionBand_11To50
	PositionBand_51To200
	PositionBand_200Plus
)

// EtaBand maps queue ETA to a banded value (admission-design §6.5).
type EtaBand uint8

const (
	EtaBand_Unspecified EtaBand = iota
	EtaBand_LessThan30s
	EtaBand_30sTo2m
	EtaBand_2mTo5m
	EtaBand_5mPlus
)

// RejectReason mirrors the §5.1/§5.4 reasons.
type RejectReason uint8

const (
	RejectReason_Unspecified RejectReason = iota
	RejectReason_RegionOverloaded
	RejectReason_QueueTimeout
)

// QueuedDetails carries the user-facing fields of an OutcomeQueue.
type QueuedDetails struct {
	RequestID    [16]byte
	PositionBand PositionBand
	EtaBand      EtaBand
}

// RejectedDetails carries the user-facing fields of an OutcomeReject.
type RejectedDetails struct {
	Reason      RejectReason
	RetryAfterS uint32
}

// AdmissionResult is what Decide returns. Exactly one of {Queued, Rejected}
// is non-nil based on Outcome.
type AdmissionResult struct {
	Outcome    Outcome
	Queued     *QueuedDetails
	Rejected   *RejectedDetails
	CreditUsed float64 // in [0, 1] — score that drove the decision
}

// Decide is the hot path. Implements admission-design §5.1.
func (s *Subsystem) Decide(consumerID ids.IdentityID, att *admission.SignedCreditAttestation, now time.Time) AdmissionResult {
	score, _ := s.resolveCreditScore(consumerID, att, now)
	s.recordDemandTick(now)

	supply := s.Supply()
	pressure := supply.Pressure

	// Admit branch: low pressure, OR no supply data yet (boot-time).
	if pressure < s.cfg.PressureAdmitThreshold || supply.ComputedAt.IsZero() {
		return AdmissionResult{Outcome: OutcomeAdmit, CreditUsed: score}
	}

	// Queue branch: medium pressure with room.
	if pressure < s.cfg.PressureRejectThreshold {
		s.queueMu.Lock()
		queueSize := s.queue.Len()
		if queueSize < s.cfg.QueueCap {
			entry := QueueEntry{
				RequestID:    newRequestID(),
				ConsumerID:   consumerID,
				CreditScore:  score,
				EnqueuedAt:   now,
			}
			s.queue.AdvanceTime(now)
			s.queue.Push(entry)
			pos := s.queue.Len()
			s.queueMu.Unlock()
			return AdmissionResult{
				Outcome: OutcomeQueue,
				Queued: &QueuedDetails{
					RequestID:    entry.RequestID,
					PositionBand: bandPosition(pos),
					EtaBand:      bandEta(estimateEta(pos, supply)),
				},
				CreditUsed: score,
			}
		}
		s.queueMu.Unlock()
		// Fall through to Reject.
	}

	// Reject branch.
	retry := computeRetryAfterS(s.queue.Len(), 200*time.Millisecond, mrand.New(mrand.NewSource(now.UnixNano())))
	return AdmissionResult{
		Outcome: OutcomeReject,
		Rejected: &RejectedDetails{
			Reason:      RejectReason_RegionOverloaded,
			RetryAfterS: retry,
		},
		CreditUsed: score,
	}
}

// bandPosition maps an integer queue position (1-indexed) to a band.
func bandPosition(p int) PositionBand {
	switch {
	case p <= 0:
		return PositionBand_Unspecified
	case p <= 10:
		return PositionBand_1To10
	case p <= 50:
		return PositionBand_11To50
	case p <= 200:
		return PositionBand_51To200
	default:
		return PositionBand_200Plus
	}
}

// bandEta maps a duration to a band.
func bandEta(d time.Duration) EtaBand {
	switch {
	case d < 30*time.Second:
		return EtaBand_LessThan30s
	case d <= 2*time.Minute:
		return EtaBand_30sTo2m
	case d <= 5*time.Minute:
		return EtaBand_2mTo5m
	default:
		return EtaBand_5mPlus
	}
}

// estimateEta is a heuristic — queue position * average service time.
// Enough for the band; the exact number isn't surfaced (§6.5).
func estimateEta(pos int, _ *SupplySnapshot) time.Duration {
	const avgServiceTime = 200 * time.Millisecond
	return time.Duration(pos) * avgServiceTime
}

// computeRetryAfterS implements admission-design §5.4 backoff:
// retry_after_s = clamp(30 + jitter(0,30) + queue_drain_estimate, 60, 600)
// queue_drain_estimate = queue_size · avg_recent_service_time
func computeRetryAfterS(queueSize int, avgServiceTime time.Duration, r *mrand.Rand) uint32 {
	const baseS = 30
	jitter := r.Intn(30) // [0, 30)
	drainS := int(time.Duration(queueSize) * avgServiceTime / time.Second)
	v := baseS + jitter + drainS
	if v < 60 {
		v = 60
	}
	if v > 600 {
		v = 600
	}
	return uint32(v)
}

// newRequestID returns a 16-byte random request ID. Crypto-rand because
// the ID surfaces to the consumer and a predictable ID would let other
// consumers correlate queue positions.
func newRequestID() [16]byte {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		// Fall back to time-based — not security critical, just don't
		// return all zeros if /dev/urandom is unavailable.
		binary.BigEndian.PutUint64(id[:8], uint64(time.Now().UnixNano()))
	}
	return id
}
```

- [ ] **Step 4: Run, confirm PASS**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/admission/...
```

Expected: all 8 new tests PASS plus prior tests still green.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission
git add tracker/internal/admission/decide.go tracker/internal/admission/decide_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/admission): Decide hot path — ADMIT | QUEUE | REJECT

Implements admission-design §5.1 step 4 branching against the most-
recently-published SupplySnapshot. Pre-publish (boot-time) Supply()
returns emptySupplySnapshot with zero ComputedAt; Decide treats that
as "admit unconditionally" until the 5s aggregator first publishes.

Banded position/eta (§6.5) limit information leakage. retry_after_s
follows §5.4: clamp(30 + jitter[0,30) + queue_drain_estimate, 60, 600).
RequestID is crypto-rand 16 bytes so consumers can't correlate queue
positions with each other.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

## Task 10: Decision-matrix table-driven test (§6.4)

**Files:**
- Create: `tracker/internal/admission/decision_matrix_test.go`

The §6.4 cross-product test: every `{attestation × local_history × pressure}` combination → expected outcome. Catches integration regressions the per-file unit tests miss.

- [ ] **Step 1: Write the table-driven test**

Write `tracker/internal/admission/decision_matrix_test.go`:

```go
package admission

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/admission"
)

type attestationKind uint8

const (
	attNone attestationKind = iota
	attValid
	attExpired
	attForged
	attEjectedIssuer
)

type historyKind uint8

const (
	histNone historyKind = iota
	histSome
	histExtensive
)

type pressureKind uint8

const (
	pressLow pressureKind = iota // < 0.85 → ADMIT regardless
	pressMid                     // 0.85 ≤ p < 1.5 → QUEUE if room
	pressHigh                    // ≥ 1.5 → REJECT
)

type matrixRow struct {
	att     attestationKind
	hist    historyKind
	press   pressureKind
	want    Outcome
}

func TestDecisionMatrix_FullProduct(t *testing.T) {
	rows := []matrixRow{}
	for _, a := range []attestationKind{attNone, attValid, attExpired, attForged, attEjectedIssuer} {
		for _, h := range []historyKind{histNone, histSome, histExtensive} {
			for _, p := range []pressureKind{pressLow, pressMid, pressHigh} {
				rows = append(rows, matrixRow{
					att: a, hist: h, press: p,
					want: expectedOutcome(p),
				})
			}
		}
	}

	for _, row := range rows {
		row := row
		t.Run(matrixName(row), func(t *testing.T) {
			now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
			peerPriv, peerID := fixturePeerKeypair(t)
			peers := staticPeers{}
			if row.att == attValid || row.att == attExpired || row.att == attForged {
				peers[peerID] = true // recognized issuer
			}
			// attEjectedIssuer: peer is NOT in the set even though att is well-formed
			s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)), WithPeerSet(peers))

			// Set up local history per row.
			id := makeID(0xC0)
			switch row.hist {
			case histSome:
				st := consumerShardFor(s.consumerShards, id).getOrInit(id, now.AddDate(0, 0, -10))
				st.SettlementBuckets[0] = DayBucket{Total: 10, A: 9, DayStamp: stripToDay(now)}
				st.LastBalanceSeen = 1000
			case histExtensive:
				st := consumerShardFor(s.consumerShards, id).getOrInit(id, now.AddDate(0, 0, -30))
				st.SettlementBuckets[0] = DayBucket{Total: 100, A: 100, DayStamp: stripToDay(now)}
				st.LastBalanceSeen = 50000
			}

			// Build attestation per row.
			var att *admission.SignedCreditAttestation
			switch row.att {
			case attValid, attEjectedIssuer:
				body := validAttestationBody(now, peerID, 0xC0)
				att = signFixtureAttestation(t, peerPriv, body)
			case attExpired:
				body := validAttestationBody(now.Add(-48*time.Hour), peerID, 0xC0)
				body.ExpiresAt = uint64(now.Add(-1 * time.Hour).Unix())
				att = signFixtureAttestation(t, peerPriv, body)
			case attForged:
				body := validAttestationBody(now, peerID, 0xC0)
				att = signFixtureAttestation(t, peerPriv, body)
				att.Body.Score = 9999 // post-sign mutation
			}

			// Set up pressure per row.
			switch row.press {
			case pressLow:
				s.publishSupply(&SupplySnapshot{ComputedAt: now, TotalHeadroom: 10, Pressure: 0.5})
			case pressMid:
				s.publishSupply(&SupplySnapshot{ComputedAt: now, TotalHeadroom: 10, Pressure: 1.0})
			case pressHigh:
				s.publishSupply(&SupplySnapshot{ComputedAt: now, TotalHeadroom: 10, Pressure: 2.0})
			}

			res := s.Decide(id, att, now)
			require.Equal(t, row.want, res.Outcome,
				"att=%v hist=%v press=%v → got %v, want %v",
				row.att, row.hist, row.press, res.Outcome, row.want)

			// Sanity: REJECT must carry retry_after in [60, 600].
			if res.Outcome == OutcomeReject {
				assert.GreaterOrEqual(t, res.Rejected.RetryAfterS, uint32(60))
				assert.LessOrEqual(t, res.Rejected.RetryAfterS, uint32(600))
			}
		})
	}
}

// expectedOutcome derives the canonical outcome for a (pressure) bucket.
// Note: history and attestation kind affect CreditUsed but not Outcome
// for v1's threshold-based decision — admit/queue/reject is purely
// pressure-driven (§5.1 step 4). Higher credit changes queue *priority*
// (Task 8) but doesn't change the admit/queue/reject category.
func expectedOutcome(p pressureKind) Outcome {
	switch p {
	case pressLow:
		return OutcomeAdmit
	case pressMid:
		return OutcomeQueue
	default:
		return OutcomeReject
	}
}

func matrixName(r matrixRow) string {
	atts := []string{"none", "valid", "expired", "forged", "ejected"}
	hists := []string{"hist=none", "hist=some", "hist=extensive"}
	press := []string{"press=low", "press=mid", "press=high"}
	return atts[r.att] + "/" + hists[r.hist] + "/" + press[r.press]
}
```

- [ ] **Step 2: Run, confirm PASS**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 -run TestDecisionMatrix ./internal/admission/...
```

Expected: 45 subtests (5 × 3 × 3), all PASS.

- [ ] **Step 3: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission
git add tracker/internal/admission/decision_matrix_test.go
git commit -m "$(cat <<'EOF'
test(tracker/admission): §6.4 decision matrix — 45-row cross-product

Every {attestation: nil|valid|expired|forged|ejected_issuer} ×
{local_history: none|some|extensive} × {pressure: low|mid|high}
combination gets one row. Validates that the integration of
resolveCreditScore + Decide branching produces the same outcome the spec
predicts, and catches regressions where component-level unit tests
might miss cross-cutting bugs.

For v1 threshold semantics, admit/queue/reject is pressure-driven; the
attestation and history dimensions affect CreditUsed (queue priority)
but not the Outcome category — encoded in expectedOutcome.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

## Task 11: Aging-boost integration test (§10 #6 acceptance)

**Files:**
- Create: `tracker/internal/admission/aging_boost_test.go`

End-to-end aging-boost acceptance: two consumers in the queue at different times, advance the clock, drain, assert priority order. Goes beyond Task 8's unit test by exercising Decide → queue → Reheapify under a clock advance.

- [ ] **Step 1: Write the integration test**

Write `tracker/internal/admission/aging_boost_test.go`:

```go
package admission

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgingBoost_LowCreditWaitingOutranksFreshHigherCredit(t *testing.T) {
	t0 := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	clk := newFixedClock(t0)
	s, _ := openTempSubsystem(t, WithClock(clk.Now))
	// Force QUEUE: pressure between admit and reject thresholds.
	s.publishSupply(&SupplySnapshot{ComputedAt: t0, TotalHeadroom: 10, Pressure: 1.0})

	// Consumer A: low credit (0.3), seeded with thin local history.
	idA := makeID(0xAA)
	stA := consumerShardFor(s.consumerShards, idA).getOrInit(idA, t0.AddDate(0, 0, -3))
	stA.SettlementBuckets[0] = DayBucket{Total: 10, A: 3, DayStamp: stripToDay(t0)} // reliability 0.3
	stA.LastBalanceSeen = 100

	// Consumer B: higher credit (0.5).
	idB := makeID(0xBB)
	stB := consumerShardFor(s.consumerShards, idB).getOrInit(idB, t0.AddDate(0, 0, -10))
	stB.SettlementBuckets[0] = DayBucket{Total: 10, A: 5, DayStamp: stripToDay(t0)} // reliability 0.5
	stB.LastBalanceSeen = 100

	// A enqueues at t0.
	resA := s.Decide(idA, nil, t0)
	require.Equal(t, OutcomeQueue, resA.Outcome)

	// 9 minutes later, B enqueues.
	t1 := t0.Add(9 * time.Minute)
	clk.Advance(9 * time.Minute)
	resB := s.Decide(idB, nil, t1)
	require.Equal(t, OutcomeQueue, resB.Outcome)

	// Advance to t=10min and drain the queue's top.
	t2 := t0.Add(10 * time.Minute)
	clk.Advance(1 * time.Minute)

	s.queueMu.Lock()
	s.queue.AdvanceTime(t2)
	s.queue.Reheapify()
	first, ok := s.queue.Peek()
	s.queueMu.Unlock()

	require.True(t, ok)
	assert.Equal(t, idA, first.ConsumerID,
		"aged-A (0.3 credit, 10-min wait) outranks fresh-B (0.5 credit, 1-min wait) — admission-design §10 #6")

	// Effective priorities sanity:
	// A: 0.3 + 0.05 * 10 = 0.8
	// B: 0.5 + 0.05 * 1  = 0.55
}
```

- [ ] **Step 2: Run, confirm PASS**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 -run TestAgingBoost ./internal/admission/...
```

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission
git add tracker/internal/admission/aging_boost_test.go
git commit -m "$(cat <<'EOF'
test(tracker/admission): §10 #6 aging-boost acceptance

Aged-0.3-credit-A (waiting 10min) outranks fresh-0.5-credit-B (waiting
1min) once the heap re-orders. Effective priorities:
  A: 0.3 + 0.05 * 10 = 0.80
  B: 0.5 + 0.05 *  1 = 0.55

Validates the queue-drain step's expectation that low-credit consumers
eventually serve under sustained pressure rather than starving forever.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

## Task 12: Attestation issuance + per-consumer rate limiter

**Files:**
- Create: `tracker/internal/admission/attestation.go`
- Create: `tracker/internal/admission/attestation_test.go`
- Modify: `tracker/internal/admission/admission.go` (initialize rate limiter in Open)

Implements admission-design §5.5. `IssueAttestation(consumerID, now)` returns a signed `*admission.SignedCreditAttestation` whose body's signal fields come from `ComputeLocalScore`. Per-consumer sliding-window rate limit (default 6/hr).

- [ ] **Step 1: Write failing tests**

Write `tracker/internal/admission/attestation_test.go`:

```go
package admission

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/signing"
)

func TestIssueAttestation_NoLocalHistory_ReturnsSentinel(t *testing.T) {
	s, _ := openTempSubsystem(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	att, err := s.IssueAttestation(makeID(0x11), now)
	assert.Nil(t, att)
	assert.True(t, errors.Is(err, ErrNoLocalHistory))
}

func TestIssueAttestation_HappyPath_RoundTripsScoreAndVerifies(t *testing.T) {
	s, _ := openTempSubsystem(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	id := makeID(0x11)

	// Seed local history.
	st := consumerShardFor(s.consumerShards, id).getOrInit(id, now.AddDate(0, 0, -10))
	st.SettlementBuckets[0] = DayBucket{Total: 50, A: 50, DayStamp: stripToDay(now)}
	st.LastBalanceSeen = 1000

	att, err := s.IssueAttestation(id, now)
	require.NoError(t, err)
	require.NotNil(t, att)

	// Verifies under tracker's own pubkey.
	assert.True(t, signing.VerifyCreditAttestation(s.pub, att))

	// Body shape sanity.
	assert.Equal(t, id.Bytes()[:], att.Body.IdentityId)
	assert.Equal(t, s.trackerID.Bytes()[:], att.Body.IssuerTrackerId)
	assert.Greater(t, att.Body.Score, uint32(0))
	assert.LessOrEqual(t, att.Body.Score, uint32(10000))
	assert.Equal(t, uint64(now.Unix()), att.Body.ComputedAt)
	assert.Equal(t, uint64(now.Add(time.Duration(s.cfg.AttestationTTLSeconds)*time.Second).Unix()), att.Body.ExpiresAt)
}

func TestIssueAttestation_EmbeddedScoreMatchesLocalCompute(t *testing.T) {
	s, _ := openTempSubsystem(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	id := makeID(0x11)

	// Seed history with predictable signals.
	st := consumerShardFor(s.consumerShards, id).getOrInit(id, now.AddDate(0, 0, -30))
	st.SettlementBuckets[0] = DayBucket{Total: 100, A: 100, DayStamp: stripToDay(now)}
	st.LastBalanceSeen = 1000

	want, _ := ComputeLocalScore(st, s.cfg, now)
	att, err := s.IssueAttestation(id, now)
	require.NoError(t, err)
	gotScore := float64(att.Body.Score) / 10000.0
	assert.InDelta(t, want, gotScore, 1e-3, "embedded score must match ComputeLocalScore (admission-design §10 #8)")
}

func TestIssueAttestation_RateLimitExceeded_ReturnsSentinel(t *testing.T) {
	s, _ := openTempSubsystem(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	id := makeID(0x11)
	st := consumerShardFor(s.consumerShards, id).getOrInit(id, now.AddDate(0, 0, -10))
	st.SettlementBuckets[0] = DayBucket{Total: 10, A: 10, DayStamp: stripToDay(now)}

	// Default cap is 6/hr.
	for i := 0; i < s.cfg.AttestationIssuancePerConsumerPerHour; i++ {
		_, err := s.IssueAttestation(id, now.Add(time.Duration(i)*time.Minute))
		require.NoError(t, err)
	}
	// (cap+1)th call must reject.
	_, err := s.IssueAttestation(id, now.Add(time.Duration(s.cfg.AttestationIssuancePerConsumerPerHour)*time.Minute))
	assert.True(t, errors.Is(err, ErrRateLimited))
}

func TestIssueAttestation_RateLimitWindowSlides(t *testing.T) {
	s, _ := openTempSubsystem(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	id := makeID(0x11)
	st := consumerShardFor(s.consumerShards, id).getOrInit(id, now.AddDate(0, 0, -10))
	st.SettlementBuckets[0] = DayBucket{Total: 10, A: 10, DayStamp: stripToDay(now)}

	// Burn the cap.
	for i := 0; i < s.cfg.AttestationIssuancePerConsumerPerHour; i++ {
		_, err := s.IssueAttestation(id, now)
		require.NoError(t, err)
	}
	// One hour and one second later, the window has slid past — call succeeds.
	_, err := s.IssueAttestation(id, now.Add(time.Hour+time.Second))
	require.NoError(t, err)
}
```

- [ ] **Step 2: Run, confirm FAIL**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/admission/...
```

Expected: compile failure — `IssueAttestation`, `ErrNoLocalHistory`, `ErrRateLimited` undefined.

- [ ] **Step 3: Write `attestation.go`**

```go
package admission

import (
	"errors"
	"math"
	"sync"
	"time"

	"github.com/token-bay/token-bay/shared/admission"
	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/shared/signing"
)

// Sentinel errors returned by IssueAttestation.
var (
	ErrNoLocalHistory = errors.New("admission: no local consumer history")
	ErrRateLimited    = errors.New("admission: per-consumer attestation issuance rate limit exceeded")
)

// IssueAttestation returns a signed attestation built from the consumer's
// local credit state. Implements admission-design §5.5.
//
// Returns ErrNoLocalHistory if no ConsumerCreditState exists for the
// consumer (caller must have ledger history before requesting). Returns
// ErrRateLimited if the per-consumer rate limit (default 6/hr) is hit.
func (s *Subsystem) IssueAttestation(consumerID ids.IdentityID, now time.Time) (*admission.SignedCreditAttestation, error) {
	st, ok := consumerShardFor(s.consumerShards, consumerID).get(consumerID)
	if !ok {
		return nil, ErrNoLocalHistory
	}

	if !s.attestRL.allow(consumerID, now) {
		return nil, ErrRateLimited
	}

	score, sigs := ComputeLocalScore(st, s.cfg, now)

	body := &admission.CreditAttestationBody{
		IdentityId:            consumerID.Bytes()[:],
		IssuerTrackerId:       s.trackerID.Bytes()[:],
		Score:                 uint32(math.Round(score * 10000)),
		TenureDays:            uint32(sigs.TenureDays),
		SettlementReliability: nonNegativeFixedPoint(sigs.SettlementReliability),
		DisputeRate:           nonNegativeFixedPoint(sigs.DisputeRate),
		NetCreditFlow_30D:     sigs.NetFlow,
		BalanceCushionLog2:    int32(sigs.BalanceCushionLog2),
		ComputedAt:            uint64(now.Unix()),
		ExpiresAt:             uint64(now.Add(time.Duration(s.cfg.AttestationTTLSeconds) * time.Second).Unix()),
	}
	if err := admission.ValidateCreditAttestationBody(body); err != nil {
		// Defensive — should never happen given clamps in ComputeLocalScore.
		return nil, err
	}
	sig, err := signing.SignCreditAttestation(s.priv, body)
	if err != nil {
		return nil, err
	}
	return &admission.SignedCreditAttestation{Body: body, TrackerSig: sig}, nil
}

// nonNegativeFixedPoint converts a [-1, 1] signal to a uint32 in [0, 10000].
// Sentinel -1 (undefined) maps to 0.
func nonNegativeFixedPoint(v float64) uint32 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		v = 1
	}
	return uint32(math.Round(v * 10000))
}

// rateLimiter is a per-identity sliding-window counter. One window per
// identity, sharded by IdentityID mod numShards. Each window holds the
// last N issuance timestamps; allow() prunes events older than 1h and
// rejects when len ≥ cap.
type rateLimiter struct {
	cap     int
	window  time.Duration
	shards  []*rlShard
}

type rlShard struct {
	mu sync.Mutex
	m  map[ids.IdentityID][]time.Time
}

func newRateLimiter(perHourCap int, numShards int) *rateLimiter {
	rl := &rateLimiter{
		cap:    perHourCap,
		window: time.Hour,
		shards: make([]*rlShard, numShards),
	}
	for i := range rl.shards {
		rl.shards[i] = &rlShard{m: make(map[ids.IdentityID][]time.Time)}
	}
	return rl
}

func (rl *rateLimiter) allow(id ids.IdentityID, now time.Time) bool {
	sh := rl.shards[shardIndex(id, len(rl.shards))]
	sh.mu.Lock()
	defer sh.mu.Unlock()

	cutoff := now.Add(-rl.window)
	events := sh.m[id]
	// Prune.
	pruned := events[:0]
	for _, t := range events {
		if t.After(cutoff) {
			pruned = append(pruned, t)
		}
	}
	if len(pruned) >= rl.cap {
		sh.m[id] = pruned
		return false
	}
	sh.m[id] = append(pruned, now)
	return true
}
```

In `admission.go`, add field `attestRL *rateLimiter` to Subsystem, and initialize in `Open` after shard setup:

```go
s.attestRL = newRateLimiter(cfg.AttestationIssuancePerConsumerPerHour, registry.DefaultShardCount)
```

- [ ] **Step 4: Run, confirm PASS**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/admission/...
```

Expected: all 5 new tests PASS.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission
git add tracker/internal/admission/attestation.go tracker/internal/admission/attestation_test.go tracker/internal/admission/admission.go
git commit -m "$(cat <<'EOF'
feat(tracker/admission): IssueAttestation + per-consumer rate limiter

Implements admission-design §5.5: builds a CreditAttestationBody from
ComputeLocalScore output, fills issuer = tracker pubkey hash, signs via
shared/signing.SignCreditAttestation. Embedded score round-trips through
the §10 #8 acceptance: clients re-verifying must see exactly the score
the local compute produced.

Sentinel errors: ErrNoLocalHistory (no ConsumerCreditState exists),
ErrRateLimited (per-consumer 6/hr cap exceeded). Rate limiter is a
sliding-window counter sharded the same way as consumerShards.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

## Task 13: `LedgerEventObserver` interface + `OnLedgerEvent` for all kinds

**Files:**
- Create: `tracker/internal/admission/events.go`
- Create: `tracker/internal/admission/events_test.go`

Implements admission-design §5.6 — incremental in-memory state updates from ledger events. **No tlog write in this plan** — `OnLedgerEvent` is in-memory only; persistence lands in plan 3.

- [ ] **Step 1: Write failing tests**

Write `tracker/internal/admission/events_test.go`:

```go
package admission

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOnLedgerEvent_SettlementClean_BumpsTotalAndA(t *testing.T) {
	s, _ := openTempSubsystem(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	consumer := makeID(0xC1)
	seeder := makeID(0x5E)
	s.OnLedgerEvent(LedgerEvent{
		Kind:        LedgerEventSettlement,
		ConsumerID:  consumer,
		SeederID:    seeder,
		CostCredits: 100,
		Flags:       0, // clean
		Timestamp:   now,
	})

	cs, ok := consumerShardFor(s.consumerShards, consumer).get(consumer)
	require.True(t, ok)
	idx := dayBucketIndex(now, rollingWindowDays)
	assert.Equal(t, uint32(1), cs.SettlementBuckets[idx].Total)
	assert.Equal(t, uint32(1), cs.SettlementBuckets[idx].A, "clean settlement bumps A")
	assert.Equal(t, uint32(100), cs.FlowBuckets[idx].B, "consumer flow.Spent += cost")
	assert.Equal(t, int64(-100), cs.LastBalanceSeen)

	ss, ok := consumerShardFor(s.consumerShards, seeder).get(seeder)
	require.True(t, ok)
	assert.Equal(t, uint32(100), ss.FlowBuckets[idx].A, "seeder flow.Earned += cost")
	assert.Equal(t, int64(100), ss.LastBalanceSeen)
}

func TestOnLedgerEvent_SettlementWithFlag_BumpsTotalNotA(t *testing.T) {
	s, _ := openTempSubsystem(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	s.OnLedgerEvent(LedgerEvent{
		Kind:        LedgerEventSettlement,
		ConsumerID:  makeID(0xC1),
		SeederID:    makeID(0x5E),
		CostCredits: 100,
		Flags:       1, // consumer_sig_missing
		Timestamp:   now,
	})

	cs, _ := consumerShardFor(s.consumerShards, makeID(0xC1)).get(makeID(0xC1))
	idx := dayBucketIndex(now, rollingWindowDays)
	assert.Equal(t, uint32(1), cs.SettlementBuckets[idx].Total)
	assert.Equal(t, uint32(0), cs.SettlementBuckets[idx].A, "non-clean settlement does not bump A")
}

func TestOnLedgerEvent_StarterGrant_InitializesState(t *testing.T) {
	s, _ := openTempSubsystem(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	id := makeID(0x42)

	s.OnLedgerEvent(LedgerEvent{
		Kind:        LedgerEventStarterGrant,
		ConsumerID:  id,
		CostCredits: 1000,
		Timestamp:   now,
	})

	cs, ok := consumerShardFor(s.consumerShards, id).get(id)
	require.True(t, ok)
	assert.True(t, cs.FirstSeenAt.Equal(now))
	assert.Equal(t, int64(1000), cs.LastBalanceSeen)
}

func TestOnLedgerEvent_TransferIn_OutMirrorBalance(t *testing.T) {
	s, _ := openTempSubsystem(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	id := makeID(0x11)

	// transfer_in: balance increases.
	s.OnLedgerEvent(LedgerEvent{
		Kind: LedgerEventTransferIn, ConsumerID: id, CostCredits: 500, Timestamp: now,
	})
	cs, _ := consumerShardFor(s.consumerShards, id).get(id)
	assert.Equal(t, int64(500), cs.LastBalanceSeen)

	// transfer_out: balance decreases.
	s.OnLedgerEvent(LedgerEvent{
		Kind: LedgerEventTransferOut, ConsumerID: id, CostCredits: 200, Timestamp: now,
	})
	cs2, _ := consumerShardFor(s.consumerShards, id).get(id)
	assert.Equal(t, int64(300), cs2.LastBalanceSeen)

	// Neither transfer touches usage buckets.
	idx := dayBucketIndex(now, rollingWindowDays)
	assert.Equal(t, uint32(0), cs2.SettlementBuckets[idx].Total)
	assert.Equal(t, uint32(0), cs2.FlowBuckets[idx].A)
}

func TestOnLedgerEvent_DisputeFiledThenUpheld(t *testing.T) {
	s, _ := openTempSubsystem(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	id := makeID(0x11)

	// Filed.
	s.OnLedgerEvent(LedgerEvent{Kind: LedgerEventDisputeFiled, ConsumerID: id, Timestamp: now})
	cs, _ := consumerShardFor(s.consumerShards, id).get(id)
	idx := dayBucketIndex(now, rollingWindowDays)
	assert.Equal(t, uint32(1), cs.DisputeBuckets[idx].A)
	assert.Equal(t, uint32(0), cs.DisputeBuckets[idx].B)

	// Upheld.
	s.OnLedgerEvent(LedgerEvent{Kind: LedgerEventDisputeResolved, ConsumerID: id, DisputeUpheld: true, Timestamp: now})
	cs2, _ := consumerShardFor(s.consumerShards, id).get(id)
	assert.Equal(t, uint32(1), cs2.DisputeBuckets[idx].B)

	// Resolved-but-rejected does NOT bump B.
	s.OnLedgerEvent(LedgerEvent{Kind: LedgerEventDisputeResolved, ConsumerID: id, DisputeUpheld: false, Timestamp: now})
	cs3, _ := consumerShardFor(s.consumerShards, id).get(id)
	assert.Equal(t, uint32(1), cs3.DisputeBuckets[idx].B, "rejected dispute does not bump Upheld")
}

func TestOnLedgerEvent_StaleBucket_Rotates(t *testing.T) {
	s, _ := openTempSubsystem(t)
	id := makeID(0x11)

	// First event lands in some bucket.
	t1 := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s.OnLedgerEvent(LedgerEvent{
		Kind: LedgerEventSettlement, ConsumerID: id, SeederID: makeID(0x5E),
		CostCredits: 100, Flags: 0, Timestamp: t1,
	})

	// 35 days later, same bucket index — must zero before incrementing.
	t2 := t1.AddDate(0, 0, 35)
	s.OnLedgerEvent(LedgerEvent{
		Kind: LedgerEventSettlement, ConsumerID: id, SeederID: makeID(0x5E),
		CostCredits: 50, Flags: 0, Timestamp: t2,
	})

	cs, _ := consumerShardFor(s.consumerShards, id).get(id)
	idx := dayBucketIndex(t2, rollingWindowDays)
	assert.Equal(t, uint32(1), cs.SettlementBuckets[idx].Total, "stale bucket must rotate to 0 before incrementing")
	assert.Equal(t, uint32(1), cs.SettlementBuckets[idx].A)
}

func TestOnLedgerEvent_UnspecifiedKind_NoOp(t *testing.T) {
	s, _ := openTempSubsystem(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	// LedgerEventUnspecified must be ignored cleanly (no panic, no state change).
	s.OnLedgerEvent(LedgerEvent{Kind: LedgerEventUnspecified, ConsumerID: makeID(0x11), Timestamp: now})
	_, ok := consumerShardFor(s.consumerShards, makeID(0x11)).get(makeID(0x11))
	assert.False(t, ok, "unspecified kind must not initialize state")
}
```

- [ ] **Step 2: Run, confirm FAIL**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/admission/...
```

Expected: compile failure — `LedgerEvent`, `LedgerEventKind`, `LedgerEvent*` consts, `OnLedgerEvent` undefined.

- [ ] **Step 3: Write `events.go`**

```go
package admission

import (
	"time"

	"github.com/token-bay/token-bay/shared/ids"
)

// LedgerEventKind classifies an in-process ledger event observed by
// admission. Mirrors admission-design §5.6.
type LedgerEventKind uint8

const (
	LedgerEventUnspecified     LedgerEventKind = iota
	LedgerEventSettlement                       // 1: usage entry finalized
	LedgerEventTransferIn                       // 2: peer-region credit landing
	LedgerEventTransferOut                      // 3: peer-region credit leaving
	LedgerEventStarterGrant                     // 4: starter grant issued
	LedgerEventDisputeFiled                     // 5: dispute opened
	LedgerEventDisputeResolved                  // 6: dispute closed
)

// LedgerEvent is the single input to admission's OnLedgerEvent. Real
// ledger emission is a separate plan; tests drive this directly.
type LedgerEvent struct {
	Kind          LedgerEventKind
	ConsumerID    ids.IdentityID
	SeederID      ids.IdentityID // zero for non-usage kinds
	CostCredits   uint64
	Flags         uint32 // bit 0 = consumer_sig_missing (settlements only)
	DisputeUpheld bool   // settlements: false; resolved disputes: true → UPHELD
	Timestamp     time.Time
}

// LedgerEventObserver is the consumer-side contract. Subsystem implements
// it; admission-aware ledger callers (broker, settlement) will dispatch
// to one or more observers via a tiny pub/sub introduced in a follow-up
// ledger plan.
type LedgerEventObserver interface {
	OnLedgerEvent(ev LedgerEvent)
}

// OnLedgerEvent updates in-memory bucket state per admission-design §5.6.
// Persistence (admission.tlog) is plan 3 — this revision is in-memory only.
func (s *Subsystem) OnLedgerEvent(ev LedgerEvent) {
	switch ev.Kind {
	case LedgerEventSettlement:
		s.applySettlement(ev)
	case LedgerEventTransferIn:
		s.applyTransfer(ev.ConsumerID, +int64(ev.CostCredits), ev.Timestamp)
	case LedgerEventTransferOut:
		s.applyTransfer(ev.ConsumerID, -int64(ev.CostCredits), ev.Timestamp)
	case LedgerEventStarterGrant:
		s.applyStarterGrant(ev)
	case LedgerEventDisputeFiled:
		s.applyDispute(ev, false)
	case LedgerEventDisputeResolved:
		if ev.DisputeUpheld {
			s.applyDispute(ev, true)
		}
		// rejected resolutions: no state change beyond the original Filed.
	default:
		// LedgerEventUnspecified — no-op.
	}
}

func (s *Subsystem) applySettlement(ev LedgerEvent) {
	idx := dayBucketIndex(ev.Timestamp, rollingWindowDays)

	// Consumer side.
	cst := consumerShardFor(s.consumerShards, ev.ConsumerID).getOrInit(ev.ConsumerID, ev.Timestamp)
	cst.SettlementBuckets[idx] = rotateIfStale(cst.SettlementBuckets[idx], ev.Timestamp, rollingWindowDays)
	cst.SettlementBuckets[idx].Total++
	if ev.Flags&1 == 0 {
		cst.SettlementBuckets[idx].A++
	}
	cst.FlowBuckets[idx] = rotateIfStale(cst.FlowBuckets[idx], ev.Timestamp, rollingWindowDays)
	cst.FlowBuckets[idx].B += uint32(ev.CostCredits) // spent
	cst.LastBalanceSeen -= int64(ev.CostCredits)

	// Seeder side (same hashmap; seeder may also be a consumer).
	if ev.SeederID != (ids.IdentityID{}) {
		sst := consumerShardFor(s.consumerShards, ev.SeederID).getOrInit(ev.SeederID, ev.Timestamp)
		sst.FlowBuckets[idx] = rotateIfStale(sst.FlowBuckets[idx], ev.Timestamp, rollingWindowDays)
		sst.FlowBuckets[idx].A += uint32(ev.CostCredits) // earned
		sst.LastBalanceSeen += int64(ev.CostCredits)
	}
}

func (s *Subsystem) applyTransfer(id ids.IdentityID, delta int64, now time.Time) {
	st := consumerShardFor(s.consumerShards, id).getOrInit(id, now)
	st.LastBalanceSeen += delta
}

func (s *Subsystem) applyStarterGrant(ev LedgerEvent) {
	st := consumerShardFor(s.consumerShards, ev.ConsumerID).getOrInit(ev.ConsumerID, ev.Timestamp)
	if st.FirstSeenAt.IsZero() || st.FirstSeenAt.After(ev.Timestamp) {
		st.FirstSeenAt = ev.Timestamp
	}
	st.LastBalanceSeen = int64(ev.CostCredits)
}

func (s *Subsystem) applyDispute(ev LedgerEvent, upheld bool) {
	idx := dayBucketIndex(ev.Timestamp, rollingWindowDays)
	st := consumerShardFor(s.consumerShards, ev.ConsumerID).getOrInit(ev.ConsumerID, ev.Timestamp)
	st.DisputeBuckets[idx] = rotateIfStale(st.DisputeBuckets[idx], ev.Timestamp, rollingWindowDays)
	if upheld {
		st.DisputeBuckets[idx].B++
	} else {
		st.DisputeBuckets[idx].A++
	}
}
```

Note on locking: `getOrInit` returns a pointer that callers mutate. Multiple goroutines mutating the same `*ConsumerCreditState` concurrently would race; production callers (broker → admission, future ledger-bus) deliver events serially per-consumer (the ledger orchestrator's `mu` already serializes settlements). Tests drive events synchronously. If concurrent-mutation pressure arises, add a per-state mutex in plan 3 alongside the tlog write — for plan 2 the in-memory updates are race-clean because the test driver is sequential and `OnLedgerEvent` itself doesn't fork goroutines.

- [ ] **Step 4: Run, confirm PASS**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/admission/...
```

Expected: all 7 new tests PASS.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission
git add tracker/internal/admission/events.go tracker/internal/admission/events_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/admission): LedgerEventObserver + OnLedgerEvent (in-memory)

Implements admission-design §5.6 in-memory state updates:
  - SETTLEMENT: SettlementBuckets.Total++ always, .A++ when flags bit 0
    is unset (clean); FlowBuckets bump consumer.Spent / seeder.Earned;
    LastBalanceSeen mirror updated
  - TRANSFER_IN / TRANSFER_OUT: LastBalanceSeen mirror only (transfers
    are not usage and don't feed reliability/dispute/flow signals)
  - STARTER_GRANT: initialize FirstSeenAt + LastBalanceSeen
  - DISPUTE_FILED: DisputeBuckets.A++ (filed)
  - DISPUTE_RESOLVED + UPHELD: DisputeBuckets.B++ (upheld). Rejected
    resolutions are no-ops — the original Filed event already counted.

NO tlog write in this plan — that's plan 3. The interface defined here
(LedgerEventObserver) is the consumer-side contract; ledger-side
emission of these events is a separate ledger-package plan.

Stale-bucket rotation is handled by rotateIfStale on every increment so
a bucket that hasn't been touched in > 30 days zeroes before
incrementing.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

## Task 14: `helpers_test.go` — extract test fixtures

**Files:**
- Create: `tracker/internal/admission/helpers_test.go`
- Modify: every prior `*_test.go` in `tracker/internal/admission/` — remove inline duplicates of helpers, leave only test functions

The earlier tasks defined `openTempSubsystem`, `fixtureKeypair`, `fixturePeerKeypair`, `staticPeers`, `validAttestationBody`, `signFixtureAttestation`, `staticClockFn`, `makeIDi`, `fixedRand`, `newFixedClock`, `registerSeeder`, `defaultScoreConfig` inline in the first test file that needed each. This task **moves them all to `helpers_test.go`** and removes the duplicates. Pure refactor; no behavior change. Run all tests after to confirm green.

- [ ] **Step 1: Create `helpers_test.go`**

Write `tracker/internal/admission/helpers_test.go`:

```go
package admission

import (
	"crypto/ed25519"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/admission"
	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/shared/signing"
	"github.com/token-bay/token-bay/tracker/internal/config"
	"github.com/token-bay/token-bay/tracker/internal/registry"
)

// fixtureTrackerSeed is the deterministic seed for the test tracker
// keypair. Stable across runs so attestation goldens (if added) reproduce.
var fixtureTrackerSeed = []byte("admission-fixture-tracker-seed-1")

// fixturePeerSeed is the deterministic seed for a "remote" tracker
// (peer) keypair used in attestation-validation tests.
var fixturePeerSeed = []byte("admission-fixture-peer-seed-v1-x")

// defaultScoreConfig returns the AdmissionConfig used by openTempSubsystem.
// Mirrors the spec defaults from admission-design §9.3.
func defaultScoreConfig() config.AdmissionConfig {
	return config.AdmissionConfig{
		PressureAdmitThreshold:                0.85,
		PressureRejectThreshold:               1.5,
		QueueCap:                              512,
		TrialTierScore:                        0.4,

		AgingAlphaPerMinute:                   0.05,
		QueueTimeoutS:                         300,

		ScoreWeights: config.AdmissionScoreWeights{
			SettlementReliability: 0.30,
			InverseDisputeRate:    0.10,
			Tenure:                0.20,
			NetCreditFlow:         0.30,
			BalanceCushion:        0.10,
		},

		NetFlowNormalizationConstant:          10000,
		TenureCapDays:                         30,
		StarterGrantCredits:                   1000,
		RollingWindowDays:                     30,

		AttestationTTLSeconds:                 86400,
		AttestationMaxTTLSeconds:              604800,
		AttestationIssuancePerConsumerPerHour: 6,
		MaxAttestationScoreImported:           0.95,

		HeartbeatWindowMinutes:                10,
		HeartbeatFreshnessDecayMaxS:           300,
	}
}

// openTempSubsystem returns a wired *Subsystem with fixed clock, default
// config, empty registry, and the fixture tracker keypair. Cleanup hooks
// close it on test completion.
func openTempSubsystem(t *testing.T, opts ...Option) (*Subsystem, ids.IdentityID) {
	t.Helper()
	require.Len(t, fixtureTrackerSeed, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(fixtureTrackerSeed)

	reg, err := registry.New(16)
	require.NoError(t, err)

	s, err := Open(defaultScoreConfig(), reg, priv, opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s, s.trackerID
}

// fixturePeerKeypair returns the fixture peer-tracker (Ed25519) keypair
// + its derived IdentityID. Used in attestation-validation tests where
// the peer is the "issuer" of imported attestations.
func fixturePeerKeypair(t *testing.T) (ed25519.PrivateKey, ids.IdentityID) {
	t.Helper()
	require.Len(t, fixturePeerSeed, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(fixturePeerSeed)
	pub := priv.Public().(ed25519.PublicKey)
	var id ids.IdentityID
	copy(id[:], pub[:32])
	return priv, id
}

// staticPeers is a deterministic PeerSet implementation for tests.
type staticPeers map[ids.IdentityID]bool

func (s staticPeers) Contains(id ids.IdentityID) bool { return s[id] }

// validAttestationBody returns a populated body that passes
// admission.ValidateCreditAttestationBody. Tests mutate single fields
// (Score, ExpiresAt, etc.) to test specific code paths.
func validAttestationBody(now time.Time, peerID ids.IdentityID, consumerByte byte) *admission.CreditAttestationBody {
	return &admission.CreditAttestationBody{
		IdentityId:            makeID(consumerByte).Bytes(),
		IssuerTrackerId:       peerID.Bytes(),
		Score:                 7000,
		TenureDays:            60,
		SettlementReliability: 9000,
		DisputeRate:           100,
		NetCreditFlow_30D:     50000,
		BalanceCushionLog2:    1,
		ComputedAt:            uint64(now.Add(-time.Hour).Unix()),
		ExpiresAt:             uint64(now.Add(23 * time.Hour).Unix()),
	}
}

// signFixtureAttestation signs body with priv via shared/signing helpers.
func signFixtureAttestation(t *testing.T, priv ed25519.PrivateKey, body *admission.CreditAttestationBody) *admission.SignedCreditAttestation {
	t.Helper()
	sig, err := signing.SignCreditAttestation(priv, body)
	require.NoError(t, err)
	return &admission.SignedCreditAttestation{Body: body, TrackerSig: sig}
}

// fixedClock returns the same Now() value until Advance shifts it.
type fixedClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFixedClock(t time.Time) *fixedClock { return &fixedClock{now: t} }

func (c *fixedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fixedClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// staticClockFn returns a func that always reports t.
func staticClockFn(t time.Time) func() time.Time { return func() time.Time { return t } }

// registerSeeder is a one-line registry mutation used by supply-aggregator
// tests.
func registerSeeder(t *testing.T, reg *registry.Registry, id ids.IdentityID, headroom float64, lastHB time.Time) {
	t.Helper()
	reg.Register(registry.SeederRecord{
		IdentityID:       id,
		HeadroomEstimate: headroom,
		LastHeartbeat:    lastHB,
		Available:        true,
		ReputationScore:  1.0,
	})
}

// makeID builds an IdentityID with all bytes set to b.
func makeID(b byte) ids.IdentityID {
	var id ids.IdentityID
	for i := range id {
		id[i] = b
	}
	return id
}

// makeIDi builds an IdentityID encoding i in the first two bytes.
// Used by queue-overflow tests that need many distinct identities.
func makeIDi(i int) ids.IdentityID {
	var id ids.IdentityID
	id[0] = byte(i & 0xff)
	id[1] = byte((i >> 8) & 0xff)
	return id
}

// fixedRand returns a deterministic *rand.Rand for jitter tests.
func fixedRand(seed int64) *rand.Rand { return rand.New(rand.NewSource(seed)) }
```

- [ ] **Step 2: Remove inline duplicates from prior `*_test.go` files**

Each of these files currently re-defines one or more helpers from above. Delete those local definitions; the package-level versions in `helpers_test.go` resolve them automatically.

- `state_test.go` — remove the local `makeID` definition.
- `score_test.go` — remove the local `defaultScoreConfig`, `validAttestationBody`, `signFixtureAttestation`, `fixturePeerKeypair`, `staticPeers`, `openTempSubsystem` definitions.
- `supply_test.go` — remove the local `fixedClock`, `newFixedClock`, `registerSeeder` definitions.
- `decide_test.go` — remove the local `staticClockFn`, `makeIDi`, `fixedRand` definitions.

- [ ] **Step 3: Run all tests, confirm green**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/admission/...
```

Expected: all tests still green; no behavior change. If any test fails because a helper isn't found, the deletion in Step 2 missed a duplicate definition or left an orphan reference.

- [ ] **Step 4: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission
git add tracker/internal/admission/helpers_test.go tracker/internal/admission/*_test.go
git commit -m "$(cat <<'EOF'
refactor(tracker/admission): extract test fixtures into helpers_test.go

Moves the inline helpers (openTempSubsystem, fixturePeerKeypair,
staticPeers, validAttestationBody, signFixtureAttestation, fixedClock,
registerSeeder, makeID, makeIDi, fixedRand, defaultScoreConfig) out of
the per-feature test files and into a single helpers_test.go.

Pure refactor — no behavior change. Each prior task's commit was
self-contained because the inline helpers were duplicated; with all
tasks landed, the duplication is consolidated.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

## Task 15: Final integration — race-clean + repo-root `make check` + branch push

**Files:** none modified. Final integration check.

- [ ] **Step 1: `go test -race -count=10` on the admission package**

Repeat the admission tests 10 times with the race detector to flush out timing-dependent flakes that a single run might miss.

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=10 ./internal/admission/...
```

Expected: every iteration PASS, no race-detector reports.

- [ ] **Step 2: Repo-root `make check`**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" make check
```

Expected: every module's tests pass; lint clean across all modules.

- [ ] **Step 3: Coverage check on `internal/admission`**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 -coverprofile=/tmp/admission.cov ./internal/admission/...
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go tool cover -func=/tmp/admission.cov | tail -30
rm -f /tmp/admission.cov
```

Expected: every function-level row reports ≥ 90% coverage. Functions below the threshold get a follow-up commit adding a test (or, if the uncovered branch is unreachable like the `SignBalanceSnapshot` marshal-error path, accept the gap and document why).

- [ ] **Step 4: Push the branch**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission
git push -u origin tracker_admission_core
```

- [ ] **Step 5: Open PR**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission
gh pr create --base main --title "feat(tracker/admission): in-memory core — Decide, score, queue, attestation, ledger-event observer" --body "$(cat <<'EOF'
## Summary

Plan 2 of three for the admission subsystem (spec at
docs/superpowers/specs/admission/2026-04-25-admission-design.md).

Lands `tracker/internal/admission` core, in-memory only:
- ConsumerCreditState (rolling 30-day buckets) + SeederHeartbeatState
- ComputeLocalScore — 5-signal weighted compose with undefined-redistribution
- resolveCreditScore — attestation → local → trial-tier with §8.6 clamp
- 5s SupplySnapshot aggregator + freshness decay + demand EWMA
- QueueEntry + max-heap with aging boost
- Decide — ADMIT | QUEUE | REJECT branching
- IssueAttestation + per-consumer rate limiter
- LedgerEventObserver interface + OnLedgerEvent in-memory updates
- §6.4 decision-matrix table-driven test
- §10 #6 aging-boost acceptance integration test

Out of scope (deferred):
- Persistence (admission.tlog + snapshot + replay) → plan 3
- Admin HTTP endpoints, Prometheus metrics → plan 3
- Broker integration → future tracker control-plane plan
- FetchHeadroom RPC → future tracker↔seeder RPC plan
- Real ledger event-bus emission → future ledger-package plan
- Real federation peer-set → future federation plan; v1 uses always-false stub

Plan: docs/superpowers/plans/2026-04-26-tracker-admission-core.md

## Test plan

- [ ] make check from repo root, no failures
- [ ] go test -race -count=10 ./internal/admission/... — no race reports across iterations
- [ ] Coverage on internal/admission/ ≥ 90% per function
- [ ] decision-matrix produces 45 subtests, all green
- [ ] aging-boost integration test asserts §10 #6 ordering
EOF
)"
```

If `gh pr create --fill` is preferred (per `tracker_admission/CLAUDE.md` PR-submission section), substitute that instead.

- [ ] **Step 6: Watch CI**

```bash
gh pr checks --watch
```

Expected: every check `pass`. Per `CLAUDE.md`'s PR submission rules, do NOT merge with failing or pending checks; do NOT bypass branch protections without explicit user approval.

- [ ] **Step 7: Merge when CI is green**

```bash
gh pr merge --squash --delete-branch
```

(Or coordinate with the user; merge is a "visible to others" action.)

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
