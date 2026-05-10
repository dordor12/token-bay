package reputation

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStorageScores_UpsertAndLoadAll(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()
	a, b := mkID(0xA1), mkID(0xB1)
	now := time.Unix(1_700_000_000, 0)

	require.NoError(t, s.upsertScore(ctx, a, 0.42, now))
	require.NoError(t, s.upsertScore(ctx, b, 0.99, now))
	require.NoError(t, s.upsertScore(ctx, a, 0.31, now))

	out, err := s.loadAllScores(ctx)
	require.NoError(t, err)
	require.Len(t, out, 2)
	require.InDelta(t, 0.31, out[a], 0.0001)
	require.InDelta(t, 0.99, out[b], 0.0001)
}

func TestStorageScores_LoadAllStates(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()
	a, b := mkID(0xA2), mkID(0xB2)
	now := time.Unix(1_700_000_000, 0)
	require.NoError(t, s.ensureState(ctx, a, now))
	require.NoError(t, s.ensureState(ctx, b, now))
	require.NoError(t, s.transition(ctx, b, StateAudit,
		ReasonRecord{
			Kind: "zscore", Signal: "broker_request",
			Z: 5, Window: "1h", At: now.Unix(),
		}, now))

	out, err := s.loadAllStates(ctx)
	require.NoError(t, err)
	require.Equal(t, StateOK, out[a].State)
	require.Equal(t, StateAudit, out[b].State)
}
