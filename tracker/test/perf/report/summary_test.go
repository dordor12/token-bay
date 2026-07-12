package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func healthySummary() Summary {
	return Summary{
		Duration:          time.Hour,
		Consumers:         100,
		Seeders:           100,
		TrackersFinal:     4,
		TrackersHealthy:   4,
		Outcomes:          Outcomes{Assigned: 900, NoCapacity: 50, Queued: 30, Rejected: 10, BudgetExhausted: 5},
		TotalOps:          5000,
		ErrorOps:          10,
		BrokerP50:         20 * time.Millisecond,
		BrokerP95:         200 * time.Millisecond,
		BrokerP99:         time.Second,
		UsageReportsSent:  900,
		SettlementsSigned: 890,
		FedSteadyEdges:    12,
		FedExpectedEdges:  12,
	}
}

func defaultThresholds() Thresholds {
	return Thresholds{
		MaxErrRate:    0.01,
		MaxBrokerP99:  5 * time.Second,
		MinSettleRate: 0.95,
		MinFedSteady:  0.80,
	}
}

func TestEvaluate_HealthyRunPasses(t *testing.T) {
	assert.Empty(t, Evaluate(healthySummary(), defaultThresholds()))
}

func TestEvaluate_Violations(t *testing.T) {
	th := defaultThresholds()

	t.Run("dead tracker", func(t *testing.T) {
		s := healthySummary()
		s.TrackersDied = []string{"tracker-3"}
		v := Evaluate(s, th)
		require.Len(t, v, 1)
		assert.Contains(t, v[0].Detail, "tracker-3")
	})

	t.Run("error rate", func(t *testing.T) {
		s := healthySummary()
		s.ErrorOps = 500 // 10%
		assert.NotEmpty(t, Evaluate(s, th))
	})

	t.Run("broker p99", func(t *testing.T) {
		s := healthySummary()
		s.BrokerP99 = 30 * time.Second
		assert.NotEmpty(t, Evaluate(s, th))
	})

	t.Run("settle rate", func(t *testing.T) {
		s := healthySummary()
		s.SettlementsSigned = 100
		assert.NotEmpty(t, Evaluate(s, th))
	})

	t.Run("double assignment", func(t *testing.T) {
		s := healthySummary()
		s.DuplicateAssignments = 1
		assert.NotEmpty(t, Evaluate(s, th))
	})

	t.Run("federation partition", func(t *testing.T) {
		s := healthySummary()
		s.FedSteadyEdges = 4
		assert.NotEmpty(t, Evaluate(s, th))
	})

	t.Run("no broker traffic at all is a violation", func(t *testing.T) {
		s := healthySummary()
		s.Outcomes = Outcomes{}
		s.UsageReportsSent = 0
		s.SettlementsSigned = 0
		assert.NotEmpty(t, Evaluate(s, th), "a run that never assigned anything must not pass silently")
	})
}

func TestRenderMarkdownAndWriteJSON(t *testing.T) {
	s := healthySummary()
	s.TrackerFinals = []TrackerFinal{{Name: "tracker-1", BrokerDecisions: 500, FedPeersSteady: 3}}
	violations := []Violation{{Name: "error_rate", Detail: "too many errors"}}

	md := RenderMarkdown(s, violations)
	assert.Contains(t, md, "tracker-1")
	assert.Contains(t, md, "error_rate")
	assert.Contains(t, md, "FAIL")

	mdPass := RenderMarkdown(s, nil)
	assert.Contains(t, mdPass, "PASS")

	dir := t.TempDir()
	path := filepath.Join(dir, "summary.json")
	require.NoError(t, WriteJSON(path, s, violations))
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var decoded struct {
		Summary    Summary
		Violations []Violation
	}
	require.NoError(t, json.Unmarshal(raw, &decoded))
	assert.Equal(t, s.TotalOps, decoded.Summary.TotalOps)
	require.Len(t, decoded.Violations, 1)
	assert.True(t, strings.Contains(decoded.Violations[0].Detail, "too many"))
}
