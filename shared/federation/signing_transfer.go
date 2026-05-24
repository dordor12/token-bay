// Package federation: canonical pre-sig byte builders for the three
// transfer messages. Each helper clears the relevant signature field so
// the signer and verifier reconstruct the same byte string.
//
// All three route through shared/signing.DeterministicMarshal per
// shared/CLAUDE.md §6: every signed proto goes through that single
// determinism choke point.
package federation

import (
	"crypto/ed25519"
	"errors"
	"fmt"

	"github.com/token-bay/token-bay/shared/signing"
	"google.golang.org/protobuf/proto"
)

// CanonicalTransferProofRequestPreSig returns the deterministic byte
// representation of m with consumer_sig cleared. The consumer signs the
// returned bytes; the verifier reconstructs identically.
func CanonicalTransferProofRequestPreSig(m *TransferProofRequest) ([]byte, error) {
	if m == nil {
		return nil, errors.New("federation: nil TransferProofRequest")
	}
	clone, ok := proto.Clone(m).(*TransferProofRequest)
	if !ok {
		return nil, errors.New("federation: clone TransferProofRequest")
	}
	clone.ConsumerSig = nil
	return signing.DeterministicMarshal(clone)
}

// CanonicalTransferProofPreSig returns the deterministic byte
// representation of m with source_tracker_sig cleared. The source
// tracker signs the bytes; the destination tracker reconstructs
// identically to verify.
func CanonicalTransferProofPreSig(m *TransferProof) ([]byte, error) {
	if m == nil {
		return nil, errors.New("federation: nil TransferProof")
	}
	clone, ok := proto.Clone(m).(*TransferProof)
	if !ok {
		return nil, errors.New("federation: clone TransferProof")
	}
	clone.SourceTrackerSig = nil
	return signing.DeterministicMarshal(clone)
}

// CanonicalTransferAppliedPreSig returns the deterministic byte
// representation of m with dest_tracker_sig cleared.
func CanonicalTransferAppliedPreSig(m *TransferApplied) ([]byte, error) {
	if m == nil {
		return nil, errors.New("federation: nil TransferApplied")
	}
	clone, ok := proto.Clone(m).(*TransferApplied)
	if !ok {
		return nil, errors.New("federation: clone TransferApplied")
	}
	clone.DestTrackerSig = nil
	return signing.DeterministicMarshal(clone)
}

// CanonicalTransferRejectPreSig returns the deterministic byte
// representation of m with source_tracker_sig cleared. Slice 13.
func CanonicalTransferRejectPreSig(m *TransferReject) ([]byte, error) {
	if m == nil {
		return nil, errors.New("federation: nil TransferReject")
	}
	clone, ok := proto.Clone(m).(*TransferReject)
	if !ok {
		return nil, errors.New("federation: clone TransferReject")
	}
	clone.SourceTrackerSig = nil
	return signing.DeterministicMarshal(clone)
}

// CanonicalTransferReversalPreSig returns the deterministic byte
// representation of m with dest_tracker_sig cleared. Slice 14.
func CanonicalTransferReversalPreSig(m *TransferReversal) ([]byte, error) {
	if m == nil {
		return nil, errors.New("federation: nil TransferReversal")
	}
	clone, ok := proto.Clone(m).(*TransferReversal)
	if !ok {
		return nil, errors.New("federation: clone TransferReversal")
	}
	clone.DestTrackerSig = nil
	return signing.DeterministicMarshal(clone)
}

// SignTransferProofRequest returns the consumer's Ed25519 signature over
// CanonicalTransferProofRequestPreSig(m). It mirrors signing.SignEnvelope
// semantics: nil message and wrong-length priv key both return errors;
// the helper does NOT re-run ValidateTransferProofRequest, so callers
// choose between fail-fast and best-effort signing.
//
// The signature this returns is what api handlers stuff into
// TransferProofRequest.ConsumerSig and what source-side OnRequest
// verifies via ed25519.Verify(ConsumerPub, canonical, ConsumerSig).
func SignTransferProofRequest(priv ed25519.PrivateKey, m *TransferProofRequest) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("federation: private key length %d, want %d", len(priv), ed25519.PrivateKeySize)
	}
	canonical, err := CanonicalTransferProofRequestPreSig(m)
	if err != nil {
		return nil, fmt.Errorf("federation: canonical TransferProofRequest: %w", err)
	}
	return ed25519.Sign(priv, canonical), nil
}
