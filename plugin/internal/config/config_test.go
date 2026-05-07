package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestDefaultConfig_LeavesRequiredFieldsZero(t *testing.T) {
	c := DefaultConfig()
	assert.Empty(t, c.Role, "role is required — must be operator-provided")
	assert.Empty(t, c.Tracker, "tracker is required — must be operator-provided")
}

func TestDefaultConfig_PopulatesOptionalFields(t *testing.T) {
	c := DefaultConfig()
	assert.Equal(t, "~/.token-bay/identity.key", c.IdentityKeyPath)
	assert.Equal(t, "~/.token-bay/audit.log", c.AuditLogPath)
	assert.Equal(t, "claude", c.CCBridge.ClaudeBin)
	assert.False(t, c.CCBridge.Sandbox.Enabled)
	assert.Equal(t, "bubblewrap", c.CCBridge.Sandbox.Driver)
	assert.Equal(t, 15*time.Minute, c.Consumer.NetworkModeTTL.AsDuration())
	assert.Equal(t, 15*time.Minute, c.Seeder.HeadroomWindow.AsDuration())
	assert.Equal(t, "scheduled", c.IdlePolicy.Mode)
	assert.Equal(t, "02:00-06:00", c.IdlePolicy.Window)
	assert.Equal(t, 10*time.Minute, c.IdlePolicy.ActivityGrace.AsDuration())
	assert.Equal(t, "standard", c.PrivacyTier)
	assert.Equal(t, int64(500), c.MaxSpendPerHour)
}

func TestDuration_UnmarshalYAML_ParsesHumanShape(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"15m", 15 * time.Minute},
		{"2h", 2 * time.Hour},
		{"500ms", 500 * time.Millisecond},
		{"1h30m", 90 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			var d Duration
			require.NoError(t, yaml.Unmarshal([]byte(tc.in), &d))
			assert.Equal(t, tc.want, d.AsDuration())
		})
	}
}

func TestDuration_UnmarshalYAML_RejectsBadInput(t *testing.T) {
	cases := []string{
		"twelve minutes",
		"15", // no unit
		"-",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			var d Duration
			err := yaml.Unmarshal([]byte(in), &d)
			require.Error(t, err)
		})
	}
}

func TestDuration_MarshalYAML_RoundTrips(t *testing.T) {
	in := Duration(2*time.Hour + 30*time.Minute)
	out, err := yaml.Marshal(in)
	require.NoError(t, err)

	var back Duration
	require.NoError(t, yaml.Unmarshal(out, &back))
	assert.Equal(t, in.AsDuration(), back.AsDuration())
}
