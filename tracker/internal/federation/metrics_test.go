package federation_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/token-bay/token-bay/tracker/internal/federation"
)

func TestMetrics_RegisterAndCount(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m := federation.NewMetrics(reg)
	m.FramesIn("KIND_PING")
	m.FramesIn("KIND_PING")
	m.RootAttestationsPublished()
	m.EquivocationsDetected()

	expected := `# HELP tokenbay_federation_frames_in_total
# TYPE tokenbay_federation_frames_in_total counter
tokenbay_federation_frames_in_total{kind="KIND_PING"} 2
`
	if err := testutil.CollectAndCompare(m.FramesInVec(), bytes.NewReader([]byte(expected)), "tokenbay_federation_frames_in_total"); err != nil {
		t.Fatalf("frames_in_total compare: %v", err)
	}
	if got := testutil.ToFloat64(m.RootAttestationsPublishedCounter()); got != 1 {
		t.Fatalf("root_attestations_published_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.EquivocationsDetectedCounter()); got != 1 {
		t.Fatalf("equivocations_detected_total = %v, want 1", got)
	}
	_ = strings.TrimSpace // keep import use stable
}
