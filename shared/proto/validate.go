package proto

import (
	"errors"
	"fmt"

	"github.com/token-bay/token-bay/shared/exhaustionproof"
)

// envelopeIDLen is the required length of EnvelopeBody.ConsumerId.
const envelopeIDLen = 32

// envelopeBodyHashLen is the required length of EnvelopeBody.BodyHash.
const envelopeBodyHashLen = 32

// envelopeNonceLen is the required length of EnvelopeBody.Nonce.
const envelopeNonceLen = 16

// ValidateEnvelopeBody enforces v1 wire-format invariants on an EnvelopeBody.
// Senders run pre-sign; receivers (tracker) run post-parse before trusting
// any field. Returns nil if well-formed; an error describing the first
// violation otherwise.
func ValidateEnvelopeBody(b *EnvelopeBody) error {
	if b == nil {
		return errors.New("proto: nil EnvelopeBody")
	}
	if b.ProtocolVersion != uint32(ProtocolVersion) {
		return fmt.Errorf("proto: protocol_version = %d, want %d", b.ProtocolVersion, ProtocolVersion)
	}
	if len(b.ConsumerId) != envelopeIDLen {
		return fmt.Errorf("proto: consumer_id length %d, want %d", len(b.ConsumerId), envelopeIDLen)
	}
	if b.Model == "" {
		return errors.New("proto: model is empty")
	}
	if len(b.BodyHash) != envelopeBodyHashLen {
		return fmt.Errorf("proto: body_hash length %d, want %d", len(b.BodyHash), envelopeBodyHashLen)
	}
	switch b.Tier {
	case PrivacyTier_PRIVACY_TIER_STANDARD, PrivacyTier_PRIVACY_TIER_TEE:
		// ok
	default:
		return fmt.Errorf("proto: tier = %v, must be STANDARD or TEE", b.Tier)
	}
	if b.ExhaustionProof == nil {
		return errors.New("proto: missing exhaustion_proof")
	}
	if err := exhaustionproof.ValidateProofV1(b.ExhaustionProof); err != nil {
		return fmt.Errorf("proto: exhaustion_proof: %w", err)
	}
	if b.BalanceProof == nil {
		return errors.New("proto: missing balance_proof")
	}
	if b.CapturedAt == 0 {
		return errors.New("proto: captured_at is zero")
	}
	if len(b.Nonce) != envelopeNonceLen {
		return fmt.Errorf("proto: nonce length %d, want %d", len(b.Nonce), envelopeNonceLen)
	}
	return nil
}

// entryHashLen is the required length for prev_hash and ref.
const entryHashLen = 32

// entryRequestIDLen is the required length for request_id (UUID-shaped).
const entryRequestIDLen = 16

// entryModelMaxLen mirrors ledger spec §3.1 model: string(32).
const entryModelMaxLen = 32

// entryFlagsMask covers all defined flag bits. Bit 0 = consumer_sig_missing.
// Reject any body with unknown bits set so v2 flags don't silently appear
// as garbage to a v1 verifier.
const entryFlagsMask = uint32(0x1)

// ValidateEntryBody enforces v1 wire-format invariants on an EntryBody.
// Tracker callers run pre-sign and post-parse before trusting any field.
// Does NOT verify signatures — see entry.VerifyAll on the tracker side.
func ValidateEntryBody(b *EntryBody) error {
	if b == nil {
		return errors.New("proto: nil EntryBody")
	}
	if len(b.PrevHash) != entryHashLen {
		return fmt.Errorf("proto: prev_hash length %d, want %d", len(b.PrevHash), entryHashLen)
	}
	if len(b.Ref) != entryHashLen {
		return fmt.Errorf("proto: ref length %d, want %d", len(b.Ref), entryHashLen)
	}
	if b.Flags & ^entryFlagsMask != 0 {
		return fmt.Errorf("proto: flags = %#x, unknown bits set", b.Flags)
	}
	switch b.Kind {
	case EntryKind_ENTRY_KIND_USAGE:
		if len(b.ConsumerId) != entryHashLen {
			return fmt.Errorf("proto: usage consumer_id length %d, want %d", len(b.ConsumerId), entryHashLen)
		}
		if len(b.SeederId) != entryHashLen {
			return fmt.Errorf("proto: usage seeder_id length %d, want %d", len(b.SeederId), entryHashLen)
		}
		if b.Model == "" {
			return errors.New("proto: usage entry has empty model")
		}
		if len(b.Model) > entryModelMaxLen {
			return fmt.Errorf("proto: model length %d exceeds %d", len(b.Model), entryModelMaxLen)
		}
		if len(b.RequestId) != entryRequestIDLen {
			return fmt.Errorf("proto: usage request_id length %d, want %d", len(b.RequestId), entryRequestIDLen)
		}
	case EntryKind_ENTRY_KIND_TRANSFER_OUT:
		if len(b.ConsumerId) != entryHashLen {
			return fmt.Errorf("proto: transfer_out consumer_id length %d, want %d", len(b.ConsumerId), entryHashLen)
		}
		if len(b.SeederId) != entryHashLen || !allZero(b.SeederId) {
			return errors.New("proto: transfer_out seeder_id must be 32 zero bytes")
		}
		if b.Model != "" {
			return errors.New("proto: transfer_out model must be empty")
		}
		if len(b.RequestId) != entryRequestIDLen || !allZero(b.RequestId) {
			return errors.New("proto: transfer_out request_id must be 16 zero bytes")
		}
		if b.InputTokens != 0 || b.OutputTokens != 0 {
			return errors.New("proto: transfer_out tokens must be zero")
		}
		if b.Flags != 0 {
			return fmt.Errorf("proto: transfer_out flags must be 0, got %#x", b.Flags)
		}
	case EntryKind_ENTRY_KIND_TRANSFER_IN:
		if len(b.ConsumerId) != entryHashLen || !allZero(b.ConsumerId) {
			return errors.New("proto: transfer_in consumer_id must be 32 zero bytes")
		}
		if len(b.SeederId) != entryHashLen || !allZero(b.SeederId) {
			return errors.New("proto: transfer_in seeder_id must be 32 zero bytes")
		}
		if b.Model != "" {
			return errors.New("proto: transfer_in model must be empty")
		}
		if len(b.RequestId) != entryRequestIDLen || !allZero(b.RequestId) {
			return errors.New("proto: transfer_in request_id must be 16 zero bytes")
		}
		if b.InputTokens != 0 || b.OutputTokens != 0 {
			return errors.New("proto: transfer_in tokens must be zero")
		}
		if b.Flags != 0 {
			return fmt.Errorf("proto: transfer_in flags must be 0, got %#x", b.Flags)
		}
	case EntryKind_ENTRY_KIND_STARTER_GRANT:
		if len(b.ConsumerId) != entryHashLen {
			return fmt.Errorf("proto: starter_grant consumer_id length %d, want %d", len(b.ConsumerId), entryHashLen)
		}
		if len(b.SeederId) != entryHashLen || !allZero(b.SeederId) {
			return errors.New("proto: starter_grant seeder_id must be 32 zero bytes")
		}
		if b.Model != "" {
			return errors.New("proto: starter_grant model must be empty")
		}
		if len(b.RequestId) != entryRequestIDLen || !allZero(b.RequestId) {
			return errors.New("proto: starter_grant request_id must be 16 zero bytes")
		}
		if !allZero(b.Ref) {
			return errors.New("proto: starter_grant ref must be 32 zero bytes")
		}
		if b.InputTokens != 0 || b.OutputTokens != 0 {
			return errors.New("proto: starter_grant tokens must be zero")
		}
		if b.Flags != 0 {
			return fmt.Errorf("proto: starter_grant flags must be 0, got %#x", b.Flags)
		}
	default:
		return fmt.Errorf("proto: kind = %v, must be USAGE, TRANSFER_OUT, TRANSFER_IN, or STARTER_GRANT", b.Kind)
	}
	return nil
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

// ValidateRPCRequest enforces structural invariants on an incoming
// RpcRequest. method must be one of the defined non-zero values
// (method zero is the heartbeat-channel marker, validated separately).
// payload must not exceed MaxRPCPayloadSize.
func ValidateRPCRequest(r *RpcRequest) error {
	if r == nil {
		return errors.New("proto: RpcRequest is nil")
	}
	switch r.Method {
	case RpcMethod_RPC_METHOD_ENROLL,
		RpcMethod_RPC_METHOD_BROKER_REQUEST,
		RpcMethod_RPC_METHOD_BALANCE,
		RpcMethod_RPC_METHOD_SETTLE,
		RpcMethod_RPC_METHOD_USAGE_REPORT,
		RpcMethod_RPC_METHOD_ADVERTISE,
		RpcMethod_RPC_METHOD_TRANSFER_REQUEST,
		RpcMethod_RPC_METHOD_STUN_ALLOCATE,
		RpcMethod_RPC_METHOD_TURN_RELAY_OPEN:
	default:
		return fmt.Errorf("proto: RpcRequest.Method invalid: %v", r.Method)
	}
	if len(r.Payload) > MaxRPCPayloadSize {
		return fmt.Errorf("proto: RpcRequest.Payload %d > %d", len(r.Payload), MaxRPCPayloadSize)
	}
	return nil
}

// ValidateRPCResponse enforces invariants on an RpcResponse. status must
// be one of the defined non-zero values. If status != OK, error must be
// non-nil and have a non-empty code.
func ValidateRPCResponse(r *RpcResponse) error {
	if r == nil {
		return errors.New("proto: RpcResponse is nil")
	}
	switch r.Status {
	case RpcStatus_RPC_STATUS_OK,
		RpcStatus_RPC_STATUS_INVALID,
		RpcStatus_RPC_STATUS_NO_CAPACITY,
		RpcStatus_RPC_STATUS_FROZEN,
		RpcStatus_RPC_STATUS_INTERNAL,
		RpcStatus_RPC_STATUS_UNAUTHENTICATED,
		RpcStatus_RPC_STATUS_NOT_FOUND:
	default:
		return fmt.Errorf("proto: RpcResponse.Status invalid: %v", r.Status)
	}
	if r.Status != RpcStatus_RPC_STATUS_OK {
		if r.Error == nil || r.Error.Code == "" {
			return errors.New("proto: RpcResponse.Error required when Status != OK")
		}
	}
	if len(r.Payload) > MaxRPCPayloadSize {
		return fmt.Errorf("proto: RpcResponse.Payload %d > %d", len(r.Payload), MaxRPCPayloadSize)
	}
	return nil
}

// ValidateSeederAssignment — non-empty addr, pubkey 32, reservation_token 16.
func ValidateSeederAssignment(a *SeederAssignment) error {
	if a == nil {
		return errors.New("proto: SeederAssignment is nil")
	}
	if len(a.SeederAddr) == 0 {
		return errors.New("proto: SeederAssignment.SeederAddr empty")
	}
	if len(a.SeederPubkey) != 32 {
		return fmt.Errorf("proto: SeederAssignment.SeederPubkey len=%d, want 32", len(a.SeederPubkey))
	}
	if len(a.ReservationToken) != 16 {
		return fmt.Errorf("proto: SeederAssignment.ReservationToken len=%d, want 16", len(a.ReservationToken))
	}
	return nil
}

func ValidateQueued(q *Queued) error {
	if q == nil {
		return errors.New("proto: Queued is nil")
	}
	if len(q.RequestId) != 16 {
		return fmt.Errorf("proto: Queued.RequestId len=%d, want 16", len(q.RequestId))
	}
	if q.PositionBand == PositionBand_POSITION_BAND_UNSPECIFIED {
		return errors.New("proto: Queued.PositionBand UNSPECIFIED")
	}
	if q.EtaBand == EtaBand_ETA_BAND_UNSPECIFIED {
		return errors.New("proto: Queued.EtaBand UNSPECIFIED")
	}
	return nil
}

func ValidateRejected(r *Rejected) error {
	if r == nil {
		return errors.New("proto: Rejected is nil")
	}
	if r.Reason == RejectReason_REJECT_REASON_UNSPECIFIED {
		return errors.New("proto: Rejected.Reason UNSPECIFIED")
	}
	if r.RetryAfterS < 60 || r.RetryAfterS > 600 {
		return fmt.Errorf("proto: Rejected.RetryAfterS %d outside [60, 600]", r.RetryAfterS)
	}
	return nil
}

func ValidateBrokerRequestResponse(r *BrokerRequestResponse) error {
	if r == nil {
		return errors.New("proto: BrokerRequestResponse is nil")
	}
	switch o := r.Outcome.(type) {
	case *BrokerRequestResponse_SeederAssignment:
		return ValidateSeederAssignment(o.SeederAssignment)
	case *BrokerRequestResponse_NoCapacity:
		if o.NoCapacity == nil {
			return errors.New("proto: BrokerRequestResponse.NoCapacity is nil")
		}
		return nil
	case *BrokerRequestResponse_Queued:
		return ValidateQueued(o.Queued)
	case *BrokerRequestResponse_Rejected:
		return ValidateRejected(o.Rejected)
	case nil:
		return errors.New("proto: BrokerRequestResponse.Outcome unset")
	default:
		return fmt.Errorf("proto: BrokerRequestResponse unknown Outcome %T", o)
	}
}

// ValidateOfferPush — non-empty consumer_id (32) + envelope_hash (32) + model.
func ValidateOfferPush(o *OfferPush) error {
	if o == nil {
		return errors.New("proto: OfferPush is nil")
	}
	if len(o.ConsumerId) != 32 {
		return fmt.Errorf("proto: OfferPush.ConsumerId len=%d, want 32", len(o.ConsumerId))
	}
	if len(o.EnvelopeHash) != 32 {
		return fmt.Errorf("proto: OfferPush.EnvelopeHash len=%d, want 32", len(o.EnvelopeHash))
	}
	if o.Model == "" {
		return errors.New("proto: OfferPush.Model empty")
	}
	return nil
}

// ValidateSettlementPush — non-empty preimage_hash (32) + preimage_body.
func ValidateSettlementPush(s *SettlementPush) error {
	if s == nil {
		return errors.New("proto: SettlementPush is nil")
	}
	if len(s.PreimageHash) != 32 {
		return fmt.Errorf("proto: SettlementPush.PreimageHash len=%d, want 32", len(s.PreimageHash))
	}
	if len(s.PreimageBody) == 0 {
		return errors.New("proto: SettlementPush.PreimageBody empty")
	}
	return nil
}
