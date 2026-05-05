package signing

import (
	"crypto/ed25519"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	tbadmission "github.com/token-bay/token-bay/shared/admission"
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

// SignEntry returns an Ed25519 signature over DeterministicMarshal(body).
// All three ledger-entry signers (consumer, seeder, tracker) use this same
// helper since they all sign the identical preimage — see ledger spec §3.1
// and tracker spec §5.2 step 4.
//
// Pre-condition: body has been validated by tbproto.ValidateEntryBody.
// Sign helpers do NOT re-run validation, mirroring SignEnvelope semantics.
func SignEntry(priv ed25519.PrivateKey, body *tbproto.EntryBody) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("signing: private key length %d, want %d", len(priv), ed25519.PrivateKeySize)
	}
	if body == nil {
		return nil, errors.New("signing: SignEntry on nil body")
	}
	buf, err := DeterministicMarshal(body)
	if err != nil {
		return nil, fmt.Errorf("signing: marshal entry body: %w", err)
	}
	return ed25519.Sign(priv, buf), nil
}

// VerifyEntry reports whether sig is a valid Ed25519 signature under pub
// over DeterministicMarshal(body). Nil body, malformed key/sig, or marshal
// failure all return false without panicking — same nil-safety contract as
// VerifyEnvelope and VerifyBalanceSnapshot.
func VerifyEntry(pub ed25519.PublicKey, body *tbproto.EntryBody, sig []byte) bool {
	if body == nil {
		return false
	}
	buf, err := DeterministicMarshal(body)
	if err != nil {
		return false
	}
	return Verify(pub, buf, sig)
}

// SignCreditAttestation returns an Ed25519 signature over
// DeterministicMarshal(body). Mirrors SignEnvelope semantics: returns an
// error on a wrong-length private key or nil body. Pre-condition: body
// has been validated by tbadmission.ValidateCreditAttestationBody — this
// helper does NOT re-run validation.
func SignCreditAttestation(priv ed25519.PrivateKey, body *tbadmission.CreditAttestationBody) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("signing: private key length %d, want %d", len(priv), ed25519.PrivateKeySize)
	}
	if body == nil {
		return nil, errors.New("signing: SignCreditAttestation on nil body")
	}
	buf, err := DeterministicMarshal(body)
	if err != nil {
		return nil, fmt.Errorf("signing: marshal credit attestation body: %w", err)
	}
	return ed25519.Sign(priv, buf), nil
}

// VerifyCreditAttestation reports whether signed.TrackerSig is a valid
// Ed25519 signature under pub over DeterministicMarshal(signed.Body).
// Nil inputs, malformed key/sig, or marshal failure all return false
// without panicking — same nil-safety contract as VerifyEnvelope.
func VerifyCreditAttestation(pub ed25519.PublicKey, signed *tbadmission.SignedCreditAttestation) bool {
	if signed == nil || signed.Body == nil {
		return false
	}
	buf, err := DeterministicMarshal(signed.Body)
	if err != nil {
		return false
	}
	return Verify(pub, buf, signed.TrackerSig)
}
