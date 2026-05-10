// Package proto: canonical-bytes + sign/verify helpers for plugin-tracker
// signed messages. The canonical bytes for any signed message are the
// DeterministicMarshal of the message with the Sig field cleared. The
// signing call sets Sig to ed25519.Sign(priv, canonical); the verifying
// call recomputes canonical and ed25519.Verify's the stored Sig.
//
// This is the same pattern federation uses for transfer proofs and
// revocation messages (see tracker/internal/federation/transfer.go and
// shared/federation/canonical.go).
package proto

import (
	"crypto/ed25519"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"
)

// ErrBootstrapPeerListBadSig is returned when VerifyBootstrapPeerListSig
// rejects a signature. Callers can errors.Is this sentinel to surface a
// specific "tampered or wrong-key" outcome.
var ErrBootstrapPeerListBadSig = errors.New("proto: BootstrapPeerList signature invalid")

// CanonicalBootstrapPeerListPreSig returns the bytes signed by the issuer.
// It is DeterministicMarshal of the list with Sig cleared, computed on a
// proto.Clone so the caller's value is unchanged.
func CanonicalBootstrapPeerListPreSig(list *BootstrapPeerList) ([]byte, error) {
	if list == nil {
		return nil, errors.New("proto: nil BootstrapPeerList")
	}
	clone, ok := proto.Clone(list).(*BootstrapPeerList)
	if !ok {
		return nil, errors.New("proto: BootstrapPeerList clone type mismatch")
	}
	clone.Sig = nil
	return proto.MarshalOptions{Deterministic: true}.Marshal(clone)
}

// SignBootstrapPeerList computes the canonical bytes and writes the
// resulting Ed25519 signature into list.Sig. priv must be a 64-byte
// Ed25519 private key.
func SignBootstrapPeerList(priv ed25519.PrivateKey, list *BootstrapPeerList) error {
	if len(priv) != ed25519.PrivateKeySize {
		return fmt.Errorf("proto: SignBootstrapPeerList: priv len %d != %d", len(priv), ed25519.PrivateKeySize)
	}
	cb, err := CanonicalBootstrapPeerListPreSig(list)
	if err != nil {
		return err
	}
	list.Sig = ed25519.Sign(priv, cb)
	return nil
}

// VerifyBootstrapPeerListSig recomputes the canonical bytes and verifies
// list.Sig against pub. Returns ErrBootstrapPeerListBadSig on signature
// mismatch; other errors indicate malformed inputs.
func VerifyBootstrapPeerListSig(pub ed25519.PublicKey, list *BootstrapPeerList) error {
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("proto: VerifyBootstrapPeerListSig: pub len %d != %d", len(pub), ed25519.PublicKeySize)
	}
	if list == nil {
		return errors.New("proto: nil BootstrapPeerList")
	}
	if len(list.Sig) != ed25519.SignatureSize {
		return fmt.Errorf("proto: BootstrapPeerList.sig len %d != %d", len(list.Sig), ed25519.SignatureSize)
	}
	cb, err := CanonicalBootstrapPeerListPreSig(list)
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub, cb, list.Sig) {
		return ErrBootstrapPeerListBadSig
	}
	return nil
}
