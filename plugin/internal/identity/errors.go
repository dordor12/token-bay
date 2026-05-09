package identity

import "errors"

var (
	ErrKeyNotFound         = errors.New("identity: key file not found")
	ErrKeyExists           = errors.New("identity: key file already exists")
	ErrInvalidKey          = errors.New("identity: invalid key file")
	ErrRecordNotFound      = errors.New("identity: record file not found")
	ErrInvalidRecord       = errors.New("identity: invalid record file")
	ErrIneligibleAuth      = errors.New("identity: claude auth state not eligible")
	ErrFingerprintMismatch = errors.New("identity: account fingerprint changed since enrollment")
)
