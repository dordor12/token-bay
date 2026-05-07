// Package identity owns the plugin's long-lived identity primitives:
// the Ed25519 keypair on disk, the SHA-256(orgId) account fingerprint
// derived from `claude auth status --json`, the token-bay-enroll:v1
// signed preimage, and the cached identity-binding record.
//
// The package is the production source of trackerclient.Signer and the
// single chokepoint for the account-fingerprint derivation. It is pure
// state — no goroutines, no exec calls. Production wiring fills the
// AuthSnapshot struct from internal/ccproxy.ClaudeAuthProber.
//
// See docs/superpowers/specs/plugin/2026-05-07-plugin-identity-design.md.
package identity
