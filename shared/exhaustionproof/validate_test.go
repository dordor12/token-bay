package exhaustionproof

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validProof() *ExhaustionProofV1 {
	return &ExhaustionProofV1{
		StopFailure: &StopFailure{
			Matcher:    "rate_limit",
			At:         1714000000,
			ErrorShape: []byte(`{}`),
		},
		UsageProbe: &UsageProbe{
			At:     1714000010,
			Output: []byte(`limit hit`),
		},
		CapturedAt: 1714000020,
		Nonce:      []byte("0123456789abcdef"),
	}
}

func TestValidateProofV1_HappyPath(t *testing.T) {
	require.NoError(t, ValidateProofV1(validProof()))
}

func TestValidateProofV1_Rejections(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(p *ExhaustionProofV1)
		errFrag string
	}{
		{"nil", func(p *ExhaustionProofV1) { *p = ExhaustionProofV1{} }, "stop_failure"},
		{"missing stop_failure", func(p *ExhaustionProofV1) { p.StopFailure = nil }, "stop_failure"},
		{"missing usage_probe", func(p *ExhaustionProofV1) { p.UsageProbe = nil }, "usage_probe"},
		{"wrong matcher", func(p *ExhaustionProofV1) { p.StopFailure.Matcher = "server_error" }, "matcher"},
		{"empty matcher", func(p *ExhaustionProofV1) { p.StopFailure.Matcher = "" }, "matcher"},
		{"zero stop_failure.at", func(p *ExhaustionProofV1) { p.StopFailure.At = 0 }, "stop_failure.at"},
		{"zero usage_probe.at", func(p *ExhaustionProofV1) { p.UsageProbe.At = 0 }, "usage_probe.at"},
		{"signals 61s apart", func(p *ExhaustionProofV1) { p.UsageProbe.At = p.StopFailure.At + 61 }, "freshness"},
		{"signals 61s apart reversed", func(p *ExhaustionProofV1) { p.StopFailure.At = p.UsageProbe.At + 61 }, "freshness"},
		{"zero captured_at", func(p *ExhaustionProofV1) { p.CapturedAt = 0 }, "captured_at"},
		{"nonce too short", func(p *ExhaustionProofV1) { p.Nonce = []byte("short") }, "nonce"},
		{"nonce too long", func(p *ExhaustionProofV1) { p.Nonce = []byte("01234567890123456789") }, "nonce"},
		{"nonce nil", func(p *ExhaustionProofV1) { p.Nonce = nil }, "nonce"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validProof()
			tc.mutate(p)
			err := ValidateProofV1(p)
			require.Error(t, err, "case %q should fail validation", tc.name)
			assert.Contains(t, err.Error(), tc.errFrag, "error should mention %q", tc.errFrag)
		})
	}
}

func TestValidateProofV1_NilProof(t *testing.T) {
	err := ValidateProofV1(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil")
}
