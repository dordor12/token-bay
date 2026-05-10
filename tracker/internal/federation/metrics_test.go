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

func TestMetrics_RevocationsEmittedCounts(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m := federation.NewMetrics(reg)
	m.RevocationsEmitted()
	m.RevocationsEmitted()
	if got := testutil.ToFloat64(m.RevocationsEmittedCounter()); got != 2 {
		t.Fatalf("revocations_emitted_total = %v, want 2", got)
	}
}

func TestMetrics_RevocationsReceivedLabels(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m := federation.NewMetrics(reg)
	m.RevocationsReceived("archived")
	m.RevocationsReceived("sig")
	m.RevocationsReceived("archived")
	vec := m.RevocationsReceivedVec()
	if got := testutil.ToFloat64(vec.WithLabelValues("archived")); got != 2 {
		t.Fatalf("archived = %v, want 2", got)
	}
	if got := testutil.ToFloat64(vec.WithLabelValues("sig")); got != 1 {
		t.Fatalf("sig = %v, want 1", got)
	}
}

func TestMetrics_PeerExchangeEmittedCounts(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m := federation.NewMetrics(reg)
	m.PeerExchangeEmitted()
	m.PeerExchangeEmitted()
	if got := testutil.ToFloat64(m.PeerExchangeEmittedCounter()); got != 2 {
		t.Fatalf("peer_exchange_emitted_total = %v, want 2", got)
	}
}

func TestMetrics_PeerExchangeReceivedLabels(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m := federation.NewMetrics(reg)
	m.PeerExchangeReceived("merged")
	m.PeerExchangeReceived("merged")
	m.PeerExchangeReceived("peer_exchange_disabled")
	vec := m.PeerExchangeReceivedVec()
	if got := testutil.ToFloat64(vec.WithLabelValues("merged")); got != 2 {
		t.Fatalf("merged = %v, want 2", got)
	}
	if got := testutil.ToFloat64(vec.WithLabelValues("peer_exchange_disabled")); got != 1 {
		t.Fatalf("peer_exchange_disabled = %v, want 1", got)
	}
}

func TestMetrics_KnownPeersSize(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m := federation.NewMetrics(reg)
	m.SetKnownPeersSize(7)
	if got := testutil.ToFloat64(m.KnownPeersSizeGauge()); got != 7 {
		t.Fatalf("known_peers_size = %v, want 7", got)
	}
}
