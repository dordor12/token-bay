package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
)

// EnrollPreimagePrefix is the domain-separation tag prefixed to every
// enroll preimage. Bytes-stable across plugin versions.
const EnrollPreimagePrefix = "token-bay-enroll:v1"

// enrollPreimageLen = len(prefix) + 16 (nonce) + 32 (fingerprint) + 32 (pubkey)
const enrollPreimageLen = len(EnrollPreimagePrefix) + 16 + 32 + 32

// EnrollPreimage assembles the canonical bytes signed by the consumer
// per plugin spec §4.2:
//
//	"token-bay-enroll:v1" || nonce[16] || account_fingerprint[32] || identity_pubkey[32]
//
// Total length: 99 bytes. Returns ErrInvalidKey if pub is not 32 bytes.
func EnrollPreimage(nonce [16]byte, fingerprint [32]byte, pub ed25519.PublicKey) ([]byte, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: pub length %d, want %d", ErrInvalidKey, len(pub), ed25519.PublicKeySize)
	}
	out := make([]byte, 0, enrollPreimageLen)
	out = append(out, EnrollPreimagePrefix...)
	out = append(out, nonce[:]...)
	out = append(out, fingerprint[:]...)
	out = append(out, pub...)
	return out, nil
}

// EnrollPayload is the self-contained payload the caller copies into
// trackerclient.EnrollRequest. Sig is the 64-byte Ed25519 signature
// over EnrollPreimage.
type EnrollPayload struct {
	IdentityPubkey     ed25519.PublicKey
	Role               uint32
	AccountFingerprint [32]byte
	Nonce              [16]byte
	Sig                []byte
}

// BuildEnrollPayload generates a fresh 16-byte nonce via crypto/rand,
// builds the preimage, and signs it. Returns the payload alongside the
// chosen nonce so callers can log/audit the enroll attempt.
func BuildEnrollPayload(s *Signer, fingerprint [32]byte, role uint32) (*EnrollPayload, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("identity: nonce: %w", err)
	}
	pre, err := EnrollPreimage(nonce, fingerprint, s.PublicKey())
	if err != nil {
		return nil, err
	}
	sig, err := s.Sign(pre)
	if err != nil {
		return nil, fmt.Errorf("identity: sign preimage: %w", err)
	}
	return &EnrollPayload{
		IdentityPubkey:     s.PublicKey(),
		Role:               role,
		AccountFingerprint: fingerprint,
		Nonce:              nonce,
		Sig:                sig,
	}, nil
}
