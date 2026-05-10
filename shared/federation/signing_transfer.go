// Package federation: canonical pre-sig byte builders for the three
// transfer messages. Each helper clears the relevant signature field so
// the signer and verifier reconstruct the same byte string.
//
// All three route through shared/signing.DeterministicMarshal per
// shared/CLAUDE.md §6: every signed proto goes through that single
// determinism choke point.
package federation

import (
	"errors"

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
