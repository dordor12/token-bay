package admission

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// §10 #9 — Crash mid-OnLedgerEvent → tlog catches up via ledger cross-check.
func TestAcceptance_S10_9_CrashMidEvent_LedgerFillsTLogGap(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tlog := filepath.Join(dir, "admission.tlog")
	src := &fakeLedgerSource{}

	s1, _ := openTempSubsystem(t, WithClock(staticClockFn(now)), WithTLogPath(tlog))
	for i := 0; i < 10; i++ {
		ev := LedgerEvent{
			Kind: LedgerEventSettlement, ConsumerID: makeIDi(i),
			CostCredits: 5, Timestamp: now,
		}
		s1.OnLedgerEvent(ev)
		src.append(ev)
	}
	require.NoError(t, s1.Close())
	src.append(LedgerEvent{
		Kind: LedgerEventSettlement, ConsumerID: makeIDi(42),
		CostCredits: 5, Timestamp: now,
	})

	s2, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithTLogPath(tlog), WithSkipAutoReplay(), WithLedgerSource(src))
	require.NoError(t, s2.StartupReplay(context.Background()))

	for i := 0; i < 10; i++ {
		_, ok := consumerShardFor(s2.consumerShards, makeIDi(i)).get(makeIDi(i))
		assert.True(t, ok, "tlog-derived #%d", i)
	}
	_, ok := consumerShardFor(s2.consumerShards, makeIDi(42)).get(makeIDi(42))
	assert.True(t, ok, "ledger cross-check filled gap")
}

// §10 #10 — tlog mid-record corruption: replay halts; decisions still flow.
func TestAcceptance_S10_10_MidTLogCorruptionTruncatesReplayDecideStillFlows(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tlog := filepath.Join(dir, "admission.tlog")

	s1, _ := openTempSubsystem(t, WithClock(staticClockFn(now)), WithTLogPath(tlog))
	for i := 0; i < 5; i++ {
		s1.OnLedgerEvent(LedgerEvent{
			Kind: LedgerEventSettlement, ConsumerID: makeIDi(i),
			CostCredits: 5, Timestamp: now,
		})
	}
	require.NoError(t, s1.Close())
	corruptByteAt(t, tlog, 30)

	s2, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithTLogPath(tlog), WithSkipAutoReplay())
	err := s2.StartupReplay(context.Background())
	require.Error(t, err)

	res := s2.Decide(makeID(0xAA), nil, now)
	assert.Equal(t, OutcomeAdmit, res.Outcome)
	assert.GreaterOrEqual(t, s2.TLogCorruptions(), uint64(1))
}

// §10 #11 — Latest snapshot deleted → next-older loads, replay catches up,
// recovery <30s on a multi-hundred-entry tlog.
func TestAcceptance_S10_11_LatestSnapshotDeletedFallback(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	prefix := filepath.Join(dir, "snap")
	tlog := filepath.Join(dir, "admission.tlog")

	s1, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithSnapshotPrefix(prefix), WithTLogPath(tlog))
	for i := 0; i < 200; i++ {
		s1.OnLedgerEvent(LedgerEvent{
			Kind: LedgerEventSettlement, ConsumerID: makeIDi(i),
			CostCredits: 5, Timestamp: now,
		})
	}
	require.NoError(t, s1.runSnapshotEmitOnce(now))
	for i := 200; i < 400; i++ {
		s1.OnLedgerEvent(LedgerEvent{
			Kind: LedgerEventSettlement, ConsumerID: makeIDi(i),
			CostCredits: 5, Timestamp: now,
		})
	}
	require.NoError(t, s1.runSnapshotEmitOnce(now))
	require.NoError(t, s1.Close())

	files, err := enumerateSnapshots(prefix)
	require.NoError(t, err)
	require.Len(t, files, 2)
	require.NoError(t, os.Remove(files[1].path))

	s2, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithSnapshotPrefix(prefix), WithTLogPath(tlog), WithSkipAutoReplay())
	start := time.Now()
	require.NoError(t, s2.StartupReplay(context.Background()))
	took := time.Since(start)
	assert.Less(t, took, 30*time.Second, "recovery from older snapshot < 30s")

	for i := 0; i < 400; i++ {
		_, ok := consumerShardFor(s2.consumerShards, makeIDi(i)).get(makeIDi(i))
		require.True(t, ok, "consumer %d replayed", i)
	}
}

// §10 #12 — All snapshots corrupted → degraded mode + decisions still flow.
func TestAcceptance_S10_12_AllSnapshotsCorruptDegradedMode(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	prefix := filepath.Join(dir, "snap")
	tlog := filepath.Join(dir, "admission.tlog")

	s1, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithSnapshotPrefix(prefix), WithTLogPath(tlog))
	require.NoError(t, s1.runSnapshotEmitOnce(now))
	require.NoError(t, s1.tlog.Append(TLogRecord{Seq: 5, Kind: TLogKindSettlement, Ts: uint64(now.Unix())}))
	require.NoError(t, s1.runSnapshotEmitOnce(now))
	require.NoError(t, s1.tlog.Append(TLogRecord{Seq: 8, Kind: TLogKindSettlement, Ts: uint64(now.Unix())}))
	require.NoError(t, s1.runSnapshotEmitOnce(now))
	require.NoError(t, s1.Close())

	files, _ := enumerateSnapshots(prefix)
	for _, f := range files {
		corruptByteAt(t, f.path, 30)
	}

	s2, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithSnapshotPrefix(prefix), WithTLogPath(tlog), WithSkipAutoReplay())
	require.NoError(t, s2.StartupReplay(context.Background()))
	require.True(t, s2.DegradedMode())

	res := s2.Decide(makeID(0xBB), nil, now)
	assert.Equal(t, OutcomeAdmit, res.Outcome,
		"degraded admit until rebuilt online from incoming events")
}
