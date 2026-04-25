package config

// Config is the root configuration for token-bay-tracker. Every other
// tracker subsystem reads its section from a *Config produced by Load.
//
// Required fields (data_dir, server.*, ledger.storage_path) have no
// defaults and must be populated by the operator. See the design spec
// §4.2 for the full required-field list.
type Config struct {
	DataDir    string           `yaml:"data_dir"`
	LogLevel   string           `yaml:"log_level"`
	Server     ServerConfig     `yaml:"server"`
	Admin      AdminConfig      `yaml:"admin"`
	Ledger     LedgerConfig     `yaml:"ledger"`
	Broker     BrokerConfig     `yaml:"broker"`
	Settlement SettlementConfig `yaml:"settlement"`
	Federation FederationConfig `yaml:"federation"`
	Reputation ReputationConfig `yaml:"reputation"`
	Admission  AdmissionConfig  `yaml:"admission"`
	STUNTURN   STUNTURNConfig   `yaml:"stun_turn"`
	Metrics    MetricsConfig    `yaml:"metrics"`
}

type ServerConfig struct {
	ListenAddr      string `yaml:"listen_addr"`
	IdentityKeyPath string `yaml:"identity_key_path"`
	TLSCertPath     string `yaml:"tls_cert_path"`
	TLSKeyPath      string `yaml:"tls_key_path"`
}

type AdminConfig struct {
	ListenAddr string `yaml:"listen_addr"`
}

type LedgerConfig struct {
	StoragePath           string `yaml:"storage_path"`
	MerkleRootIntervalMin int    `yaml:"merkle_root_interval_minutes"`
}

type BrokerConfig struct {
	HeadroomThreshold       float64            `yaml:"headroom_threshold"`
	LoadThreshold           int                `yaml:"load_threshold"`
	ScoreWeights            BrokerScoreWeights `yaml:"score_weights"`
	OfferTimeoutMs          int                `yaml:"offer_timeout_ms"`
	MaxOfferAttempts        int                `yaml:"max_offer_attempts"`
	BrokerRequestRatePerSec float64            `yaml:"broker_request_rate_per_sec"`
}

type BrokerScoreWeights struct {
	Reputation float64 `yaml:"reputation"`
	Headroom   float64 `yaml:"headroom"`
	RTT        float64 `yaml:"rtt"`
	Load       float64 `yaml:"load"`
}

type SettlementConfig struct {
	TunnelSetupMs      int `yaml:"tunnel_setup_ms"`
	StreamIdleS        int `yaml:"stream_idle_s"`
	SettlementTimeoutS int `yaml:"settlement_timeout_s"`
	ReservationTTLS    int `yaml:"reservation_ttl_s"`
}

type FederationConfig struct {
	PeerCountMin          int `yaml:"peer_count_min"`
	PeerCountMax          int `yaml:"peer_count_max"`
	GossipDedupeTTLS      int `yaml:"gossip_dedupe_ttl_s"`
	TransferRetryWindowH  int `yaml:"transfer_retry_window_hours"`
	EnrollRatePerMinPerIP int `yaml:"enroll_rate_per_min_per_ip"`
}

type ReputationConfig struct {
	EvaluationIntervalS int                     `yaml:"evaluation_interval_s"`
	SignalWindows       ReputationSignalWindows `yaml:"signal_windows"`
	ZScoreThreshold     float64                 `yaml:"z_score_threshold"`
	DefaultScore        float64                 `yaml:"default_score"`
	FreezeListCacheTTLS int                     `yaml:"freeze_list_cache_ttl_s"`
}

type ReputationSignalWindows struct {
	ShortS  int `yaml:"short_s"`
	MediumS int `yaml:"medium_s"`
	LongS   int `yaml:"long_s"`
}

type AdmissionConfig struct {
	PressureAdmitThreshold  float64 `yaml:"pressure_admit_threshold"`
	PressureRejectThreshold float64 `yaml:"pressure_reject_threshold"`
	QueueCap                int     `yaml:"queue_cap"`
	TrialTierScore          float64 `yaml:"trial_tier_score"`

	AgingAlphaPerMinute float64 `yaml:"aging_alpha_per_minute"`
	QueueTimeoutS       int     `yaml:"queue_timeout_s"`

	ScoreWeights AdmissionScoreWeights `yaml:"score_weights"`

	NetFlowNormalizationConstant int `yaml:"net_flow_normalization_constant"`
	TenureCapDays                int `yaml:"tenure_cap_days"`
	StarterGrantCredits          int `yaml:"starter_grant_credits"`
	RollingWindowDays            int `yaml:"rolling_window_days"`

	TrialSettlementsRequired int `yaml:"trial_settlements_required"`
	TrialDurationHours       int `yaml:"trial_duration_hours"`

	AttestationTTLSeconds                 int     `yaml:"attestation_ttl_seconds"`
	AttestationMaxTTLSeconds              int     `yaml:"attestation_max_ttl_seconds"`
	AttestationIssuancePerConsumerPerHour int     `yaml:"attestation_issuance_per_consumer_per_hour"`
	MaxAttestationScoreImported           float64 `yaml:"max_attestation_score_imported"`

	TLogPath           string `yaml:"tlog_path"`
	SnapshotPathPrefix string `yaml:"snapshot_path_prefix"`
	SnapshotIntervalS  int    `yaml:"snapshot_interval_s"`
	SnapshotsRetained  int    `yaml:"snapshots_retained"`
	FsyncBatchWindowMs int    `yaml:"fsync_batch_window_ms"`

	HeartbeatWindowMinutes      int `yaml:"heartbeat_window_minutes"`
	HeartbeatFreshnessDecayMaxS int `yaml:"heartbeat_freshness_decay_max_s"`

	AttestationPeerBlocklist []string `yaml:"attestation_peer_blocklist"`
}

type AdmissionScoreWeights struct {
	SettlementReliability float64 `yaml:"settlement_reliability"`
	InverseDisputeRate    float64 `yaml:"inverse_dispute_rate"`
	Tenure                float64 `yaml:"tenure"`
	NetCreditFlow         float64 `yaml:"net_credit_flow"`
	BalanceCushion        float64 `yaml:"balance_cushion"`
}

type STUNTURNConfig struct {
	STUNListenAddr   string `yaml:"stun_listen_addr"`
	TURNListenAddr   string `yaml:"turn_listen_addr"`
	TURNRelayMaxKbps int    `yaml:"turn_relay_max_kbps"`
}

type MetricsConfig struct {
	ListenAddr string `yaml:"listen_addr"`
}
