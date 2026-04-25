package proto

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestSignedBalanceSnapshot_RoundTrip(t *testing.T) {
	original := &SignedBalanceSnapshot{
		Body: &BalanceSnapshotBody{
			IdentityId:   make32(0xAA),
			Credits:      12345,
			ChainTipHash: make32(0xBB),
			ChainTipSeq:  9876,
			IssuedAt:     1714000000,
			ExpiresAt:    1714000600,
		},
		TrackerSig: make64(0xCC),
	}

	first, err := proto.MarshalOptions{Deterministic: true}.Marshal(original)
	require.NoError(t, err)

	var parsed SignedBalanceSnapshot
	require.NoError(t, proto.Unmarshal(first, &parsed))

	second, err := proto.MarshalOptions{Deterministic: true}.Marshal(&parsed)
	require.NoError(t, err)

	assert.Equal(t, first, second, "Deterministic marshal must be byte-stable")
	require.True(t, proto.Equal(original, &parsed), "unmarshal must reproduce original exactly")
}

// Test helpers — generate fixed-pattern byte slices for fixture data.
// Used by balance_test.go and (later) envelope_test.go in the same package.
func make16(b byte) []byte { return bytesN(b, 16) }
func make32(b byte) []byte { return bytesN(b, 32) }
func make64(b byte) []byte { return bytesN(b, 64) }
func bytesN(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}
