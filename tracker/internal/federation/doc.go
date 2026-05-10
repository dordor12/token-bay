// Package federation is the tracker subsystem that participates in the
// peer-tracker gossip graph. This first slice ships peering, gossip core,
// and the ROOT_ATTESTATION + EQUIVOCATION_EVIDENCE flow against the
// existing peer_root_archive storage.
//
// Construct via Open(cfg, deps); the returned *Federation owns one
// goroutine per peer connection plus a single publisher goroutine.
//
// Spec: docs/superpowers/specs/federation/2026-05-09-tracker-internal-federation-core-design.md.
//
// Concurrency: this package is on the always-`-race` list per tracker
// spec §6 / admission-design §11.3. A `-race` failure here is always a
// real bug.
package federation
