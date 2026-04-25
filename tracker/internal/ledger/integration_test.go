package ledger

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
)

// TestIntegration_ConcurrentStarterGrants confirms 100 concurrent grants
// produce a strictly increasing seq sequence with no gaps, no duplicates,
// and chain integrity holds.
func TestIntegration_ConcurrentStarterGrants(t *testing.T) {
	l := openTempLedger(t)
	ctx := context.Background()
	identity := bytes.Repeat([]byte{0x11}, 32)

	const N = 100
	var wg sync.WaitGroup
	results := make(chan uint64, N)
	errs := make(chan error, N)

	for range N {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, err := l.IssueStarterGrant(ctx, identity, 100)
			if err != nil {
				errs <- err
				return
			}
			results <- e.Body.Seq
		}()
	}
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	seen := make(map[uint64]bool, N)
	for seq := range results {
		assert.False(t, seen[seq], "duplicate seq %d", seq)
		seen[seq] = true
	}
	for i := uint64(1); i <= N; i++ {
		assert.True(t, seen[i], "missing seq %d", i)
	}

	require.NoError(t, l.AssertChainIntegrity(ctx, 0, 0))
}

// TestIntegration_MixedAppendsPassAudit interleaves grants, usage, and
// transfer-out entries and confirms the resulting chain is intact.
func TestIntegration_MixedAppendsPassAudit(t *testing.T) {
	l := openTempLedger(t)
	ctx := context.Background()

	consumerID := bytes.Repeat([]byte{0x11}, 32)
	seederID := bytes.Repeat([]byte{0x22}, 32)
	cPub, cPriv := labeledKeypair("consumer")
	sPub, sPriv := labeledKeypair("seeder")

	// 5 starter grants pre-fund consumer + seeder.
	for range 5 {
		_, err := l.IssueStarterGrant(ctx, consumerID, 1000)
		require.NoError(t, err)
	}

	// 5 usage entries (consumer pays seeder).
	for range 5 {
		rec, _ := signedUsageRecord(t, l, consumerID, seederID, cPub, cPriv, sPub, sPriv, 100)
		_, err := l.AppendUsage(ctx, rec)
		require.NoError(t, err)
	}

	// 3 transfer-outs from consumer.
	for range 3 {
		rec, _ := signedTransferOutRecord(t, l, consumerID, 100)
		_, err := l.AppendTransferOut(ctx, rec)
		require.NoError(t, err)
	}

	require.NoError(t, l.AssertChainIntegrity(ctx, 0, 0))

	tipSeq, _, _, err := l.Tip(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint64(13), tipSeq)
}

// TestIntegration_RestartPreservesChainIntegrity opens a Ledger, appends
// entries, closes both Ledger and Store, reopens the Store from the same
// path, opens a new Ledger over it, and asserts chain integrity.
func TestIntegration_RestartPreservesChainIntegrity(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	ctx := context.Background()
	_, priv := trackerKeypair()

	store1, err := storage.Open(ctx, dbPath)
	require.NoError(t, err)
	l1, err := Open(store1, priv)
	require.NoError(t, err)

	identity := bytes.Repeat([]byte{0x11}, 32)
	for range 5 {
		_, err := l1.IssueStarterGrant(ctx, identity, 100)
		require.NoError(t, err)
	}

	require.NoError(t, l1.Close())
	require.NoError(t, store1.Close())

	// Reopen.
	store2, err := storage.Open(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store2.Close() })
	l2, err := Open(store2, priv)
	require.NoError(t, err)

	tipSeq, _, _, err := l2.Tip(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint64(5), tipSeq)

	require.NoError(t, l2.AssertChainIntegrity(ctx, 0, 0))

	// Continue chain post-restart.
	_, err = l2.IssueStarterGrant(ctx, identity, 100)
	require.NoError(t, err)
	require.NoError(t, l2.AssertChainIntegrity(ctx, 0, 0))
}

// TestAssertChainIntegrity_DetectsTamperedPrevHash simulates disk
// corruption by closing the ledger's store, mutating a row through a
// raw SQL connection, then reopening — and confirms the audit catches
// the chain break. Storage intentionally doesn't expose the underlying
// *sql.DB to its callers, so we route around it instead of through.
func TestAssertChainIntegrity_DetectsTamperedPrevHash(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	ctx := context.Background()
	_, priv := trackerKeypair()
	identity := bytes.Repeat([]byte{0x11}, 32)

	// Phase 1: append 3 entries through a real Ledger.
	store1, err := storage.Open(ctx, dbPath)
	require.NoError(t, err)
	l1, err := Open(store1, priv)
	require.NoError(t, err)
	for range 3 {
		_, err := l1.IssueStarterGrant(ctx, identity, 100)
		require.NoError(t, err)
	}
	require.NoError(t, l1.Close())
	require.NoError(t, store1.Close())

	// Phase 2: corrupt entry seq=2's canonical blob via raw connection.
	// Modifying the indexed prev_hash column wouldn't suffice — storage
	// returns parsed bodies from the canonical blob, treating canonical
	// as the source of truth. So we replace the canonical with a fresh
	// proto encoding that has prev_hash=FFFF... instead.
	rawDB, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=journal_mode(WAL)")
	require.NoError(t, err)

	// Read canonical for seq=2, parse, mutate prev_hash, re-encode.
	var origCanonical []byte
	require.NoError(t, rawDB.QueryRowContext(ctx, "SELECT canonical FROM entries WHERE seq = 2").Scan(&origCanonical))

	body := &tbproto.EntryBody{}
	require.NoError(t, proto.Unmarshal(origCanonical, body))
	body.PrevHash = bytes.Repeat([]byte{0xFF}, 32)
	tamperedCanonical, err := proto.MarshalOptions{Deterministic: true}.Marshal(body)
	require.NoError(t, err)

	_, err = rawDB.ExecContext(ctx,
		"UPDATE entries SET canonical = ? WHERE seq = 2", tamperedCanonical)
	require.NoError(t, err)
	require.NoError(t, rawDB.Close())

	// Phase 3: reopen and audit.
	store2, err := storage.Open(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store2.Close() })
	l2, err := Open(store2, priv)
	require.NoError(t, err)

	err = l2.AssertChainIntegrity(ctx, 0, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chain break at seq=2")
}

func TestAssertChainIntegrity_EmptyChain(t *testing.T) {
	l := openTempLedger(t)
	require.NoError(t, l.AssertChainIntegrity(context.Background(), 0, 0))
}

func TestAssertChainIntegrity_MidChainAnchor(t *testing.T) {
	l := openTempLedger(t)
	ctx := context.Background()
	identity := bytes.Repeat([]byte{0x11}, 32)

	for range 5 {
		_, err := l.IssueStarterGrant(ctx, identity, 100)
		require.NoError(t, err)
	}

	// Audit (3, 5] — uses entry 3 as the anchor.
	require.NoError(t, l.AssertChainIntegrity(ctx, 3, 5))
}

func TestAssertChainIntegrity_MissingAnchorIsError(t *testing.T) {
	l := openTempLedger(t)
	err := l.AssertChainIntegrity(context.Background(), 99, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "anchor seq=99 missing")
}
