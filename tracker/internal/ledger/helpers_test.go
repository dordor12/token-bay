package ledger

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/ledger/entry"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
)

// openTempLedger returns a fresh Ledger backed by a tempdir-scoped Store
// and a deterministic tracker keypair. Closes the Store via t.Cleanup.
func openTempLedger(t *testing.T, opts ...Option) *Ledger {
	t.Helper()
	l, _ := openTempLedgerWithPath(t, opts...)
	return l
}

// openTempLedgerWithPath is like openTempLedger but also returns the
// SQLite file path — used by integration tests that need raw DB access
// to simulate corruption or restart.
func openTempLedgerWithPath(t *testing.T, opts ...Option) (*Ledger, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.db")
	s, err := storage.Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	_, priv := trackerKeypair()
	l, err := Open(s, priv, opts...)
	require.NoError(t, err)
	return l, path
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

// mustEntryHash returns entry.Hash(body) as a slice or fails the test.
func mustEntryHash(body *tbproto.EntryBody) ([32]byte, error) {
	return entry.Hash(body)
}

// nextTipForTest returns (prev_hash, next_seq) for the next entry on l's
// chain. Used by AppendUsage / AppendTransferOut tests that need to
// pre-build a body matching the current tip — the real broker does the
// same calculation.
func nextTipForTest(t *testing.T, l *Ledger) ([]byte, uint64) {
	t.Helper()
	tipSeq, tipHash, hasTip, err := l.Tip(context.Background())
	require.NoError(t, err)
	if !hasTip {
		return make([]byte, 32), 1
	}
	return tipHash, tipSeq + 1
}
