package identity

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
)

const recordCurrentVersion = 1

// Record is the cached identity binding persisted to disk after enrollment.
type Record struct {
	Version            int
	IdentityID         ids.IdentityID
	AccountFingerprint [32]byte
	Role               uint32
	EnrolledAt         time.Time // UTC
}

type recordWire struct {
	Version            int    `json:"version"`
	IdentityID         string `json:"identity_id"`
	AccountFingerprint string `json:"account_fingerprint"`
	Role               uint32 `json:"role"`
	EnrolledAt         string `json:"enrolled_at"`
}

// LoadRecord reads path. Returns ErrRecordNotFound when path doesn't
// exist, ErrInvalidRecord on schema or hex-decode failure.
func LoadRecord(path string) (*Record, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrRecordNotFound, path)
		}
		return nil, fmt.Errorf("identity: read %s: %w", path, err)
	}
	var w recordWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRecord, err)
	}
	if w.Version != recordCurrentVersion {
		return nil, fmt.Errorf("%w: version %d, want %d", ErrInvalidRecord, w.Version, recordCurrentVersion)
	}
	idHash, err := decodeHex32(w.IdentityID)
	if err != nil {
		return nil, fmt.Errorf("%w: identity_id: %v", ErrInvalidRecord, err)
	}
	fp, err := decodeHex32(w.AccountFingerprint)
	if err != nil {
		return nil, fmt.Errorf("%w: account_fingerprint: %v", ErrInvalidRecord, err)
	}
	enrolledAt, err := time.Parse(time.RFC3339Nano, w.EnrolledAt)
	if err != nil {
		return nil, fmt.Errorf("%w: enrolled_at: %v", ErrInvalidRecord, err)
	}
	return &Record{
		Version:            w.Version,
		IdentityID:         ids.IdentityID(idHash),
		AccountFingerprint: fp,
		Role:               w.Role,
		EnrolledAt:         enrolledAt.UTC(),
	}, nil
}

// SaveRecord atomically writes path, mode 0600. Overwrite-allowed.
func SaveRecord(path string, r *Record) error {
	if r == nil {
		return errors.New("identity: SaveRecord nil record")
	}
	version := r.Version
	if version == 0 {
		version = recordCurrentVersion
	}
	w := recordWire{
		Version:            version,
		IdentityID:         hex.EncodeToString(r.IdentityID[:]),
		AccountFingerprint: hex.EncodeToString(r.AccountFingerprint[:]),
		Role:               r.Role,
		EnrolledAt:         r.EnrolledAt.UTC().Format(time.RFC3339Nano),
	}
	blob, err := json.Marshal(w)
	if err != nil {
		return fmt.Errorf("identity: marshal record: %w", err)
	}
	tmp := path + ".token-bay-tmp-" + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, blob, 0o600); err != nil {
		return fmt.Errorf("identity: write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("identity: rename: %w", err)
	}
	return nil
}

func decodeHex32(s string) ([32]byte, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return [32]byte{}, err
	}
	if len(b) != 32 {
		return [32]byte{}, fmt.Errorf("expected 32 bytes, got %d", len(b))
	}
	var out [32]byte
	copy(out[:], b)
	return out, nil
}
