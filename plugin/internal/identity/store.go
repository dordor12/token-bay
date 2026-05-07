package identity

import (
	"errors"
	"fmt"
	"path/filepath"
)

const (
	keyFilename    = "identity.key"
	recordFilename = "identity.json"
)

// Open loads (signer, record) from a directory:
//
//	<dir>/identity.key   read by LoadKey
//	<dir>/identity.json  read by LoadRecord
//
// Returns the typed errors LoadKey / LoadRecord surface (ErrKeyNotFound,
// ErrInvalidKey, ErrRecordNotFound, ErrInvalidRecord) so callers can
// branch into the enrollment flow.
func Open(dir string) (*Signer, *Record, error) {
	s, err := LoadKey(filepath.Join(dir, keyFilename))
	if err != nil {
		return nil, nil, err
	}
	r, err := LoadRecord(filepath.Join(dir, recordFilename))
	if err != nil {
		return nil, nil, err
	}
	return s, r, nil
}

// VerifyBinding compares the cached record's fingerprint with the live
// AuthSnapshot. Returns nil on match, ErrFingerprintMismatch on
// divergence, ErrIneligibleAuth if the snapshot itself is ineligible
// (caller should treat that as a re-enrollment trigger as well).
func VerifyBinding(r *Record, s AuthSnapshot) error {
	if r == nil {
		return errors.New("identity: VerifyBinding nil record")
	}
	fp, err := AccountFingerprint(s)
	if err != nil {
		return err
	}
	if fp != r.AccountFingerprint {
		return fmt.Errorf("%w: cached vs live differ", ErrFingerprintMismatch)
	}
	return nil
}
