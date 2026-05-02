package admission

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateCreditAttestationBody_HappyPath(t *testing.T) {
	require.NoError(t, ValidateCreditAttestationBody(fixtureCreditAttestationBody()))
}

func TestValidateCreditAttestationBody_NilBody(t *testing.T) {
	err := ValidateCreditAttestationBody(nil)
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "nil")
}

func TestValidateCreditAttestationBody_Rejections(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*CreditAttestationBody)
		errFrag string
	}{
		{"identity_id short", func(b *CreditAttestationBody) { b.IdentityId = []byte{1, 2, 3} }, "identity_id"},
		{"identity_id long", func(b *CreditAttestationBody) { b.IdentityId = make([]byte, 64) }, "identity_id"},
		{"identity_id nil", func(b *CreditAttestationBody) { b.IdentityId = nil }, "identity_id"},
		{"issuer_tracker_id short", func(b *CreditAttestationBody) { b.IssuerTrackerId = []byte{1, 2, 3} }, "issuer_tracker_id"},
		{"issuer_tracker_id long", func(b *CreditAttestationBody) { b.IssuerTrackerId = make([]byte, 64) }, "issuer_tracker_id"},
		{"issuer_tracker_id nil", func(b *CreditAttestationBody) { b.IssuerTrackerId = nil }, "issuer_tracker_id"},
		{"score over 10000", func(b *CreditAttestationBody) { b.Score = 10001 }, "score"},
		{"tenure_days over 365", func(b *CreditAttestationBody) { b.TenureDays = 366 }, "tenure_days"},
		{"settlement_reliability over 10000", func(b *CreditAttestationBody) { b.SettlementReliability = 10001 }, "settlement_reliability"},
		{"dispute_rate over 10000", func(b *CreditAttestationBody) { b.DisputeRate = 10001 }, "dispute_rate"},
		{"balance_cushion_log2 below -8", func(b *CreditAttestationBody) { b.BalanceCushionLog2 = -9 }, "balance_cushion_log2"},
		{"balance_cushion_log2 above 8", func(b *CreditAttestationBody) { b.BalanceCushionLog2 = 9 }, "balance_cushion_log2"},
		{"computed_at zero", func(b *CreditAttestationBody) { b.ComputedAt = 0 }, "computed_at"},
		{"expires_at not after computed_at (equal)", func(b *CreditAttestationBody) { b.ExpiresAt = b.ComputedAt }, "expires_at"},
		{"expires_at before computed_at", func(b *CreditAttestationBody) { b.ExpiresAt = b.ComputedAt - 1 }, "expires_at"},
		{"ttl exceeds 7 days", func(b *CreditAttestationBody) { b.ExpiresAt = b.ComputedAt + 604801 }, "ttl"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := fixtureCreditAttestationBody()
			tc.mutate(b)
			err := ValidateCreditAttestationBody(b)
			require.Error(t, err, "case %q should fail validation", tc.name)
			assert.Contains(t, err.Error(), tc.errFrag, "error should mention %q", tc.errFrag)
		})
	}
}

func TestValidateCreditAttestationBody_BoundaryValues(t *testing.T) {
	t.Run("score == 10000 ok", func(t *testing.T) {
		b := fixtureCreditAttestationBody()
		b.Score = 10000
		require.NoError(t, ValidateCreditAttestationBody(b))
	})
	t.Run("tenure == 365 ok", func(t *testing.T) {
		b := fixtureCreditAttestationBody()
		b.TenureDays = 365
		require.NoError(t, ValidateCreditAttestationBody(b))
	})
	t.Run("balance_cushion == -8 ok", func(t *testing.T) {
		b := fixtureCreditAttestationBody()
		b.BalanceCushionLog2 = -8
		require.NoError(t, ValidateCreditAttestationBody(b))
	})
	t.Run("balance_cushion == 8 ok", func(t *testing.T) {
		b := fixtureCreditAttestationBody()
		b.BalanceCushionLog2 = 8
		require.NoError(t, ValidateCreditAttestationBody(b))
	})
	t.Run("ttl == 7 days ok", func(t *testing.T) {
		b := fixtureCreditAttestationBody()
		b.ExpiresAt = b.ComputedAt + 604800
		require.NoError(t, ValidateCreditAttestationBody(b))
	})
}

func TestValidateFetchHeadroomRequest_HappyPath(t *testing.T) {
	require.NoError(t, ValidateFetchHeadroomRequest(&FetchHeadroomRequest{
		RequestNonce: 1,
		ModelFilter:  "claude-sonnet-4-6",
	}))
}

func TestValidateFetchHeadroomRequest_EmptyModelFilterOk(t *testing.T) {
	require.NoError(t, ValidateFetchHeadroomRequest(&FetchHeadroomRequest{
		RequestNonce: 1,
		ModelFilter:  "",
	}))
}

func TestValidateFetchHeadroomRequest_NilBody(t *testing.T) {
	err := ValidateFetchHeadroomRequest(nil)
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "nil")
}

func TestValidateFetchHeadroomRequest_ZeroNonce(t *testing.T) {
	err := ValidateFetchHeadroomRequest(&FetchHeadroomRequest{RequestNonce: 0})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "request_nonce")
}

func fixtureFetchHeadroomResponse() *FetchHeadroomResponse {
	return &FetchHeadroomResponse{
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
}

func TestValidateFetchHeadroomResponse_HappyPath(t *testing.T) {
	require.NoError(t, ValidateFetchHeadroomResponse(fixtureFetchHeadroomResponse()))
}

func TestValidateFetchHeadroomResponse_NoPerModelOk(t *testing.T) {
	r := fixtureFetchHeadroomResponse()
	r.PerModel = nil
	require.NoError(t, ValidateFetchHeadroomResponse(r))
}

func TestValidateFetchHeadroomResponse_NilBody(t *testing.T) {
	err := ValidateFetchHeadroomResponse(nil)
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "nil")
}

func TestValidateFetchHeadroomResponse_Rejections(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*FetchHeadroomResponse)
		errFrag string
	}{
		{"unspecified source", func(r *FetchHeadroomResponse) { r.Source = TickSource_TICK_SOURCE_UNSPECIFIED }, "source"},
		{"unknown source value", func(r *FetchHeadroomResponse) { r.Source = TickSource(99) }, "source"},
		{"headroom over 10000", func(r *FetchHeadroomResponse) { r.HeadroomEstimate = 10001 }, "headroom_estimate"},
		{"per_model headroom over 10000", func(r *FetchHeadroomResponse) {
			r.PerModel[0].HeadroomEstimate = 10001
		}, "per_model"},
		{"per_model empty model name", func(r *FetchHeadroomResponse) {
			r.PerModel[0].Model = ""
		}, "model"},
		{"per_model nil entry", func(r *FetchHeadroomResponse) {
			r.PerModel[0] = nil
		}, "per_model"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := fixtureFetchHeadroomResponse()
			tc.mutate(r)
			err := ValidateFetchHeadroomResponse(r)
			require.Error(t, err, "case %q should fail validation", tc.name)
			assert.Contains(t, err.Error(), tc.errFrag, "error should mention %q", tc.errFrag)
		})
	}
}

func TestValidateFetchHeadroomResponse_BoundaryHeadroom(t *testing.T) {
	r := fixtureFetchHeadroomResponse()
	r.HeadroomEstimate = 10000
	r.PerModel[0].HeadroomEstimate = 10000
	require.NoError(t, ValidateFetchHeadroomResponse(r))
}
