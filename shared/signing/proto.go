package signing

import (
	"crypto/ed25519"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// DeterministicMarshal serializes m with proto.MarshalOptions{Deterministic: true}.
// This is the single canonical-bytes choke point for all sign/verify operations
// across the shared/ module — see shared/CLAUDE.md rule 6. Calling proto.Marshal
// directly on a message that gets signed bypasses the determinism guarantee.
func DeterministicMarshal(m proto.Message) ([]byte, error) {
	if m == nil {
		return nil, errors.New("signing: DeterministicMarshal on nil message")
	}
	return proto.MarshalOptions{Deterministic: true}.Marshal(m)
}

// SignEnvelope returns an Ed25519 signature over DeterministicMarshal(body).
// Returns an error on a wrong-length private key or nil body. Pre-condition:
// body has been validated by tbproto.ValidateEnvelopeBody — this helper does
// NOT re-run validation, so callers can choose between fail-fast and
// best-effort signing semantics.
func SignEnvelope(priv ed25519.PrivateKey, body *tbproto.EnvelopeBody) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("signing: private key length %d, want %d", len(priv), ed25519.PrivateKeySize)
	}
	if body == nil {
		return nil, errors.New("signing: SignEnvelope on nil body")
	}
	buf, err := DeterministicMarshal(body)
	if err != nil {
		return nil, fmt.Errorf("signing: marshal envelope body: %w", err)
	}
	return ed25519.Sign(priv, buf), nil
}

// VerifyEnvelope reports whether signed.ConsumerSig is a valid Ed25519 signature
// under pub over DeterministicMarshal(signed.Body). Nil inputs, malformed keys
// or sigs, or marshal failures all return false without panicking.
func VerifyEnvelope(pub ed25519.PublicKey, signed *tbproto.EnvelopeSigned) bool {
	if signed == nil || signed.Body == nil {
		return false
	}
	buf, err := DeterministicMarshal(signed.Body)
	if err != nil {
		return false
	}
	return Verify(pub, buf, signed.ConsumerSig)
}

// SignBalanceSnapshot is the parallel of SignEnvelope for the tracker's
// balance attestation. The tracker calls this; consumers verify via
// VerifyBalanceSnapshot.
func SignBalanceSnapshot(priv ed25519.PrivateKey, body *tbproto.BalanceSnapshotBody) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("signing: private key length %d, want %d", len(priv), ed25519.PrivateKeySize)
	}
	if body == nil {
		return nil, errors.New("signing: SignBalanceSnapshot on nil body")
	}
	buf, err := DeterministicMarshal(body)
	if err != nil {
		return nil, fmt.Errorf("signing: marshal balance snapshot body: %w", err)
	}
	return ed25519.Sign(priv, buf), nil
}

// VerifyBalanceSnapshot — see VerifyEnvelope for nil-safety contract.
func VerifyBalanceSnapshot(pub ed25519.PublicKey, signed *tbproto.SignedBalanceSnapshot) bool {
	if signed == nil || signed.Body == nil {
		return false
	}
	buf, err := DeterministicMarshal(signed.Body)
	if err != nil {
		return false
	}
	return Verify(pub, buf, signed.TrackerSig)
}
