package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestEntryBySeq_HappyPath(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	in := builtUsageInput(t, 1, make([]byte, 32))
	_, err := s.AppendEntry(ctx, in)
	require.NoError(t, err)

	got, ok, err := s.EntryBySeq(ctx, 1)
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, got)

	assert.True(t, proto.Equal(in.Entry.Body, got.Body), "body must round-trip exactly")
	assert.Equal(t, in.Entry.ConsumerSig, got.ConsumerSig)
	assert.Equal(t, in.Entry.SeederSig, got.SeederSig)
	assert.Equal(t, in.Entry.TrackerSig, got.TrackerSig)
}

func TestEntryBySeq_Miss(t *testing.T) {
	s := openTempStore(t)
	got, ok, err := s.EntryBySeq(context.Background(), 999)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Nil(t, got)
}

func TestEntryByHash_HappyPath(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	in := builtUsageInput(t, 1, make([]byte, 32))
	_, err := s.AppendEntry(ctx, in)
	require.NoError(t, err)

	got, ok, err := s.EntryByHash(ctx, in.Hash[:])
	require.NoError(t, err)
	require.True(t, ok)
	assert.True(t, proto.Equal(in.Entry.Body, got.Body))
}

func TestEntryByHash_Miss(t *testing.T) {
	s := openTempStore(t)
	got, ok, err := s.EntryByHash(context.Background(), make([]byte, 32))
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Nil(t, got)
}

// Starter-grant entries have NULL consumer_sig + seeder_sig in the schema.
// Lookups must surface those as nil/empty slices, not opaque database NULL
// markers that confuse downstream proto code.
func TestEntryBySeq_NullSignaturesDecodeAsNilSlices(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	in := builtStarterGrantInput(t, 1, make([]byte, 32))
	_, err := s.AppendEntry(ctx, in)
	require.NoError(t, err)

	got, ok, err := s.EntryBySeq(ctx, 1)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Nil(t, got.ConsumerSig, "starter_grant consumer_sig should round-trip as nil")
	assert.Nil(t, got.SeederSig, "starter_grant seeder_sig should round-trip as nil")
	assert.NotEmpty(t, got.TrackerSig)
}

func TestEntryBySeq_CorruptCanonicalReturnsError(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	in := builtUsageInput(t, 1, make([]byte, 32))
	_, err := s.AppendEntry(ctx, in)
	require.NoError(t, err)

	// Corrupt the canonical blob via raw SQL — simulates disk corruption.
	_, err = s.db.ExecContext(ctx, "UPDATE entries SET canonical = ? WHERE seq = 1", []byte{0xff, 0xfe, 0xfd})
	require.NoError(t, err)

	_, _, err = s.EntryBySeq(ctx, 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unmarshal canonical")
}

func TestBalance_HappyPath(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	in := builtUsageInput(t, 1, make([]byte, 32))
	_, err := s.AppendEntry(ctx, in)
	require.NoError(t, err)

	got, ok, err := s.Balance(ctx, in.Balances[0].IdentityID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, in.Balances[0].IdentityID, got.IdentityID)
	assert.Equal(t, in.Balances[0].Credits, got.Credits)
	assert.Equal(t, uint64(1), got.LastSeq)
}

func TestBalance_Miss(t *testing.T) {
	s := openTempStore(t)
	got, ok, err := s.Balance(context.Background(), make([]byte, 32))
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, got.IdentityID)
}
