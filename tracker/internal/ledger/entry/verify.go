package entry

import (
	"crypto/ed25519"
	"errors"
	"fmt"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
)

// Pubkeys carries the verification keys for an Entry's three potential
// signers. Consumer / Seeder may be empty for kinds that don't include
// the corresponding signature; Tracker is always required.
type Pubkeys struct {
	Consumer ed25519.PublicKey
	Seeder   ed25519.PublicKey
	Tracker  ed25519.PublicKey
}

// VerifyAll runs the per-kind signature matrix:
//
//	USAGE         consumer (or flag set + empty), seeder, tracker
//	TRANSFER_OUT  consumer, no seeder, tracker
//	TRANSFER_IN   no consumer, no seeder, tracker
//	STARTER_GRANT no consumer, no seeder, tracker
//
// Returns nil on success; the first violation otherwise. Callers must
// have run ValidateEntryBody first — VerifyAll does not re-check field
// shape. Stray sigs on kinds that don't accept them are a hard error so
// an attacker cannot piggy-back a confusing signature onto a starter
// grant or transfer.
func VerifyAll(e *tbproto.Entry, keys Pubkeys) error {
	if e == nil || e.Body == nil {
		return errors.New("entry: VerifyAll on nil entry")
	}
	if len(e.TrackerSig) == 0 {
		return errors.New("entry: tracker_sig is required")
	}
	if len(keys.Tracker) != ed25519.PublicKeySize {
		return errors.New("entry: tracker pubkey missing")
	}
	if !signing.VerifyEntry(keys.Tracker, e.Body, e.TrackerSig) {
		return errors.New("entry: tracker_sig invalid")
	}

	flagSigMissing := e.Body.Flags&flagConsumerSigMissing != 0

	switch e.Body.Kind {
	case tbproto.EntryKind_ENTRY_KIND_USAGE:
		if len(e.SeederSig) == 0 {
			return errors.New("entry: usage seeder_sig is required")
		}
		if len(keys.Seeder) != ed25519.PublicKeySize {
			return errors.New("entry: seeder pubkey missing")
		}
		if !signing.VerifyEntry(keys.Seeder, e.Body, e.SeederSig) {
			return errors.New("entry: seeder_sig invalid")
		}
		if flagSigMissing {
			if len(e.ConsumerSig) != 0 {
				return errors.New("entry: usage consumer_sig must be empty when consumer_sig_missing flag set")
			}
		} else {
			if len(e.ConsumerSig) == 0 {
				return errors.New("entry: usage consumer_sig required (or set consumer_sig_missing flag)")
			}
			if len(keys.Consumer) != ed25519.PublicKeySize {
				return errors.New("entry: consumer pubkey missing")
			}
			if !signing.VerifyEntry(keys.Consumer, e.Body, e.ConsumerSig) {
				return errors.New("entry: consumer_sig invalid")
			}
		}
	case tbproto.EntryKind_ENTRY_KIND_TRANSFER_OUT:
		if flagSigMissing {
			return errors.New("entry: transfer_out cannot set consumer_sig_missing")
		}
		if len(e.ConsumerSig) == 0 {
			return errors.New("entry: transfer_out consumer_sig required")
		}
		if len(keys.Consumer) != ed25519.PublicKeySize {
			return errors.New("entry: consumer pubkey missing")
		}
		if !signing.VerifyEntry(keys.Consumer, e.Body, e.ConsumerSig) {
			return errors.New("entry: consumer_sig invalid")
		}
		if len(e.SeederSig) != 0 {
			return errors.New("entry: transfer_out seeder_sig must be empty")
		}
	case tbproto.EntryKind_ENTRY_KIND_TRANSFER_IN, tbproto.EntryKind_ENTRY_KIND_STARTER_GRANT:
		if len(e.ConsumerSig) != 0 {
			return errors.New("entry: consumer_sig must be empty for this kind")
		}
		if len(e.SeederSig) != 0 {
			return errors.New("entry: seeder_sig must be empty for this kind")
		}
	default:
		return fmt.Errorf("entry: unknown kind %v", e.Body.Kind)
	}
	return nil
}
