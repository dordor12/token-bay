package reputation

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/tracker/internal/config"
)

func openForTest(t *testing.T) *Subsystem {
	t.Helper()
	cfg := config.DefaultConfig().Reputation
	cfg.StoragePath = filepath.Join(t.TempDir(), "rep.sqlite")
	cfg.MinPopulationForZScore = 100
	s, err := Open(context.Background(), cfg, WithClock(func() time.Time {
		return time.Unix(1_700_000_000, 0)
	}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpen_RejectsEmptyStoragePath(t *testing.T) {
	cfg := config.DefaultConfig().Reputation
	cfg.StoragePath = ""
	_, err := Open(context.Background(), cfg)
	require.Error(t, err)
}

func TestOpen_StartsAndCloses(t *testing.T) {
	s := openForTest(t)
	require.NoError(t, s.Close())
	require.NoError(t, s.Close()) // idempotent
}

func TestOpen_AfterCloseMethodsReturnSentinel(t *testing.T) {
	cfg := config.DefaultConfig().Reputation
	cfg.StoragePath = filepath.Join(t.TempDir(), "rep.sqlite")
	s, err := Open(context.Background(), cfg)
	require.NoError(t, err)
	require.NoError(t, s.Close())

	err = s.RecordBrokerRequest(mkID(0x01), "admit")
	require.True(t, errors.Is(err, ErrSubsystemClosed))
}
