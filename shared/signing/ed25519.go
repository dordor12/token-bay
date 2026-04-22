// Package signing provides Ed25519 sign and verify helpers plus
// canonical serialization primitives used throughout Token-Bay.
//
// Using stdlib crypto/ed25519 directly would work, but these wrappers
// exist so call sites can't accidentally use an unchecked signature
// or a non-canonical byte preimage.
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
