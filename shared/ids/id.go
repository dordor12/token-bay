// Package ids defines opaque identifier types used on the Token-Bay wire.
//
// Each ID is a strongly-typed wrapper around a fixed-size byte array. Strong
// typing prevents accidentally passing a tracker ID where an identity ID is
// expected, even though both are 32 bytes.
package ids

// IdentityID is an Ed25519 pubkey hash identifying a plugin instance.
type IdentityID [32]byte

// Bytes returns the underlying byte array.
func (i IdentityID) Bytes() [32]byte { return i }
