package stunturn

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
)

// AllocatorConfig parameterizes a *Allocator. Now and Rand are
// dependency-injected so tests run with deterministic time and bytes.
//
// Production wiring: cfg.MaxKbpsPerSeeder = STUNTURN.TURNRelayMaxKbps,
// cfg.SessionTTL = time.Duration(STUNTURN.SessionTTLSeconds) * time.Second,
// cfg.Now = time.Now, cfg.Rand = crypto/rand.Reader.
type AllocatorConfig struct {
	// MaxKbpsPerSeeder is the per-seeder bandwidth cap, in kilobits per
	// second. Must be > 0. Internally converted to bytes-per-second.
	MaxKbpsPerSeeder int

	// SessionTTL is the idle-expiry threshold. A session whose
	// LastActive is older than SessionTTL is treated as expired by
	// ResolveAndCharge and removed by Sweep. Must be > 0.
	SessionTTL time.Duration

	// Now returns the current time. Must be non-nil.
	Now func() time.Time

	// Rand is the source of randomness for token generation. Must be
	// non-nil. Production callers pass crypto/rand.Reader.
	Rand io.Reader
}

// Session is the public projection of a TURN session. Returned by
// Allocate / Resolve / ResolveAndCharge as a value; mutations to a
// returned Session do not affect allocator state.
type Session struct {
	// Token is the 16-byte opaque handle identifying this session.
	Token Token

	// SessionID is the monotonically increasing internal session ID.
	SessionID uint64

	// ConsumerID is the identity of the consumer that requested the session.
	ConsumerID ids.IdentityID

	// SeederID is the identity of the seeder serving the session.
	SeederID ids.IdentityID

	// RequestID is the client-supplied deduplication key.
	RequestID [16]byte

	// AllocatedAt is the time the session was created.
	AllocatedAt time.Time

	// LastActive is the time of the last ResolveAndCharge call for this session.
	LastActive time.Time
}

// Allocator manages TURN session state. Safe for concurrent use; every
// public method holds an internal sync.Mutex. See package doc for the
// scaling notes and the documented forward path.
type Allocator struct {
	cfg AllocatorConfig

	mu      sync.Mutex //nolint:unused // used by Allocate/Release/ResolveAndCharge/Sweep in later tasks
	nextID  uint64     //nolint:unused // incremented by Allocate in later tasks
	byToken map[Token]*sessionEntry
	bySID   map[uint64]*sessionEntry
	byReq   map[[16]byte]*sessionEntry
	buckets map[ids.IdentityID]*tokenBucket
}

// sessionEntry is the internal heap record. The same pointer is shared
// across byToken / bySID / byReq.
type sessionEntry struct {
	session Session //nolint:unused // read by Resolve/ResolveAndCharge in later tasks
}

// tokenBucket models per-seeder kbps rate limiting. capacityBytes is one
// second of burst at MaxKbpsPerSeeder; refillPerSec equals capacityBytes
// (steady-state matches the cap).
type tokenBucket struct {
	capacityBytes float64   //nolint:unused // used by Charge/ResolveAndCharge in later tasks
	refillPerSec  float64   //nolint:unused // used by refill logic in later tasks
	available     float64   //nolint:unused // debited by Charge in later tasks
	lastRefill    time.Time //nolint:unused // updated by refill logic in later tasks
}

// NewAllocator validates cfg and returns an empty Allocator.
func NewAllocator(cfg AllocatorConfig) (*Allocator, error) {
	if cfg.MaxKbpsPerSeeder <= 0 {
		return nil, fmt.Errorf("%w: MaxKbpsPerSeeder must be > 0, got %d",
			ErrInvalidConfig, cfg.MaxKbpsPerSeeder)
	}
	if cfg.SessionTTL <= 0 {
		return nil, fmt.Errorf("%w: SessionTTL must be > 0, got %s",
			ErrInvalidConfig, cfg.SessionTTL)
	}
	if cfg.Now == nil {
		return nil, fmt.Errorf("%w: Now must not be nil", ErrInvalidConfig)
	}
	if cfg.Rand == nil {
		return nil, fmt.Errorf("%w: Rand must not be nil", ErrInvalidConfig)
	}
	return &Allocator{
		cfg:     cfg,
		byToken: make(map[Token]*sessionEntry),
		bySID:   make(map[uint64]*sessionEntry),
		byReq:   make(map[[16]byte]*sessionEntry),
		buckets: make(map[ids.IdentityID]*tokenBucket),
	}, nil
}
