package storage

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func samplePeerRevocation(trackerByte, identityByte byte) PeerRevocation {
	return PeerRevocation{
		TrackerID:  bytes.Repeat([]byte{trackerByte}, 32),
		IdentityID: bytes.Repeat([]byte{identityByte}, 32),
		Reason:     1, // ABUSE
		RevokedAt:  1714000000,
		TrackerSig: bytes.Repeat([]byte{trackerByte ^ 0xAA}, 64),
		ReceivedAt: 1714000005,
	}
}

func TestPutPeerRevocation_RoundTrip(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	r := samplePeerRevocation(0x01, 0x02)
	require.NoError(t, s.PutPeerRevocation(ctx, r))

	got, ok, err := s.GetPeerRevocation(ctx, r.TrackerID, r.IdentityID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, r.TrackerID, got.TrackerID)
	assert.Equal(t, r.IdentityID, got.IdentityID)
	assert.Equal(t, r.Reason, got.Reason)
	assert.Equal(t, r.RevokedAt, got.RevokedAt)
	assert.Equal(t, r.TrackerSig, got.TrackerSig)
	assert.Equal(t, r.ReceivedAt, got.ReceivedAt)
}

func TestPutPeerRevocation_DuplicateIsNoop(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	r := samplePeerRevocation(0x01, 0x02)
	require.NoError(t, s.PutPeerRevocation(ctx, r))

	// Second put with different reason — INSERT OR IGNORE, first wins.
	r2 := r
	r2.Reason = 2 // MANUAL
	r2.ReceivedAt = 9999999999
	require.NoError(t, s.PutPeerRevocation(ctx, r2))

	got, ok, err := s.GetPeerRevocation(ctx, r.TrackerID, r.IdentityID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, uint32(1), got.Reason, "first writer wins")
	assert.Equal(t, uint64(1714000005), got.ReceivedAt, "first writer wins")
}

func TestGetPeerRevocation_Missing(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	_, ok, err := s.GetPeerRevocation(ctx,
		bytes.Repeat([]byte{0x99}, 32),
		bytes.Repeat([]byte{0x88}, 32))
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestIsIdentityRevoked_PresentAndMissing(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	// One revocation from tracker 0x01, identity 0x02.
	require.NoError(t, s.PutPeerRevocation(ctx, samplePeerRevocation(0x01, 0x02)))

	revoked, err := s.IsIdentityRevoked(ctx, bytes.Repeat([]byte{0x02}, 32))
	require.NoError(t, err)
	assert.True(t, revoked, "identity present in archive must report revoked")

	revoked, err = s.IsIdentityRevoked(ctx, bytes.Repeat([]byte{0x99}, 32))
	require.NoError(t, err)
	assert.False(t, revoked, "missing identity must report not revoked")
}

func TestIsIdentityRevoked_MatchesAcrossIssuers(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	// Two trackers each revoke the *same* identity. The lookup
	// must report revoked regardless of which (tracker_id, identity_id)
	// row exists — the broker only cares whether ANY peer froze it.
	require.NoError(t, s.PutPeerRevocation(ctx, samplePeerRevocation(0x10, 0x42)))
	require.NoError(t, s.PutPeerRevocation(ctx, samplePeerRevocation(0x20, 0x42)))

	revoked, err := s.IsIdentityRevoked(ctx, bytes.Repeat([]byte{0x42}, 32))
	require.NoError(t, err)
	assert.True(t, revoked)
}

func TestPutPeerRevocation_RejectsEmptyFields(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()

	bad := samplePeerRevocation(0x01, 0x02)
	bad.TrackerID = nil
	require.Error(t, s.PutPeerRevocation(ctx, bad))
}
