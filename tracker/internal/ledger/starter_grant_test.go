package ledger

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tbproto "github.com/token-bay/token-bay/shared/proto"
)

func TestIssueStarterGrant_HappyPath(t *testing.T) {
	l := openTempLedger(t, WithClock(fixedClock(time.Unix(1714000000, 0))))
	consumerID := bytes.Repeat([]byte{0x11}, 32)

	e, err := l.IssueStarterGrant(context.Background(), consumerID, 100000)
	require.NoError(t, err)
	require.NotNil(t, e)

	assert.Equal(t, tbproto.EntryKind_ENTRY_KIND_STARTER_GRANT, e.Body.Kind)
	assert.Equal(t, uint64(1), e.Body.Seq)
	assert.Equal(t, make([]byte, 32), e.Body.PrevHash)
	assert.Equal(t, uint64(100000), e.Body.CostCredits)
	assert.Equal(t, consumerID, e.Body.ConsumerId)
	assert.Equal(t, uint64(1714000000), e.Body.Timestamp)
	assert.NotEmpty(t, e.TrackerSig)
	assert.Empty(t, e.ConsumerSig)
	assert.Empty(t, e.SeederSig)
}

func TestIssueStarterGrant_RejectsZeroAmount(t *testing.T) {
	l := openTempLedger(t)
	_, err := l.IssueStarterGrant(context.Background(), bytes.Repeat([]byte{0x11}, 32), 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "amount must be > 0")
}

func TestIssueStarterGrant_RejectsBadIdentityLength(t *testing.T) {
	l := openTempLedger(t)
	_, err := l.IssueStarterGrant(context.Background(), []byte{1, 2, 3}, 100)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "identity_id length")
}

func TestIssueStarterGrant_ChainsCorrectly(t *testing.T) {
	l := openTempLedger(t)
	ctx := context.Background()
	id1 := bytes.Repeat([]byte{0x11}, 32)
	id2 := bytes.Repeat([]byte{0x22}, 32)

	e1, err := l.IssueStarterGrant(ctx, id1, 1000)
	require.NoError(t, err)

	e2, err := l.IssueStarterGrant(ctx, id2, 2000)
	require.NoError(t, err)

	assert.Equal(t, uint64(1), e1.Body.Seq)
	assert.Equal(t, uint64(2), e2.Body.Seq)
	// e2.prev_hash must equal hash(e1.body).
	hash1, err := mustEntryHash(e1.Body)
	require.NoError(t, err)
	assert.Equal(t, hash1[:], e2.Body.PrevHash)
}

func TestIssueStarterGrant_BalanceAccumulates(t *testing.T) {
	l := openTempLedger(t)
	ctx := context.Background()
	consumerID := bytes.Repeat([]byte{0x11}, 32)

	for range 3 {
		_, err := l.IssueStarterGrant(ctx, consumerID, 1000)
		require.NoError(t, err)
	}

	bal, ok, err := l.store.Balance(ctx, consumerID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(3000), bal.Credits)
	assert.Equal(t, uint64(3), bal.LastSeq)
}

func TestIssueStarterGrant_ConcurrentCallsProduceCoherentChain(t *testing.T) {
	// 50 concurrent grants → seq sequence 1..50 with no gaps, balances sum
	// correctly. ledger.mu serializes appends; this verifies chain integrity
	// holds under contention.
	l := openTempLedger(t)
	ctx := context.Background()
	consumerID := bytes.Repeat([]byte{0x11}, 32)

	const N = 50
	type result struct {
		seq uint64
		err error
	}
	results := make(chan result, N)
	for range N {
		go func() {
			e, err := l.IssueStarterGrant(ctx, consumerID, 100)
			if err != nil {
				results <- result{err: err}
				return
			}
			results <- result{seq: e.Body.Seq}
		}()
	}

	seqs := make(map[uint64]bool, N)
	for range N {
		r := <-results
		require.NoError(t, r.err)
		assert.False(t, seqs[r.seq], "duplicate seq %d", r.seq)
		seqs[r.seq] = true
	}
	for i := uint64(1); i <= N; i++ {
		assert.True(t, seqs[i], "missing seq %d in %v", i, seqs)
	}

	bal, ok, err := l.store.Balance(ctx, consumerID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(N*100), bal.Credits)
}
