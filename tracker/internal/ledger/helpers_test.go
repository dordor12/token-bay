package ledger

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
)

// openTempLedger returns a fresh Ledger backed by a tempdir-scoped Store
// and a deterministic tracker keypair. Closes the Store via t.Cleanup.
func openTempLedger(t *testing.T, opts ...Option) *Ledger {
	t.Helper()
	store := openTempStoreForLedger(t)
	_, priv := trackerKeypair()
	l, err := Open(store, priv, opts...)
	require.NoError(t, err)
	return l
}

// openTempStoreForLedger returns a fresh Store backed by a tempdir-scoped
// SQLite file. Closes via t.Cleanup.
func openTempStoreForLedger(t *testing.T) *storage.Store {
	t.Helper()
	s, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "ledger.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// trackerKeypair returns a deterministic Ed25519 keypair so tracker_sig
// bytes are reproducible across test runs.
func trackerKeypair() (ed25519.PublicKey, ed25519.PrivateKey) {
	seed := sha256.Sum256([]byte("ledger-test-tracker-key"))
	priv := ed25519.NewKeyFromSeed(seed[:])
	return priv.Public().(ed25519.PublicKey), priv
}

// labeledKeypair returns a deterministic Ed25519 keypair from a label,
// used for test consumer/seeder identities.
func labeledKeypair(label string) (ed25519.PublicKey, ed25519.PrivateKey) {
	seed := sha256.Sum256([]byte("ledger-test-key-" + label))
	priv := ed25519.NewKeyFromSeed(seed[:])
	return priv.Public().(ed25519.PublicKey), priv
}

// fixedClock returns a clock fixed at t. Tests use this so timestamps in
// entries and snapshots are reproducible.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}
