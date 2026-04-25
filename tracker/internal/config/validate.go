package config

import (
	"net"
	"path/filepath"
	"strconv"
)

// Validate runs every cross-field invariant declared in the design spec
// §6 against c and returns a *ValidationError with every failure
// recorded. Returns nil when c is valid.
//
// Order: rules run in section order so error output is stable for tests.
// Validation never short-circuits on first error — operators must see
// every problem in one run.
func Validate(c *Config) error {
	v := &validator{}
	v.checkRequired(c)
	v.checkLogLevel(c)
	v.checkListeners(c)
	v.checkLedger(c)
	v.checkBroker(c)
	v.checkSettlement(c)
	return v.toError()
}

type validator struct {
	errs []FieldError
}

func (v *validator) add(field, msg string) {
	v.errs = append(v.errs, FieldError{Field: field, Message: msg})
}

func (v *validator) toError() error {
	if len(v.errs) == 0 {
		return nil
	}
	return &ValidationError{Errors: v.errs}
}

// §6.1
func (v *validator) checkRequired(c *Config) {
	if c.DataDir == "" {
		v.add("data_dir", "must be non-empty")
	} else if !filepath.IsAbs(c.DataDir) {
		v.add("data_dir", "must be an absolute path")
	}
	if c.Server.ListenAddr == "" {
		v.add("server.listen_addr", "must be non-empty")
	}
	if c.Server.IdentityKeyPath == "" {
		v.add("server.identity_key_path", "must be non-empty")
	}
	if c.Server.TLSCertPath == "" {
		v.add("server.tls_cert_path", "must be non-empty")
	}
	if c.Server.TLSKeyPath == "" {
		v.add("server.tls_key_path", "must be non-empty")
	}
	if c.Ledger.StoragePath == "" {
		v.add("ledger.storage_path", "must be non-empty")
	}
}

// §6.1 — log level enum
func (v *validator) checkLogLevel(c *Config) {
	switch c.LogLevel {
	case "", "debug", "info", "warn", "error":
		// "" is allowed because ApplyDefaults fills it; if Validate is
		// called without ApplyDefaults the empty value is still valid.
	default:
		v.add("log_level", "must be one of: debug, info, warn, error")
	}
}

// §6.2 — every listener parses; all five collectively distinct.
func (v *validator) checkListeners(c *Config) {
	listeners := []struct {
		field string
		addr  string
	}{
		{"server.listen_addr", c.Server.ListenAddr},
		{"admin.listen_addr", c.Admin.ListenAddr},
		{"metrics.listen_addr", c.Metrics.ListenAddr},
		{"stun_turn.stun_listen_addr", c.STUNTURN.STUNListenAddr},
		{"stun_turn.turn_listen_addr", c.STUNTURN.TURNListenAddr},
	}
	seen := make(map[string]string, len(listeners))
	for _, l := range listeners {
		if l.addr == "" {
			// Required listeners are caught by checkRequired; optional
			// listeners with empty addr are not parsed.
			continue
		}
		if _, _, err := net.SplitHostPort(l.addr); err != nil {
			v.add(l.field, "must be a valid host:port — "+err.Error())
			continue
		}
		if prev, ok := seen[l.addr]; ok {
			v.add(l.field, "collides with "+prev+" (both "+l.addr+")")
			continue
		}
		seen[l.addr] = l.field
	}
}

// §6.3
func (v *validator) checkLedger(c *Config) {
	if c.Ledger.MerkleRootIntervalMin <= 0 {
		v.add("ledger.merkle_root_interval_minutes", "must be > 0")
	}
}

// §6.4
func (v *validator) checkBroker(c *Config) {
	if c.Broker.HeadroomThreshold < 0 || c.Broker.HeadroomThreshold > 1 {
		v.add("broker.headroom_threshold", "must be in [0.0, 1.0]")
	}
	if c.Broker.LoadThreshold < 1 {
		v.add("broker.load_threshold", "must be >= 1")
	}
	w := c.Broker.ScoreWeights
	checkNonNeg := func(field string, val float64) {
		if val < 0 {
			v.add(field, "must be >= 0")
		}
	}
	checkNonNeg("broker.score_weights.reputation", w.Reputation)
	checkNonNeg("broker.score_weights.headroom", w.Headroom)
	checkNonNeg("broker.score_weights.rtt", w.RTT)
	checkNonNeg("broker.score_weights.load", w.Load)

	sum := w.Reputation + w.Headroom + w.RTT + w.Load
	if !approxEqual(sum, 1.0, 1e-3) {
		v.add("broker.score_weights",
			"weights must sum to 1.0 ± 0.001 (got "+ftoa(sum)+")")
	}

	if c.Broker.OfferTimeoutMs <= 0 {
		v.add("broker.offer_timeout_ms", "must be > 0")
	}
	if c.Broker.MaxOfferAttempts < 1 {
		v.add("broker.max_offer_attempts", "must be >= 1")
	}
	if c.Broker.BrokerRequestRatePerSec <= 0 {
		v.add("broker.broker_request_rate_per_sec", "must be > 0")
	}
}

// §6.5
func (v *validator) checkSettlement(c *Config) {
	if c.Settlement.TunnelSetupMs <= 0 {
		v.add("settlement.tunnel_setup_ms", "must be > 0")
	}
	if c.Settlement.StreamIdleS <= 0 {
		v.add("settlement.stream_idle_s", "must be > 0")
	}
	if c.Settlement.SettlementTimeoutS <= 0 {
		v.add("settlement.settlement_timeout_s", "must be > 0")
	}
	if c.Settlement.ReservationTTLS <= 0 {
		v.add("settlement.reservation_ttl_s", "must be > 0")
	}
	// Cross-field rules — only meaningful when the underlying values are positive.
	if c.Settlement.TunnelSetupMs > 0 && c.Settlement.SettlementTimeoutS > 0 {
		if c.Settlement.TunnelSetupMs >= c.Settlement.SettlementTimeoutS*1000 {
			v.add("settlement.tunnel_setup_ms",
				"must be < settlement_timeout_s × 1000")
		}
	}
	if c.Settlement.ReservationTTLS > 0 && c.Settlement.SettlementTimeoutS > 0 {
		if c.Settlement.ReservationTTLS < c.Settlement.SettlementTimeoutS {
			v.add("settlement.reservation_ttl_s",
				"must be >= settlement_timeout_s (TTL must outlast settlement)")
		}
	}
}

// approxEqual returns true iff |a - b| < tolerance.
func approxEqual(a, b, tolerance float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < tolerance
}

// ftoa renders a float in a stable, operator-readable form.
func ftoa(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
