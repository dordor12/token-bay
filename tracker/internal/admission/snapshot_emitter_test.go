package admission

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSnapshotEmitter_TickEmitsAndPrunes(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	clk := newFixedClock(now)
	dir := t.TempDir()
	s, _ := openTempSubsystem(t,
		WithClock(clk.Now),
		WithSnapshotPrefix(filepath.Join(dir, "snap")),
		WithTLogPath(filepath.Join(dir, "admission.tlog")),
		WithSnapshotsRetained(2),
	)

	for i := 0; i < 3; i++ {
		// Bump the writer's LastSeq so each emit lands at a distinct path.
		require.NoError(t, s.tlog.Append(TLogRecord{
			Seq: uint64(i + 1), Kind: TLogKindSettlement, Ts: uint64(now.Unix()),
		}))
		require.NoError(t, s.runSnapshotEmitOnce(now))
	}

	files, err := enumerateSnapshots(filepath.Join(dir, "snap"))
	require.NoError(t, err)
	assert.Len(t, files, 2, "retention=2 keeps the two newest")
	assert.Greater(t, files[1].seq, files[0].seq)
}

func TestSnapshotEmitter_TickAppendsSnapshotMark(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	clk := newFixedClock(now)
	dir := t.TempDir()
	tlogPath := filepath.Join(dir, "admission.tlog")
	s, _ := openTempSubsystem(t,
		WithClock(clk.Now),
		WithSnapshotPrefix(filepath.Join(dir, "snap")),
		WithTLogPath(tlogPath),
	)

	require.NoError(t, s.runSnapshotEmitOnce(now))
	require.NoError(t, s.tlog.Close())

	recs, _, err := readTLogFile(tlogPath)
	require.NoError(t, err)
	require.NotEmpty(t, recs)
	last := recs[len(recs)-1]
	assert.Equal(t, TLogKindSnapshotMark, last.Kind)
}

func TestSnapshotEmitter_RaceCleanConcurrentEmitAndEvent(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	clk := newFixedClock(now)
	dir := t.TempDir()
	s, _ := openTempSubsystem(t,
		WithClock(clk.Now),
		WithSnapshotPrefix(filepath.Join(dir, "snap")),
		WithTLogPath(filepath.Join(dir, "admission.tlog")),
	)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			s.OnLedgerEvent(LedgerEvent{
				Kind: LedgerEventSettlement, ConsumerID: makeIDi(i),
				CostCredits: 10, Timestamp: now,
			})
		}
	}()
	for i := 0; i < 5; i++ {
		require.NoError(t, s.runSnapshotEmitOnce(now))
	}
	<-done
}
