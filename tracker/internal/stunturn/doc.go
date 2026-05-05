// Package stunturn provides STUN binding-response reflection and TURN
// session-state allocation for the tracker.
//
// This package is pure logic. It does not own any UDP socket; the caller
// (internal/server) opens the listeners on STUNListenAddr (:3478) and
// TURNListenAddr (:3479) and dispatches each datagram into the functions
// here.
//
// STUN side: DecodeBindingRequest validates an inbound RFC 5389 binding
// request and returns its 12-byte transaction ID; EncodeBindingResponse
// produces the wire-ready binding-response bytes carrying one
// XOR-MAPPED-ADDRESS attribute. Both wrap github.com/pion/stun/v2 — wire
// format compliance is delegated to that library. Reflect is a one-line
// helper that combines them for the listener's hot path.
//
// TURN side: Allocator manages session state — 16-byte opaque tokens,
// per-seeder kbps token-bucket rate limiting (driven by
// STUNTURNConfig.TURNRelayMaxKbps), idle-session expiry driven by
// STUNTURNConfig.SessionTTLSeconds, and dedupe-by-RequestID. The hot path
// is ResolveAndCharge, called once per inbound TURN datagram by the
// listener. Sweep is the periodic GC that callers run from a single
// goroutine on a timer.
//
// Concurrency model: every public method on *Allocator holds a single
// sync.Mutex. Critical sections are O(1) map lookups and small
// arithmetic; expected scale is ≤ 10^2 concurrent sessions per tracker
// per spec §11. STUN binding requests do not touch the allocator —
// Reflect is stateless — so STUN traffic does not contend on this lock.
//
// Future scaling: if the allocator ever shows up under contention, shard
// by IdentityID (the seeder), embed the shard index in the high byte of
// Token, and walk shards in Sweep. See design spec §7.2.
//
// Time and randomness are caller-injected: AllocatorConfig.Now and Rand.
// The package never calls time.Now() and never imports crypto/rand
// directly. Tests pass a fixed clock and a deterministic byte stream.
//
// Returned Session values are deep copies. Callers cannot mutate the
// allocator's internal state through them.
//
// The package intentionally does not know about: the broker selection
// algorithm, reputation freezes, federation, the registry's external
// address bookkeeping, or persistence. The tracker's own session and
// server modules wire stunturn into those subsystems.
//
// Spec: docs/superpowers/specs/tracker/2026-05-02-tracker-stunturn-design.md.
package stunturn
