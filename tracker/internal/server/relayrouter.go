package server

import (
	"net/netip"
	"sync"
	"time"

	"github.com/token-bay/token-bay/tracker/internal/stunturn"
)

// relayRouter maps a relay session token to the two peer UDP addresses learned
// from traffic. It is the data-plane state the pure stunturn.Allocator
// deliberately does not hold. Safe for concurrent use by the single relay read
// loop plus the reaper.
type relayRouter struct {
	mu   sync.Mutex
	bind map[stunturn.Token]*peerBinding
}

type peerBinding struct {
	a, b     netip.AddrPort
	lastSeen time.Time
}

func newRelayRouter() *relayRouter {
	return &relayRouter{bind: make(map[stunturn.Token]*peerBinding)}
}

// peerFor returns the datagram's forwarding destination for (tok, src). It
// learns src as side A (first datagram for tok) or side B (second distinct
// src), returning ok=false until both sides are known, and drops a third
// distinct src (a token binds exactly two peers).
func (r *relayRouter) peerFor(tok stunturn.Token, src netip.AddrPort, now time.Time) (netip.AddrPort, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pb, exists := r.bind[tok]
	if !exists {
		r.bind[tok] = &peerBinding{a: src, lastSeen: now}
		return netip.AddrPort{}, false
	}
	pb.lastSeen = now
	switch src {
	case pb.a:
		if pb.b.IsValid() {
			return pb.b, true
		}
		return netip.AddrPort{}, false
	case pb.b:
		return pb.a, true
	default:
		if !pb.b.IsValid() {
			pb.b = src
			return pb.a, true
		}
		return netip.AddrPort{}, false // third peer — dropped
	}
}

// reap removes bindings whose lastSeen predates before. Returns the count removed.
func (r *relayRouter) reap(before time.Time) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for tok, pb := range r.bind {
		if pb.lastSeen.Before(before) {
			delete(r.bind, tok)
			n++
		}
	}
	return n
}

func (r *relayRouter) activeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bind)
}
