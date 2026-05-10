package seederflow

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/token-bay/token-bay/plugin/internal/ccbridge"
)

// Config groups every dependency the coordinator requires. The cmd
// layer constructs it from disk-resident inputs (config, identity
// files, audit log) and injects each subsystem via interfaces so tests
// can substitute fakes.
type Config struct {
	Logger         zerolog.Logger
	Bridge         Bridge
	Tracker        UsageReporter
	AuditLog       AuditSink
	Signer         Signer
	Acceptor       TunnelAcceptor
	Runner         ccbridge.Runner
	ConformanceFn  ConformanceFn
	IdlePolicy     IdlePolicy
	ActivityGrace  time.Duration
	HeadroomWindow time.Duration
	Models         []string
	MaxContext     uint32
	Tiers          uint32

	// Clock returns the current time. Defaults to time.Now.
	Clock func() time.Time
	// Rand is the source of randomness for ephemeral keys. Defaults
	// to crypto/rand.Reader.
	Rand io.Reader
}

// Coordinator is the seeder-side flow orchestrator. Public methods are
// safe for concurrent use.
type Coordinator struct {
	cfg Config

	mu sync.Mutex
	// tracker is the live UsageReporter; SetTracker swaps it under mu
	// without mutating cfg.
	tracker UsageReporter
	// reservations is keyed by EnvelopeHash (32 bytes hex-encoded).
	reservations map[string]*reservation
	// activeClients maps ccbridge.ClientHash(consumer-identity-pub) → live count.
	activeClients map[string]int
	// lastActivityAt is the most recent SessionStart/SessionEnd-derived
	// stamp.
	lastActivityAt time.Time
	// activityActive is true between SessionStart and SessionEnd.
	activityActive bool
	// lastRateLimitAt is the most recent observed rate-limit signal.
	lastRateLimitAt time.Time
	// conformanceFailed flips true when the boot or version-change
	// conformance run fails. Sticky until ResetConformance is called.
	conformanceFailed bool

	startMu sync.Mutex
	started bool
	closed  bool
}

// reservation holds the per-offer state created on accept and consumed
// when the consumer's tunnel dial arrives.
type reservation struct {
	envelopeHash    [32]byte
	consumerIDHash  [32]byte
	model           string
	maxInputTokens  uint32
	maxOutputTokens uint32
	ephemeralPub    ed25519.PublicKey
	ephemeralPriv   ed25519.PrivateKey
	registeredAt    time.Time
}

// New validates cfg and constructs (but does not start) a Coordinator.
func New(cfg Config) (*Coordinator, error) {
	if err := validateConfig(&cfg); err != nil {
		return nil, err
	}
	return &Coordinator{
		cfg:           cfg,
		tracker:       cfg.Tracker,
		reservations:  make(map[string]*reservation),
		activeClients: make(map[string]int),
	}, nil
}

// usageReporter returns the live UsageReporter under mu.
func (c *Coordinator) usageReporter() UsageReporter {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tracker
}

// SetTracker swaps the coordinator's UsageReporter. Used by the cmd
// layer to break the circular construction order between the
// trackerclient (which needs the coordinator as OfferHandler) and the
// coordinator (which needs the trackerclient as Tracker): construct
// the coordinator with a placeholder UsageReporter, build the final
// trackerclient with the coordinator as OfferHandler, then call
// SetTracker before Run. Safe for concurrent use.
func (c *Coordinator) SetTracker(t UsageReporter) {
	if t == nil {
		return
	}
	c.mu.Lock()
	c.tracker = t
	c.mu.Unlock()
}

func validateConfig(cfg *Config) error {
	if cfg.Bridge == nil {
		return fmt.Errorf("%w: Bridge is required", ErrInvalidConfig)
	}
	if cfg.Tracker == nil {
		return fmt.Errorf("%w: Tracker is required", ErrInvalidConfig)
	}
	if cfg.AuditLog == nil {
		return fmt.Errorf("%w: AuditLog is required", ErrInvalidConfig)
	}
	if cfg.Signer == nil {
		return fmt.Errorf("%w: Signer is required", ErrInvalidConfig)
	}
	if cfg.Acceptor == nil {
		return fmt.Errorf("%w: Acceptor is required", ErrInvalidConfig)
	}
	if len(cfg.Models) == 0 {
		return fmt.Errorf("%w: Models must be non-empty", ErrInvalidConfig)
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Reader
	}
	if cfg.HeadroomWindow == 0 {
		cfg.HeadroomWindow = 15 * time.Minute
	}
	if cfg.ActivityGrace == 0 {
		cfg.ActivityGrace = 10 * time.Minute
	}
	return nil
}
