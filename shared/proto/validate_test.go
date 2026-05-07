package proto

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateEnvelopeBody_HappyPath(t *testing.T) {
	require.NoError(t, ValidateEnvelopeBody(fixtureEnvelopeBody()))
}

func TestValidateEnvelopeBody_Rejections(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(b *EnvelopeBody)
		errFrag string
	}{
		{"wrong protocol_version", func(b *EnvelopeBody) { b.ProtocolVersion = 99 }, "protocol_version"},
		{"consumer_id too short", func(b *EnvelopeBody) { b.ConsumerId = []byte{1, 2, 3} }, "consumer_id"},
		{"consumer_id too long", func(b *EnvelopeBody) { b.ConsumerId = make([]byte, 64) }, "consumer_id"},
		{"empty model", func(b *EnvelopeBody) { b.Model = "" }, "model"},
		{"body_hash wrong length", func(b *EnvelopeBody) { b.BodyHash = []byte{1, 2} }, "body_hash"},
		{"body_hash too long", func(b *EnvelopeBody) { b.BodyHash = make([]byte, 64) }, "body_hash"},
		{"missing exhaustion_proof", func(b *EnvelopeBody) { b.ExhaustionProof = nil }, "exhaustion_proof"},
		{"missing balance_proof", func(b *EnvelopeBody) { b.BalanceProof = nil }, "balance_proof"},
		{"zero captured_at", func(b *EnvelopeBody) { b.CapturedAt = 0 }, "captured_at"},
		{"nonce nil", func(b *EnvelopeBody) { b.Nonce = nil }, "nonce"},
		{"nonce too short", func(b *EnvelopeBody) { b.Nonce = []byte("short") }, "nonce"},
		{"nonce too long", func(b *EnvelopeBody) { b.Nonce = make([]byte, 32) }, "nonce"},
		{"unspecified tier", func(b *EnvelopeBody) { b.Tier = PrivacyTier_PRIVACY_TIER_UNSPECIFIED }, "tier"},
		{"unknown tier value", func(b *EnvelopeBody) { b.Tier = PrivacyTier(99) }, "tier"},
		{"invalid exhaustion_proof", func(b *EnvelopeBody) { b.ExhaustionProof.StopFailure.Matcher = "wrong" }, "exhaustion_proof"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := fixtureEnvelopeBody()
			tc.mutate(b)
			err := ValidateEnvelopeBody(b)
			require.Error(t, err, "case %q should fail validation", tc.name)
			assert.Contains(t, err.Error(), tc.errFrag, "error should mention %q", tc.errFrag)
		})
	}
}

func TestValidateEnvelopeBody_NilBody(t *testing.T) {
	err := ValidateEnvelopeBody(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil")
}

// TestValidateEnvelopeBody_WrapsExhaustionProofError verifies that an inner
// ValidateProofV1 failure is wrapped with the proto package prefix while
// preserving the inner error chain via %w. A regression that drops the %w
// would still pass the rejection-matrix test (which only asserts the prefix
// substring) — this test catches that.
func TestValidateEnvelopeBody_WrapsExhaustionProofError(t *testing.T) {
	b := fixtureEnvelopeBody()
	b.ExhaustionProof.StopFailure.Matcher = "wrong"

	err := ValidateEnvelopeBody(b)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "proto: exhaustion_proof:", "wrapper prefix must be present")
	assert.Contains(t, err.Error(), "matcher", "inner ValidateProofV1 error must be preserved (substring 'matcher' is unique to the inner error)")
}

func TestValidateEntryBody_AcceptsAllKinds(t *testing.T) {
	t.Run("usage", func(t *testing.T) {
		require.NoError(t, ValidateEntryBody(fixtureUsageEntryBody()))
	})
	t.Run("transfer_out", func(t *testing.T) {
		require.NoError(t, ValidateEntryBody(fixtureTransferOutEntryBody()))
	})
	t.Run("transfer_in", func(t *testing.T) {
		require.NoError(t, ValidateEntryBody(fixtureTransferInEntryBody()))
	})
	t.Run("starter_grant", func(t *testing.T) {
		require.NoError(t, ValidateEntryBody(fixtureStarterGrantEntryBody()))
	})
}

func TestValidateEntryBody_NilBody(t *testing.T) {
	err := ValidateEntryBody(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil")
}

// Cross-kind structural rejections — invariants every kind must satisfy.
func TestValidateEntryBody_StructuralRejections(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*EntryBody)
		errFrag string
	}{
		{"prev_hash short", func(b *EntryBody) { b.PrevHash = make([]byte, 16) }, "prev_hash"},
		{"prev_hash long", func(b *EntryBody) { b.PrevHash = make([]byte, 64) }, "prev_hash"},
		{"ref wrong length", func(b *EntryBody) { b.Ref = make([]byte, 8) }, "ref"},
		{"unspecified kind", func(b *EntryBody) { b.Kind = EntryKind_ENTRY_KIND_UNSPECIFIED }, "kind"},
		{"unknown kind", func(b *EntryBody) { b.Kind = EntryKind(99) }, "kind"},
		{"flags unknown bit", func(b *EntryBody) { b.Flags = 1 << 5 }, "flags"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := fixtureUsageEntryBody()
			tc.mutate(b)
			err := ValidateEntryBody(b)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errFrag)
		})
	}
}

// USAGE-specific field invariants.
func TestValidateEntryBody_UsageRejections(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*EntryBody)
		errFrag string
	}{
		{"consumer_id short", func(b *EntryBody) { b.ConsumerId = make([]byte, 16) }, "consumer_id"},
		{"seeder_id short", func(b *EntryBody) { b.SeederId = make([]byte, 16) }, "seeder_id"},
		{"empty model", func(b *EntryBody) { b.Model = "" }, "model"},
		{"model too long", func(b *EntryBody) { b.Model = strings.Repeat("x", 33) }, "model"},
		{"request_id wrong length", func(b *EntryBody) { b.RequestId = make([]byte, 8) }, "request_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := fixtureUsageEntryBody()
			tc.mutate(b)
			err := ValidateEntryBody(b)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errFrag)
		})
	}
}

// TRANSFER_OUT must zero out the seeder/model/request_id slots and reject
// any flags.
func TestValidateEntryBody_TransferOutRejections(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*EntryBody)
		errFrag string
	}{
		{"seeder_id non-zero", func(b *EntryBody) { b.SeederId = make32(0xFF) }, "seeder_id"},
		{"model non-empty", func(b *EntryBody) { b.Model = "claude" }, "model"},
		{"request_id non-zero", func(b *EntryBody) { b.RequestId = make16(0xFF) }, "request_id"},
		{"input_tokens non-zero", func(b *EntryBody) { b.InputTokens = 1 }, "tokens"},
		{"flag set", func(b *EntryBody) { b.Flags = 1 }, "flags"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := fixtureTransferOutEntryBody()
			tc.mutate(b)
			err := ValidateEntryBody(b)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errFrag)
		})
	}
}

// TRANSFER_IN: both counterparty IDs must be zero.
func TestValidateEntryBody_TransferInRejections(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*EntryBody)
		errFrag string
	}{
		{"consumer_id non-zero", func(b *EntryBody) { b.ConsumerId = make32(0xFF) }, "consumer_id"},
		{"seeder_id non-zero", func(b *EntryBody) { b.SeederId = make32(0xFF) }, "seeder_id"},
		{"model non-empty", func(b *EntryBody) { b.Model = "claude" }, "model"},
		{"flag set", func(b *EntryBody) { b.Flags = 1 }, "flags"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := fixtureTransferInEntryBody()
			tc.mutate(b)
			err := ValidateEntryBody(b)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errFrag)
		})
	}
}

// STARTER_GRANT: consumer_id required, everything else zero.
func TestValidateEntryBody_StarterGrantRejections(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*EntryBody)
		errFrag string
	}{
		{"consumer_id short", func(b *EntryBody) { b.ConsumerId = make([]byte, 16) }, "consumer_id"},
		{"seeder_id non-zero", func(b *EntryBody) { b.SeederId = make32(0xFF) }, "seeder_id"},
		{"model non-empty", func(b *EntryBody) { b.Model = "claude" }, "model"},
		{"ref non-zero", func(b *EntryBody) { b.Ref = make32(0xAA) }, "ref"},
		{"flag set", func(b *EntryBody) { b.Flags = 1 }, "flags"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := fixtureStarterGrantEntryBody()
			tc.mutate(b)
			err := ValidateEntryBody(b)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errFrag)
		})
	}
}

func TestValidateRpcRequestRejections(t *testing.T) {
	cases := []struct {
		name string
		in   *RpcRequest
		msg  string
	}{
		{"nil", nil, "is nil"},
		{"unspecified", &RpcRequest{Method: RpcMethod_RPC_METHOD_UNSPECIFIED}, "Method invalid"},
		{"out_of_range", &RpcRequest{Method: 999}, "Method invalid"},
		{"oversize", &RpcRequest{
			Method:  RpcMethod_RPC_METHOD_BROKER_REQUEST,
			Payload: make([]byte, MaxRPCPayloadSize+1),
		}, "Payload"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateRPCRequest(c.in)
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.msg)
		})
	}
}

func TestValidateRpcRequestAccepts(t *testing.T) {
	require.NoError(t, ValidateRPCRequest(&RpcRequest{Method: RpcMethod_RPC_METHOD_BALANCE}))
}

func TestValidateRpcResponseRejections(t *testing.T) {
	cases := []struct {
		name string
		in   *RpcResponse
		msg  string
	}{
		{"nil", nil, "is nil"},
		{"unspecified", &RpcResponse{Status: RpcStatus_RPC_STATUS_UNSPECIFIED}, "Status invalid"},
		{"err_without_error", &RpcResponse{Status: RpcStatus_RPC_STATUS_INVALID}, "required"},
		{"err_blank_code", &RpcResponse{
			Status: RpcStatus_RPC_STATUS_INVALID,
			Error:  &RpcError{Code: "", Message: "x"},
		}, "required"},
		{"oversize", &RpcResponse{
			Status:  RpcStatus_RPC_STATUS_OK,
			Payload: make([]byte, MaxRPCPayloadSize+1),
		}, "Payload"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateRPCResponse(c.in)
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.msg)
		})
	}
}

func TestValidateQueued(t *testing.T) {
	require.Error(t, ValidateQueued(nil))
	require.Error(t, ValidateQueued(&Queued{RequestId: make([]byte, 15)}))
	require.Error(t, ValidateQueued(&Queued{RequestId: make([]byte, 16)})) // unspecified bands
	require.NoError(t, ValidateQueued(&Queued{
		RequestId:    make([]byte, 16),
		PositionBand: PositionBand_POSITION_BAND_1_TO_10,
		EtaBand:      EtaBand_ETA_BAND_LT_30S,
	}))
}

func TestValidateRejected(t *testing.T) {
	require.Error(t, ValidateRejected(nil))
	require.Error(t, ValidateRejected(&Rejected{Reason: RejectReason_REJECT_REASON_UNSPECIFIED, RetryAfterS: 60}))
	require.Error(t, ValidateRejected(&Rejected{Reason: RejectReason_REJECT_REASON_REGION_OVERLOADED, RetryAfterS: 59}))
	require.Error(t, ValidateRejected(&Rejected{Reason: RejectReason_REJECT_REASON_REGION_OVERLOADED, RetryAfterS: 601}))
	require.NoError(t, ValidateRejected(&Rejected{Reason: RejectReason_REJECT_REASON_QUEUE_TIMEOUT, RetryAfterS: 120}))
}

func TestValidateBrokerRequestResponse(t *testing.T) {
	require.Error(t, ValidateBrokerRequestResponse(nil))
	require.Error(t, ValidateBrokerRequestResponse(&BrokerRequestResponse{}))
	require.NoError(t, ValidateBrokerRequestResponse(&BrokerRequestResponse{
		Outcome: &BrokerRequestResponse_SeederAssignment{SeederAssignment: &SeederAssignment{
			SeederAddr:       []byte("127.0.0.1:0"),
			SeederPubkey:     make([]byte, 32),
			ReservationToken: make([]byte, 16),
		}},
	}))
	require.NoError(t, ValidateBrokerRequestResponse(&BrokerRequestResponse{
		Outcome: &BrokerRequestResponse_NoCapacity{NoCapacity: &NoCapacity{Reason: "ok"}},
	}))
	require.NoError(t, ValidateBrokerRequestResponse(&BrokerRequestResponse{
		Outcome: &BrokerRequestResponse_Queued{Queued: &Queued{
			RequestId:    make([]byte, 16),
			PositionBand: PositionBand_POSITION_BAND_1_TO_10,
			EtaBand:      EtaBand_ETA_BAND_LT_30S,
		}},
	}))
	require.NoError(t, ValidateBrokerRequestResponse(&BrokerRequestResponse{
		Outcome: &BrokerRequestResponse_Rejected{Rejected: &Rejected{
			Reason: RejectReason_REJECT_REASON_REGION_OVERLOADED, RetryAfterS: 60,
		}},
	}))
}

func TestValidateSeederAssignment(t *testing.T) {
	require.Error(t, ValidateSeederAssignment(nil))
	require.Error(t, ValidateSeederAssignment(&SeederAssignment{}))
	require.Error(t, ValidateSeederAssignment(&SeederAssignment{
		SeederAddr: []byte(""), SeederPubkey: make([]byte, 32), ReservationToken: make([]byte, 16),
	}))
	require.Error(t, ValidateSeederAssignment(&SeederAssignment{
		SeederAddr: []byte("127.0.0.1:0"), SeederPubkey: make([]byte, 31), ReservationToken: make([]byte, 16),
	}))
	require.Error(t, ValidateSeederAssignment(&SeederAssignment{
		SeederAddr: []byte("127.0.0.1:0"), SeederPubkey: make([]byte, 32), ReservationToken: make([]byte, 15),
	}))
	require.NoError(t, ValidateSeederAssignment(&SeederAssignment{
		SeederAddr: []byte("127.0.0.1:0"), SeederPubkey: make([]byte, 32), ReservationToken: make([]byte, 16),
	}))
}

func TestValidateOfferAndSettlementPush(t *testing.T) {
	require.Error(t, ValidateOfferPush(nil))
	require.Error(t, ValidateOfferPush(&OfferPush{ConsumerId: make([]byte, 31)}))
	require.NoError(t, ValidateOfferPush(&OfferPush{
		ConsumerId:   make([]byte, 32),
		EnvelopeHash: make([]byte, 32),
		Model:        "x",
	}))
	require.Error(t, ValidateSettlementPush(nil))
	require.Error(t, ValidateSettlementPush(&SettlementPush{PreimageHash: make([]byte, 32)}))
	require.NoError(t, ValidateSettlementPush(&SettlementPush{
		PreimageHash: make([]byte, 32),
		PreimageBody: []byte{1},
	}))
}
