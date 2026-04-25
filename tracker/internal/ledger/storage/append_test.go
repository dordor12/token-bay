package storage

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAppendEntry_HappyPath_Genesis(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	in := builtStarterGrantInput(t, 1, make([]byte, 32))
	seq, err := s.AppendEntry(ctx, in)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), seq)

	// Tip reflects the new entry.
	tipSeq, tipHash, ok, err := s.Tip(ctx)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, uint64(1), tipSeq)
	assert.Equal(t, in.Hash[:], tipHash)
}

func TestAppendEntry_HappyPath_UsageWithTwoBalances(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	in := builtUsageInput(t, 1, make([]byte, 32))
	_, err := s.AppendEntry(ctx, in)
	require.NoError(t, err)

	// Both consumer + seeder balance rows exist.
	var n int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM balances").Scan(&n))
	assert.Equal(t, 2, n)
}

func TestAppendEntry_NoBalanceUpdates(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	in := builtStarterGrantInput(t, 1, make([]byte, 32))
	in.Balances = nil // strip balances; entry-only append

	_, err := s.AppendEntry(ctx, in)
	require.NoError(t, err)

	var n int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM balances").Scan(&n))
	assert.Equal(t, 0, n, "no balance updates means no balance rows written")
}

func TestAppendEntry_DuplicateHash(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	in := builtStarterGrantInput(t, 1, make([]byte, 32))
	_, err := s.AppendEntry(ctx, in)
	require.NoError(t, err)

	// Re-attempt the same entry — orchestrator's idempotent retry path.
	_, err = s.AppendEntry(ctx, in)
	assert.ErrorIs(t, err, ErrAlreadyExists)

	var n int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM entries").Scan(&n))
	assert.Equal(t, 1, n, "duplicate hash must not write a second entry row")
}

func TestAppendEntry_DifferentBodySameSeq(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	first := builtStarterGrantInput(t, 1, make([]byte, 32))
	_, err := s.AppendEntry(ctx, first)
	require.NoError(t, err)

	// Synthesize a *different* entry at the same seq — an orchestrator bug
	// (storage doesn't compute seq itself). Different body → different hash,
	// but seq collides on the PRIMARY KEY. Surfaces as ErrAlreadyExists.
	second := builtUsageInput(t, 1, make([]byte, 32))
	_, err = s.AppendEntry(ctx, second)
	assert.ErrorIs(t, err, ErrAlreadyExists)
}

func TestAppendEntry_RejectsNilEntry(t *testing.T) {
	s := openTempStore(t)
	_, err := s.AppendEntry(context.Background(), AppendInput{})
	require.Error(t, err)
}

func TestAppendEntry_RejectsMissingTrackerSig(t *testing.T) {
	s := openTempStore(t)
	in := builtStarterGrantInput(t, 1, make([]byte, 32))
	in.Entry.TrackerSig = nil

	_, err := s.AppendEntry(context.Background(), in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tracker_sig")
}

func TestAppendEntry_RejectsBadBalanceLength(t *testing.T) {
	s := openTempStore(t)
	in := builtStarterGrantInput(t, 1, make([]byte, 32))
	in.Balances[0].IdentityID = []byte{1, 2, 3} // wrong length

	_, err := s.AppendEntry(context.Background(), in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "identity_id length")
}

func TestAppendEntry_ConcurrentWritersSerialized(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	const N = 10

	// Pre-build N distinct entries with seqs 1..N. Storage doesn't compute
	// seq itself; the orchestrator (or, here, the test) does. The point of
	// this test is that writeMu produces a total ordering — concurrent
	// AppendEntry calls don't corrupt the chain via interleaved writes.
	appendIn := make([]AppendInput, N)
	for i := range N {
		appendIn[i] = builtStarterGrantInput(t, uint64(i+1), make([]byte, 32))
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		gotSeqs []uint64
		errs    []error
	)
	for i := range N {
		wg.Add(1)
		go func() {
			defer wg.Done()
			seq, err := s.AppendEntry(ctx, appendIn[i])
			mu.Lock()
			if err != nil {
				errs = append(errs, err)
			} else {
				gotSeqs = append(gotSeqs, seq)
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	require.Empty(t, errs, "no append should error: %v", errs)
	assert.Len(t, gotSeqs, N)

	var rows int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM entries").Scan(&rows))
	assert.Equal(t, N, rows)
}

func TestAppendEntry_RollsBackOnConflict(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	// First append: seq=1, two balance rows.
	first := builtUsageInput(t, 1, make([]byte, 32))
	_, err := s.AppendEntry(ctx, first)
	require.NoError(t, err)

	// Second append: same hash → ErrAlreadyExists. Verify balances unchanged.
	var creditsBefore int64
	require.NoError(t, s.db.QueryRow(
		"SELECT credits FROM balances WHERE identity_id = ?", first.Balances[0].IdentityID,
	).Scan(&creditsBefore))

	conflict := first
	// Tweak the balance update to a value we'd notice if it leaked through.
	conflict.Balances = []BalanceUpdate{{
		IdentityID: first.Balances[0].IdentityID, Credits: 999999, LastSeq: 99, UpdatedAt: 999,
	}}
	_, err = s.AppendEntry(ctx, conflict)
	assert.True(t, errors.Is(err, ErrAlreadyExists))

	var creditsAfter int64
	require.NoError(t, s.db.QueryRow(
		"SELECT credits FROM balances WHERE identity_id = ?", first.Balances[0].IdentityID,
	).Scan(&creditsAfter))
	assert.Equal(t, creditsBefore, creditsAfter, "rollback must restore balance row to pre-attempt state")
}

func TestAppendEntry_BalanceUpserts(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	// First entry sets initial credits.
	first := builtUsageInput(t, 1, make([]byte, 32))
	_, err := s.AppendEntry(ctx, first)
	require.NoError(t, err)

	// Second entry with same identities updates the rows.
	tip, _, _, err := s.Tip(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), tip)

	prevHash := first.Hash[:]
	second := builtUsageInput(t, 2, prevHash)
	_, err = s.AppendEntry(ctx, second)
	require.NoError(t, err)

	// Still 2 balance rows (upsert, not insert).
	var n int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM balances").Scan(&n))
	assert.Equal(t, 2, n)

	// Credits reflect the second update (-2000 for consumer, +2000 for seeder).
	var consumerCredits int64
	require.NoError(t, s.db.QueryRow(
		"SELECT credits FROM balances WHERE identity_id = ?", second.Balances[0].IdentityID,
	).Scan(&consumerCredits))
	assert.Equal(t, int64(-2000), consumerCredits)
}
