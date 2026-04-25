package ledger

import (
	"context"
	"crypto/ed25519"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
)

func TestOpen_HappyPath(t *testing.T) {
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "ledger.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	_, priv := trackerKeypair()
	l, err := Open(store, priv)
	require.NoError(t, err)
	require.NotNil(t, l)
	assert.NotNil(t, l.trackerPub)
	assert.NotNil(t, l.nowFn)
}

func TestOpen_RejectsNilStore(t *testing.T) {
	_, priv := trackerKeypair()
	_, err := Open(nil, priv)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-nil Store")
}

func TestOpen_RejectsBadKey(t *testing.T) {
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "ledger.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	_, err = Open(store, ed25519.PrivateKey{1, 2, 3})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tracker key length")
}

func TestOpen_WithClock_OverridesNow(t *testing.T) {
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "ledger.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	fixed := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	_, priv := trackerKeypair()
	l, err := Open(store, priv, WithClock(fixedClock(fixed)))
	require.NoError(t, err)

	assert.Equal(t, fixed, l.nowFn())
}

func TestClose_NoOpReturnsNil(t *testing.T) {
	l := openTempLedger(t)
	require.NoError(t, l.Close())
	require.NoError(t, l.Close(), "second Close must also succeed")
}
