package ratelimit

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseUsageProbe_RealFixture_LowUtilization_Headroom(t *testing.T) {
	// The captured fixture (2026-04-23) contains three `% used` tokens
	// after ANSI stripping: 47% (Current session, interleaved form),
	// 4% (Current week, interleaved form), and 47% (re-rendered with
	// explicit space). All well below the 95% threshold — classifier
	// returns Headroom.
	//
	// Note: a naive grep for `\d+% used` on the raw bytes would only
	// find the redrawn 47% (with explicit space), missing the ANSI-
	// interleaved forms. The parser's ANSI-strip pass is what makes
	// the extra matches visible.
	data, err := os.ReadFile("testdata/usage_sample.ansi")
	require.NoError(t, err, "fixture must exist; run TB_CAPTURE_FIXTURE to regenerate")
	assert.Equal(t, UsageHeadroom, ParseUsageProbe(data))
}

func TestParseUsageProbe_RealFixture_ANSIStripRevealsMultipleTokens(t *testing.T) {
	data, err := os.ReadFile("testdata/usage_sample.ansi")
	require.NoError(t, err)
	stripped := ansiEscapeRE.ReplaceAll(data, nil)
	matches := usagePctUsedRE.FindAllSubmatch(stripped, -1)
	assert.GreaterOrEqual(t, len(matches), 2,
		"after ANSI strip, real fixture should expose at least 2 `% used` tokens")
}

func TestParseUsageProbe_Synthetic_AllLow_Headroom(t *testing.T) {
	data := []byte("Current session 10% used / Current week 5% used")
	assert.Equal(t, UsageHeadroom, ParseUsageProbe(data))
}

func TestParseUsageProbe_Synthetic_OneHigh_Exhausted(t *testing.T) {
	data := []byte("Current session 99% used / Current week 5% used")
	assert.Equal(t, UsageExhausted, ParseUsageProbe(data))
}

func TestParseUsageProbe_Synthetic_ExactlyThreshold_Exhausted(t *testing.T) {
	data := []byte("Current session 95% used / Current week 5% used")
	assert.Equal(t, UsageExhausted, ParseUsageProbe(data))
}

func TestParseUsageProbe_Synthetic_JustBelowThreshold_Headroom(t *testing.T) {
	data := []byte("Current session 94% used / Current week 5% used")
	assert.Equal(t, UsageHeadroom, ParseUsageProbe(data))
}

func TestParseUsageProbe_SingleToken_Uncertain(t *testing.T) {
	data := []byte("Current session 50% used")
	assert.Equal(t, UsageUncertain, ParseUsageProbe(data))
}

func TestParseUsageProbe_NoTokens_Uncertain(t *testing.T) {
	data := []byte("Loading usage data... (truncated before render)")
	assert.Equal(t, UsageUncertain, ParseUsageProbe(data))
}

func TestParseUsageProbe_ThreeTokens_OneHigh_Exhausted(t *testing.T) {
	// Matches the shape of a real full render: three windows.
	data := []byte("Session 99% used · Week (all) 40% used · Week (Sonnet) 10% used")
	assert.Equal(t, UsageExhausted, ParseUsageProbe(data))
}

func TestParseUsageProbe_TokensWithIntermediateANSIBytes_StillMatches(t *testing.T) {
	// Simulate the PTY format where ANSI cursor-control escapes can
	// appear between `%` and `used`. The regex allows non-word bytes
	// in that gap.
	data := []byte("Session 30%\x1b[0m used / Week 2%\x1b[0m used")
	assert.Equal(t, UsageHeadroom, ParseUsageProbe(data))
}
