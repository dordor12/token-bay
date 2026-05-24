package ledger

import (
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestIntegrityMetricsRecordResult(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewIntegrityMetrics(reg)

	m.RecordResult("pass")
	m.RecordResult("pass")
	m.RecordResult("fail")

	if got := testutil.ToFloat64(m.ChecksTotal.WithLabelValues("pass")); got != 2 {
		t.Fatalf("pass counter: got %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.ChecksTotal.WithLabelValues("fail")); got != 1 {
		t.Fatalf("fail counter: got %v, want 1", got)
	}
}

func TestIntegrityMetricsMetricNameAndLabel(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewIntegrityMetrics(reg)
	m.RecordResult("pass")

	got, err := testutil.GatherAndCount(reg, "token_bay_ledger_integrity_checks_total")
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	if got != 1 {
		t.Fatalf("expected one series for token_bay_ledger_integrity_checks_total, got %d", got)
	}
}

func TestIntegrityMetricsRecordResultIsRaceClean(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewIntegrityMetrics(reg)

	var wg sync.WaitGroup
	const N = 100
	for range N {
		wg.Add(2)
		go func() { defer wg.Done(); m.RecordResult("pass") }()
		go func() { defer wg.Done(); m.RecordResult("fail") }()
	}
	wg.Wait()

	if got := testutil.ToFloat64(m.ChecksTotal.WithLabelValues("pass")); got != float64(N) {
		t.Fatalf("pass counter: got %v, want %d", got, N)
	}
	if got := testutil.ToFloat64(m.ChecksTotal.WithLabelValues("fail")); got != float64(N) {
		t.Fatalf("fail counter: got %v, want %d", got, N)
	}
}
