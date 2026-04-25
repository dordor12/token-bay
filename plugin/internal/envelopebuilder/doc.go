// Package envelopebuilder assembles a *proto.EnvelopeSigned from a typed
// RequestSpec, a *exhaustionproof.ExhaustionProofV1, and a tracker-issued
// *proto.SignedBalanceSnapshot. The Signer interface owns Ed25519; the
// production implementation lives in plugin/internal/identity (separate
// plan), tests use a fake.
//
// The builder validates the assembled body via shared/proto.ValidateEnvelopeBody
// before signing. Time and randomness are injected as function-typed fields
// for deterministic tests.
//
// This package imports only shared/... + Go stdlib. It is a leaf of the
// plugin-internal dependency graph.
package envelopebuilder
