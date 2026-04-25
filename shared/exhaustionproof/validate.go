package exhaustionproof

import (
	"errors"
	"fmt"
)

// proofFreshnessWindow is the maximum allowed gap between the StopFailure
// timestamp and the UsageProbe timestamp. Beyond this, the two signals
// no longer plausibly correlate to the same rate-limit event.
const proofFreshnessWindow = 60 // seconds

// nonceLen is the required length of an ExhaustionProofV1.Nonce.
const nonceLen = 16

// ValidateProofV1 checks an ExhaustionProofV1 against the v1 wire-format rules.
// Returns nil if the proof is well-formed; an error describing the first
// violation otherwise. Callers (sender pre-sign / receiver post-parse) must
// run this before trusting the proof contents.
func ValidateProofV1(p *ExhaustionProofV1) error {
	if p == nil {
		return errors.New("exhaustionproof: nil ExhaustionProofV1")
	}
	if p.StopFailure == nil {
		return errors.New("exhaustionproof: missing stop_failure")
	}
	if p.UsageProbe == nil {
		return errors.New("exhaustionproof: missing usage_probe")
	}
	if p.StopFailure.Matcher != "rate_limit" {
		return fmt.Errorf("exhaustionproof: stop_failure.matcher = %q, want \"rate_limit\"", p.StopFailure.Matcher)
	}
	if p.StopFailure.At == 0 {
		return errors.New("exhaustionproof: stop_failure.at is zero")
	}
	if p.UsageProbe.At == 0 {
		return errors.New("exhaustionproof: usage_probe.at is zero")
	}
	gap := int64(p.StopFailure.At) - int64(p.UsageProbe.At)
	if gap < 0 {
		gap = -gap
	}
	if gap > proofFreshnessWindow {
		return fmt.Errorf("exhaustionproof: signals %ds apart, exceeds freshness window %ds", gap, proofFreshnessWindow)
	}
	if p.CapturedAt == 0 {
		return errors.New("exhaustionproof: captured_at is zero")
	}
	if len(p.Nonce) != nonceLen {
		return fmt.Errorf("exhaustionproof: nonce length %d, want %d", len(p.Nonce), nonceLen)
	}
	return nil
}
