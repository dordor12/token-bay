package admission

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReplay_CleanRestart(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tlog := filepath.Join(dir, "admission.tlog")
	prefix := filepath.Join(dir, "snap")

	s1, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithTLogPath(tlog), WithSnapshotPrefix(prefix))
	for i := 0; i < 5; i++ {
		s1.OnLedgerEvent(LedgerEvent{
			Kind: LedgerEventSettlement, ConsumerID: makeIDi(i),
			CostCredits: 10, Timestamp: now,
		})
	}
	require.NoError(t, s1.runSnapshotEmitOnce(now))
	require.NoError(t, s1.Close())

	s2, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithTLogPath(tlog), WithSnapshotPrefix(prefix), WithSkipAutoReplay())
	require.NoError(t, s2.StartupReplay(context.Background()))

	for i := 0; i < 5; i++ {
		shard := consumerShardFor(s2.consumerShards, makeIDi(i))
		st, ok := shard.get(makeIDi(i))
		require.True(t, ok, "consumer %d state not replayed", i)
		assert.Equal(t, uint32(1), st.SettlementBuckets[dayBucketIndex(now, rollingWindowDays)].Total)
	}
	assert.False(t, s2.DegradedMode())
}

func TestReplay_NoSnapshotOnlyTLog(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tlog := filepath.Join(dir, "admission.tlog")

	s1, _ := openTempSubsystem(t, WithClock(staticClockFn(now)), WithTLogPath(tlog))
	s1.OnLedgerEvent(LedgerEvent{
		Kind: LedgerEventSettlement, ConsumerID: makeIDi(7),
		CostCredits: 10, Timestamp: now,
	})
	require.NoError(t, s1.Close())

	s2, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithTLogPath(tlog), WithSkipAutoReplay())
	require.NoError(t, s2.StartupReplay(context.Background()))

	shard := consumerShardFor(s2.consumerShards, makeIDi(7))
	_, ok := shard.get(makeIDi(7))
	assert.True(t, ok)
	assert.False(t, s2.DegradedMode(),
		"clean first-boot with no snapshot is not degraded; tlog replay rebuilds state")
}

func TestReplay_CorruptLatestSnapshotFallsBackToOlder(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tlog := filepath.Join(dir, "admission.tlog")
	prefix := filepath.Join(dir, "snap")

	s1, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithTLogPath(tlog), WithSnapshotPrefix(prefix))
	s1.OnLedgerEvent(LedgerEvent{
		Kind: LedgerEventSettlement, ConsumerID: makeIDi(1),
		CostCredits: 10, Timestamp: now,
	})
	require.NoError(t, s1.runSnapshotEmitOnce(now))

	// Bump tlog seq so the next snapshot path differs.
	require.NoError(t, s1.tlog.Append(TLogRecord{Seq: 99, Kind: TLogKindSettlement, Ts: uint64(now.Unix())}))
	s1.OnLedgerEvent(LedgerEvent{
		Kind: LedgerEventSettlement, ConsumerID: makeIDi(2),
		CostCredits: 10, Timestamp: now,
	})
	require.NoError(t, s1.runSnapshotEmitOnce(now))
	require.NoError(t, s1.Close())

	files, err := enumerateSnapshots(prefix)
	require.NoError(t, err)
	require.Len(t, files, 2)
	corruptByteAt(t, files[1].path, 30)

	s2, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithTLogPath(tlog), WithSnapshotPrefix(prefix), WithSkipAutoReplay())
	require.NoError(t, s2.StartupReplay(context.Background()))
	assert.False(t, s2.DegradedMode(), "older snapshot loaded successfully")
}

func TestReplay_AllSnapshotsCorrupt_DegradedMode(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tlog := filepath.Join(dir, "admission.tlog")
	prefix := filepath.Join(dir, "snap")

	s1, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithTLogPath(tlog), WithSnapshotPrefix(prefix))
	require.NoError(t, s1.runSnapshotEmitOnce(now))
	require.NoError(t, s1.tlog.Append(TLogRecord{Seq: 5, Kind: TLogKindSettlement, Ts: uint64(now.Unix())}))
	require.NoError(t, s1.runSnapshotEmitOnce(now))
	require.NoError(t, s1.Close())

	files, err := enumerateSnapshots(prefix)
	require.NoError(t, err)
	for _, f := range files {
		corruptByteAt(t, f.path, 30)
	}

	s2, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithTLogPath(tlog), WithSnapshotPrefix(prefix), WithSkipAutoReplay())
	require.NoError(t, s2.StartupReplay(context.Background()))
	assert.True(t, s2.DegradedMode(), "all snapshots corrupt → degraded")
}

func TestReplay_PartialTrailingTLogFrameHeals(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tlog := filepath.Join(dir, "admission.tlog")

	s1, _ := openTempSubsystem(t, WithClock(staticClockFn(now)), WithTLogPath(tlog))
	for i := 0; i < 3; i++ {
		s1.OnLedgerEvent(LedgerEvent{
			Kind: LedgerEventSettlement, ConsumerID: makeIDi(i),
			CostCredits: 10, Timestamp: now,
		})
	}
	require.NoError(t, s1.Close())
	appendGarbageBytes(t, tlog, 5) // half-written tail

	s2, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithTLogPath(tlog), WithSkipAutoReplay())
	require.NoError(t, s2.StartupReplay(context.Background()))
	for i := 0; i < 3; i++ {
		_, ok := consumerShardFor(s2.consumerShards, makeIDi(i)).get(makeIDi(i))
		assert.True(t, ok)
	}
}

func TestReplay_MidFileCorruptHalts(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tlog := filepath.Join(dir, "admission.tlog")

	s1, _ := openTempSubsystem(t, WithClock(staticClockFn(now)), WithTLogPath(tlog))
	for i := 0; i < 5; i++ {
		s1.OnLedgerEvent(LedgerEvent{
			Kind: LedgerEventSettlement, ConsumerID: makeIDi(i),
			CostCredits: 10, Timestamp: now,
		})
	}
	require.NoError(t, s1.Close())
	corruptByteAt(t, tlog, 20)

	s2, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithTLogPath(tlog), WithSkipAutoReplay())
	err := s2.StartupReplay(context.Background())
	require.Error(t, err)
	require.ErrorIs(t, err, ErrTLogCorrupt)
}

func TestReplay_LedgerCrossCheck_AppliesMissingEntries(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tlog := filepath.Join(dir, "admission.tlog")

	src := &fakeLedgerSource{events: []LedgerEvent{
		{Kind: LedgerEventSettlement, ConsumerID: makeIDi(99), CostCredits: 50, Timestamp: now},
	}}
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithTLogPath(tlog), WithSkipAutoReplay(), WithLedgerSource(src))
	require.NoError(t, s.StartupReplay(context.Background()))

	_, ok := consumerShardFor(s.consumerShards, makeIDi(99)).get(makeIDi(99))
	assert.True(t, ok, "ledger cross-check applied missing settlement")
}
