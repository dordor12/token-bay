package storage

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// appendUsageAtTimestamp builds and persists a USAGE entry whose
// body.timestamp = ts and seq = ts (so each test entry is unique). Returns
// the entry's hash for use in leaf assertions.
func appendUsageAtTimestamp(t *testing.T, s *Store, seq uint64, ts uint64) []byte {
	t.Helper()
	in := builtUsageInput(t, seq, make([]byte, 32))
	in.Entry.Body.Timestamp = ts

	// Re-sign body and recompute hash since we mutated timestamp.
	_, cPriv := keyFor("consumer")
	_, sPriv := keyFor("seeder")
	_, tPriv := keyFor("tracker")
	in.Entry.ConsumerSig = mustSign(t, cPriv, in.Entry.Body)
	in.Entry.SeederSig = mustSign(t, sPriv, in.Entry.Body)
	in.Entry.TrackerSig = mustSign(t, tPriv, in.Entry.Body)
	in.Hash = mustHash(t, in.Entry.Body)

	_, err := s.AppendEntry(context.Background(), in)
	require.NoError(t, err)
	return in.Hash[:]
}

func TestLeavesForHour_BoundsAreStartInclusiveEndExclusive(t *testing.T) {
	s := openTempStore(t)
	const hour uint64 = 100
	start := hour * 3600
	end := start + 3600

	// Append three entries: one before the hour, one at the boundary
	// (start, included), one at end-1 (included), one at end (excluded).
	beforeHash := appendUsageAtTimestamp(t, s, 1, start-1)
	startHash := appendUsageAtTimestamp(t, s, 2, start)
	endMinusHash := appendUsageAtTimestamp(t, s, 3, end-1)
	endHash := appendUsageAtTimestamp(t, s, 4, end)
	_ = beforeHash
	_ = endHash

	got, err := s.LeavesForHour(context.Background(), hour)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.True(t, bytes.Equal(got[0], startHash))
	assert.True(t, bytes.Equal(got[1], endMinusHash))
}

func TestLeavesForHour_EmptyHour(t *testing.T) {
	s := openTempStore(t)
	got, err := s.LeavesForHour(context.Background(), 100)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestPutAndGetMerkleRoot_RoundTrip(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	root := bytes.Repeat([]byte{0xAA}, 32)
	sig := bytes.Repeat([]byte{0xBB}, 64)

	require.NoError(t, s.PutMerkleRoot(ctx, 100, root, sig))

	gotRoot, gotSig, ok, err := s.GetMerkleRoot(ctx, 100)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, root, gotRoot)
	assert.Equal(t, sig, gotSig)
}

func TestGetMerkleRoot_Miss(t *testing.T) {
	s := openTempStore(t)
	root, sig, ok, err := s.GetMerkleRoot(context.Background(), 999)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Nil(t, root)
	assert.Nil(t, sig)
}

func TestPutMerkleRoot_DuplicateHourReturnsSentinel(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	root := bytes.Repeat([]byte{0xAA}, 32)
	sig := bytes.Repeat([]byte{0xBB}, 64)

	require.NoError(t, s.PutMerkleRoot(ctx, 100, root, sig))

	// Same row, second time → still a duplicate.
	err := s.PutMerkleRoot(ctx, 100, root, sig)
	assert.ErrorIs(t, err, ErrDuplicateMerkleRoot)

	// Different root, same hour → also duplicate.
	otherRoot := bytes.Repeat([]byte{0xCC}, 32)
	err = s.PutMerkleRoot(ctx, 100, otherRoot, sig)
	assert.ErrorIs(t, err, ErrDuplicateMerkleRoot)

	// Original row unchanged.
	gotRoot, _, _, err := s.GetMerkleRoot(ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, root, gotRoot)
}

func TestPutMerkleRoot_RejectsEmptyInputs(t *testing.T) {
	s := openTempStore(t)
	err := s.PutMerkleRoot(context.Background(), 1, nil, []byte{0x01})
	require.Error(t, err)
	err = s.PutMerkleRoot(context.Background(), 1, []byte{0x01}, nil)
	require.Error(t, err)
}
