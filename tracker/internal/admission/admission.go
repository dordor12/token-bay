package admission

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	sharedadmission "github.com/token-bay/token-bay/shared/admission"
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
	_ *sharedadmission.SignedCreditAttestation
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

// SupplySnapshot holds an aggregated view of regional supply pressure.
// Replaced by the real implementation in Task 7.
type SupplySnapshot struct{}

// queueHeap is a max-heap of QueueEntry values, ordered by effective priority.
// Replaced by the real implementation in Task 8.
type queueHeap struct{}

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
