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
