package identity

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
)

// LoadKey reads path, parses the 32-byte Ed25519 seed, and returns a
// *Signer. Returns ErrKeyNotFound when path doesn't exist, ErrInvalidKey
// on size mismatch.
func LoadKey(path string) (*Signer, error) {
	seed, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrKeyNotFound, path)
		}
		return nil, fmt.Errorf("identity: read %s: %w", path, err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%w: %s: size %d, want %d", ErrInvalidKey, path, len(seed), ed25519.SeedSize)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return New(priv)
}

// SaveKey atomically writes the 32-byte seed to path, mode 0600.
// Refuses to clobber an existing file (returns ErrKeyExists).
func SaveKey(path string, s *Signer) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%w: %s", ErrKeyExists, path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("identity: stat %s: %w", path, err)
	}
	seed := s.PrivateKey().Seed()
	tmp := path + ".token-bay-tmp-" + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, seed, 0o600); err != nil {
		return fmt.Errorf("identity: write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("identity: rename: %w", err)
	}
	return nil
}
