package report

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const promSample = `# HELP broker_submit_decisions_total Broker submit decisions.
# TYPE broker_submit_decisions_total counter
broker_submit_decisions_total{outcome="assigned"} 120
broker_submit_decisions_total{outcome="rejected"} 5
broker_inflight_count 3
tokenbay_federation_peers{state="steady"} 2
tokenbay_federation_peers{state="pending"} 1
broker_submit_duration_seconds_sum 1.5
malformed line without value
`

func TestParsePromText(t *testing.T) {
	m, err := ParsePromText(strings.NewReader(promSample))
	require.NoError(t, err)

	assert.InDelta(t, 120.0, m[`broker_submit_decisions_total{outcome="assigned"}`], 1e-9)
	assert.InDelta(t, 3.0, m["broker_inflight_count"], 1e-9)
	assert.InDelta(t, 2.0, m[`tokenbay_federation_peers{state="steady"}`], 1e-9)
	assert.InDelta(t, 1.5, m["broker_submit_duration_seconds_sum"], 1e-9)
	// Comments and malformed lines are skipped, not errors.
	assert.NotContains(t, m, "# HELP broker_submit_decisions_total Broker submit decisions.")
}

func TestSumMetric_SumsAcrossLabelSets(t *testing.T) {
	m, err := ParsePromText(strings.NewReader(promSample))
	require.NoError(t, err)

	assert.InDelta(t, 125.0, SumMetric(m, "broker_submit_decisions_total"), 1e-9)
	assert.InDelta(t, 3.0, SumMetric(m, "broker_inflight_count"), 1e-9)
	assert.InDelta(t, 0.0, SumMetric(m, "does_not_exist"), 1e-9)
	// A name that is a prefix of another must not absorb its series.
	assert.InDelta(t, 1.5, SumMetric(m, "broker_submit_duration_seconds_sum"), 1e-9)
}
