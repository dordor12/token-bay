// Package signing provides Ed25519 verify helpers used throughout Token-Bay.
// Canonical serialization primitives will be added here as feature plans progress.
//
// Using stdlib crypto/ed25519 directly would work, but this wrapper exists
// so call sites can't accidentally skip the length guards that prevent a
// panic on malformed keys or signatures.
package signing

import "crypto/ed25519"

// Verify returns true iff sig is a valid Ed25519 signature over msg
// under pub. Returns false on any length mismatch or signature error.
func Verify(pub ed25519.PublicKey, msg, sig []byte) bool {
	if len(pub) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(pub, msg, sig)
}
