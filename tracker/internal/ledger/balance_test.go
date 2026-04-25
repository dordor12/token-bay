package ledger

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/signing"
)

func TestSignedBalance_EmptyChainAndIdentity(t *testing.T) {
	l := openTempLedger(t, WithClock(fixedClock(time.Unix(1714000000, 0))))
	identityID := bytes.Repeat([]byte{0x11}, 32)

	snap, err := l.SignedBalance(context.Background(), identityID)
	require.NoError(t, err)
	require.NotNil(t, snap)

	assert.Equal(t, identityID, snap.Body.IdentityId)
	assert.Equal(t, int64(0), snap.Body.Credits)
	assert.Equal(t, make([]byte, 32), snap.Body.ChainTipHash)
	assert.Equal(t, uint64(0), snap.Body.ChainTipSeq)
	assert.Equal(t, uint64(1714000000), snap.Body.IssuedAt)
	assert.Equal(t, uint64(1714000000+600), snap.Body.ExpiresAt)

	tPub, _ := trackerKeypair()
	assert.True(t, signing.VerifyBalanceSnapshot(tPub, snap), "tracker_sig must verify")
}

func TestSignedBalance_AfterStarterGrant(t *testing.T) {
	l := openTempLedger(t, WithClock(fixedClock(time.Unix(1714000000, 0))))
	ctx := context.Background()
	identityID := bytes.Repeat([]byte{0x11}, 32)

	e, err := l.IssueStarterGrant(ctx, identityID, 5000)
	require.NoError(t, err)

	snap, err := l.SignedBalance(ctx, identityID)
	require.NoError(t, err)

	assert.Equal(t, int64(5000), snap.Body.Credits)
	assert.Equal(t, uint64(1), snap.Body.ChainTipSeq)
	// chain_tip_hash should equal the entry's hash.
	hash, err := mustEntryHash(e.Body)
	require.NoError(t, err)
	assert.Equal(t, hash[:], snap.Body.ChainTipHash)

	tPub, _ := trackerKeypair()
	assert.True(t, signing.VerifyBalanceSnapshot(tPub, snap))
}

func TestSignedBalance_TTLIs600s(t *testing.T) {
	l := openTempLedger(t, WithClock(fixedClock(time.Unix(2000000, 0))))

	snap, err := l.SignedBalance(context.Background(), bytes.Repeat([]byte{0x11}, 32))
	require.NoError(t, err)
	assert.Equal(t, uint64(600), snap.Body.ExpiresAt-snap.Body.IssuedAt)
}

func TestSignedBalance_RejectsBadIdentityLength(t *testing.T) {
	l := openTempLedger(t)
	_, err := l.SignedBalance(context.Background(), []byte{1, 2, 3})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "identity_id length")
}

func TestSignedBalance_TipFoundButBalanceMissing(t *testing.T) {
	// Other identity has a starter grant; query a different identity.
	// The snapshot for the unrelated identity should still have a tip set
	// (chain isn't empty) but credits=0.
	l := openTempLedger(t, WithClock(fixedClock(time.Unix(1714000000, 0))))
	ctx := context.Background()

	_, err := l.IssueStarterGrant(ctx, bytes.Repeat([]byte{0x22}, 32), 100)
	require.NoError(t, err)

	other := bytes.Repeat([]byte{0x11}, 32)
	snap, err := l.SignedBalance(ctx, other)
	require.NoError(t, err)
	assert.Equal(t, other, snap.Body.IdentityId)
	assert.Equal(t, int64(0), snap.Body.Credits)
	assert.Equal(t, uint64(1), snap.Body.ChainTipSeq, "tip reflects the unrelated grant")
	assert.NotEqual(t, make([]byte, 32), snap.Body.ChainTipHash, "tip hash is non-zero on a non-empty chain")
}
