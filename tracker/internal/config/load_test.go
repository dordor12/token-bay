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

func TestLoad_FullYAMLLoadsAndAppliesDefaultsAndValidates(t *testing.T) {
	// At this point in the plan, ApplyDefaults and Validate are not yet
	// wired into Load — Task 12 will complete the wiring. For now we only
	// assert the file-open + Parse path. The test file will be extended
	// as the wiring lands; this initial form is intentionally loose.
	c, err := Load("testdata/full.yaml")

	require.NoError(t, err)
	require.NotNil(t, c)
	assert.Equal(t, "/var/lib/token-bay", c.DataDir)
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
