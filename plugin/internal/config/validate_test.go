package config

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validConfig returns a Config that passes Validate. Tests mutate it
// to trigger one rule at a time. Path fields are populated from
// t.TempDir() so the test runs on Windows (filepath.IsAbs requires a
// drive letter; "/tmp/..." is not absolute on Windows).
func validConfig(t *testing.T) *Config {
	t.Helper()
	tmp := t.TempDir()
	c := &Config{Role: "both", Tracker: "auto"}
	ApplyDefaults(c)
	c.IdentityKeyPath = filepath.Join(tmp, "identity.key")
	c.AuditLogPath = filepath.Join(tmp, "audit.log")
	return c
}

func TestValidate_HappyPath(t *testing.T) {
	require.NoError(t, Validate(validConfig(t)))
}

func TestValidate_MissingRequired(t *testing.T) {
	c := &Config{}
	ApplyDefaults(c)

	err := Validate(c)

	require.Error(t, err)
	fields := errFields(t, err)
	assert.Contains(t, fields, "role")
	assert.Contains(t, fields, "tracker")
}

func TestValidate_AccumulatesEveryFailure(t *testing.T) {
	c := &Config{Role: "wrong", Tracker: "ftp://x"}
	ApplyDefaults(c)
	c.IdentityKeyPath = "/tmp/identity.key"
	c.AuditLogPath = "/tmp/audit.log"

	err := Validate(c)

	require.Error(t, err)
	fields := errFields(t, err)
	// Must accumulate, not short-circuit on first error.
	assert.Contains(t, fields, "role")
	assert.Contains(t, fields, "tracker")
	assert.GreaterOrEqual(t, len(fields), 2)
}

func TestValidate_BadRole(t *testing.T) {
	c := validConfig(t)
	c.Role = "wrong"

	err := Validate(c)

	require.Error(t, err)
	assert.Contains(t, errFields(t, err), "role")
}

func TestValidate_TrackerAutoIsValid(t *testing.T) {
	c := validConfig(t)
	c.Tracker = "auto"
	require.NoError(t, Validate(c))
}

func TestValidate_TrackerHTTPSIsValid(t *testing.T) {
	c := validConfig(t)
	c.Tracker = "https://eu-central-1.bootstrap.token-bay.dev"
	require.NoError(t, Validate(c))
}

func TestValidate_TrackerQUICIsValid(t *testing.T) {
	c := validConfig(t)
	c.Tracker = "quic://eu-central-1.bootstrap.token-bay.dev:7777"
	require.NoError(t, Validate(c))
}

func TestValidate_TrackerWrongScheme(t *testing.T) {
	c := validConfig(t)
	c.Tracker = "ftp://example.com"

	err := Validate(c)

	require.Error(t, err)
	assert.Contains(t, errFields(t, err), "tracker")
}

func TestValidate_TrackerNoHost(t *testing.T) {
	c := validConfig(t)
	c.Tracker = "https://"

	err := Validate(c)

	require.Error(t, err)
	assert.Contains(t, errFields(t, err), "tracker")
}

func TestValidate_NonAbsolutePathRejected(t *testing.T) {
	c := validConfig(t)
	c.AuditLogPath = "relative/path/audit.log"

	err := Validate(c)

	require.Error(t, err)
	assert.Contains(t, errFields(t, err), "audit_log_path")
}

func TestValidate_SandboxEnabledNeedsDriver(t *testing.T) {
	c := validConfig(t)
	c.CCBridge.Sandbox.Enabled = true
	c.CCBridge.Sandbox.Driver = "kvm"

	err := Validate(c)

	require.Error(t, err)
	assert.Contains(t, errFields(t, err), "cc_bridge.sandbox.driver")
}

func TestValidate_SandboxDisabledIgnoresDriver(t *testing.T) {
	c := validConfig(t)
	c.CCBridge.Sandbox.Enabled = false
	c.CCBridge.Sandbox.Driver = "anything-goes"

	require.NoError(t, Validate(c))
}

func TestValidate_NetworkModeTTLPositive(t *testing.T) {
	c := validConfig(t)
	c.Consumer.NetworkModeTTL = 0

	err := Validate(c)

	require.Error(t, err)
	assert.Contains(t, errFields(t, err), "consumer.network_mode_ttl")
}

func TestValidate_HeadroomWindowPositive(t *testing.T) {
	c := validConfig(t)
	c.Seeder.HeadroomWindow = 0

	err := Validate(c)

	require.Error(t, err)
	assert.Contains(t, errFields(t, err), "seeder.headroom_window")
}

func TestValidate_IdleModeUnknown(t *testing.T) {
	c := validConfig(t)
	c.IdlePolicy.Mode = "weekends"

	err := Validate(c)

	require.Error(t, err)
	assert.Contains(t, errFields(t, err), "idle_policy.mode")
}

func TestValidate_AlwaysOnDoesNotRequireWindow(t *testing.T) {
	c := validConfig(t)
	c.IdlePolicy.Mode = "always_on"
	c.IdlePolicy.Window = ""
	require.NoError(t, Validate(c))
}

func TestValidate_ScheduledRequiresWindow(t *testing.T) {
	c := validConfig(t)
	c.IdlePolicy.Mode = "scheduled"
	c.IdlePolicy.Window = ""

	err := Validate(c)

	require.Error(t, err)
	assert.Contains(t, errFields(t, err), "idle_policy.window")
}

func TestValidate_WindowMustMatchPattern(t *testing.T) {
	cases := []string{
		"02:00",        // missing dash and end
		"02:00-",       // missing end
		"2:00-06:00",   // single-digit hour
		"24:00-06:00",  // hour out of range
		"02:60-06:00",  // minute out of range
		"02:00-06:00x", // trailing junk
	}
	for _, w := range cases {
		t.Run(w, func(t *testing.T) {
			c := validConfig(t)
			c.IdlePolicy.Window = w

			err := Validate(c)

			require.Error(t, err, "expected window %q to fail", w)
			assert.Contains(t, errFields(t, err), "idle_policy.window")
		})
	}
}

func TestValidate_ActivityGracePositive(t *testing.T) {
	c := validConfig(t)
	c.IdlePolicy.ActivityGrace = 0

	err := Validate(c)

	require.Error(t, err)
	assert.Contains(t, errFields(t, err), "idle_policy.activity_grace")
}

func TestValidate_PrivacyTierUnknown(t *testing.T) {
	c := validConfig(t)
	c.PrivacyTier = "obfuscated"

	err := Validate(c)

	require.Error(t, err)
	assert.Contains(t, errFields(t, err), "privacy_tier")
}

func TestValidate_NegativeMaxSpend(t *testing.T) {
	c := validConfig(t)
	c.MaxSpendPerHour = -1

	err := Validate(c)

	require.Error(t, err)
	assert.Contains(t, errFields(t, err), "max_spend_per_hour")
}

func errFields(t *testing.T, err error) []string {
	t.Helper()
	var ve *ValidationError
	require.True(t, errors.As(err, &ve), "got %T: %v", err, err)
	out := make([]string, 0, len(ve.Errors))
	for _, fe := range ve.Errors {
		out = append(out, fe.Field)
	}
	return out
}
