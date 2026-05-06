package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse_FullYAMLRoundTrip(t *testing.T) {
	f, err := os.Open("testdata/full.yaml")
	require.NoError(t, err)
	defer f.Close()

	c, err := Parse(f)

	require.NoError(t, err)
	require.NotNil(t, c)
	assert.Equal(t, "both", c.Role)
	assert.Equal(t, "https://eu-central-1.bootstrap.token-bay.dev", c.Tracker)
	assert.Equal(t, "/home/alice/.token-bay/identity.key", c.IdentityKeyPath)
	assert.Equal(t, "/home/alice/.token-bay/audit.log", c.AuditLogPath)
	assert.Equal(t, "/usr/local/bin/claude", c.CCBridge.ClaudeBin)
	assert.Equal(t, []string{"--debug"}, c.CCBridge.ExtraFlags)
	assert.True(t, c.CCBridge.Sandbox.Enabled)
	assert.Equal(t, "bubblewrap", c.CCBridge.Sandbox.Driver)
	assert.Equal(t, 30*time.Minute, c.Consumer.NetworkModeTTL.AsDuration())
	assert.Equal(t, 20*time.Minute, c.Seeder.HeadroomWindow.AsDuration())
	assert.Equal(t, "scheduled", c.IdlePolicy.Mode)
	assert.Equal(t, "02:00-06:00", c.IdlePolicy.Window)
	assert.Equal(t, 5*time.Minute, c.IdlePolicy.ActivityGrace.AsDuration())
	assert.Equal(t, "tee_preferred", c.PrivacyTier)
	assert.Equal(t, int64(1000), c.MaxSpendPerHour)
}

func TestParse_MinimalYAMLLoadsRequiredOnly(t *testing.T) {
	f, err := os.Open("testdata/minimal.yaml")
	require.NoError(t, err)
	defer f.Close()

	c, err := Parse(f)

	require.NoError(t, err)
	assert.Equal(t, "both", c.Role)
	assert.Equal(t, "auto", c.Tracker)
	// Optional fields stay zero — Parse does not apply defaults.
	assert.Empty(t, c.CCBridge.ClaudeBin)
	assert.Equal(t, time.Duration(0), c.Consumer.NetworkModeTTL.AsDuration())
}

func TestParse_RejectsUnknownField(t *testing.T) {
	f, err := os.Open("testdata/unknown_field.yaml")
	require.NoError(t, err)
	defer f.Close()

	c, err := Parse(f)

	require.Error(t, err)
	assert.Nil(t, c)
	var pe *ParseError
	require.True(t, errors.As(err, &pe))
	assert.Contains(t, pe.Error(), "anthropic_token")
}

func TestParse_RejectsMalformedYAML(t *testing.T) {
	r := strings.NewReader("role: [this is: not a string]\n")

	c, err := Parse(r)

	require.Error(t, err)
	assert.Nil(t, c)
	var pe *ParseError
	require.True(t, errors.As(err, &pe))
}

func TestLoad_MissingFileReturnsErrorPathAbsent(t *testing.T) {
	c, err := Load("testdata/this_file_does_not_exist.yaml")

	require.Error(t, err)
	assert.Nil(t, c)
	assert.True(t, os.IsNotExist(err) || strings.Contains(err.Error(), "no such file"))
}

func TestLoad_DirectoryReturnsError(t *testing.T) {
	dir := t.TempDir()

	c, err := Load(dir)

	require.Error(t, err)
	assert.Nil(t, c)
}

func TestLoad_UnknownFieldYAMLAttachesPath(t *testing.T) {
	c, err := Load("testdata/unknown_field.yaml")

	require.Error(t, err)
	assert.Nil(t, c)
	var pe *ParseError
	require.True(t, errors.As(err, &pe))
	assert.Equal(t, "testdata/unknown_field.yaml", pe.Path)
}

func TestLoad_MinimalAppliesDefaultsAndPasses(t *testing.T) {
	tmp := t.TempDir()
	yaml := "role: both\ntracker: auto\n" +
		"identity_key_path: " + filepath.Join(tmp, "identity.key") + "\n" +
		"audit_log_path: " + filepath.Join(tmp, "audit.log") + "\n"
	path := filepath.Join(tmp, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o644))

	c, err := Load(path)

	require.NoError(t, err, "expected clean Load")
	require.NotNil(t, c)
	assert.Equal(t, "claude", c.CCBridge.ClaudeBin, "default applied")
	assert.Equal(t, 15*time.Minute, c.Consumer.NetworkModeTTL.AsDuration())
}

func TestLoad_PermissionDeniedReturnsError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permission checks")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "forbidden.yaml")
	require.NoError(t, os.WriteFile(path, []byte("role: both\n"), 0o000))

	c, err := Load(path)

	require.Error(t, err)
	assert.Nil(t, c)
}
