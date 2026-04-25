package storage

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
	"github.com/token-bay/token-bay/tracker/internal/ledger/entry"
)

// openTempDB returns a SQLite handle backed by a fresh tempdir-scoped file
// with v1 migrations applied. Closes automatically via t.Cleanup.
//
// Used by tests that need raw SQL access without the Store wrapper.
// Higher-level tests use openTempStore.
func openTempDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "ledger.db") +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, applyMigrations(context.Background(), db))
	return db
}

// openTempStore returns a fresh Store backed by a tempdir-scoped DB file.
// Closes automatically via t.Cleanup.
func openTempStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.db")
	s, err := Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// keyFor returns a deterministic Ed25519 keypair for a label. Used across
// storage tests so signed fixtures are reproducible.
func keyFor(label string) (ed25519.PublicKey, ed25519.PrivateKey) {
	seed := sha256.Sum256([]byte("storage-test-seed-" + label))
	priv := ed25519.NewKeyFromSeed(seed[:])
	return priv.Public().(ed25519.PublicKey), priv
}

// builtUsageInput constructs an AppendInput for a signed USAGE entry at
// (seq, prevHash) with synthetic 1000-credit-cost balance updates for two
// distinct identities. The orchestrator equivalents will live in
// internal/ledger; this is enough for storage to exercise its writes.
func builtUsageInput(t *testing.T, seq uint64, prevHash []byte) AppendInput {
	t.Helper()

	consumerID := bytes.Repeat([]byte{0x11}, 32)
	seederID := bytes.Repeat([]byte{0x22}, 32)
	requestID := make([]byte, 16)
	// Spread requestIDs across appends so the fixture is unique per seq.
	for i := range requestID {
		requestID[i] = byte(seq)
	}

	body, err := entry.BuildUsageEntry(entry.UsageInput{
		PrevHash:     prevHash,
		Seq:          seq,
		ConsumerID:   consumerID,
		SeederID:     seederID,
		Model:        "claude-sonnet-4-6",
		InputTokens:  100,
		OutputTokens: 50,
		CostCredits:  1000,
		Timestamp:    1714000000 + seq,
		RequestID:    requestID,
	})
	require.NoError(t, err)

	_, cPriv := keyFor("consumer")
	_, sPriv := keyFor("seeder")
	_, tPriv := keyFor("tracker")
	cSig, _ := signing.SignEntry(cPriv, body)
	sSig, _ := signing.SignEntry(sPriv, body)
	tSig, _ := signing.SignEntry(tPriv, body)

	hash, err := entry.Hash(body)
	require.NoError(t, err)

	return AppendInput{
		Entry: &tbproto.Entry{
			Body:        body,
			ConsumerSig: cSig,
			SeederSig:   sSig,
			TrackerSig:  tSig,
		},
		Hash: hash,
		Balances: []BalanceUpdate{
			{IdentityID: consumerID, Credits: -int64(seq) * 1000, LastSeq: seq, UpdatedAt: 1714000000 + seq},
			{IdentityID: seederID, Credits: int64(seq) * 1000, LastSeq: seq, UpdatedAt: 1714000000 + seq},
		},
	}
}

// builtStarterGrantInput constructs a starter-grant entry — useful for
// AppendEntry tests that want a single balance update.
func builtStarterGrantInput(t *testing.T, seq uint64, prevHash []byte) AppendInput {
	t.Helper()
	consumerID := bytes.Repeat([]byte{0x33}, 32)

	body, err := entry.BuildStarterGrantEntry(entry.StarterGrantInput{
		PrevHash:   prevHash,
		Seq:        seq,
		ConsumerID: consumerID,
		Amount:     100000,
		Timestamp:  1714000000,
	})
	require.NoError(t, err)

	_, tPriv := keyFor("tracker")
	tSig, _ := signing.SignEntry(tPriv, body)

	hash, err := entry.Hash(body)
	require.NoError(t, err)

	return AppendInput{
		Entry: &tbproto.Entry{Body: body, TrackerSig: tSig},
		Hash:  hash,
		Balances: []BalanceUpdate{
			{IdentityID: consumerID, Credits: 100000, LastSeq: seq, UpdatedAt: 1714000000},
		},
	}
}
