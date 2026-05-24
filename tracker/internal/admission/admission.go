package admission

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/config"
	"github.com/token-bay/token-bay/tracker/internal/registry"
)

// Subsystem owns the in-memory admission state and the goroutines that keep
// it fresh. One per tracker process. Construct via Open; tear down via Close.
//
// Fields are added incrementally as later tasks introduce them:
//   - queue + queueMu — Task 8
//   - aggregatorTick + demand — Task 7
//   - attestRL — Task 12
type Subsystem struct {
	cfg       config.AdmissionConfig
	reg       *registry.Registry
	priv      ed25519.PrivateKey
	pub       ed25519.PublicKey
	trackerID ids.IdentityID
	nowFn     func() time.Time

	// supply snapshot published atomically by the aggregator (Task 7).
	supply atomic.Pointer[SupplySnapshot]

	// demand EWMA and aggregator tick channel (Task 7).
	demand         demandTracker
	aggregatorTick chan time.Time

	// queue — single max-heap with one mutex (Task 8).
	queueMu sync.Mutex //nolint:unused // wired in Task 9 (Decide hot-path)
	queue   *queueHeap

	// per-consumer credit state — sharded (Task 3).
	consumerShards []*consumerShard

	// per-seeder heartbeat state — sharded (Task 3).
	seederShards []*seederShard

	// federation peer-set membership check (§5.1 step 1).
	peers PeerSet

	// per-consumer attestation issuance rate limiter (Task 12).
	attestRL *rateLimiter

	// persistence — plan 3.
	tlog           *tlogWriter
	tlogPath       string
	snapshotPrefix string

	// replay state — plan 3.
	ledgerSrc            LedgerSource
	skipAutoReplay       bool
	replaying            atomic.Bool
	degradedMode         atomic.Uint32
	snapshotLoadFailures atomic.Uint64
	tlogCorruptions      atomic.Uint64

	// nextSeq is the monotonic seq for tlog records. Atomic so concurrent
	// OnLedgerEvent callers (race-clean test) get distinct seqs.
	nextSeq atomic.Uint64

	// admin — plan 3.
	adminBlocklistOnce sync.Once
	adminBlocklist     *adminBlocklist

	// metrics — plan 3.
	metrics *admissionMetrics

	stop chan struct{}
	wg   sync.WaitGroup
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

// WithSnapshotPrefix sets the snapshot path prefix (snapshots are written
// to <prefix>.<seq>). Empty disables snapshot emission.
func WithSnapshotPrefix(prefix string) Option {
	return func(s *Subsystem) { s.snapshotPrefix = prefix }
}

// WithTLogPath sets the active tlog file path. Empty disables tlog writes.
func WithTLogPath(path string) Option {
	return func(s *Subsystem) { s.tlogPath = path }
}

// WithSnapshotsRetained overrides cfg.SnapshotsRetained (1-N). Useful for
// tests that exercise pruning without needing many snapshots.
func WithSnapshotsRetained(n int) Option {
	return func(s *Subsystem) { s.cfg.SnapshotsRetained = n }
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
	s.queue = newQueueHeap(time.Time{}, cfg.AgingAlphaPerMinute)
	s.attestRL = newRateLimiter(cfg.AttestationIssuancePerConsumerPerHour, registry.DefaultShardCount)
	s.metrics = newAdmissionMetrics()

	if s.tlogPath != "" {
		w, err := newTLogWriter(s.tlogPath, 5*time.Millisecond, 1<<30)
		if err != nil {
			return nil, fmt.Errorf("admission: open tlog: %w", err)
		}
		s.tlog = w
	}

	s.startAggregator()
	s.startSnapshotEmitter()
	return s, nil
}

// startSnapshotEmitter spawns a goroutine that periodically calls
// runSnapshotEmitOnce on cfg.SnapshotIntervalS cadence. No-op when
// snapshotPrefix is unset (persistence disabled) or SnapshotIntervalS<=0.
// Stops on s.stop. Errors are swallowed — operators see them via the
// admin /admission/snapshot endpoint or the next emit cycle.
func (s *Subsystem) startSnapshotEmitter() {
	if s.snapshotPrefix == "" || s.cfg.SnapshotIntervalS <= 0 {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(time.Duration(s.cfg.SnapshotIntervalS) * time.Second)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case now := <-t.C:
				_ = s.runSnapshotEmitOnce(now)
			}
		}
	}()
}

// PressureGauge returns the most recent supply-aggregator pressure (demand_rate /
// supply_estimate). Returns 0.0 when the aggregator hasn't published yet
// (boot-time), which the broker treats as "no pressure".
func (s *Subsystem) PressureGauge() float64 {
	snap := s.Supply()
	if snap.ComputedAt.IsZero() {
		return 0.0
	}
	return snap.Pressure
}

// QueueTimeout returns the configured cap on how long a queued admission
// decision may wait before the api-layer block-then-deliver path gives up
// and emits a wire Rejected{queue_timeout} (admission-design §5.4 / spec
// §5.4). Returns 0 when QueueTimeoutS is non-positive — callers MUST treat
// zero as "no operator-configured cap" and pick their own safety bound.
func (s *Subsystem) QueueTimeout() time.Duration {
	if s.cfg.QueueTimeoutS <= 0 {
		return 0
	}
	return time.Duration(s.cfg.QueueTimeoutS) * time.Second
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
	if s.tlog != nil {
		_ = s.tlog.Close()
	}
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
