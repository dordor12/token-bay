package storage

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBalanceWithTip_EmptyChainAndMissingBalance(t *testing.T) {
	s := openTempStore(t)

	snap, err := s.BalanceWithTip(context.Background(), bytes.Repeat([]byte{0x11}, 32))
	require.NoError(t, err)
	assert.False(t, snap.BalanceFound)
	assert.False(t, snap.TipFound)
	assert.Zero(t, snap.TipSeq)
	assert.Nil(t, snap.TipHash)
}

func TestBalanceWithTip_PopulatedChainAndBalance(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	in := builtUsageInput(t, 1, make([]byte, 32))
	_, err := s.AppendEntry(ctx, in)
	require.NoError(t, err)

	snap, err := s.BalanceWithTip(ctx, in.Balances[0].IdentityID)
	require.NoError(t, err)

	require.True(t, snap.BalanceFound)
	assert.Equal(t, in.Balances[0].IdentityID, snap.Balance.IdentityID)
	assert.Equal(t, in.Balances[0].Credits, snap.Balance.Credits)
	assert.Equal(t, uint64(1), snap.Balance.LastSeq)

	require.True(t, snap.TipFound)
	assert.Equal(t, uint64(1), snap.TipSeq)
	assert.Equal(t, in.Hash[:], snap.TipHash)
}

func TestBalanceWithTip_TipFoundButBalanceMissing(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	in := builtUsageInput(t, 1, make([]byte, 32))
	_, err := s.AppendEntry(ctx, in)
	require.NoError(t, err)

	// Query an identity not touched by the entry.
	other := bytes.Repeat([]byte{0xFF}, 32)
	snap, err := s.BalanceWithTip(ctx, other)
	require.NoError(t, err)

	assert.False(t, snap.BalanceFound)
	assert.True(t, snap.TipFound)
	assert.Equal(t, uint64(1), snap.TipSeq)
	assert.Equal(t, in.Hash[:], snap.TipHash)
}

func TestBalanceWithTip_RejectsClosedStore(t *testing.T) {
	s := openTempStore(t)
	require.NoError(t, s.Close())

	_, err := s.BalanceWithTip(context.Background(), bytes.Repeat([]byte{0x11}, 32))
	require.Error(t, err)
}
