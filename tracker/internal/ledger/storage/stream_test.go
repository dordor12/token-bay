package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chainOfN appends N starter-grant entries with linked prev_hash,
// returning each entry's hash for use in subsequent assertions.
func chainOfN(t *testing.T, s *Store, n int) [][32]byte {
	t.Helper()
	ctx := context.Background()

	hashes := make([][32]byte, 0, n)
	prev := make([]byte, 32)
	for i := 1; i <= n; i++ {
		in := builtStarterGrantInput(t, uint64(i), prev)
		_, err := s.AppendEntry(ctx, in)
		require.NoError(t, err)
		hashes = append(hashes, in.Hash)
		prev = in.Hash[:]
	}
	return hashes
}

func TestEntriesSince_ReturnsEverythingFromZero(t *testing.T) {
	s := openTempStore(t)
	chainOfN(t, s, 5)

	got, err := s.EntriesSince(context.Background(), 0, 0)
	require.NoError(t, err)
	assert.Len(t, got, 5)

	for i, e := range got {
		assert.Equal(t, uint64(i+1), e.Body.Seq, "entries returned in ascending seq order")
	}
}

func TestEntriesSince_FromTipReturnsEmpty(t *testing.T) {
	s := openTempStore(t)
	chainOfN(t, s, 3)

	got, err := s.EntriesSince(context.Background(), 3, 0)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestEntriesSince_RespectsLimit(t *testing.T) {
	s := openTempStore(t)
	chainOfN(t, s, 5)

	got, err := s.EntriesSince(context.Background(), 1, 2)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, uint64(2), got[0].Body.Seq)
	assert.Equal(t, uint64(3), got[1].Body.Seq)
}

func TestEntriesSince_EmptyChain(t *testing.T) {
	s := openTempStore(t)
	got, err := s.EntriesSince(context.Background(), 0, 100)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestEntriesSince_CorruptCanonicalReturnsError(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()
	chainOfN(t, s, 3)

	_, err := s.db.ExecContext(ctx, "UPDATE entries SET canonical = ? WHERE seq = 2", []byte{0xff, 0xfe})
	require.NoError(t, err)

	_, err = s.EntriesSince(ctx, 0, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unmarshal canonical")
}
