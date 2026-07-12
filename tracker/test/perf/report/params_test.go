package report

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeEnv builds a getenv func over a map; absent keys return "".
func fakeEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestParamsFromEnv_Defaults(t *testing.T) {
	p, err := ParamsFromEnv(fakeEnv(nil))
	require.NoError(t, err)

	assert.Equal(t, time.Hour, p.Duration, "default run duration is 1 hour")
	assert.Equal(t, 10_000, p.Consumers)
	assert.Equal(t, 10_000, p.Seeders)
	assert.Equal(t, 2, p.TrackersInitial)
	assert.Equal(t, 8, p.TrackersMax)
	assert.Equal(t, 5*time.Minute, p.TrackerAddInterval)
	assert.Equal(t, time.Minute, p.RequestInterval)
	assert.Equal(t, 32, p.Sockets)
	assert.Equal(t, 15*time.Second, p.ScrapeInterval)
	assert.Equal(t, "perf-report", p.ReportDir)
	assert.Equal(t, "claude-haiku-4-5-20251001", p.Model)

	assert.InDelta(t, 0.01, p.Thresholds.MaxErrRate, 1e-9)
	assert.Equal(t, 5*time.Second, p.Thresholds.MaxBrokerP99)
	assert.InDelta(t, 0.95, p.Thresholds.MinSettleRate, 1e-9)
	assert.InDelta(t, 0.80, p.Thresholds.MinFedSteady, 1e-9)
}

func TestParamsFromEnv_Overrides(t *testing.T) {
	p, err := ParamsFromEnv(fakeEnv(map[string]string{
		"PERF_DURATION":             "90s",
		"PERF_CONSUMERS":            "50",
		"PERF_SEEDERS":              "40",
		"PERF_TRACKERS_INITIAL":     "1",
		"PERF_TRACKERS_MAX":         "3",
		"PERF_TRACKER_ADD_INTERVAL": "20s",
		"PERF_REQUEST_INTERVAL":     "5s",
		"PERF_SOCKETS":              "4",
		"PERF_SCRAPE_INTERVAL":      "2s",
		"PERF_REPORT_DIR":           "/tmp/out",
		"PERF_MODEL":                "claude-sonnet-4-6",
		"PERF_MAX_ERR_RATE":         "0.05",
		"PERF_MAX_BROKER_P99":       "10s",
		"PERF_MIN_SETTLE_RATE":      "0.5",
		"PERF_MIN_FED_STEADY":       "0.6",
	}))
	require.NoError(t, err)

	assert.Equal(t, 90*time.Second, p.Duration)
	assert.Equal(t, 50, p.Consumers)
	assert.Equal(t, 40, p.Seeders)
	assert.Equal(t, 1, p.TrackersInitial)
	assert.Equal(t, 3, p.TrackersMax)
	assert.Equal(t, 20*time.Second, p.TrackerAddInterval)
	assert.Equal(t, 5*time.Second, p.RequestInterval)
	assert.Equal(t, 4, p.Sockets)
	assert.Equal(t, 2*time.Second, p.ScrapeInterval)
	assert.Equal(t, "/tmp/out", p.ReportDir)
	assert.Equal(t, "claude-sonnet-4-6", p.Model)
	assert.InDelta(t, 0.05, p.Thresholds.MaxErrRate, 1e-9)
	assert.Equal(t, 10*time.Second, p.Thresholds.MaxBrokerP99)
	assert.InDelta(t, 0.5, p.Thresholds.MinSettleRate, 1e-9)
	assert.InDelta(t, 0.6, p.Thresholds.MinFedSteady, 1e-9)
}

func TestParamsFromEnv_Invalid(t *testing.T) {
	cases := map[string]map[string]string{
		"bad duration":          {"PERF_DURATION": "soon"},
		"zero duration":         {"PERF_DURATION": "0s"},
		"negative consumers":    {"PERF_CONSUMERS": "-1"},
		"non-numeric seeders":   {"PERF_SEEDERS": "many"},
		"initial above max":     {"PERF_TRACKERS_INITIAL": "9", "PERF_TRACKERS_MAX": "8"},
		"zero initial trackers": {"PERF_TRACKERS_INITIAL": "0"},
		"zero sockets":          {"PERF_SOCKETS": "0"},
		"err rate above 1":      {"PERF_MAX_ERR_RATE": "1.5"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParamsFromEnv(fakeEnv(env))
			assert.Error(t, err)
		})
	}
}
