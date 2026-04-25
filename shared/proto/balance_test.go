package proto

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestBalanceSnapshot_Getters calls every generated getter on BalanceSnapshotBody
// and SignedBalanceSnapshot — both nil and non-nil receivers — to reach the §7.3
// ≥90% coverage target. Generated getters are not called by round-trip tests.
func TestBalanceSnapshot_Getters(t *testing.T) {
	body := &BalanceSnapshotBody{
		IdentityId:  make32(0xAA),
		Credits:     500,
		ChainTipHash: make32(0xBB),
		ChainTipSeq: 7,
		IssuedAt:    1714000000,
		ExpiresAt:   1714000600,
	}
	signed := &SignedBalanceSnapshot{Body: body, TrackerSig: make64(0xCC)}

	assert.Equal(t, make32(0xAA), body.GetIdentityId())
	assert.Equal(t, int64(500), body.GetCredits())
	assert.Equal(t, make32(0xBB), body.GetChainTipHash())
	assert.Equal(t, uint64(7), body.GetChainTipSeq())
	assert.Equal(t, uint64(1714000000), body.GetIssuedAt())
	assert.Equal(t, uint64(1714000600), body.GetExpiresAt())

	assert.Equal(t, body, signed.GetBody())
	assert.Equal(t, make64(0xCC), signed.GetTrackerSig())

	// Nil-receiver paths return zero values without panic.
	var nilBody *BalanceSnapshotBody
	assert.Nil(t, nilBody.GetIdentityId())
	assert.Equal(t, int64(0), nilBody.GetCredits())
	assert.Nil(t, nilBody.GetChainTipHash())
	assert.Equal(t, uint64(0), nilBody.GetChainTipSeq())
	assert.Equal(t, uint64(0), nilBody.GetIssuedAt())
	assert.Equal(t, uint64(0), nilBody.GetExpiresAt())

	var nilSigned *SignedBalanceSnapshot
	assert.Nil(t, nilSigned.GetBody())
	assert.Nil(t, nilSigned.GetTrackerSig())

	// String, Descriptor, and Reset for coverage.
	_ = body.String()
	_ = signed.String()
	_, _ = body.Descriptor()
	_, _ = signed.Descriptor()
	b2 := *body
	b2.Reset()
	s2 := *signed
	s2.Reset()
}

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
func make32(b byte) []byte { return bytesN(b, 32) }
func make64(b byte) []byte { return bytesN(b, 64) }
func bytesN(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}
