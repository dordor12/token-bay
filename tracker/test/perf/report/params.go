// Package report holds the pure, unit-testable parts of the tracker
// performance harness (tracker/test/perf): run parameters, the
// client-side latency histogram, Prometheus text-format parsing,
// threshold evaluation, and report rendering. Everything here is free
// of Docker, QUIC, and build tags so `make test` exercises it on every
// CI run even though the load harness itself only runs under
// `-tags=perf`.
package report

import (
	"fmt"
	"strconv"
	"time"
)

// Thresholds are the hard pass/fail floors of a perf run (spec §6).
type Thresholds struct {
	// MaxErrRate is the maximum tolerated ratio of transport/dial/
	// unexpected-RPC-status errors to total client operations.
	MaxErrRate float64
	// MaxBrokerP99 bounds the client-observed BROKER_REQUEST p99.
	MaxBrokerP99 time.Duration
	// MinSettleRate is the minimum ratio of counter-signed settlements
	// to usage reports sent.
	MinSettleRate float64
	// MinFedSteady is the minimum ratio of steady federation peer edges
	// to expected full-mesh edges at the end of the run.
	MinFedSteady float64
}

// Params are the env-tunable inputs of a perf run (spec §7).
type Params struct {
	Duration           time.Duration
	Consumers          int
	Seeders            int
	TrackersInitial    int
	TrackersMax        int
	TrackerAddInterval time.Duration
	RequestInterval    time.Duration
	Sockets            int
	ScrapeInterval     time.Duration
	ReportDir          string
	Model              string
	Thresholds         Thresholds
}

// ParamsFromEnv reads Params from getenv (empty string = unset =
// default). getenv is injected so tests never mutate the process
// environment.
func ParamsFromEnv(getenv func(string) string) (Params, error) {
	p := Params{
		Duration:           time.Hour,
		Consumers:          10_000,
		Seeders:            10_000,
		TrackersInitial:    2,
		TrackersMax:        8,
		TrackerAddInterval: 5 * time.Minute,
		RequestInterval:    time.Minute,
		Sockets:            32,
		ScrapeInterval:     15 * time.Second,
		ReportDir:          "perf-report",
		Model:              "claude-haiku-4-5-20251001",
		Thresholds: Thresholds{
			MaxErrRate:    0.01,
			MaxBrokerP99:  5 * time.Second,
			MinSettleRate: 0.95,
			MinFedSteady:  0.80,
		},
	}

	var err error
	if p.Duration, err = envDuration(getenv, "PERF_DURATION", p.Duration); err != nil {
		return Params{}, err
	}
	if p.Consumers, err = envInt(getenv, "PERF_CONSUMERS", p.Consumers); err != nil {
		return Params{}, err
	}
	if p.Seeders, err = envInt(getenv, "PERF_SEEDERS", p.Seeders); err != nil {
		return Params{}, err
	}
	if p.TrackersInitial, err = envInt(getenv, "PERF_TRACKERS_INITIAL", p.TrackersInitial); err != nil {
		return Params{}, err
	}
	if p.TrackersMax, err = envInt(getenv, "PERF_TRACKERS_MAX", p.TrackersMax); err != nil {
		return Params{}, err
	}
	if p.TrackerAddInterval, err = envDuration(getenv, "PERF_TRACKER_ADD_INTERVAL", p.TrackerAddInterval); err != nil {
		return Params{}, err
	}
	if p.RequestInterval, err = envDuration(getenv, "PERF_REQUEST_INTERVAL", p.RequestInterval); err != nil {
		return Params{}, err
	}
	if p.Sockets, err = envInt(getenv, "PERF_SOCKETS", p.Sockets); err != nil {
		return Params{}, err
	}
	if p.ScrapeInterval, err = envDuration(getenv, "PERF_SCRAPE_INTERVAL", p.ScrapeInterval); err != nil {
		return Params{}, err
	}
	if v := getenv("PERF_REPORT_DIR"); v != "" {
		p.ReportDir = v
	}
	if v := getenv("PERF_MODEL"); v != "" {
		p.Model = v
	}
	if p.Thresholds.MaxErrRate, err = envRatio(getenv, "PERF_MAX_ERR_RATE", p.Thresholds.MaxErrRate); err != nil {
		return Params{}, err
	}
	if p.Thresholds.MaxBrokerP99, err = envDuration(getenv, "PERF_MAX_BROKER_P99", p.Thresholds.MaxBrokerP99); err != nil {
		return Params{}, err
	}
	if p.Thresholds.MinSettleRate, err = envRatio(getenv, "PERF_MIN_SETTLE_RATE", p.Thresholds.MinSettleRate); err != nil {
		return Params{}, err
	}
	if p.Thresholds.MinFedSteady, err = envRatio(getenv, "PERF_MIN_FED_STEADY", p.Thresholds.MinFedSteady); err != nil {
		return Params{}, err
	}

	if p.Duration <= 0 {
		return Params{}, fmt.Errorf("perf params: PERF_DURATION must be positive, got %s", p.Duration)
	}
	if p.Consumers < 0 || p.Seeders < 0 {
		return Params{}, fmt.Errorf("perf params: consumer/seeder counts must be >= 0")
	}
	if p.TrackersInitial < 1 {
		return Params{}, fmt.Errorf("perf params: PERF_TRACKERS_INITIAL must be >= 1")
	}
	if p.TrackersMax < p.TrackersInitial {
		return Params{}, fmt.Errorf("perf params: PERF_TRACKERS_MAX (%d) must be >= PERF_TRACKERS_INITIAL (%d)",
			p.TrackersMax, p.TrackersInitial)
	}
	if p.Sockets < 1 {
		return Params{}, fmt.Errorf("perf params: PERF_SOCKETS must be >= 1")
	}
	if p.TrackerAddInterval <= 0 || p.RequestInterval <= 0 || p.ScrapeInterval <= 0 {
		return Params{}, fmt.Errorf("perf params: intervals must be positive")
	}
	return p, nil
}

func envDuration(getenv func(string) string, key string, def time.Duration) (time.Duration, error) {
	v := getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("perf params: %s=%q: %w", key, v, err)
	}
	return d, nil
}

func envInt(getenv func(string) string, key string, def int) (int, error) {
	v := getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("perf params: %s=%q: %w", key, v, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("perf params: %s=%q must not be negative", key, v)
	}
	return n, nil
}

// envRatio parses a float in [0, 1].
func envRatio(getenv func(string) string, key string, def float64) (float64, error) {
	v := getenv(key)
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("perf params: %s=%q: %w", key, v, err)
	}
	if f < 0 || f > 1 {
		return 0, fmt.Errorf("perf params: %s=%q must be in [0, 1]", key, v)
	}
	return f, nil
}
