package exhaustionproofbuilder

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/shared/exhaustionproof"
)

// (errors imported for errors.Is in TestBuilder_Build_RejectsInvalidInput etc.;
//  io for io.ErrUnexpectedEOF in errRand; proto for the determinism test.)

// fixedNow returns a fixed time.Time for deterministic captured_at.
var fixedNow = func() time.Time {
	return time.Unix(1714000010, 0).UTC()
}

// zeroRand fills the buffer with 0x00 bytes — a deterministic stand-in for
// crypto/rand.Read in tests.
func zeroRand(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// errRand always returns an io error.
func errRand(_ []byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}

// validInput returns a fully-populated ProofInput that should pass all rules.
func validInput() ProofInput {
	return ProofInput{
		StopFailureMatcher:    "rate_limit",
		StopFailureAt:         time.Unix(1714000000, 0).UTC(),
		StopFailureErrorShape: []byte(`{"type":"rate_limit_error"}`),
		UsageProbeAt:          time.Unix(1714000005, 0).UTC(),
		UsageProbeOutput:      []byte(`Current session: 99% used`),
	}
}

func newTestBuilder() *Builder {
	b := NewBuilder()
	b.Now = fixedNow
	b.RandRead = zeroRand
	return b
}

func TestBuilder_Build_HappyPath(t *testing.T) {
	b := newTestBuilder()

	proof, err := b.Build(validInput())
	require.NoError(t, err)
	require.NotNil(t, proof)

	assert.Equal(t, "rate_limit", proof.StopFailure.Matcher)
	assert.Equal(t, uint64(1714000000), proof.StopFailure.At)
	assert.Equal(t, []byte(`{"type":"rate_limit_error"}`), proof.StopFailure.ErrorShape)
	assert.Equal(t, uint64(1714000005), proof.UsageProbe.At)
	assert.Equal(t, []byte(`Current session: 99% used`), proof.UsageProbe.Output)
	assert.Equal(t, uint64(1714000010), proof.CapturedAt)
	assert.Len(t, proof.Nonce, 16)
	assert.Equal(t, make([]byte, 16), proof.Nonce, "zeroRand should fill 16 zero bytes")

	// Independent re-validation: builder claims valid → ValidateProofV1 agrees.
	require.NoError(t, exhaustionproof.ValidateProofV1(proof))
}

func TestBuilder_Build_RejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(*ProofInput)
		wantField string
	}{
		{"empty matcher", func(in *ProofInput) { in.StopFailureMatcher = "" }, "StopFailureMatcher"},
		{"zero stop_failure.at", func(in *ProofInput) { in.StopFailureAt = time.Time{} }, "StopFailureAt"},
		{"zero usage_probe.at", func(in *ProofInput) { in.UsageProbeAt = time.Time{} }, "UsageProbeAt"},
		{"empty error_shape", func(in *ProofInput) { in.StopFailureErrorShape = nil }, "StopFailureErrorShape"},
		{"empty usage_probe.output", func(in *ProofInput) { in.UsageProbeOutput = nil }, "UsageProbeOutput"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validInput()
			tc.mutate(&in)
			b := newTestBuilder()
			proof, err := b.Build(in)
			assert.Nil(t, proof)
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrInvalidInput),
				"expected ErrInvalidInput, got: %v", err)
			assert.Contains(t, err.Error(), tc.wantField,
				"error should name the offending field: %v", err)
		})
	}
}

func TestBuilder_Build_FreshnessWindowExceeded(t *testing.T) {
	in := validInput()
	// 61s gap — outside the 60s window enforced by ValidateProofV1.
	in.UsageProbeAt = in.StopFailureAt.Add(61 * time.Second)
	b := newTestBuilder()
	proof, err := b.Build(in)
	assert.Nil(t, proof)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrValidation),
		"expected ErrValidation, got: %v", err)
	assert.Contains(t, err.Error(), "freshness")
}

func TestBuilder_Build_RandReadFailure(t *testing.T) {
	b := NewBuilder()
	b.Now = fixedNow
	b.RandRead = errRand
	proof, err := b.Build(validInput())
	assert.Nil(t, proof)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrRandFailed),
		"expected ErrRandFailed, got: %v", err)
}

func TestBuilder_Build_Determinism(t *testing.T) {
	b := newTestBuilder()
	a, err := b.Build(validInput())
	require.NoError(t, err)
	c, err := b.Build(validInput())
	require.NoError(t, err)

	first, err := proto.MarshalOptions{Deterministic: true}.Marshal(a)
	require.NoError(t, err)
	second, err := proto.MarshalOptions{Deterministic: true}.Marshal(c)
	require.NoError(t, err)
	assert.Equal(t, first, second,
		"same inputs + same fixed Now/RandRead must produce byte-identical marshal")
}

func TestNewBuilder_Defaults(t *testing.T) {
	b := NewBuilder()
	require.NotNil(t, b)
	assert.NotNil(t, b.Now)
	assert.NotNil(t, b.RandRead)
}
