package ledger

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTip_EmptyChain(t *testing.T) {
	l := openTempLedger(t)
	seq, hash, ok, err := l.Tip(context.Background())
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Zero(t, seq)
	assert.Nil(t, hash)
}

func TestEntryBySeq_Miss(t *testing.T) {
	l := openTempLedger(t)
	got, ok, err := l.EntryBySeq(context.Background(), 999)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Nil(t, got)
}

func TestEntryByHash_Miss(t *testing.T) {
	l := openTempLedger(t)
	got, ok, err := l.EntryByHash(context.Background(), make([]byte, 32))
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Nil(t, got)
}

func TestEntriesSince_EmptyChain(t *testing.T) {
	l := openTempLedger(t)
	got, err := l.EntriesSince(context.Background(), 0, 100)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestMerkleRoot_NoRootYet(t *testing.T) {
	l := openTempLedger(t)
	root, sig, ok, err := l.MerkleRoot(context.Background(), 1)
	require.NoError(t, err)
	assert.False(t, ok, "expected ok=false before any root is stored")
	assert.Nil(t, root)
	assert.Nil(t, sig)
}
