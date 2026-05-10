package config

// ApplyDefaults fills every zero-valued field with the value from
// DefaultConfig() and expands ${data_dir}-relative paths. Idempotent:
// running it twice produces the same Config.
//
// Implementation policy: explicit per-field code, no reflection. The
// rule lives in CLAUDE.md; future contributors who add fields must add
// a corresponding default-fill stanza here.
func ApplyDefaults(c *Config) {
	d := DefaultConfig()

	// Top-level
	if c.LogLevel == "" {
		c.LogLevel = d.LogLevel
	}

	// Server (optional fields only — required fields stay zero-valued)
	if c.Server.MaxFrameSize == 0 {
		c.Server.MaxFrameSize = d.Server.MaxFrameSize
	}
	if c.Server.IdleTimeoutS == 0 {
		c.Server.IdleTimeoutS = d.Server.IdleTimeoutS
	}
	if c.Server.MaxIncomingStreams == 0 {
		c.Server.MaxIncomingStreams = d.Server.MaxIncomingStreams
	}
	if c.Server.ShutdownGraceS == 0 {
		c.Server.ShutdownGraceS = d.Server.ShutdownGraceS
	}

	// Admin
	if c.Admin.ListenAddr == "" {
		c.Admin.ListenAddr = d.Admin.ListenAddr
	}

	// Ledger
	if c.Ledger.MerkleRootIntervalMin == 0 {
		c.Ledger.MerkleRootIntervalMin = d.Ledger.MerkleRootIntervalMin
	}

	// Broker
	if c.Broker.HeadroomThreshold == 0 {
		c.Broker.HeadroomThreshold = d.Broker.HeadroomThreshold
	}
	if c.Broker.LoadThreshold == 0 {
		c.Broker.LoadThreshold = d.Broker.LoadThreshold
	}
	// Score-weights block: if every weight is zero, fill all from default;
	// otherwise leave operator's values alone (Validate enforces sum=1.0).
	bw := c.Broker.ScoreWeights
	if bw.Reputation == 0 && bw.Headroom == 0 && bw.RTT == 0 && bw.Load == 0 {
		c.Broker.ScoreWeights = d.Broker.ScoreWeights
	}
	if c.Broker.OfferTimeoutMs == 0 {
		c.Broker.OfferTimeoutMs = d.Broker.OfferTimeoutMs
	}
	if c.Broker.MaxOfferAttempts == 0 {
		c.Broker.MaxOfferAttempts = d.Broker.MaxOfferAttempts
	}
	if c.Broker.BrokerRequestRatePerSec == 0 {
		c.Broker.BrokerRequestRatePerSec = d.Broker.BrokerRequestRatePerSec
	}
	if c.Broker.QueueDrainIntervalMs == 0 {
		c.Broker.QueueDrainIntervalMs = d.Broker.QueueDrainIntervalMs
	}
	if c.Broker.InflightTerminalTTLS == 0 {
		c.Broker.InflightTerminalTTLS = d.Broker.InflightTerminalTTLS
	}

	// Settlement
	if c.Settlement.TunnelSetupMs == 0 {
		c.Settlement.TunnelSetupMs = d.Settlement.TunnelSetupMs
	}
	if c.Settlement.StreamIdleS == 0 {
		c.Settlement.StreamIdleS = d.Settlement.StreamIdleS
	}
	if c.Settlement.SettlementTimeoutS == 0 {
		c.Settlement.SettlementTimeoutS = d.Settlement.SettlementTimeoutS
	}
	if c.Settlement.ReservationTTLS == 0 {
		c.Settlement.ReservationTTLS = d.Settlement.ReservationTTLS
	}
	if c.Settlement.StaleTipRetries == 0 {
		c.Settlement.StaleTipRetries = d.Settlement.StaleTipRetries
	}

	// Federation
	if c.Federation.PeerCountMin == 0 {
		c.Federation.PeerCountMin = d.Federation.PeerCountMin
	}
	if c.Federation.PeerCountMax == 0 {
		c.Federation.PeerCountMax = d.Federation.PeerCountMax
	}
	if c.Federation.GossipDedupeTTLS == 0 {
		c.Federation.GossipDedupeTTLS = d.Federation.GossipDedupeTTLS
	}
	if c.Federation.TransferRetryWindowH == 0 {
		c.Federation.TransferRetryWindowH = d.Federation.TransferRetryWindowH
	}
	if c.Federation.EnrollRatePerMinPerIP == 0 {
		c.Federation.EnrollRatePerMinPerIP = d.Federation.EnrollRatePerMinPerIP
	}
	if c.Federation.HandshakeTimeoutS == 0 {
		c.Federation.HandshakeTimeoutS = d.Federation.HandshakeTimeoutS
	}
	if c.Federation.GossipRateQPS == 0 {
		c.Federation.GossipRateQPS = d.Federation.GossipRateQPS
	}
	if c.Federation.SendQueueDepth == 0 {
		c.Federation.SendQueueDepth = d.Federation.SendQueueDepth
	}
	if c.Federation.PublishCadenceS == 0 {
		c.Federation.PublishCadenceS = d.Federation.PublishCadenceS
	}
	if c.Federation.IdleTimeoutS == 0 {
		c.Federation.IdleTimeoutS = d.Federation.IdleTimeoutS
	}
	if c.Federation.RedialBaseS == 0 {
		c.Federation.RedialBaseS = d.Federation.RedialBaseS
	}
	if c.Federation.RedialMaxS == 0 {
		c.Federation.RedialMaxS = d.Federation.RedialMaxS
	}
	// ListenAddr: empty is a valid value (federation network-disabled),
	// so ApplyDefaults does NOT fill it in.
	// Peers: no default — operator-managed; leave nil if not set.
	if c.Federation.Bootstrap.MaxPeers == 0 {
		c.Federation.Bootstrap.MaxPeers = d.Federation.Bootstrap.MaxPeers
	}
	if c.Federation.Bootstrap.TTLSeconds == 0 {
		c.Federation.Bootstrap.TTLSeconds = d.Federation.Bootstrap.TTLSeconds
	}

	// Reputation
	if c.Reputation.EvaluationIntervalS == 0 {
		c.Reputation.EvaluationIntervalS = d.Reputation.EvaluationIntervalS
	}
	if c.Reputation.SignalWindows.ShortS == 0 {
		c.Reputation.SignalWindows.ShortS = d.Reputation.SignalWindows.ShortS
	}
	if c.Reputation.SignalWindows.MediumS == 0 {
		c.Reputation.SignalWindows.MediumS = d.Reputation.SignalWindows.MediumS
	}
	if c.Reputation.SignalWindows.LongS == 0 {
		c.Reputation.SignalWindows.LongS = d.Reputation.SignalWindows.LongS
	}
	if c.Reputation.ZScoreThreshold == 0 {
		c.Reputation.ZScoreThreshold = d.Reputation.ZScoreThreshold
	}
	if c.Reputation.DefaultScore == 0 {
		c.Reputation.DefaultScore = d.Reputation.DefaultScore
	}
	if c.Reputation.FreezeListCacheTTLS == 0 {
		c.Reputation.FreezeListCacheTTLS = d.Reputation.FreezeListCacheTTLS
	}
	if c.Reputation.MinPopulationForZScore == 0 {
		c.Reputation.MinPopulationForZScore = d.Reputation.MinPopulationForZScore
	}
	if c.Reputation.StoragePath == "" && c.DataDir != "" {
		c.Reputation.StoragePath = c.DataDir + "/reputation.sqlite"
	}

	// Admission — non-path fields
	if c.Admission.PressureAdmitThreshold == 0 {
		c.Admission.PressureAdmitThreshold = d.Admission.PressureAdmitThreshold
	}
	if c.Admission.PressureRejectThreshold == 0 {
		c.Admission.PressureRejectThreshold = d.Admission.PressureRejectThreshold
	}
	if c.Admission.QueueCap == 0 {
		c.Admission.QueueCap = d.Admission.QueueCap
	}
	if c.Admission.TrialTierScore == 0 {
		c.Admission.TrialTierScore = d.Admission.TrialTierScore
	}
	if c.Admission.AgingAlphaPerMinute == 0 {
		c.Admission.AgingAlphaPerMinute = d.Admission.AgingAlphaPerMinute
	}
	if c.Admission.QueueTimeoutS == 0 {
		c.Admission.QueueTimeoutS = d.Admission.QueueTimeoutS
	}
	asw := c.Admission.ScoreWeights
	if asw.SettlementReliability == 0 && asw.InverseDisputeRate == 0 &&
		asw.Tenure == 0 && asw.NetCreditFlow == 0 && asw.BalanceCushion == 0 {
		c.Admission.ScoreWeights = d.Admission.ScoreWeights
	}
	if c.Admission.NetFlowNormalizationConstant == 0 {
		c.Admission.NetFlowNormalizationConstant = d.Admission.NetFlowNormalizationConstant
	}
	if c.Admission.TenureCapDays == 0 {
		c.Admission.TenureCapDays = d.Admission.TenureCapDays
	}
	if c.Admission.StarterGrantCredits == 0 {
		c.Admission.StarterGrantCredits = d.Admission.StarterGrantCredits
	}
	if c.Admission.RollingWindowDays == 0 {
		c.Admission.RollingWindowDays = d.Admission.RollingWindowDays
	}
	if c.Admission.TrialSettlementsRequired == 0 {
		c.Admission.TrialSettlementsRequired = d.Admission.TrialSettlementsRequired
	}
	if c.Admission.TrialDurationHours == 0 {
		c.Admission.TrialDurationHours = d.Admission.TrialDurationHours
	}
	if c.Admission.AttestationTTLSeconds == 0 {
		c.Admission.AttestationTTLSeconds = d.Admission.AttestationTTLSeconds
	}
	if c.Admission.AttestationMaxTTLSeconds == 0 {
		c.Admission.AttestationMaxTTLSeconds = d.Admission.AttestationMaxTTLSeconds
	}
	if c.Admission.AttestationIssuancePerConsumerPerHour == 0 {
		c.Admission.AttestationIssuancePerConsumerPerHour = d.Admission.AttestationIssuancePerConsumerPerHour
	}
	if c.Admission.MaxAttestationScoreImported == 0 {
		c.Admission.MaxAttestationScoreImported = d.Admission.MaxAttestationScoreImported
	}
	if c.Admission.SnapshotIntervalS == 0 {
		c.Admission.SnapshotIntervalS = d.Admission.SnapshotIntervalS
	}
	if c.Admission.SnapshotsRetained == 0 {
		c.Admission.SnapshotsRetained = d.Admission.SnapshotsRetained
	}
	if c.Admission.FsyncBatchWindowMs == 0 {
		c.Admission.FsyncBatchWindowMs = d.Admission.FsyncBatchWindowMs
	}
	if c.Admission.HeartbeatWindowMinutes == 0 {
		c.Admission.HeartbeatWindowMinutes = d.Admission.HeartbeatWindowMinutes
	}
	if c.Admission.HeartbeatFreshnessDecayMaxS == 0 {
		c.Admission.HeartbeatFreshnessDecayMaxS = d.Admission.HeartbeatFreshnessDecayMaxS
	}

	// Admission — paths derive from data_dir
	if c.Admission.TLogPath == "" && c.DataDir != "" {
		c.Admission.TLogPath = c.DataDir + "/admission.tlog"
	}
	if c.Admission.SnapshotPathPrefix == "" && c.DataDir != "" {
		c.Admission.SnapshotPathPrefix = c.DataDir + "/admission.snapshot"
	}

	// STUN/TURN
	if c.STUNTURN.STUNListenAddr == "" {
		c.STUNTURN.STUNListenAddr = d.STUNTURN.STUNListenAddr
	}
	if c.STUNTURN.TURNListenAddr == "" {
		c.STUNTURN.TURNListenAddr = d.STUNTURN.TURNListenAddr
	}
	if c.STUNTURN.TURNRelayMaxKbps == 0 {
		c.STUNTURN.TURNRelayMaxKbps = d.STUNTURN.TURNRelayMaxKbps
	}
	if c.STUNTURN.SessionTTLSeconds == 0 {
		c.STUNTURN.SessionTTLSeconds = d.STUNTURN.SessionTTLSeconds
	}

	// Metrics
	if c.Metrics.ListenAddr == "" {
		c.Metrics.ListenAddr = d.Metrics.ListenAddr
	}

	// Pricing
	if len(c.Pricing.Models) == 0 {
		c.Pricing.Models = d.Pricing.Models
	}
}
