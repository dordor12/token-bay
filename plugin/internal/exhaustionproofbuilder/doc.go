// Package exhaustionproofbuilder assembles a *exhaustionproof.ExhaustionProofV1
// from a typed ProofInput. The caller (ccproxy-network) adapts its own state
// (cached fallback ticket + /usage probe timestamp) into ProofInput.
//
// The builder validates the assembled proof via shared/exhaustionproof.ValidateProofV1
// before returning. Time and randomness are injected as function-typed fields
// to keep tests deterministic; production uses time.Now and crypto/rand.Read.
//
// This package imports only shared/exhaustionproof + Go stdlib. It is a leaf
// of the plugin-internal dependency graph.
package exhaustionproofbuilder
