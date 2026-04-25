package envelopebuilder

import (
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/token-bay/token-bay/shared/exhaustionproof"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// Sentinel errors. Callers discriminate via errors.Is.
var (
	ErrNilProof    = errors.New("envelopebuilder: proof is nil")
	ErrNilBalance  = errors.New("envelopebuilder: balance is nil")
	ErrInvalidSpec = errors.New("envelopebuilder: invalid RequestSpec")
	ErrRandFailed  = errors.New("envelopebuilder: random read failed")
	ErrValidation  = errors.New("envelopebuilder: validation failed")
	ErrSign        = errors.New("envelopebuilder: signer failed")
)

// nonceLen matches shared/proto's required EnvelopeBody.Nonce length.
const nonceLen = 16

// bodyHashLen matches shared/proto's required EnvelopeBody.BodyHash length.
const bodyHashLen = 32

// RequestSpec is the per-request slice of the envelope, supplied by the
// caller (ccproxy-network) after parsing /v1/messages and resolving the
// privacy tier from config.
type RequestSpec struct {
	Model           string
	MaxInputTokens  uint64
	MaxOutputTokens uint64
	Tier            tbproto.PrivacyTier
	BodyHash        []byte // SHA-256, len 32
}

// Signer abstracts the consumer's identity. Production impl lives in
// plugin/internal/identity; tests use a fake. Implementations must be
// safe for concurrent use — Build calls Sign once per request.
type Signer interface {
	// Sign returns an Ed25519 signature over DeterministicMarshal(body).
	Sign(body *tbproto.EnvelopeBody) ([]byte, error)
	// IdentityID returns this Signer's 32-byte identity (pubkey hash).
	IdentityID() ids.IdentityID
}

// Builder is stateless beyond its long-lived deps. Safe for concurrent use.
type Builder struct {
	// Signer is required. NewBuilder panics on nil.
	Signer Signer
	// Now returns the current time. Defaults to time.Now. Override in tests.
	Now func() time.Time
	// RandRead fills p with random bytes. Defaults to crypto/rand.Read.
	// Override in tests for deterministic nonces.
	RandRead func(p []byte) (n int, err error)
}

// NewBuilder returns a Builder with production defaults. Panics on nil
// Signer — a programmer error caught at startup, not a runtime condition.
// All Build()-time errors still fail closed via sentinels.
func NewBuilder(s Signer) *Builder {
	if s == nil {
		panic("envelopebuilder: NewBuilder called with nil Signer")
	}
	return &Builder{
		Signer:   s,
		Now:      time.Now,
		RandRead: rand.Read,
	}
}

// Build assembles, validates, signs, and returns *EnvelopeSigned.
func (b *Builder) Build(
	spec RequestSpec,
	proof *exhaustionproof.ExhaustionProofV1,
	balance *tbproto.SignedBalanceSnapshot,
) (*tbproto.EnvelopeSigned, error) {
	if proof == nil {
		return nil, ErrNilProof
	}
	if balance == nil {
		return nil, ErrNilBalance
	}
	if err := validateSpec(spec); err != nil {
		return nil, err
	}

	nonce := make([]byte, nonceLen)
	if _, err := b.RandRead(nonce); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRandFailed, err)
	}

	id := b.Signer.IdentityID()
	body := &tbproto.EnvelopeBody{
		ProtocolVersion: uint32(tbproto.ProtocolVersion),
		ConsumerId:      append([]byte(nil), id[:]...),
		Model:           spec.Model,
		MaxInputTokens:  spec.MaxInputTokens,
		MaxOutputTokens: spec.MaxOutputTokens,
		Tier:            spec.Tier,
		BodyHash:        spec.BodyHash,
		ExhaustionProof: proof,
		BalanceProof:    balance,
		CapturedAt:      unixSecondsU(b.Now()),
		Nonce:           nonce,
	}

	if err := tbproto.ValidateEnvelopeBody(body); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}

	sig, err := b.Signer.Sign(body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSign, err)
	}

	return &tbproto.EnvelopeSigned{
		Body:        body,
		ConsumerSig: sig,
	}, nil
}

// unixSecondsU returns t.Unix() as uint64, saturating at 0 for pre-1970
// inputs. Avoids gosec G115's int64→uint64 narrowing complaint without
// adding spurious error paths for clocks that will never be negative.
func unixSecondsU(t time.Time) uint64 {
	s := t.Unix()
	if s < 0 {
		return 0
	}
	return uint64(s)
}

// validateSpec rejects obviously-bad RequestSpec fields with ErrInvalidSpec.
// ValidateEnvelopeBody re-checks these post-assembly, but the dedicated
// pre-check error names the caller-side field for easier diagnosis.
func validateSpec(spec RequestSpec) error {
	if spec.Model == "" {
		return fmt.Errorf("%w: Model is empty", ErrInvalidSpec)
	}
	if len(spec.BodyHash) != bodyHashLen {
		return fmt.Errorf("%w: BodyHash length %d, want %d", ErrInvalidSpec, len(spec.BodyHash), bodyHashLen)
	}
	if spec.Tier == tbproto.PrivacyTier_PRIVACY_TIER_UNSPECIFIED {
		return fmt.Errorf("%w: Tier is UNSPECIFIED", ErrInvalidSpec)
	}
	return nil
}
