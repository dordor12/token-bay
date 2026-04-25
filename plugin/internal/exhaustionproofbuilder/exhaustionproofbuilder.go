package exhaustionproofbuilder

import (
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/token-bay/token-bay/shared/exhaustionproof"
)

// Sentinel errors. Callers discriminate via errors.Is.
var (
	ErrInvalidInput = errors.New("exhaustionproofbuilder: invalid input")
	ErrRandFailed   = errors.New("exhaustionproofbuilder: random read failed")
	ErrValidation   = errors.New("exhaustionproofbuilder: validation failed")
)

// nonceLen matches shared/exhaustionproof's required Nonce length.
const nonceLen = 16

// ProofInput carries the raw materials the proof needs. Caller (ccproxy-network)
// adapts its EntryMetadata + the /usage probe timestamp into this struct.
type ProofInput struct {
	StopFailureMatcher    string    // expected: "rate_limit"
	StopFailureAt         time.Time // when the StopFailure hook fired
	StopFailureErrorShape []byte    // raw JSON from StopFailureHookInput
	UsageProbeAt          time.Time // when `claude /usage` was executed
	UsageProbeOutput      []byte    // raw PTY-captured /usage bytes
}

// Builder is stateless beyond its long-lived deps. Safe for concurrent use.
type Builder struct {
	// Now returns the current time. Defaults to time.Now. Override in tests.
	Now func() time.Time
	// RandRead fills p with random bytes. Defaults to crypto/rand.Read.
	// Override in tests for deterministic nonces.
	RandRead func(p []byte) (n int, err error)
}

// NewBuilder returns a Builder with production defaults.
func NewBuilder() *Builder {
	return &Builder{
		Now:      time.Now,
		RandRead: rand.Read,
	}
}

// Build assembles, validates, and returns a *ExhaustionProofV1.
func (b *Builder) Build(in ProofInput) (*exhaustionproof.ExhaustionProofV1, error) {
	if err := validateInput(in); err != nil {
		return nil, err
	}

	nonce := make([]byte, nonceLen)
	if _, err := b.RandRead(nonce); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRandFailed, err)
	}

	proof := &exhaustionproof.ExhaustionProofV1{
		StopFailure: &exhaustionproof.StopFailure{
			Matcher:    in.StopFailureMatcher,
			At:         uint64(in.StopFailureAt.Unix()),
			ErrorShape: in.StopFailureErrorShape,
		},
		UsageProbe: &exhaustionproof.UsageProbe{
			At:     uint64(in.UsageProbeAt.Unix()),
			Output: in.UsageProbeOutput,
		},
		CapturedAt: uint64(b.Now().Unix()),
		Nonce:      nonce,
	}

	if err := exhaustionproof.ValidateProofV1(proof); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}

	return proof, nil
}

// validateInput rejects empty/zero ProofInput fields up front. ValidateProofV1
// also checks most of these, but a dedicated input-shape error names the
// caller-side field so call-site bugs are easier to diagnose. Empty
// StopFailureErrorShape and UsageProbeOutput are rejected here defensively;
// ValidateProofV1 does not check the byte length of those fields.
func validateInput(in ProofInput) error {
	if in.StopFailureMatcher == "" {
		return fmt.Errorf("%w: StopFailureMatcher is empty", ErrInvalidInput)
	}
	if in.StopFailureAt.IsZero() {
		return fmt.Errorf("%w: StopFailureAt is zero", ErrInvalidInput)
	}
	if in.UsageProbeAt.IsZero() {
		return fmt.Errorf("%w: UsageProbeAt is zero", ErrInvalidInput)
	}
	if len(in.StopFailureErrorShape) == 0 {
		return fmt.Errorf("%w: StopFailureErrorShape is empty", ErrInvalidInput)
	}
	if len(in.UsageProbeOutput) == 0 {
		return fmt.Errorf("%w: UsageProbeOutput is empty", ErrInvalidInput)
	}
	return nil
}
