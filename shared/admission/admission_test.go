package admission

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// fixtureCreditAttestationBody returns a fully-populated body with
// deterministic field values — used by round-trip, validator,
// sign/verify, and golden-fixture tests across the shared/ packages.
func fixtureCreditAttestationBody() *CreditAttestationBody {
	return &CreditAttestationBody{
		IdentityId:            make32(0x11),
		IssuerTrackerId:       make32(0x22),
		Score:                 8500,
		TenureDays:            120,
		SettlementReliability: 9700,
		DisputeRate:           50,
		NetCreditFlow_30D:     250000,
		BalanceCushionLog2:    3,
		ComputedAt:            1714000000,
		ExpiresAt:             1714086400,
	}
}

func make32(b byte) []byte { return repeat(b, 32) }
func make64(b byte) []byte { return repeat(b, 64) }
func repeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

func TestCreditAttestationBody_RoundTrip(t *testing.T) {
	original := fixtureCreditAttestationBody()

	first, err := proto.MarshalOptions{Deterministic: true}.Marshal(original)
	require.NoError(t, err)

	var parsed CreditAttestationBody
	require.NoError(t, proto.Unmarshal(first, &parsed))

	second, err := proto.MarshalOptions{Deterministic: true}.Marshal(&parsed)
	require.NoError(t, err)

	assert.Equal(t, first, second, "Deterministic marshal must be byte-stable")
	require.True(t, proto.Equal(original, &parsed), "unmarshal must reproduce original exactly")
}

func TestSignedCreditAttestation_RoundTrip(t *testing.T) {
	signed := &SignedCreditAttestation{
		Body:       fixtureCreditAttestationBody(),
		TrackerSig: make64(0x77),
	}

	first, err := proto.MarshalOptions{Deterministic: true}.Marshal(signed)
	require.NoError(t, err)

	var parsed SignedCreditAttestation
	require.NoError(t, proto.Unmarshal(first, &parsed))

	second, err := proto.MarshalOptions{Deterministic: true}.Marshal(&parsed)
	require.NoError(t, err)

	assert.Equal(t, first, second)
	require.True(t, proto.Equal(signed, &parsed), "unmarshal must reproduce original exactly")
}

func TestFetchHeadroomRequest_RoundTrip(t *testing.T) {
	original := &FetchHeadroomRequest{RequestNonce: 42, ModelFilter: "claude-sonnet-4-6"}

	first, err := proto.MarshalOptions{Deterministic: true}.Marshal(original)
	require.NoError(t, err)

	var parsed FetchHeadroomRequest
	require.NoError(t, proto.Unmarshal(first, &parsed))

	second, err := proto.MarshalOptions{Deterministic: true}.Marshal(&parsed)
	require.NoError(t, err)

	assert.Equal(t, first, second)
	require.True(t, proto.Equal(original, &parsed))
}

func TestFetchHeadroomResponse_RoundTrip(t *testing.T) {
	original := &FetchHeadroomResponse{
		RequestNonce:     42,
		HeadroomEstimate: 7200,
		Source:           TickSource_TICK_SOURCE_USAGE_PROBE,
		ProbeAgeS:        15,
		CanProbeUsage:    true,
		PerModel: []*PerModelHeadroom{
			{Model: "claude-sonnet-4-6", HeadroomEstimate: 7000},
			{Model: "claude-opus-4-7", HeadroomEstimate: 8500},
		},
	}

	first, err := proto.MarshalOptions{Deterministic: true}.Marshal(original)
	require.NoError(t, err)

	var parsed FetchHeadroomResponse
	require.NoError(t, proto.Unmarshal(first, &parsed))

	second, err := proto.MarshalOptions{Deterministic: true}.Marshal(&parsed)
	require.NoError(t, err)

	assert.Equal(t, first, second)
	require.True(t, proto.Equal(original, &parsed))
}

// TestTickSource_AllValuesRoundTrip pins every enum value the schema defines
// (admission-design §6.3 #1).
func TestTickSource_AllValuesRoundTrip(t *testing.T) {
	cases := []TickSource{
		TickSource_TICK_SOURCE_UNSPECIFIED,
		TickSource_TICK_SOURCE_HEURISTIC,
		TickSource_TICK_SOURCE_USAGE_PROBE,
	}
	for _, src := range cases {
		t.Run(src.String(), func(t *testing.T) {
			original := &FetchHeadroomResponse{Source: src, RequestNonce: 1}
			buf, err := proto.MarshalOptions{Deterministic: true}.Marshal(original)
			require.NoError(t, err)
			var parsed FetchHeadroomResponse
			require.NoError(t, proto.Unmarshal(buf, &parsed))
			assert.Equal(t, src, parsed.Source)
		})
	}
}
