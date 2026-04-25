package entry

import (
	"crypto/sha256"
	"errors"
	"fmt"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
)

// Hash returns SHA-256 over the canonical bytes of body. This value is
// what the next entry's prev_hash must equal (ledger spec §3.2 chain).
//
// The hash covers EntryBody only — never the wire-form Entry. Rationale
// in the package doc: chain links survive tracker key rotation because
// the bodies don't change, only tracker_sig does.
func Hash(body *tbproto.EntryBody) ([32]byte, error) {
	if body == nil {
		return [32]byte{}, errors.New("entry: Hash on nil body")
	}
	buf, err := signing.DeterministicMarshal(body)
	if err != nil {
		return [32]byte{}, fmt.Errorf("entry: marshal body: %w", err)
	}
	return sha256.Sum256(buf), nil
}
