package storage

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIntegration_RestartPreservesEverything exercises the durability
// promise from ledger spec §6: append, close, reopen — entries, balances,
// and tip all survive. It does not simulate a mid-transaction crash
// (Go can't do that portably), but it does verify the close/reopen path.
func TestIntegration_RestartPreservesEverything(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	ctx := context.Background()

	// Phase 1: open, append 10 entries, close.
	s1, err := Open(ctx, path)
	require.NoError(t, err)

	const N = 10
	hashes := make([][32]byte, N)
	prev := make([]byte, 32)
	for i := 1; i <= N; i++ {
		in := builtUsageInput(t, uint64(i), prev)
		_, err := s1.AppendEntry(ctx, in)
		require.NoError(t, err)
		hashes[i-1] = in.Hash
		prev = in.Hash[:]
	}

	tipSeq1, tipHash1, ok, err := s1.Tip(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, uint64(N), tipSeq1)
	assert.Equal(t, hashes[N-1][:], tipHash1)

	require.NoError(t, s1.Close())

	// Phase 2: reopen the same file.
	s2, err := Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })

	// Tip preserved.
	tipSeq2, tipHash2, ok, err := s2.Tip(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, tipSeq1, tipSeq2)
	assert.Equal(t, tipHash1, tipHash2)

	// All entries readable.
	got, err := s2.EntriesSince(ctx, 0, 0)
	require.NoError(t, err)
	require.Len(t, got, N)
	for i, e := range got {
		assert.Equal(t, uint64(i+1), e.Body.Seq)
	}

	// Balances preserved. Last upsert in builtUsageInput sets credits
	// to ±N*1000 with last_seq=N.
	usageEntry := builtUsageInput(t, 1, make([]byte, 32))
	consumerID := usageEntry.Balances[0].IdentityID
	seederID := usageEntry.Balances[1].IdentityID

	bConsumer, ok, err := s2.Balance(ctx, consumerID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(-N*1000), bConsumer.Credits)
	assert.Equal(t, uint64(N), bConsumer.LastSeq)

	bSeeder, ok, err := s2.Balance(ctx, seederID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(N*1000), bSeeder.Credits)
	assert.Equal(t, uint64(N), bSeeder.LastSeq)

	// Phase 3: continue the chain post-restart.
	in := builtUsageInput(t, N+1, hashes[N-1][:])
	seq, err := s2.AppendEntry(ctx, in)
	require.NoError(t, err)
	assert.Equal(t, uint64(N+1), seq)

	tipSeq3, _, _, err := s2.Tip(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint64(N+1), tipSeq3)
}

// TestIntegration_MerkleRootsAndPeerRootsSurviveRestart locks down that
// the Merkle and peer-root tables are fully durable across an open/close
// cycle.
func TestIntegration_MerkleRootsAndPeerRootsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	ctx := context.Background()

	s1, err := Open(ctx, path)
	require.NoError(t, err)

	root := []byte{0xAA, 0xBB, 0xCC}
	sig := []byte{0xDD, 0xEE, 0xFF}
	require.NoError(t, s1.PutMerkleRoot(ctx, 100, root, sig))

	peer := samplePeerRoot(0x01, 100)
	require.NoError(t, s1.PutPeerRoot(ctx, peer))

	require.NoError(t, s1.Close())

	s2, err := Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })

	gotRoot, gotSig, ok, err := s2.GetMerkleRoot(ctx, 100)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, root, gotRoot)
	assert.Equal(t, sig, gotSig)

	gotPeer, ok, err := s2.GetPeerRoot(ctx, peer.TrackerID, peer.Hour)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, peer.Root, gotPeer.Root)
}
