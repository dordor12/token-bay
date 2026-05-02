// Package admission holds the wire-format types for the tracker admission
// subsystem: portable credit attestations and tracker-initiated headroom
// fetches. Types parallel shared/proto: a Body message plus a Signed wrapper
// for any ed25519-signed payload, with the signature taken over
// DeterministicMarshal(body). Validation helpers in this package run pre-sign
// and post-parse before any field is trusted.
//
// Spec: docs/superpowers/specs/admission/2026-04-25-admission-design.md.
package admission
