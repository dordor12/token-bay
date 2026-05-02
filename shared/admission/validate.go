package admission

import (
	"errors"
	"fmt"
)

// identityIDLen is the required length of CreditAttestationBody.IdentityId
// and IssuerTrackerId — Ed25519 pubkey hashes are 32 bytes per
// shared/CLAUDE.md and architecture spec §6.
const identityIDLen = 32

// scoreMax is the fixed-point ceiling for any score / reliability /
// dispute-rate field (= 1.0).
const scoreMax = uint32(10000)

// tenureDaysCap mirrors admission-design §4.1 "capped 365".
const tenureDaysCap = uint32(365)

// balanceCushionMin / balanceCushionMax bound the clamped
// log2(balance/starter_grant) signal.
const (
	balanceCushionMin = int32(-8)
	balanceCushionMax = int32(8)
)

// attestationMaxTTLSeconds bounds expires_at − computed_at (admission-design
// §6.1: 7 days).
const attestationMaxTTLSeconds = uint64(7 * 24 * 60 * 60)

// ValidateCreditAttestationBody enforces admission-design §6.1 invariants on
// a CreditAttestationBody. Senders run pre-sign; receivers (importing tracker)
// run post-parse before trusting any field. Returns nil if well-formed; an
// error describing the first violation otherwise. Does NOT verify the
// tracker_sig — see signing.VerifyCreditAttestation.
func ValidateCreditAttestationBody(b *CreditAttestationBody) error {
	if b == nil {
		return errors.New("admission: nil CreditAttestationBody")
	}
	if len(b.IdentityId) != identityIDLen {
		return fmt.Errorf("admission: identity_id length %d, want %d", len(b.IdentityId), identityIDLen)
	}
	if len(b.IssuerTrackerId) != identityIDLen {
		return fmt.Errorf("admission: issuer_tracker_id length %d, want %d", len(b.IssuerTrackerId), identityIDLen)
	}
	if b.Score > scoreMax {
		return fmt.Errorf("admission: score %d exceeds %d", b.Score, scoreMax)
	}
	if b.TenureDays > tenureDaysCap {
		return fmt.Errorf("admission: tenure_days %d exceeds cap %d", b.TenureDays, tenureDaysCap)
	}
	if b.SettlementReliability > scoreMax {
		return fmt.Errorf("admission: settlement_reliability %d exceeds %d", b.SettlementReliability, scoreMax)
	}
	if b.DisputeRate > scoreMax {
		return fmt.Errorf("admission: dispute_rate %d exceeds %d", b.DisputeRate, scoreMax)
	}
	if b.BalanceCushionLog2 < balanceCushionMin || b.BalanceCushionLog2 > balanceCushionMax {
		return fmt.Errorf("admission: balance_cushion_log2 %d out of [%d, %d]", b.BalanceCushionLog2, balanceCushionMin, balanceCushionMax)
	}
	if b.ComputedAt == 0 {
		return errors.New("admission: computed_at is zero")
	}
	if b.ExpiresAt <= b.ComputedAt {
		return fmt.Errorf("admission: expires_at %d must be > computed_at %d", b.ExpiresAt, b.ComputedAt)
	}
	if b.ExpiresAt-b.ComputedAt > attestationMaxTTLSeconds {
		return fmt.Errorf("admission: ttl %d exceeds max %d", b.ExpiresAt-b.ComputedAt, attestationMaxTTLSeconds)
	}
	return nil
}

// ValidateFetchHeadroomRequest enforces admission-design §6.1 invariants on a
// FetchHeadroomRequest. RequestNonce must be non-zero (zero is the proto-default
// indicating "not set"). ModelFilter is optional ("" means any model).
func ValidateFetchHeadroomRequest(r *FetchHeadroomRequest) error {
	if r == nil {
		return errors.New("admission: nil FetchHeadroomRequest")
	}
	if r.RequestNonce == 0 {
		return errors.New("admission: request_nonce is zero")
	}
	return nil
}
