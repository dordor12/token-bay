package config

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validConfig returns a config that passes Validate. Subtests mutate one
// field at a time and assert exactly one FieldError surfaces.
func validConfig(t *testing.T) *Config {
	t.Helper()
	c := DefaultConfig()
	c.DataDir = "/var/lib/token-bay"
	c.Server = ServerConfig{
		ListenAddr:      "0.0.0.0:7777",
		IdentityKeyPath: "/etc/token-bay/identity.key",
		TLSCertPath:     "/etc/token-bay/cert.pem",
		TLSKeyPath:      "/etc/token-bay/cert.key",
	}
	c.Ledger.StoragePath = "/var/lib/token-bay/ledger.sqlite"
	ApplyDefaults(c) // fills tlog_path / snapshot_path_prefix

	// tlog parent dir must exist for §6.8's filesystem check; use the
	// already-existing /var which is universal on linux+darwin. (Tests
	// for the missing-parent branch override this.)
	c.Admission.TLogPath = "/var/admission.tlog"
	c.Admission.SnapshotPathPrefix = "/var/admission.snapshot"
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
