package stunturn

import "encoding/hex"

// Token is the 16-byte opaque handle a TURN session is identified by.
// It is generated from AllocatorConfig.Rand at Allocate time and used
// as a map key inside the allocator. The value-array shape (rather than
// a slice) makes Token comparable and usable as a map key.
type Token [16]byte

// String returns the lowercase hex encoding of the token. Never
// special-cases the zero value.
func (t Token) String() string {
	return hex.EncodeToString(t[:])
}
