package reputation

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/ids"
)

func mkID(b byte) ids.IdentityID {
	var id ids.IdentityID
	id[0] = b
	return id
}

func TestStorageEvents_AppendAndAggregate(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()
	id := mkID(0x10)
	base := time.Unix(1_700_000_000, 0)

	for i := 0; i < 5; i++ {
		require.NoError(t, s.appendEvent(ctx, id, RoleConsumer,
			SignalBrokerRequest, 1.0, base.Add(time.Duration(i)*time.Second)))
	}

	rows, err := s.aggregateBySignal(ctx, SignalBrokerRequest,
		base.Add(-time.Hour), base.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, id, rows[0].IdentityID)
	require.InDelta(t, 5.0, rows[0].Sum, 0.0001)
	require.Equal(t, int64(5), rows[0].Count)
}

func TestStorageEvents_AggregatePerIdentity(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()
	a, b := mkID(0xA0), mkID(0xB0)
	base := time.Unix(1_700_000_000, 0)

	require.NoError(t, s.appendEvent(ctx, a, RoleConsumer,
		SignalBrokerRequest, 1.0, base))
	require.NoError(t, s.appendEvent(ctx, a, RoleConsumer,
		SignalBrokerRequest, 1.0, base.Add(time.Second)))
	require.NoError(t, s.appendEvent(ctx, b, RoleConsumer,
		SignalBrokerRequest, 1.0, base))

	rows, err := s.aggregateBySignal(ctx, SignalBrokerRequest,
		base.Add(-time.Hour), base.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, rows, 2)
	sums := map[ids.IdentityID]float64{}
	for _, r := range rows {
		sums[r.IdentityID] = r.Sum
	}
	require.InDelta(t, 2.0, sums[a], 0.0001)
	require.InDelta(t, 1.0, sums[b], 0.0001)
}

func TestStorageEvents_AggregateRespectsWindow(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()
	id := mkID(0xC0)
	base := time.Unix(1_700_000_000, 0)

	require.NoError(t, s.appendEvent(ctx, id, RoleConsumer,
		SignalBrokerRequest, 1.0, base))
	require.NoError(t, s.appendEvent(ctx, id, RoleConsumer,
		SignalBrokerRequest, 1.0, base.Add(2*time.Hour)))

	rows, err := s.aggregateBySignal(ctx, SignalBrokerRequest,
		base.Add(-time.Minute), base.Add(time.Minute))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.InDelta(t, 1.0, rows[0].Sum, 0.0001)
}

func TestStorageEvents_PrunePast(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()
	id := mkID(0xD0)
	base := time.Unix(1_700_000_000, 0)

	require.NoError(t, s.appendEvent(ctx, id, RoleConsumer,
		SignalBrokerRequest, 1.0, base.Add(-10*24*time.Hour)))
	require.NoError(t, s.appendEvent(ctx, id, RoleConsumer,
		SignalBrokerRequest, 1.0, base))

	n, err := s.pruneEventsBefore(ctx, base.Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	var remaining int
	require.NoError(t, s.db.QueryRow(
		`SELECT count(*) FROM rep_events`).Scan(&remaining))
	require.Equal(t, 1, remaining)
}

func TestStorageEvents_PopulationSize(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()
	base := time.Unix(1_700_000_000, 0)
	for i := byte(0); i < 7; i++ {
		require.NoError(t, s.appendEvent(ctx, mkID(0xE0|i),
			RoleSeeder, SignalCostReportDeviation, 1.0, base))
	}
	n, err := s.populationSize(ctx, RoleSeeder,
		base.Add(-time.Hour), base.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, 7, n)
}
