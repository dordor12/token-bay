package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyDefaults_FillsZeroValuedOptionalFields(t *testing.T) {
	c := &Config{Role: "both", Tracker: "auto"}

	ApplyDefaults(c)

	assert.Equal(t, "claude", c.CCBridge.ClaudeBin)
	assert.Equal(t, "scheduled", c.IdlePolicy.Mode)
	assert.Equal(t, "02:00-06:00", c.IdlePolicy.Window)
	assert.Equal(t, 15*time.Minute, c.Consumer.NetworkModeTTL.AsDuration())
	assert.Equal(t, 15*time.Minute, c.Seeder.HeadroomWindow.AsDuration())
	assert.Equal(t, 10*time.Minute, c.IdlePolicy.ActivityGrace.AsDuration())
	assert.Equal(t, "standard", c.PrivacyTier)
	assert.Equal(t, int64(500), c.MaxSpendPerHour)
}

func TestApplyDefaults_PreservesOperatorValues(t *testing.T) {
	c := &Config{
		Role:    "consumer",
		Tracker: "https://eu.example.com",
		CCBridge: CCBridgeConfig{
			ClaudeBin:  "/opt/claude",
			ExtraFlags: []string{"--debug"},
			Sandbox:    SandboxConfig{Enabled: true, Driver: "firejail"},
		},
		Consumer:   ConsumerConfig{NetworkModeTTL: Duration(5 * time.Minute)},
		Seeder:     SeederConfig{HeadroomWindow: Duration(45 * time.Minute)},
		IdlePolicy: IdlePolicyConfig{Mode: "always_on"},
	}

	ApplyDefaults(c)

	assert.Equal(t, "/opt/claude", c.CCBridge.ClaudeBin)
	assert.Equal(t, []string{"--debug"}, c.CCBridge.ExtraFlags)
	assert.True(t, c.CCBridge.Sandbox.Enabled)
	assert.Equal(t, "firejail", c.CCBridge.Sandbox.Driver)
	assert.Equal(t, 5*time.Minute, c.Consumer.NetworkModeTTL.AsDuration())
	assert.Equal(t, 45*time.Minute, c.Seeder.HeadroomWindow.AsDuration())
	assert.Equal(t, "always_on", c.IdlePolicy.Mode)
}

func TestApplyDefaults_IsIdempotent(t *testing.T) {
	c := &Config{Role: "both", Tracker: "auto"}
	ApplyDefaults(c)
	first := *c

	ApplyDefaults(c)

	assert.Equal(t, first, *c)
}

func TestApplyDefaults_ExpandsTildeInPaths(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	c := &Config{
		Role:            "both",
		Tracker:         "auto",
		IdentityKeyPath: "~/foo/identity.key",
		AuditLogPath:    "~",
	}

	ApplyDefaults(c)

	assert.Equal(t, filepath.Join(home, "foo/identity.key"), c.IdentityKeyPath)
	assert.Equal(t, home, c.AuditLogPath)
}

func TestApplyDefaults_DefaultPathsExpandedToHome(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	c := &Config{Role: "both", Tracker: "auto"}

	ApplyDefaults(c)

	assert.Equal(t, filepath.Join(home, ".token-bay/identity.key"), c.IdentityKeyPath)
	assert.Equal(t, filepath.Join(home, ".token-bay/audit.log"), c.AuditLogPath)
}

func TestApplyDefaults_SandboxDisabledByDefault(t *testing.T) {
	c := &Config{Role: "both", Tracker: "auto"}

	ApplyDefaults(c)

	assert.False(t, c.CCBridge.Sandbox.Enabled,
		"sandbox is opt-in per plugin spec §6.2; flag-based safety is primary")
}
