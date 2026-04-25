package storage

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func samplePeerRoot(trackerByte byte, hour uint64) PeerRoot {
	return PeerRoot{
		TrackerID:  bytes.Repeat([]byte{trackerByte}, 32),
		Hour:       hour,
		Root:       bytes.Repeat([]byte{trackerByte ^ 0x55}, 32),
		Sig:        bytes.Repeat([]byte{trackerByte ^ 0xAA}, 64),
		ReceivedAt: 1714000000 + hour,
	}
}

func TestPutPeerRoot_RoundTrip(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	p := samplePeerRoot(0x01, 100)
	require.NoError(t, s.PutPeerRoot(ctx, p))

	got, ok, err := s.GetPeerRoot(ctx, p.TrackerID, p.Hour)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, p.TrackerID, got.TrackerID)
	assert.Equal(t, p.Hour, got.Hour)
	assert.Equal(t, p.Root, got.Root)
	assert.Equal(t, p.Sig, got.Sig)
	assert.Equal(t, p.ReceivedAt, got.ReceivedAt)
}

func TestPutPeerRoot_IdempotentOnIdenticalRow(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	p := samplePeerRoot(0x01, 100)
	require.NoError(t, s.PutPeerRoot(ctx, p))
	require.NoError(t, s.PutPeerRoot(ctx, p), "identical row should not error")

	// Still exactly one row.
	rows, err := s.ListPeerRootsForHour(ctx, 100)
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}

func TestPutPeerRoot_ConflictOnDifferentRoot(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	p := samplePeerRoot(0x01, 100)
	require.NoError(t, s.PutPeerRoot(ctx, p))

	conflicting := p
	conflicting.Root = bytes.Repeat([]byte{0x99}, 32)
	err := s.PutPeerRoot(ctx, conflicting)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPeerRootConflict)
	assert.Contains(t, err.Error(), "tracker_id")
	assert.Contains(t, err.Error(), "incoming_root")

	// Original row unchanged.
	got, ok, err := s.GetPeerRoot(ctx, p.TrackerID, p.Hour)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, p.Root, got.Root)
}

func TestPutPeerRoot_ConflictOnDifferentSig(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	p := samplePeerRoot(0x01, 100)
	require.NoError(t, s.PutPeerRoot(ctx, p))

	conflicting := p
	conflicting.Sig = bytes.Repeat([]byte{0x99}, 64)
	err := s.PutPeerRoot(ctx, conflicting)
	assert.ErrorIs(t, err, ErrPeerRootConflict)
}

func TestPutPeerRoot_RejectsEmptyInputs(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	err := s.PutPeerRoot(ctx, PeerRoot{Hour: 1, Root: []byte{1}, Sig: []byte{1}})
	require.Error(t, err)

	err = s.PutPeerRoot(ctx, PeerRoot{TrackerID: []byte{1}, Hour: 1, Sig: []byte{1}})
	require.Error(t, err)

	err = s.PutPeerRoot(ctx, PeerRoot{TrackerID: []byte{1}, Hour: 1, Root: []byte{1}})
	require.Error(t, err)
}

func TestGetPeerRoot_Miss(t *testing.T) {
	s := openTempStore(t)
	got, ok, err := s.GetPeerRoot(context.Background(), bytes.Repeat([]byte{0xFF}, 32), 100)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, got.TrackerID)
}

func TestListPeerRootsForHour_OrderedByTrackerID(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	// Insert in reverse order; expect ascending tracker_id back.
	for _, b := range []byte{0x05, 0x03, 0x01, 0x04, 0x02} {
		require.NoError(t, s.PutPeerRoot(ctx, samplePeerRoot(b, 100)))
	}

	got, err := s.ListPeerRootsForHour(ctx, 100)
	require.NoError(t, err)
	require.Len(t, got, 5)

	for i, b := range []byte{0x01, 0x02, 0x03, 0x04, 0x05} {
		assert.Equal(t, byte(b), got[i].TrackerID[0], "row %d tracker_id", i)
	}
}

func TestListPeerRootsForHour_EmptyHour(t *testing.T) {
	s := openTempStore(t)
	got, err := s.ListPeerRootsForHour(context.Background(), 999)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestListPeerRootsForHour_FilterByHour(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	require.NoError(t, s.PutPeerRoot(ctx, samplePeerRoot(0x01, 100)))
	require.NoError(t, s.PutPeerRoot(ctx, samplePeerRoot(0x01, 101)))
	require.NoError(t, s.PutPeerRoot(ctx, samplePeerRoot(0x02, 100)))

	got, err := s.ListPeerRootsForHour(ctx, 100)
	require.NoError(t, err)
	assert.Len(t, got, 2)

	got, err = s.ListPeerRootsForHour(ctx, 101)
	require.NoError(t, err)
	assert.Len(t, got, 1)
}
