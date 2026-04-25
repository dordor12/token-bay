package exhaustionproof

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

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
