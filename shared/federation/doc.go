// Package federation holds the wire-format types tracker peers exchange
// in the federation gossip protocol: handshake messages
// (Hello/PeerAuth/PeeringAccept/PeeringReject), steady-state messages
// (RootAttestation, EquivocationEvidence, Ping/Pong), and the outer
// Envelope that signs over each payload.
//
// Validators run pre-marshal at the sender and post-unmarshal at the
// receiver before any field is trusted. See validate.go.
//
// Spec: docs/superpowers/specs/federation/2026-05-09-tracker-internal-federation-core-design.md.
package federation
