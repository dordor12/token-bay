// Package federation: canonical pre-sig byte builder for Revocation. Like
// signing_transfer.go, this routes through shared/signing.DeterministicMarshal
// (CLAUDE.md §6: every signed proto goes through that single determinism
// choke point).
package federation

import (
	"errors"

	"github.com/token-bay/token-bay/shared/signing"
	"google.golang.org/protobuf/proto"
)

// CanonicalRevocationPreSig returns the deterministic byte representation
// of m with tracker_sig cleared. The issuer signs the returned bytes; the
// verifier reconstructs identically.
func CanonicalRevocationPreSig(m *Revocation) ([]byte, error) {
	if m == nil {
		return nil, errors.New("federation: nil Revocation")
	}
	clone, ok := proto.Clone(m).(*Revocation)
	if !ok {
		return nil, errors.New("federation: clone Revocation")
	}
	clone.TrackerSig = nil
	return signing.DeterministicMarshal(clone)
}
