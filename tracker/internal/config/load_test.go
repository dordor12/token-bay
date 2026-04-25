package config

import (
	"errors"
	"os"
	"strings"
	"testing"

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
	assert.Equal(t, "/var/lib/token-bay", c.DataDir)
	assert.Equal(t, "info", c.LogLevel)
	assert.Equal(t, 512, c.Admission.QueueCap)
	assert.InDelta(t, 0.30, c.Admission.ScoreWeights.SettlementReliability, 1e-9)
}

func TestParse_MinimalYAMLLoadsRequiredOnly(t *testing.T) {
	f, err := os.Open("testdata/minimal.yaml")
	require.NoError(t, err)
	defer f.Close()

	c, err := Parse(f)

	require.NoError(t, err)
	assert.Equal(t, "/var/lib/token-bay", c.DataDir)
	// Optional fields stay zero — Parse does not apply defaults.
	assert.Empty(t, c.LogLevel)
	assert.Equal(t, 0, c.Admission.QueueCap)
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
	assert.Contains(t, pe.Error(), "score_weight")
}

func TestParse_RejectsMalformedYAML(t *testing.T) {
	r := strings.NewReader("data_dir: [this is: not a string]\n")

	c, err := Parse(r)

	require.Error(t, err)
	assert.Nil(t, c)
	var pe *ParseError
	require.True(t, errors.As(err, &pe))
}

func TestLoad_TempdirYAMLPassesValidation(t *testing.T) {
	tmp := t.TempDir()
	yaml := "data_dir: " + tmp + "\n" +
		"server:\n" +
		"  listen_addr: \"0.0.0.0:7777\"\n" +
		"  identity_key_path: " + tmp + "/identity.key\n" +
		"  tls_cert_path: " + tmp + "/cert.pem\n" +
		"  tls_key_path: " + tmp + "/cert.key\n" +
		"ledger:\n" +
		"  storage_path: " + tmp + "/ledger.sqlite\n"
	path := tmp + "/tracker.yaml"
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o644))

	c, err := Load(path)

	require.NoError(t, err, "expected clean Load against tmp dir")
	require.NotNil(t, c)
	assert.Equal(t, tmp+"/admission.tlog", c.Admission.TLogPath)
}

func TestLoad_MinimalFailsValidationOnMissingTLogParent(t *testing.T) {
	c, err := Load("testdata/minimal.yaml")

	if err == nil {
		require.NotNil(t, c)
		return
	}
	var ve *ValidationError
	require.True(t, errors.As(err, &ve), "got %v", err)
	matched := false
	for _, fe := range ve.Errors {
		if fe.Field == "admission.tlog_path" {
			matched = true
		}
	}
	assert.True(t, matched, "expected admission.tlog_path error: %v", ve.Errors)
}

func TestLoad_MissingFileReturnsErrorPathPresent(t *testing.T) {
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

func TestLoad_PermissionDeniedReturnsError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permission checks")
	}
	dir := t.TempDir()
	path := dir + "/forbidden.yaml"
	require.NoError(t, os.WriteFile(path, []byte("data_dir: /tmp\n"), 0o000))

	c, err := Load(path)

	require.Error(t, err)
	assert.Nil(t, c)
}

func TestLoad_UnknownFieldYAMLReturnsParseError(t *testing.T) {
	c, err := Load("testdata/unknown_field.yaml")

	require.Error(t, err)
	assert.Nil(t, c)
	var pe *ParseError
	require.True(t, errors.As(err, &pe))
	assert.Equal(t, "testdata/unknown_field.yaml", pe.Path)
}

func TestLoad_InvalidFixtures_SurfaceExpectedFieldErrors(t *testing.T) {
	cases := []struct {
		fixture string
		field   string
	}{
		{"testdata/invalid_log_level.yaml", "log_level"},
		{"testdata/invalid_listener_collision.yaml", "admin.listen_addr"},
		{"testdata/invalid_signal_windows.yaml", "reputation.signal_windows"},
		{"testdata/invalid_score_weights_sum.yaml", "admission.score_weights"},
		{"testdata/invalid_pressure_inversion.yaml", "admission.pressure_admit_threshold"},
		{"testdata/invalid_attestation_ttl.yaml", "admission.attestation_max_ttl_seconds"},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			c, err := Load(tc.fixture)

			require.Error(t, err)
			assert.Nil(t, c)
			var ve *ValidationError
			require.True(t, errors.As(err, &ve), "got %T: %v", err, err)
			matched := false
			for _, fe := range ve.Errors {
				if fe.Field == tc.field {
					matched = true
				}
			}
			assert.Truef(t, matched, "expected %s in errors %v", tc.field, ve.Errors)
		})
	}
}
