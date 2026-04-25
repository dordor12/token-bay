package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestConfig_YAMLTags_AllSnakeCase walks every field in Config and its
// embedded section types and asserts the yaml tag is present and uses
// snake_case. Catches typos and forgotten tags.
func TestConfig_YAMLTags_AllSnakeCase(t *testing.T) {
	walkYAMLTags(t, reflect.TypeOf(Config{}), "Config")
}

func walkYAMLTags(t *testing.T, ty reflect.Type, parent string) {
	t.Helper()
	for i := 0; i < ty.NumField(); i++ {
		f := ty.Field(i)
		tag := f.Tag.Get("yaml")
		path := parent + "." + f.Name
		if tag == "" {
			t.Errorf("%s: missing yaml tag", path)
			continue
		}
		assert.Falsef(t, strings.Contains(tag, " "), "%s: yaml tag must not contain spaces", path)
		assert.Equalf(t, strings.ToLower(tag), tag, "%s: yaml tag %q must be snake_case", path, tag)
		if f.Type.Kind() == reflect.Struct {
			walkYAMLTags(t, f.Type, path)
		}
	}
}

func TestConfig_HasAllExpectedSections(t *testing.T) {
	cty := reflect.TypeOf(Config{})
	expected := []string{
		"DataDir", "LogLevel",
		"Server", "Admin", "Ledger",
		"Broker", "Settlement",
		"Federation", "Reputation", "Admission",
		"STUNTURN", "Metrics",
	}
	for _, name := range expected {
		_, ok := cty.FieldByName(name)
		assert.Truef(t, ok, "Config missing field %q", name)
	}
}

func TestDefaultConfig_RequiredFieldsAreZero(t *testing.T) {
	c := DefaultConfig()

	assert.Empty(t, c.DataDir, "data_dir is required; default must be empty")
	assert.Empty(t, c.Server.ListenAddr)
	assert.Empty(t, c.Server.IdentityKeyPath)
	assert.Empty(t, c.Server.TLSCertPath)
	assert.Empty(t, c.Server.TLSKeyPath)
	assert.Empty(t, c.Ledger.StoragePath)
}

func TestDefaultConfig_SpecDefaultsPopulated(t *testing.T) {
	c := DefaultConfig()

	// Top-level
	assert.Equal(t, "info", c.LogLevel)

	// Admin / metrics / stun-turn
	assert.Equal(t, "127.0.0.1:9090", c.Admin.ListenAddr)
	assert.Equal(t, ":9100", c.Metrics.ListenAddr)
	assert.Equal(t, ":3478", c.STUNTURN.STUNListenAddr)
	assert.Equal(t, ":3479", c.STUNTURN.TURNListenAddr)
	assert.Equal(t, 1024, c.STUNTURN.TURNRelayMaxKbps)

	// Ledger
	assert.Equal(t, 60, c.Ledger.MerkleRootIntervalMin)

	// Broker (tracker spec §5.1)
	assert.InDelta(t, 0.2, c.Broker.HeadroomThreshold, 1e-9)
	assert.Equal(t, 5, c.Broker.LoadThreshold)
	assert.InDelta(t, 0.4, c.Broker.ScoreWeights.Reputation, 1e-9)
	assert.InDelta(t, 0.3, c.Broker.ScoreWeights.Headroom, 1e-9)
	assert.InDelta(t, 0.2, c.Broker.ScoreWeights.RTT, 1e-9)
	assert.InDelta(t, 0.1, c.Broker.ScoreWeights.Load, 1e-9)
	assert.Equal(t, 1500, c.Broker.OfferTimeoutMs)
	assert.Equal(t, 4, c.Broker.MaxOfferAttempts)
	assert.InDelta(t, 2.0, c.Broker.BrokerRequestRatePerSec, 1e-9)

	// Settlement (tracker spec §5.3)
	assert.Equal(t, 10000, c.Settlement.TunnelSetupMs)
	assert.Equal(t, 60, c.Settlement.StreamIdleS)
	assert.Equal(t, 900, c.Settlement.SettlementTimeoutS)
	assert.Equal(t, 1200, c.Settlement.ReservationTTLS)

	// Federation
	assert.Equal(t, 8, c.Federation.PeerCountMin)
	assert.Equal(t, 16, c.Federation.PeerCountMax)
	assert.Equal(t, 3600, c.Federation.GossipDedupeTTLS)
	assert.Equal(t, 24, c.Federation.TransferRetryWindowH)
	assert.Equal(t, 1, c.Federation.EnrollRatePerMinPerIP)

	// Reputation
	assert.Equal(t, 60, c.Reputation.EvaluationIntervalS)
	assert.Equal(t, 3600, c.Reputation.SignalWindows.ShortS)
	assert.Equal(t, 86400, c.Reputation.SignalWindows.MediumS)
	assert.Equal(t, 604800, c.Reputation.SignalWindows.LongS)
	assert.InDelta(t, 2.5, c.Reputation.ZScoreThreshold, 1e-9)
	assert.InDelta(t, 0.5, c.Reputation.DefaultScore, 1e-9)
	assert.Equal(t, 600, c.Reputation.FreezeListCacheTTLS)

	// Admission (admission spec §9.3)
	assert.InDelta(t, 0.85, c.Admission.PressureAdmitThreshold, 1e-9)
	assert.InDelta(t, 1.5, c.Admission.PressureRejectThreshold, 1e-9)
	assert.Equal(t, 512, c.Admission.QueueCap)
	assert.InDelta(t, 0.4, c.Admission.TrialTierScore, 1e-9)
	assert.InDelta(t, 0.05, c.Admission.AgingAlphaPerMinute, 1e-9)
	assert.Equal(t, 300, c.Admission.QueueTimeoutS)
	assert.InDelta(t, 0.30, c.Admission.ScoreWeights.SettlementReliability, 1e-9)
	assert.InDelta(t, 0.10, c.Admission.ScoreWeights.InverseDisputeRate, 1e-9)
	assert.InDelta(t, 0.20, c.Admission.ScoreWeights.Tenure, 1e-9)
	assert.InDelta(t, 0.30, c.Admission.ScoreWeights.NetCreditFlow, 1e-9)
	assert.InDelta(t, 0.10, c.Admission.ScoreWeights.BalanceCushion, 1e-9)
	assert.Equal(t, 10000, c.Admission.NetFlowNormalizationConstant)
	assert.Equal(t, 30, c.Admission.TenureCapDays)
	assert.Equal(t, 1000, c.Admission.StarterGrantCredits)
	assert.Equal(t, 30, c.Admission.RollingWindowDays)
	assert.Equal(t, 50, c.Admission.TrialSettlementsRequired)
	assert.Equal(t, 72, c.Admission.TrialDurationHours)
	assert.Equal(t, 86400, c.Admission.AttestationTTLSeconds)
	assert.Equal(t, 604800, c.Admission.AttestationMaxTTLSeconds)
	assert.Equal(t, 6, c.Admission.AttestationIssuancePerConsumerPerHour)
	assert.InDelta(t, 0.95, c.Admission.MaxAttestationScoreImported, 1e-9)
	assert.Empty(t, c.Admission.TLogPath, "tlog_path is data_dir-derived; default empty")
	assert.Empty(t, c.Admission.SnapshotPathPrefix)
	assert.Equal(t, 600, c.Admission.SnapshotIntervalS)
	assert.Equal(t, 3, c.Admission.SnapshotsRetained)
	assert.Equal(t, 5, c.Admission.FsyncBatchWindowMs)
	assert.Equal(t, 10, c.Admission.HeartbeatWindowMinutes)
	assert.Equal(t, 300, c.Admission.HeartbeatFreshnessDecayMaxS)
	assert.Empty(t, c.Admission.AttestationPeerBlocklist)
}
