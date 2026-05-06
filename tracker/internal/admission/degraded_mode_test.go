package admission

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// §7.1 — Process crash between tlog flush and ledger commit.
func TestFailure_71_TLogGapFilledByLedgerCrossCheck(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tlogPath := filepath.Join(dir, "admission.tlog")
	src := &fakeLedgerSource{}

	s1, _ := openTempSubsystem(t, WithClock(staticClockFn(now)), WithTLogPath(tlogPath))
	for i := 0; i < 5; i++ {
		ev := LedgerEvent{
			Kind: LedgerEventSettlement, ConsumerID: makeIDi(i),
			CostCredits: 10, Timestamp: now,
		}
		s1.OnLedgerEvent(ev)
		src.append(ev)
	}
	src.append(LedgerEvent{
		Kind: LedgerEventSettlement, ConsumerID: makeIDi(99),
		CostCredits: 10, Timestamp: now,
	})
	require.NoError(t, s1.Close())

	s2, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithTLogPath(tlogPath), WithSkipAutoReplay(), WithLedgerSource(src))
	require.NoError(t, s2.StartupReplay(context.Background()))

	for i := 0; i < 5; i++ {
		_, ok := consumerShardFor(s2.consumerShards, makeIDi(i)).get(makeIDi(i))
		assert.True(t, ok, "tlog-derived consumer %d", i)
	}
	_, ok := consumerShardFor(s2.consumerShards, makeIDi(99)).get(makeIDi(99))
	assert.True(t, ok, "ledger-cross-check filled the gap")
}

// §7.2 — tlog mid-record corruption surfaces; decisions still flow.
func TestFailure_72_MidTLogCorruptionDoesNotKillDecide(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tlogPath := filepath.Join(dir, "admission.tlog")

	s1, _ := openTempSubsystem(t, WithClock(staticClockFn(now)), WithTLogPath(tlogPath))
	for i := 0; i < 3; i++ {
		s1.OnLedgerEvent(LedgerEvent{
			Kind: LedgerEventSettlement, ConsumerID: makeIDi(i),
			CostCredits: 10, Timestamp: now,
		})
	}
	require.NoError(t, s1.Close())
	corruptByteAt(t, tlogPath, 30)

	s2, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithTLogPath(tlogPath), WithSkipAutoReplay())
	err := s2.StartupReplay(context.Background())
	require.Error(t, err)

	res := s2.Decide(makeID(0x77), nil, now)
	assert.Equal(t, OutcomeAdmit, res.Outcome, "boot-time admit until aggregator publishes")
}

// §7.3 — Snapshot corruption falls back to next-older + bumps counter.
func TestFailure_73_SnapshotCorruptionIncrementsCounter(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	prefix := filepath.Join(dir, "snap")

	s1, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithSnapshotPrefix(prefix),
		WithTLogPath(filepath.Join(dir, "admission.tlog")))
	require.NoError(t, s1.runSnapshotEmitOnce(now))
	require.NoError(t, s1.tlog.Append(TLogRecord{Seq: 5, Kind: TLogKindSettlement, Ts: uint64(now.Unix())}))
	require.NoError(t, s1.runSnapshotEmitOnce(now))
	require.NoError(t, s1.Close())

	files, err := enumerateSnapshots(prefix)
	require.NoError(t, err)
	corruptByteAt(t, files[1].path, 30)

	s2, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithSnapshotPrefix(prefix),
		WithTLogPath(filepath.Join(dir, "admission.tlog")),
		WithSkipAutoReplay())
	require.NoError(t, s2.StartupReplay(context.Background()))
	assert.False(t, s2.DegradedMode())
	assert.GreaterOrEqual(t, s2.SnapshotLoadFailures(), uint64(1))
}

// §7.4 — All snapshots corrupt → DegradedMode + Decide still flows.
func TestFailure_74_DegradedModeKeepsDecideAvailable(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	prefix := filepath.Join(dir, "snap")
	tlogPath := filepath.Join(dir, "admission.tlog")

	s1, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithSnapshotPrefix(prefix), WithTLogPath(tlogPath))
	require.NoError(t, s1.runSnapshotEmitOnce(now))
	require.NoError(t, s1.tlog.Append(TLogRecord{Seq: 5, Kind: TLogKindSettlement, Ts: uint64(now.Unix())}))
	require.NoError(t, s1.runSnapshotEmitOnce(now))
	require.NoError(t, s1.Close())

	files, _ := enumerateSnapshots(prefix)
	for _, f := range files {
		corruptByteAt(t, f.path, 30)
	}

	s2, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithSnapshotPrefix(prefix), WithTLogPath(tlogPath),
		WithSkipAutoReplay())
	require.NoError(t, s2.StartupReplay(context.Background()))
	require.True(t, s2.DegradedMode())

	res := s2.Decide(makeID(0x55), nil, now)
	assert.Equal(t, OutcomeAdmit, res.Outcome,
		"degraded mode admits new arrivals — credit history is null until rebuilt")
}
