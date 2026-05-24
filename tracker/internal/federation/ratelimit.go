package federation

import (
	"sync"
	"time"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
)

// RateLimitConfig caps inbound gossip per (peer, kind). A zero rate
// disables the limiter for that kind (allow-all). burst is the bucket
// capacity; rate is tokens-per-second refill.
//
// Slice 9 (federation §9 "gossip storm control"). Keeps the steady-
// state hot path lock-free apart from a per-(peer, kind) atomic update
// inside the token bucket.
type RateLimitConfig struct {
	RootAttestation      RateBucket
	Revocation           RateBucket
	PeerExchange         RateBucket
	EquivocationEvidence RateBucket
	Transfer             RateBucket // proof request + proof + applied combined
}

// RateBucket configures one bucket. Rate=0 disables (allow-all). Burst
// must be ≥ 1 when Rate > 0.
type RateBucket struct {
	Rate  float64 // tokens per second
	Burst int     // bucket capacity
}

// gossipLimiter is the per-peer rate-limit set keyed by kind. Lookup is
// lock-free fast path (sync.Map); per-bucket state uses a small mutex.
type gossipLimiter struct {
	cfg RateLimitConfig
	now func() time.Time

	buckets sync.Map // key: limiterKey, value: *tokenBucket
}

type limiterKey struct {
	peer ids.TrackerID
	kind fed.Kind
}

type tokenBucket struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
	rate   float64
	cap    float64
}

// newGossipLimiter constructs a limiter. now must not be nil.
func newGossipLimiter(cfg RateLimitConfig, now func() time.Time) *gossipLimiter {
	return &gossipLimiter{cfg: cfg, now: now}
}

// Allow returns true if a frame of the given kind from peer is within
// budget. False means caller should drop + metric. Unknown kinds and
// kinds with Rate=0 always allow.
func (g *gossipLimiter) Allow(peer ids.TrackerID, kind fed.Kind) bool {
	cfg, ok := g.bucketCfg(kind)
	if !ok || cfg.Rate <= 0 {
		return true
	}
	k := limiterKey{peer: peer, kind: kind}
	v, _ := g.buckets.LoadOrStore(k, &tokenBucket{
		tokens: float64(cfg.Burst),
		last:   g.now(),
		rate:   cfg.Rate,
		cap:    float64(cfg.Burst),
	})
	tb := v.(*tokenBucket)
	tb.mu.Lock()
	defer tb.mu.Unlock()
	now := g.now()
	elapsed := now.Sub(tb.last).Seconds()
	tb.last = now
	tb.tokens += elapsed * tb.rate
	if tb.tokens > tb.cap {
		tb.tokens = tb.cap
	}
	if tb.tokens < 1.0 {
		return false
	}
	tb.tokens -= 1.0
	return true
}

func (g *gossipLimiter) bucketCfg(kind fed.Kind) (RateBucket, bool) {
	switch kind {
	case fed.Kind_KIND_ROOT_ATTESTATION:
		return g.cfg.RootAttestation, true
	case fed.Kind_KIND_REVOCATION:
		return g.cfg.Revocation, true
	case fed.Kind_KIND_PEER_EXCHANGE:
		return g.cfg.PeerExchange, true
	case fed.Kind_KIND_EQUIVOCATION_EVIDENCE:
		return g.cfg.EquivocationEvidence, true
	case fed.Kind_KIND_TRANSFER_PROOF_REQUEST,
		fed.Kind_KIND_TRANSFER_PROOF,
		fed.Kind_KIND_TRANSFER_APPLIED:
		return g.cfg.Transfer, true
	}
	return RateBucket{}, false
}
