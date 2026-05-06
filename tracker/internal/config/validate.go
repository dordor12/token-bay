package config

import (
	"net"
	"os"
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
	v.checkServer(c)
	v.checkLedger(c)
	v.checkBroker(c)
	v.checkSettlement(c)
	v.checkFederation(c)
	v.checkReputation(c)
	v.checkAdmission(c)
	v.checkSTUNTURN(c)
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

// §6.2a — server transport knobs (mTLS / framing / QUIC / shutdown).
// Listener-addr parsing happens in checkListeners; required-field
// non-empty checks happen in checkRequired.
func (v *validator) checkServer(c *Config) {
	if c.Server.MaxFrameSize < 1024 {
		v.add("server.max_frame_size",
			"must be >= 1024, got "+strconv.Itoa(c.Server.MaxFrameSize))
	}
	if c.Server.IdleTimeoutS <= 0 {
		v.add("server.idle_timeout_s",
			"must be > 0, got "+strconv.Itoa(c.Server.IdleTimeoutS))
	}
	if c.Server.MaxIncomingStreams < 16 {
		v.add("server.max_incoming_streams",
			"must be >= 16, got "+strconv.Itoa(c.Server.MaxIncomingStreams))
	}
	if c.Server.ShutdownGraceS < 0 {
		v.add("server.shutdown_grace_s",
			"must be >= 0, got "+strconv.Itoa(c.Server.ShutdownGraceS))
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

// §6.6
func (v *validator) checkFederation(c *Config) {
	if c.Federation.PeerCountMin < 1 {
		v.add("federation.peer_count_min", "must be >= 1")
	}
	if c.Federation.PeerCountMax < c.Federation.PeerCountMin {
		v.add("federation.peer_count_max", "must be >= peer_count_min")
	}
	if c.Federation.GossipDedupeTTLS <= 0 {
		v.add("federation.gossip_dedupe_ttl_s", "must be > 0")
	}
	if c.Federation.TransferRetryWindowH <= 0 {
		v.add("federation.transfer_retry_window_hours", "must be > 0")
	}
	if c.Federation.EnrollRatePerMinPerIP < 1 {
		v.add("federation.enroll_rate_per_min_per_ip", "must be >= 1")
	}
}

// §6.8
func (v *validator) checkAdmission(c *Config) {
	a := c.Admission

	if a.PressureAdmitThreshold <= 0 {
		v.add("admission.pressure_admit_threshold", "must be > 0")
	}
	if a.PressureRejectThreshold <= 0 {
		v.add("admission.pressure_reject_threshold", "must be > 0")
	}
	if a.PressureAdmitThreshold > 0 && a.PressureRejectThreshold > 0 &&
		a.PressureAdmitThreshold >= a.PressureRejectThreshold {
		v.add("admission.pressure_admit_threshold",
			"must be < pressure_reject_threshold")
	}

	if a.QueueCap < 1 {
		v.add("admission.queue_cap", "must be >= 1")
	}
	if a.TrialTierScore < 0 || a.TrialTierScore > 1 {
		v.add("admission.trial_tier_score", "must be in [0.0, 1.0]")
	}
	if a.AgingAlphaPerMinute < 0 {
		v.add("admission.aging_alpha_per_minute", "must be >= 0")
	}
	if a.QueueTimeoutS <= 0 {
		v.add("admission.queue_timeout_s", "must be > 0")
	}

	w := a.ScoreWeights
	checkNonNeg := func(field string, val float64) {
		if val < 0 {
			v.add(field, "must be >= 0")
		}
	}
	checkNonNeg("admission.score_weights.settlement_reliability", w.SettlementReliability)
	checkNonNeg("admission.score_weights.inverse_dispute_rate", w.InverseDisputeRate)
	checkNonNeg("admission.score_weights.tenure", w.Tenure)
	checkNonNeg("admission.score_weights.net_credit_flow", w.NetCreditFlow)
	checkNonNeg("admission.score_weights.balance_cushion", w.BalanceCushion)
	sum := w.SettlementReliability + w.InverseDisputeRate + w.Tenure + w.NetCreditFlow + w.BalanceCushion
	if !approxEqual(sum, 1.0, 1e-3) {
		v.add("admission.score_weights",
			"weights must sum to 1.0 ± 0.001 (got "+ftoa(sum)+")")
	}

	if a.NetFlowNormalizationConstant <= 0 {
		v.add("admission.net_flow_normalization_constant", "must be > 0")
	}
	if a.TenureCapDays < 1 {
		v.add("admission.tenure_cap_days", "must be >= 1")
	}
	if a.StarterGrantCredits < 1 {
		v.add("admission.starter_grant_credits", "must be >= 1")
	}
	if a.RollingWindowDays < 1 {
		v.add("admission.rolling_window_days", "must be >= 1")
	}
	if a.TrialSettlementsRequired < 0 {
		v.add("admission.trial_settlements_required", "must be >= 0")
	}
	if a.TrialDurationHours < 0 {
		v.add("admission.trial_duration_hours", "must be >= 0")
	}
	if a.AttestationTTLSeconds <= 0 {
		v.add("admission.attestation_ttl_seconds", "must be > 0")
	}
	if a.AttestationMaxTTLSeconds < a.AttestationTTLSeconds {
		v.add("admission.attestation_max_ttl_seconds",
			"must be >= attestation_ttl_seconds")
	}
	if a.AttestationIssuancePerConsumerPerHour < 1 {
		v.add("admission.attestation_issuance_per_consumer_per_hour", "must be >= 1")
	}
	if a.MaxAttestationScoreImported < 0 || a.MaxAttestationScoreImported > 1 {
		v.add("admission.max_attestation_score_imported", "must be in [0.0, 1.0]")
	}

	if a.TLogPath == "" {
		v.add("admission.tlog_path", "must be non-empty")
	} else {
		if dir := filepath.Dir(a.TLogPath); dir != "" {
			if _, err := os.Stat(dir); err != nil {
				v.add("admission.tlog_path",
					"parent directory does not exist: "+dir)
			}
		}
	}
	if a.SnapshotPathPrefix == "" {
		v.add("admission.snapshot_path_prefix", "must be non-empty")
	}
	if a.SnapshotIntervalS <= 0 {
		v.add("admission.snapshot_interval_s", "must be > 0")
	}
	if a.SnapshotsRetained < 1 {
		v.add("admission.snapshots_retained", "must be >= 1")
	}
	if a.FsyncBatchWindowMs < 0 {
		v.add("admission.fsync_batch_window_ms", "must be >= 0")
	}
	if a.HeartbeatWindowMinutes < 1 {
		v.add("admission.heartbeat_window_minutes", "must be >= 1")
	}
	if a.HeartbeatFreshnessDecayMaxS <= 0 {
		v.add("admission.heartbeat_freshness_decay_max_s", "must be > 0")
	}
	for i, peer := range a.AttestationPeerBlocklist {
		if peer == "" {
			v.add("admission.attestation_peer_blocklist["+strconv.Itoa(i)+"]",
				"must be non-empty")
		}
	}
}

// §6.9 (listener parse + collision already handled in §6.2)
func (v *validator) checkSTUNTURN(c *Config) {
	if c.STUNTURN.TURNRelayMaxKbps <= 0 {
		v.add("stun_turn.turn_relay_max_kbps", "must be > 0")
	}
	if c.STUNTURN.SessionTTLSeconds <= 0 {
		v.add("stun_turn.session_ttl_seconds", "must be > 0")
	}
}

// §6.7
func (v *validator) checkReputation(c *Config) {
	if c.Reputation.EvaluationIntervalS <= 0 {
		v.add("reputation.evaluation_interval_s", "must be > 0")
	}
	w := c.Reputation.SignalWindows
	if !(w.ShortS < w.MediumS && w.MediumS < w.LongS) {
		v.add("reputation.signal_windows",
			"must have short_s < medium_s < long_s")
	}
	if c.Reputation.ZScoreThreshold <= 0 {
		v.add("reputation.z_score_threshold", "must be > 0")
	}
	if c.Reputation.DefaultScore < 0 || c.Reputation.DefaultScore > 1 {
		v.add("reputation.default_score", "must be in [0.0, 1.0]")
	}
	if c.Reputation.FreezeListCacheTTLS <= 0 {
		v.add("reputation.freeze_list_cache_ttl_s", "must be > 0")
	}
}
