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

	mu      sync.Mutex
	nextID  uint64
	byToken map[Token]*sessionEntry
	bySID   map[uint64]*sessionEntry
	byReq   map[[16]byte]*sessionEntry
	buckets map[ids.IdentityID]*tokenBucket
}

// sessionEntry is the internal heap record. The same pointer is shared
// across byToken / bySID / byReq.
type sessionEntry struct {
	session Session
}

// tokenBucket models per-seeder kbps rate limiting. capacityBytes is one
// second of burst at MaxKbpsPerSeeder; refillPerSec equals capacityBytes
// (steady-state matches the cap).
type tokenBucket struct {
	capacityBytes float64
	refillPerSec  float64
	available     float64
	lastRefill    time.Time
}

// Allocate creates a new TURN session for the given consumer, seeder,
// and requestID. Returns ErrDuplicateRequest if a live session already
// exists for requestID; ErrRandFailed if the injected Rand fails.
//
// The returned Session is a value copy; mutating it does not affect
// allocator state.
func (a *Allocator) Allocate(consumer, seeder ids.IdentityID, requestID [16]byte, now time.Time) (Session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, dup := a.byReq[requestID]; dup {
		return Session{}, ErrDuplicateRequest
	}
	var tokBuf [16]byte
	if _, err := io.ReadFull(a.cfg.Rand, tokBuf[:]); err != nil {
		return Session{}, fmt.Errorf("%w: %v", ErrRandFailed, err)
	}
	tok := Token(tokBuf)
	if _, collide := a.byToken[tok]; collide {
		// 2^-128 with crypto/rand; treat as a hard error rather than
		// retry-loop. Tests inject deterministic streams, so a real
		// collision would be a test bug we want to surface.
		return Session{}, fmt.Errorf("%w: token collision", ErrRandFailed)
	}

	a.nextID++
	entry := &sessionEntry{session: Session{
		Token:       tok,
		SessionID:   a.nextID,
		ConsumerID:  consumer,
		SeederID:    seeder,
		RequestID:   requestID,
		AllocatedAt: now,
		LastActive:  now,
	}}
	a.byToken[tok] = entry
	a.bySID[entry.session.SessionID] = entry
	a.byReq[requestID] = entry
	a.ensureBucket(seeder, now)
	return entry.session, nil
}

// ensureBucket initializes the per-seeder token bucket if absent.
// Must be called with a.mu held.
func (a *Allocator) ensureBucket(seederID ids.IdentityID, now time.Time) *tokenBucket {
	if b, ok := a.buckets[seederID]; ok {
		return b
	}
	cap := float64(a.cfg.MaxKbpsPerSeeder) * 1024.0 / 8.0
	b := &tokenBucket{
		capacityBytes: cap,
		refillPerSec:  cap, // 1s of burst, refilled at the cap rate
		available:     cap, // start full
		lastRefill:    now,
	}
	a.buckets[seederID] = b
	return b
}

// deleteIndexes removes entry from byToken / bySID / byReq. The
// caller must hold a.mu. Buckets are not touched.
func (a *Allocator) deleteIndexes(entry *sessionEntry) {
	delete(a.byToken, entry.session.Token)
	delete(a.bySID, entry.session.SessionID)
	delete(a.byReq, entry.session.RequestID)
}

// Resolve looks up a session by token without updating LastActive and
// without expiring an idle entry. Returns (Session{}, false) when the
// token is unknown. The returned Session is a value copy.
//
// Resolve is intended for diagnostics / admin paths. The hot
// per-datagram path is ResolveAndCharge.
func (a *Allocator) Resolve(tok Token, _ time.Time) (Session, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.byToken[tok]
	if !ok {
		return Session{}, false
	}
	return entry.session, true
}

// ResolveAndCharge atomically (under one mutex acquisition) resolves
// tok, refreshes the seeder's bucket, debits n bytes if n > 0,
// updates LastActive, and returns the session.
//
// Errors:
//
//	ErrUnknownToken   — tok not in allocator (or expired and deleted)
//	ErrSessionExpired — LastActive older than SessionTTL; entry deleted
//	                    on this call (subsequent calls return
//	                    ErrUnknownToken)
//	ErrThrottled      — bucket has < n bytes available; bucket NOT
//	                    debited
//
// n <= 0 skips the bucket check and debit but DOES update LastActive
// (the listener observed a packet — the session is alive even if the
// payload is a zero-byte keepalive).
func (a *Allocator) ResolveAndCharge(tok Token, n int, now time.Time) (Session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	entry, ok := a.byToken[tok]
	if !ok {
		return Session{}, ErrUnknownToken
	}
	if now.Sub(entry.session.LastActive) > a.cfg.SessionTTL {
		a.deleteIndexes(entry)
		return Session{}, ErrSessionExpired
	}
	if n > 0 {
		b := a.buckets[entry.session.SeederID]
		if b == nil {
			// Allocate created the bucket; this is an internal
			// invariant. Surface it loudly.
			panic("stunturn: bucket missing for known seeder; allocator invariants violated")
		}
		refill(b, now)
		if b.available < float64(n) {
			return Session{}, ErrThrottled
		}
		b.available -= float64(n)
	}
	entry.session.LastActive = now
	return entry.session, nil
}

// refill brings b.available up to date based on elapsed time since
// b.lastRefill. Caller must hold the allocator's mutex. Capped at
// b.capacityBytes; clock-backwards is a no-op.
func refill(b *tokenBucket, now time.Time) {
	elapsed := now.Sub(b.lastRefill).Seconds()
	if elapsed <= 0 {
		return
	}
	b.available += elapsed * b.refillPerSec
	if b.available > b.capacityBytes {
		b.available = b.capacityBytes
	}
	b.lastRefill = now
}

// Charge debits n bytes from the seeder's bucket without touching
// session state. Use this only when the listener has already resolved
// the session and just needs to bill more bytes; otherwise use
// ResolveAndCharge.
//
// n <= 0 returns nil without modifying the bucket. The seeder's bucket
// is lazy-initialized on first use, so Charge succeeds for seeders that
// never had Allocate called.
func (a *Allocator) Charge(seederID ids.IdentityID, n int, now time.Time) error {
	if n <= 0 {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	b := a.ensureBucket(seederID, now)
	refill(b, now)
	if b.available < float64(n) {
		return ErrThrottled
	}
	b.available -= float64(n)
	return nil
}

// Release deletes the session by SessionID. Idempotent — a no-op for
// unknown SessionIDs. The seeder's bucket is left in place (it is
// shared across that seeder's sessions and self-decays via refill
// arithmetic).
func (a *Allocator) Release(sessionID uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.bySID[sessionID]
	if !ok {
		return
	}
	a.deleteIndexes(entry)
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
