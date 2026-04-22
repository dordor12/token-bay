// Package exhaustionproof holds the types for the rate-limit exhaustion proof
// attached to broker_request envelopes.
//
// v1 is the two-signal bundle (StopFailure payload + /usage probe).
// v2 is the Claude-Code-issued signed attestation (pending upstream primitive).
//
// See docs/superpowers/specs/exhaustion-proof/ for the authoritative spec.
package exhaustionproof

// Version constants for the exhaustion proof format.
const (
	VersionV1 uint8 = 1
	VersionV2 uint8 = 2
)
