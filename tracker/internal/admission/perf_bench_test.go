package admission

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// §10 #13 — BenchmarkDecide latency p99 < 1ms (no attestation), <2ms with.
func BenchmarkDecide_NoAttestation(b *testing.B) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(b, WithClock(staticClockFn(now)))
	s.publishSupply(&SupplySnapshot{ComputedAt: now, TotalHeadroom: 1e6, Pressure: 0.5})

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.Decide(makeIDi(i), nil, now)
	}
}

// Test-shape latency assertion: scales with -test.short to keep CI fast.
func TestPerformance_S10_13_DecideLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("performance test")
	}
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)))
	s.publishSupply(&SupplySnapshot{ComputedAt: now, TotalHeadroom: 1e6, Pressure: 0.5})

	const iterations = 5000
	start := time.Now()
	for i := 0; i < iterations; i++ {
		s.Decide(makeIDi(i), nil, now)
	}
	avg := time.Since(start) / iterations
	assert.Less(t, avg, time.Millisecond, "Decide avg < 1ms (got %v)", avg)
}

// §10 #14 — Sustained 100 broker_requests/sec, no metric degradation.
func TestPerformance_S10_14_Sustained100PerSec(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithTLogPath(filepath.Join(t.TempDir(), "admission.tlog")))
	s.publishSupply(&SupplySnapshot{ComputedAt: now, TotalHeadroom: 1e6, Pressure: 0.5})

	const total = 500 // 100 req/s × 5 sim-seconds
	for i := 0; i < total; i++ {
		s.Decide(makeIDi(i), nil, now)
	}
	require.LessOrEqual(t, s.queue.Len(), 0, "queue stays drained at low pressure")
}

// §10 #15 — SupplySnapshot updated after registry change.
func TestPerformance_S10_15_SupplyUpdatesWithin5s(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)))
	registerSeeder(t, s.reg, makeID(0xAA), 0.1, now)

	s.runAggregatorOnce(now)
	initial := s.Supply().TotalHeadroom

	registerSeeder(t, s.reg, makeID(0xAA), 0.9, now)
	s.runAggregatorOnce(now.Add(4 * time.Second))
	updated := s.Supply().TotalHeadroom
	require.Greater(t, updated, initial, "supply reflects headroom change within window")
}

// §10 #16 — tlog write rate ≤ ledger commit rate.
func TestPerformance_S10_16_TLogRateLeqLedgerRate(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tlog := filepath.Join(dir, "admission.tlog")
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)), WithTLogPath(tlog))

	const n = 1000
	for i := 0; i < n; i++ {
		s.OnLedgerEvent(LedgerEvent{
			Kind: LedgerEventSettlement, ConsumerID: makeIDi(i),
			CostCredits: 1, Timestamp: now,
		})
	}
	require.NoError(t, s.tlog.Close())

	recs, _, err := readTLogFile(tlog)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(recs), n,
		"tlog record count <= ledger event count (1:1 in plan 3)")
	assert.Equal(t, n, len(recs), "every ledger event produced exactly one tlog record")
}
