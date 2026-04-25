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
