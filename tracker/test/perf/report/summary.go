package report

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Outcomes counts consumer BROKER_REQUEST attempt results. Everything
// except Errors is a VALID outcome under load (admission doing its
// job) — reported, never failing on its own.
type Outcomes struct {
	Assigned        int64 `json:"assigned"`
	NoCapacity      int64 `json:"no_capacity"`
	Queued          int64 `json:"queued"`
	Rejected        int64 `json:"rejected"`
	BudgetExhausted int64 `json:"budget_exhausted"`
	Errors          int64 `json:"errors"`
}

// TrackerFinal is one tracker's end-of-run scraped state.
type TrackerFinal struct {
	Name                string  `json:"name"`
	BrokerDecisions     float64 `json:"broker_decisions_total"`
	InflightCount       float64 `json:"broker_inflight_count"`
	AdmissionQueueDepth float64 `json:"admission_queue_depth"`
	FedPeersSteady      float64 `json:"federation_peers_steady"`
	BrokerSubmitAvgMs   float64 `json:"broker_submit_avg_ms"`
}

// Summary is the whole run's aggregated result — the input to
// threshold evaluation and report rendering.
type Summary struct {
	Duration        time.Duration `json:"duration_ns"`
	Consumers       int           `json:"consumers"`
	Seeders         int           `json:"seeders"`
	TrackersFinal   int           `json:"trackers_final"`
	TrackersHealthy int           `json:"trackers_healthy"`
	TrackersDied    []string      `json:"trackers_died,omitempty"`

	Outcomes Outcomes `json:"outcomes"`

	TotalOps int64 `json:"total_ops"`
	ErrorOps int64 `json:"error_ops"`

	BrokerP50 time.Duration `json:"broker_p50_ns"`
	BrokerP95 time.Duration `json:"broker_p95_ns"`
	BrokerP99 time.Duration `json:"broker_p99_ns"`

	UsageReportsSent     int64 `json:"usage_reports_sent"`
	SettlementsSigned    int64 `json:"settlements_signed"`
	DuplicateAssignments int64 `json:"duplicate_assignments"`

	FedSteadyEdges   int `json:"fed_steady_edges"`
	FedExpectedEdges int `json:"fed_expected_edges"`

	TrackerFinals []TrackerFinal `json:"tracker_finals,omitempty"`
}

// Violation is one failed hard threshold.
type Violation struct {
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

// ErrRate returns error operations over total operations (0 when idle).
func (s Summary) ErrRate() float64 {
	if s.TotalOps == 0 {
		return 0
	}
	return float64(s.ErrorOps) / float64(s.TotalOps)
}

// SettleRate returns counter-signed settlements over usage reports
// sent (1 when no usage was reported — the settle threshold is only
// meaningful once there is settlement traffic).
func (s Summary) SettleRate() float64 {
	if s.UsageReportsSent == 0 {
		return 1
	}
	return float64(s.SettlementsSigned) / float64(s.UsageReportsSent)
}

// Evaluate applies the hard thresholds (spec §6) and returns every
// violation. An empty slice means the run passes.
func Evaluate(s Summary, th Thresholds) []Violation {
	var out []Violation
	add := func(name, format string, args ...any) {
		out = append(out, Violation{Name: name, Detail: fmt.Sprintf(format, args...)})
	}

	if len(s.TrackersDied) > 0 {
		add("tracker_death", "tracker container(s) died during the run: %s", strings.Join(s.TrackersDied, ", "))
	}
	if s.TrackersHealthy < s.TrackersFinal {
		add("tracker_unhealthy", "%d of %d trackers unhealthy at end of run", s.TrackersFinal-s.TrackersHealthy, s.TrackersFinal)
	}
	if rate := s.ErrRate(); rate > th.MaxErrRate {
		add("error_rate", "client error rate %.4f exceeds %.4f (%d errors / %d ops)",
			rate, th.MaxErrRate, s.ErrorOps, s.TotalOps)
	}
	if s.Outcomes.Assigned > 0 && s.BrokerP99 > th.MaxBrokerP99 {
		add("broker_p99", "BROKER_REQUEST p99 %s exceeds %s", s.BrokerP99, th.MaxBrokerP99)
	}
	if rate := s.SettleRate(); rate < th.MinSettleRate {
		add("settle_rate", "settlement rate %.4f below %.4f (%d signed / %d usage reports)",
			rate, th.MinSettleRate, s.SettlementsSigned, s.UsageReportsSent)
	}
	if s.DuplicateAssignments > 0 {
		add("double_assign", "%d duplicate reservation tokens across assignments", s.DuplicateAssignments)
	}
	if s.FedExpectedEdges > 0 {
		frac := float64(s.FedSteadyEdges) / float64(s.FedExpectedEdges)
		if frac < th.MinFedSteady {
			add("federation_steady", "steady peer edges %d/%d (%.2f) below %.2f",
				s.FedSteadyEdges, s.FedExpectedEdges, frac, th.MinFedSteady)
		}
	}
	// A soak that produced zero assignments measured nothing — that is a
	// broken harness or a collapsed tracker, never a pass.
	if s.Outcomes.Assigned == 0 {
		add("no_traffic", "zero assigned broker requests over the whole run")
	}
	return out
}

// RenderMarkdown renders the report for humans ($GITHUB_STEP_SUMMARY).
func RenderMarkdown(s Summary, violations []Violation) string {
	var b strings.Builder
	verdict := "PASS"
	if len(violations) > 0 {
		verdict = "FAIL"
	}
	fmt.Fprintf(&b, "# Tracker perf run: %s\n\n", verdict)
	fmt.Fprintf(&b, "Duration %s — %d consumers, %d seeders, %d trackers (%d healthy at end)\n\n",
		s.Duration, s.Consumers, s.Seeders, s.TrackersFinal, s.TrackersHealthy)

	b.WriteString("| Outcome | Count |\n|---|---|\n")
	fmt.Fprintf(&b, "| assigned | %d |\n", s.Outcomes.Assigned)
	fmt.Fprintf(&b, "| no_capacity | %d |\n", s.Outcomes.NoCapacity)
	fmt.Fprintf(&b, "| queued | %d |\n", s.Outcomes.Queued)
	fmt.Fprintf(&b, "| rejected | %d |\n", s.Outcomes.Rejected)
	fmt.Fprintf(&b, "| budget_exhausted | %d |\n", s.Outcomes.BudgetExhausted)
	fmt.Fprintf(&b, "| errors | %d |\n\n", s.Outcomes.Errors)

	fmt.Fprintf(&b, "BROKER_REQUEST latency: p50 %s, p95 %s, p99 %s\n\n", s.BrokerP50, s.BrokerP95, s.BrokerP99)
	fmt.Fprintf(&b, "Errors: %d / %d ops (%.4f). Settlements: %d / %d usage reports (%.4f). Duplicate assignments: %d.\n\n",
		s.ErrorOps, s.TotalOps, s.ErrRate(), s.SettlementsSigned, s.UsageReportsSent, s.SettleRate(), s.DuplicateAssignments)
	fmt.Fprintf(&b, "Federation steady edges: %d / %d expected.\n\n", s.FedSteadyEdges, s.FedExpectedEdges)

	if len(s.TrackerFinals) > 0 {
		b.WriteString("| Tracker | broker decisions | inflight | admission queue | steady peers | submit avg ms |\n|---|---|---|---|---|---|\n")
		for _, tf := range s.TrackerFinals {
			fmt.Fprintf(&b, "| %s | %.0f | %.0f | %.0f | %.0f | %.2f |\n",
				tf.Name, tf.BrokerDecisions, tf.InflightCount, tf.AdmissionQueueDepth, tf.FedPeersSteady, tf.BrokerSubmitAvgMs)
		}
		b.WriteString("\n")
	}

	if len(violations) == 0 {
		b.WriteString("All thresholds passed.\n")
	} else {
		b.WriteString("## Threshold violations\n\n")
		for _, v := range violations {
			fmt.Fprintf(&b, "- **%s**: %s\n", v.Name, v.Detail)
		}
	}
	return b.String()
}

// WriteJSON writes the machine-readable report.
func WriteJSON(path string, s Summary, violations []Violation) error {
	payload := struct {
		Summary    Summary     `json:"summary"`
		Violations []Violation `json:"violations"`
	}{s, violations}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("report: marshal summary: %w", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil { //nolint:gosec // CI artifact, world-readable is fine
		return fmt.Errorf("report: write %s: %w", path, err)
	}
	return nil
}
