package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"fmt"

	"github.com/token-bay/token-bay/shared/ids"
)

// Signer satisfies trackerclient.Signer. Holds an Ed25519 keypair and
// caches the IdentityID = SHA-256(public key bytes).
type Signer struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
	id   ids.IdentityID
}

// New wraps an existing keypair. Returns ErrInvalidKey on length mismatch.
func New(priv ed25519.PrivateKey) (*Signer, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%w: priv length %d, want %d", ErrInvalidKey, len(priv), ed25519.PrivateKeySize)
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok || len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: derived pub length", ErrInvalidKey)
	}
	return &Signer{priv: priv, pub: pub, id: ids.IdentityID(sha256.Sum256(pub))}, nil
}

// Generate produces a fresh keypair via crypto/rand.
func Generate() (*Signer, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("identity: generate key: %w", err)
	}
	return &Signer{priv: priv, pub: pub, id: ids.IdentityID(sha256.Sum256(pub))}, nil
}

func (s *Signer) Sign(msg []byte) ([]byte, error) {
	return ed25519.Sign(s.priv, msg), nil
}

func (s *Signer) PrivateKey() ed25519.PrivateKey { return s.priv }
func (s *Signer) PublicKey() ed25519.PublicKey   { return s.pub }
func (s *Signer) IdentityID() ids.IdentityID     { return s.id }
