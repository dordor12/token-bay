package config

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyDefaults_EmptyConfigGetsAllDefaults(t *testing.T) {
	c := &Config{}

	ApplyDefaults(c)

	expected := DefaultConfig()
	expected.Admission.TLogPath = ""           // data_dir empty → no expansion
	expected.Admission.SnapshotPathPrefix = "" // ditto
	assert.Equal(t, expected, c)
}

func TestApplyDefaults_DataDirSubstitutesAdmissionPaths(t *testing.T) {
	c := &Config{DataDir: "/var/lib/token-bay"}

	ApplyDefaults(c)

	assert.Equal(t, "/var/lib/token-bay/admission.tlog", c.Admission.TLogPath)
	assert.Equal(t, "/var/lib/token-bay/admission.snapshot", c.Admission.SnapshotPathPrefix)
}

func TestApplyDefaults_ExplicitValuesPreserved(t *testing.T) {
	c := &Config{
		LogLevel: "debug",
		Broker: BrokerConfig{
			ScoreWeights: BrokerScoreWeights{
				Reputation: 0.5, Headroom: 0.2, RTT: 0.2, Load: 0.1,
			},
			MaxOfferAttempts: 7,
		},
		Admission: AdmissionConfig{
			TLogPath: "/custom/admission.tlog",
		},
		DataDir: "/var/lib/token-bay",
	}

	ApplyDefaults(c)

	assert.Equal(t, "debug", c.LogLevel)
	assert.InDelta(t, 0.5, c.Broker.ScoreWeights.Reputation, 1e-9)
	assert.Equal(t, 7, c.Broker.MaxOfferAttempts)
	assert.Equal(t, "/custom/admission.tlog", c.Admission.TLogPath, "explicit value not overwritten")
	assert.Equal(t, "/var/lib/token-bay/admission.snapshot", c.Admission.SnapshotPathPrefix, "snapshot still expanded")
}

func TestApplyDefaults_PartialBrokerWeightsAllZeroFilledFromDefault(t *testing.T) {
	// If the entire score_weights block is zero, treat as "operator did
	// not supply weights" and use defaults.
	c := &Config{}

	ApplyDefaults(c)

	d := DefaultConfig()
	assert.Equal(t, d.Broker.ScoreWeights, c.Broker.ScoreWeights)
}

func TestApplyDefaults_Idempotent(t *testing.T) {
	cases := map[string]*Config{
		"empty":    {},
		"defaults": DefaultConfig(),
		"partial":  {DataDir: "/var/lib/token-bay", LogLevel: "warn"},
	}
	for name, base := range cases {
		t.Run(name, func(t *testing.T) {
			a := cloneConfig(base)
			b := cloneConfig(base)

			ApplyDefaults(a)
			ApplyDefaults(b)
			ApplyDefaults(b)

			assert.True(t, reflect.DeepEqual(a, b), "ApplyDefaults must be idempotent")
		})
	}
}

// cloneConfig is a deep copy via gob-free pointer chasing — the only
// pointer-typed fields are slices, which we copy explicitly.
func cloneConfig(c *Config) *Config {
	cp := *c
	if c.Admission.AttestationPeerBlocklist != nil {
		cp.Admission.AttestationPeerBlocklist = append([]string(nil), c.Admission.AttestationPeerBlocklist...)
	}
	if c.Federation.Peers != nil {
		cp.Federation.Peers = append([]FederationPeer(nil), c.Federation.Peers...)
	}
	return &cp
}

// Sanity: DefaultConfig has no slice values, so after one ApplyDefaults call
// against an empty config the slice fields stay nil (not empty slices).
func TestApplyDefaults_ServerNewFieldsGetDefaults(t *testing.T) {
	c := &Config{}

	ApplyDefaults(c)

	assert.Equal(t, 1<<20, c.Server.MaxFrameSize)
	assert.Equal(t, 60, c.Server.IdleTimeoutS)
	assert.Equal(t, 1024, c.Server.MaxIncomingStreams)
	assert.Equal(t, 30, c.Server.ShutdownGraceS)
}

func TestApplyDefaults_ServerExplicitValuesPreserved(t *testing.T) {
	c := &Config{Server: ServerConfig{
		MaxFrameSize:       2 << 20,
		IdleTimeoutS:       120,
		MaxIncomingStreams: 4096,
		ShutdownGraceS:     90,
	}}

	ApplyDefaults(c)

	assert.Equal(t, 2<<20, c.Server.MaxFrameSize)
	assert.Equal(t, 120, c.Server.IdleTimeoutS)
	assert.Equal(t, 4096, c.Server.MaxIncomingStreams)
	assert.Equal(t, 90, c.Server.ShutdownGraceS)
}

func TestApplyDefaults_BlocklistStaysNilWhenNotSet(t *testing.T) {
	c := &Config{}

	ApplyDefaults(c)

	require.Nil(t, c.Admission.AttestationPeerBlocklist)
}

func TestApplyDefaults_ReputationMinPopulationAndStoragePath(t *testing.T) {
	c := &Config{DataDir: "/var/lib/token-bay"}
	ApplyDefaults(c)
	if c.Reputation.MinPopulationForZScore != 100 {
		t.Errorf("MinPopulationForZScore = %d, want 100", c.Reputation.MinPopulationForZScore)
	}
	if c.Reputation.StoragePath != "/var/lib/token-bay/reputation.sqlite" {
		t.Errorf("StoragePath = %q, want %q",
			c.Reputation.StoragePath, "/var/lib/token-bay/reputation.sqlite")
	}
}

func TestApplyDefaults_FederationNewFieldsGetDefaults(t *testing.T) {
	c := &Config{}

	ApplyDefaults(c)

	assert.Equal(t, 5, c.Federation.HandshakeTimeoutS)
	assert.Equal(t, 100, c.Federation.GossipRateQPS)
	assert.Equal(t, 256, c.Federation.SendQueueDepth)
	assert.Equal(t, 3600, c.Federation.PublishCadenceS)
	assert.Equal(t, "", c.Federation.ListenAddr, "listen_addr default must be empty (operator opts in)")
	assert.Equal(t, 60, c.Federation.IdleTimeoutS)
	assert.Equal(t, 1, c.Federation.RedialBaseS)
	assert.Equal(t, 30, c.Federation.RedialMaxS)
	require.Nil(t, c.Federation.Peers, "peers default must be nil — operator-managed")
}

func TestApplyDefaults_FederationExplicitValuesPreserved(t *testing.T) {
	c := &Config{Federation: FederationConfig{
		HandshakeTimeoutS: 10,
		GossipRateQPS:     500,
		SendQueueDepth:    512,
		PublishCadenceS:   7200,
		ListenAddr:        ":9000",
		IdleTimeoutS:      120,
		RedialBaseS:       2,
		RedialMaxS:        45,
	}}

	ApplyDefaults(c)

	assert.Equal(t, 10, c.Federation.HandshakeTimeoutS)
	assert.Equal(t, 500, c.Federation.GossipRateQPS)
	assert.Equal(t, 512, c.Federation.SendQueueDepth)
	assert.Equal(t, 7200, c.Federation.PublishCadenceS)
	assert.Equal(t, ":9000", c.Federation.ListenAddr)
	assert.Equal(t, 120, c.Federation.IdleTimeoutS)
	assert.Equal(t, 2, c.Federation.RedialBaseS)
	assert.Equal(t, 45, c.Federation.RedialMaxS)
}
