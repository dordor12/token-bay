package exhaustionproof

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestProofV1_Getters exercises every generated getter on ExhaustionProofV1,
// StopFailure, and UsageProbe to satisfy the §7.3 ≥90% coverage target.
// Generated getters are otherwise unreached by round-trip and validation tests.
func TestProofV1_Getters(t *testing.T) {
	sf := &StopFailure{Matcher: "rate_limit", At: 1714000000, ErrorShape: []byte(`{}`)}
	up := &UsageProbe{At: 1714000010, Output: []byte(`x`)}
	p := &ExhaustionProofV1{StopFailure: sf, UsageProbe: up, CapturedAt: 99, Nonce: []byte("n")}

	assert.Equal(t, sf, p.GetStopFailure())
	assert.Equal(t, up, p.GetUsageProbe())
	assert.Equal(t, uint64(99), p.GetCapturedAt())
	assert.Equal(t, []byte("n"), p.GetNonce())

	assert.Equal(t, "rate_limit", sf.GetMatcher())
	assert.Equal(t, uint64(1714000000), sf.GetAt())
	assert.Equal(t, []byte(`{}`), sf.GetErrorShape())

	assert.Equal(t, uint64(1714000010), up.GetAt())
	assert.Equal(t, []byte(`x`), up.GetOutput())

	// Nil-receiver paths return zero values without panic.
	var nilProof *ExhaustionProofV1
	assert.Nil(t, nilProof.GetStopFailure())
	assert.Nil(t, nilProof.GetUsageProbe())
	assert.Equal(t, uint64(0), nilProof.GetCapturedAt())
	assert.Nil(t, nilProof.GetNonce())

	var nilSF *StopFailure
	assert.Equal(t, "", nilSF.GetMatcher())
	assert.Equal(t, uint64(0), nilSF.GetAt())
	assert.Nil(t, nilSF.GetErrorShape())

	var nilUP *UsageProbe
	assert.Equal(t, uint64(0), nilUP.GetAt())
	assert.Nil(t, nilUP.GetOutput())

	// String, Descriptor, and Reset are generated helpers; call them for coverage.
	_ = p.String()
	_ = sf.String()
	_ = up.String()
	_, _ = p.Descriptor()
	_, _ = sf.Descriptor()
	_, _ = up.Descriptor()
	p2 := *p
	p2.Reset()
	sf2 := *sf
	sf2.Reset()
	up2 := *up
	up2.Reset()
}

func TestProofV1_RoundTrip(t *testing.T) {
	original := &ExhaustionProofV1{
		StopFailure: &StopFailure{
			Matcher:    "rate_limit",
			At:         1714000000,
			ErrorShape: []byte(`{"type":"rate_limit_error"}`),
		},
		UsageProbe: &UsageProbe{
			At:     1714000005,
			Output: []byte(`{"limit":"hit"}`),
		},
		CapturedAt: 1714000010,
		Nonce:      []byte("0123456789abcdef"),
	}

	first, err := proto.MarshalOptions{Deterministic: true}.Marshal(original)
	require.NoError(t, err)

	var parsed ExhaustionProofV1
	require.NoError(t, proto.Unmarshal(first, &parsed))

	second, err := proto.MarshalOptions{Deterministic: true}.Marshal(&parsed)
	require.NoError(t, err)

	assert.Equal(t, first, second, "Deterministic marshal must be byte-stable across round-trip")
	require.True(t, proto.Equal(original, &parsed), "unmarshal must reproduce original exactly")
}
