package config

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validConfig returns a config that passes Validate. Subtests mutate one
// field at a time and assert exactly one FieldError surfaces.
//
// All paths are rooted at t.TempDir() so they are absolute on every
// platform (Windows requires drive-lettered paths for filepath.IsAbs)
// and the tlog parent dir actually exists for §6.8's filesystem check.
func validConfig(t *testing.T) *Config {
	t.Helper()
	tmp := t.TempDir()
	c := DefaultConfig()
	c.DataDir = tmp
	c.Server = ServerConfig{
		ListenAddr:      "0.0.0.0:7777",
		IdentityKeyPath: filepath.Join(tmp, "identity.key"),
		TLSCertPath:     filepath.Join(tmp, "cert.pem"),
		TLSKeyPath:      filepath.Join(tmp, "cert.key"),
	}
	c.Ledger.StoragePath = filepath.Join(tmp, "ledger.sqlite")
	ApplyDefaults(c) // fills tlog_path / snapshot_path_prefix under tmp
	return c
}

func assertOneFieldError(t *testing.T, err error, field string) {
	t.Helper()
	require.Error(t, err)
	var ve *ValidationError
	require.True(t, errors.As(err, &ve), "expected *ValidationError, got %T", err)
	require.Lenf(t, ve.Errors, 1, "expected exactly one FieldError, got %v", ve.Errors)
	assert.Equal(t, field, ve.Errors[0].Field)
}

func TestValidate_DefaultConfigFlagsRequiredFields(t *testing.T) {
	err := Validate(DefaultConfig())

	require.Error(t, err)
	var ve *ValidationError
	require.True(t, errors.As(err, &ve))
	fields := make(map[string]bool, len(ve.Errors))
	for _, fe := range ve.Errors {
		fields[fe.Field] = true
	}
	assert.True(t, fields["data_dir"])
	assert.True(t, fields["server.listen_addr"])
	assert.True(t, fields["server.identity_key_path"])
	assert.True(t, fields["server.tls_cert_path"])
	assert.True(t, fields["server.tls_key_path"])
	assert.True(t, fields["ledger.storage_path"])
}

func TestValidate_HappyPath(t *testing.T) {
	c := validConfig(t)

	err := Validate(c)

	assert.NoError(t, err)
}

func TestValidate_DataDirMustBeAbsolute(t *testing.T) {
	c := validConfig(t)
	c.DataDir = "var/lib/token-bay" // relative

	err := Validate(c)

	assertOneFieldError(t, err, "data_dir")
}

func TestValidate_LogLevelRejectsBogusValue(t *testing.T) {
	c := validConfig(t)
	c.LogLevel = "chatty"

	err := Validate(c)

	assertOneFieldError(t, err, "log_level")
}

func TestValidate_LogLevelAcceptsAllFour(t *testing.T) {
	for _, lvl := range []string{"debug", "info", "warn", "error"} {
		t.Run(lvl, func(t *testing.T) {
			c := validConfig(t)
			c.LogLevel = lvl

			err := Validate(c)

			assert.NoError(t, err)
		})
	}
}

func TestValidate_UnparseableListenerAddr(t *testing.T) {
	c := validConfig(t)
	c.Server.ListenAddr = "not-a-host-port"

	err := Validate(c)

	assertOneFieldError(t, err, "server.listen_addr")
}

func TestValidate_ListenerCollision_ServerAndAdmin(t *testing.T) {
	c := validConfig(t)
	c.Admin.ListenAddr = c.Server.ListenAddr

	err := Validate(c)

	require.Error(t, err)
	var ve *ValidationError
	require.True(t, errors.As(err, &ve))
	// Collision is reported once, against the second offender:
	require.Len(t, ve.Errors, 1)
	assert.Equal(t, "admin.listen_addr", ve.Errors[0].Field)
	assert.Contains(t, ve.Errors[0].Message, "collides")
}

func TestValidate_ListenerCollision_StunAndTurn(t *testing.T) {
	c := validConfig(t)
	c.STUNTURN.TURNListenAddr = c.STUNTURN.STUNListenAddr

	err := Validate(c)

	require.Error(t, err)
	var ve *ValidationError
	require.True(t, errors.As(err, &ve))
	require.Len(t, ve.Errors, 1)
	assert.Equal(t, "stun_turn.turn_listen_addr", ve.Errors[0].Field)
}

func TestValidate_ServerMaxFrameSizeTooSmall(t *testing.T) {
	c := validConfig(t)
	c.Server.MaxFrameSize = 512

	err := Validate(c)

	assertOneFieldError(t, err, "server.max_frame_size")
}

func TestValidate_ServerIdleTimeoutSZero(t *testing.T) {
	c := validConfig(t)
	c.Server.IdleTimeoutS = 0

	err := Validate(c)

	assertOneFieldError(t, err, "server.idle_timeout_s")
}

func TestValidate_ServerMaxIncomingStreamsTooSmall(t *testing.T) {
	c := validConfig(t)
	c.Server.MaxIncomingStreams = 8

	err := Validate(c)

	assertOneFieldError(t, err, "server.max_incoming_streams")
}

func TestValidate_ServerShutdownGraceSNegative(t *testing.T) {
	c := validConfig(t)
	c.Server.ShutdownGraceS = -1

	err := Validate(c)

	assertOneFieldError(t, err, "server.shutdown_grace_s")
}

func TestValidate_LedgerMerkleIntervalMustBePositive(t *testing.T) {
	c := validConfig(t)
	c.Ledger.MerkleRootIntervalMin = 0

	err := Validate(c)

	assertOneFieldError(t, err, "ledger.merkle_root_interval_minutes")
}

func TestValidate_BrokerHeadroomThresholdRange(t *testing.T) {
	c := validConfig(t)
	c.Broker.HeadroomThreshold = 1.5

	err := Validate(c)

	assertOneFieldError(t, err, "broker.headroom_threshold")
}

func TestValidate_BrokerLoadThresholdMin(t *testing.T) {
	c := validConfig(t)
	c.Broker.LoadThreshold = 0

	err := Validate(c)

	assertOneFieldError(t, err, "broker.load_threshold")
}

func TestValidate_BrokerScoreWeightsSum(t *testing.T) {
	c := validConfig(t)
	c.Broker.ScoreWeights.Reputation = 0.5 // sum becomes 1.1

	err := Validate(c)

	assertOneFieldError(t, err, "broker.score_weights")
}

func TestValidate_BrokerScoreWeightsNegative(t *testing.T) {
	c := validConfig(t)
	c.Broker.ScoreWeights.RTT = -0.2
	c.Broker.ScoreWeights.Reputation = 0.6 // keep sum at 1.0

	err := Validate(c)

	require.Error(t, err)
	var ve *ValidationError
	require.True(t, errors.As(err, &ve))
	// Negative-weight rule fires first; sum check separate.
	found := false
	for _, fe := range ve.Errors {
		if fe.Field == "broker.score_weights.rtt" {
			found = true
		}
	}
	assert.True(t, found, "expected broker.score_weights.rtt error in %v", ve.Errors)
}

func TestValidate_BrokerOfferTimeoutPositive(t *testing.T) {
	c := validConfig(t)
	c.Broker.OfferTimeoutMs = 0

	err := Validate(c)

	assertOneFieldError(t, err, "broker.offer_timeout_ms")
}

func TestValidate_BrokerMaxOfferAttemptsMin(t *testing.T) {
	c := validConfig(t)
	c.Broker.MaxOfferAttempts = 0

	err := Validate(c)

	assertOneFieldError(t, err, "broker.max_offer_attempts")
}

func TestValidate_BrokerRequestRatePositive(t *testing.T) {
	c := validConfig(t)
	c.Broker.BrokerRequestRatePerSec = 0

	err := Validate(c)

	assertOneFieldError(t, err, "broker.broker_request_rate_per_sec")
}

func TestValidate_SettlementTimeoutsPositive(t *testing.T) {
	cases := map[string]func(*Config){
		"settlement.tunnel_setup_ms":      func(c *Config) { c.Settlement.TunnelSetupMs = 0 },
		"settlement.stream_idle_s":        func(c *Config) { c.Settlement.StreamIdleS = 0 },
		"settlement.settlement_timeout_s": func(c *Config) { c.Settlement.SettlementTimeoutS = 0 },
		"settlement.reservation_ttl_s":    func(c *Config) { c.Settlement.ReservationTTLS = 0 },
	}
	for field, mut := range cases {
		t.Run(field, func(t *testing.T) {
			c := validConfig(t)
			mut(c)
			err := Validate(c)
			require.Error(t, err)
			var ve *ValidationError
			require.True(t, errors.As(err, &ve))
			matched := false
			for _, fe := range ve.Errors {
				if fe.Field == field {
					matched = true
				}
			}
			assert.True(t, matched, "expected %s in errors %v", field, ve.Errors)
		})
	}
}

func TestValidate_SettlementTunnelSetupBelowSettlementTimeout(t *testing.T) {
	c := validConfig(t)
	c.Settlement.TunnelSetupMs = 1_000_000 // 1000s > 900s
	c.Settlement.SettlementTimeoutS = 900

	err := Validate(c)

	require.Error(t, err)
	var ve *ValidationError
	require.True(t, errors.As(err, &ve))
	matched := false
	for _, fe := range ve.Errors {
		if fe.Field == "settlement.tunnel_setup_ms" {
			matched = true
		}
	}
	assert.True(t, matched)
}

func TestValidate_SettlementReservationOutlivesSettlement(t *testing.T) {
	c := validConfig(t)
	c.Settlement.SettlementTimeoutS = 900
	c.Settlement.ReservationTTLS = 600 // shorter than settlement

	err := Validate(c)

	require.Error(t, err)
	var ve *ValidationError
	require.True(t, errors.As(err, &ve))
	matched := false
	for _, fe := range ve.Errors {
		if fe.Field == "settlement.reservation_ttl_s" {
			matched = true
		}
	}
	assert.True(t, matched)
}

func TestValidate_FederationPeerCountMin(t *testing.T) {
	c := validConfig(t)
	c.Federation.PeerCountMin = 0

	err := Validate(c)

	require.Error(t, err)
	var ve *ValidationError
	require.True(t, errors.As(err, &ve))
	matched := false
	for _, fe := range ve.Errors {
		if fe.Field == "federation.peer_count_min" {
			matched = true
		}
	}
	assert.True(t, matched)
}

func TestValidate_FederationPeerCountOrdering(t *testing.T) {
	c := validConfig(t)
	c.Federation.PeerCountMin = 20
	c.Federation.PeerCountMax = 10

	err := Validate(c)

	require.Error(t, err)
	var ve *ValidationError
	require.True(t, errors.As(err, &ve))
	matched := false
	for _, fe := range ve.Errors {
		if fe.Field == "federation.peer_count_max" {
			matched = true
		}
	}
	assert.True(t, matched)
}

func TestValidate_FederationGossipDedupePositive(t *testing.T) {
	c := validConfig(t)
	c.Federation.GossipDedupeTTLS = 0

	err := Validate(c)

	assertOneFieldError(t, err, "federation.gossip_dedupe_ttl_s")
}

func TestValidate_FederationTransferRetryPositive(t *testing.T) {
	c := validConfig(t)
	c.Federation.TransferRetryWindowH = 0

	err := Validate(c)

	assertOneFieldError(t, err, "federation.transfer_retry_window_hours")
}

func TestValidate_FederationEnrollRateMin(t *testing.T) {
	c := validConfig(t)
	c.Federation.EnrollRatePerMinPerIP = 0

	err := Validate(c)

	assertOneFieldError(t, err, "federation.enroll_rate_per_min_per_ip")
}

func TestValidate_ReputationEvalIntervalPositive(t *testing.T) {
	c := validConfig(t)
	c.Reputation.EvaluationIntervalS = 0

	err := Validate(c)

	assertOneFieldError(t, err, "reputation.evaluation_interval_s")
}

func TestValidate_ReputationSignalWindowOrdering_ShortGEMedium(t *testing.T) {
	c := validConfig(t)
	c.Reputation.SignalWindows.ShortS = 100000 // > medium

	err := Validate(c)

	require.Error(t, err)
	var ve *ValidationError
	require.True(t, errors.As(err, &ve))
	matched := false
	for _, fe := range ve.Errors {
		if fe.Field == "reputation.signal_windows" {
			matched = true
		}
	}
	assert.True(t, matched)
}

func TestValidate_ReputationZScoreThresholdPositive(t *testing.T) {
	c := validConfig(t)
	c.Reputation.ZScoreThreshold = 0

	err := Validate(c)

	assertOneFieldError(t, err, "reputation.z_score_threshold")
}

func TestValidate_ReputationDefaultScoreRange(t *testing.T) {
	c := validConfig(t)
	c.Reputation.DefaultScore = 1.5

	err := Validate(c)

	assertOneFieldError(t, err, "reputation.default_score")
}

func TestValidate_ReputationFreezeListCacheTTLPositive(t *testing.T) {
	c := validConfig(t)
	c.Reputation.FreezeListCacheTTLS = 0

	err := Validate(c)

	assertOneFieldError(t, err, "reputation.freeze_list_cache_ttl_s")
}

func TestValidate_AdmissionPressureOrdering(t *testing.T) {
	c := validConfig(t)
	c.Admission.PressureAdmitThreshold = 1.5
	c.Admission.PressureRejectThreshold = 1.0

	err := Validate(c)

	require.Error(t, err)
	var ve *ValidationError
	require.True(t, errors.As(err, &ve))
	matched := false
	for _, fe := range ve.Errors {
		if fe.Field == "admission.pressure_admit_threshold" {
			matched = true
		}
	}
	assert.True(t, matched)
}

func TestValidate_AdmissionScoreWeightsSum(t *testing.T) {
	c := validConfig(t)
	c.Admission.ScoreWeights.Tenure = 0.3 // sum becomes 1.1

	err := Validate(c)

	require.Error(t, err)
	var ve *ValidationError
	require.True(t, errors.As(err, &ve))
	matched := false
	for _, fe := range ve.Errors {
		if fe.Field == "admission.score_weights" {
			matched = true
		}
	}
	assert.True(t, matched)
}

func TestValidate_AdmissionAttestationTTLOrdering(t *testing.T) {
	c := validConfig(t)
	c.Admission.AttestationTTLSeconds = 100000
	c.Admission.AttestationMaxTTLSeconds = 50000

	err := Validate(c)

	require.Error(t, err)
	var ve *ValidationError
	require.True(t, errors.As(err, &ve))
	matched := false
	for _, fe := range ve.Errors {
		if fe.Field == "admission.attestation_max_ttl_seconds" {
			matched = true
		}
	}
	assert.True(t, matched)
}

func TestValidate_AdmissionTrialTierScoreRange(t *testing.T) {
	c := validConfig(t)
	c.Admission.TrialTierScore = 1.5

	err := Validate(c)

	assertOneFieldError(t, err, "admission.trial_tier_score")
}

func TestValidate_AdmissionMaxImportedScoreRange(t *testing.T) {
	c := validConfig(t)
	c.Admission.MaxAttestationScoreImported = 2.0

	err := Validate(c)

	assertOneFieldError(t, err, "admission.max_attestation_score_imported")
}

func TestValidate_AdmissionTLogParentExists(t *testing.T) {
	tmp := t.TempDir()
	c := validConfig(t)
	c.Admission.TLogPath = tmp + "/sub/admission.tlog" // /sub does not exist

	err := Validate(c)

	require.Error(t, err)
	var ve *ValidationError
	require.True(t, errors.As(err, &ve))
	matched := false
	for _, fe := range ve.Errors {
		if fe.Field == "admission.tlog_path" {
			matched = true
		}
	}
	assert.True(t, matched)
}

func TestValidate_AdmissionTLogParentExistsHappy(t *testing.T) {
	tmp := t.TempDir()
	c := validConfig(t)
	c.Admission.TLogPath = tmp + "/admission.tlog" // tmp exists

	err := Validate(c)

	assert.NoError(t, err)
}

func TestValidate_AdmissionEmptyBlocklistEntry(t *testing.T) {
	c := validConfig(t)
	c.Admission.AttestationPeerBlocklist = []string{"peer-a", "", "peer-c"}

	err := Validate(c)

	require.Error(t, err)
	var ve *ValidationError
	require.True(t, errors.As(err, &ve))
	matched := false
	for _, fe := range ve.Errors {
		if fe.Field == "admission.attestation_peer_blocklist[1]" {
			matched = true
		}
	}
	assert.True(t, matched)
}

func TestValidate_AdmissionFsyncBatchWindowAllowsZero(t *testing.T) {
	// Spec §6.8 says fsync_batch_window_ms ≥ 0 (zero = synchronous fsync).
	c := validConfig(t)
	c.Admission.FsyncBatchWindowMs = 0

	err := Validate(c)

	assert.NoError(t, err)
}

func TestValidate_AdmissionStunTurnRelayKbps(t *testing.T) {
	c := validConfig(t)
	c.STUNTURN.TURNRelayMaxKbps = 0

	err := Validate(c)

	assertOneFieldError(t, err, "stun_turn.turn_relay_max_kbps")
}

func TestValidate_STUNTurn_SessionTTLSecondsZero(t *testing.T) {
	c := validConfig(t)
	c.STUNTURN.SessionTTLSeconds = 0

	err := Validate(c)

	assertOneFieldError(t, err, "stun_turn.session_ttl_seconds")
}
