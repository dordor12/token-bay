// Package ratelimit parses Claude Code rate-limit signals and applies the
// two-stage gate from plugin spec §5.2.
//
// Responsibilities:
//   - Parse StopFailure hook payloads (stopfailure.go).
//   - Parse `claude -p "/usage"` output with JSON and text strategies (usage.go).
//   - Apply the verdict matrix that decides proceed/refuse (verdict.go).
//
// This package performs no I/O. The sidecar's hooks module feeds in payload
// bytes; the sidecar's ccbridge module captures /usage output and passes
// those bytes here. The sidecar then applies the GateVerdict to decide
// next action.
package ratelimit

// Error enum values mirrored from Claude Code's SDKAssistantMessageErrorSchema
// (src/entrypoints/sdk/coreSchemas.ts:1256-1266).
const (
	ErrorRateLimit            = "rate_limit"
	ErrorAuthenticationFailed = "authentication_failed"
	ErrorBillingError         = "billing_error"
	ErrorInvalidRequest       = "invalid_request"
	ErrorServerError          = "server_error"
	ErrorMaxOutputTokens      = "max_output_tokens"
	ErrorUnknown              = "unknown"
)

// UsageVerdict classifies the outcome of parsing `claude -p "/usage"`.
type UsageVerdict int

// UsageVerdict values.
const (
	UsageExhausted UsageVerdict = iota
	UsageHeadroom
	UsageUncertain
)

func (v UsageVerdict) String() string {
	switch v {
	case UsageExhausted:
		return "exhausted"
	case UsageHeadroom:
		return "headroom"
	case UsageUncertain:
		return "uncertain"
	default:
		return "unknown"
	}
}

// Trigger indicates what invoked the two-stage gate.
type Trigger int

// Trigger values.
const (
	TriggerHook Trigger = iota
	TriggerManual
)

// GateVerdict is the final decision from ApplyVerdictMatrix.
type GateVerdict int

// GateVerdict values.
const (
	VerdictProceed GateVerdict = iota
	VerdictRefuseManualOK
	VerdictRefuseWithReason
	VerdictSkipHook
)

func (v GateVerdict) String() string {
	switch v {
	case VerdictProceed:
		return "proceed"
	case VerdictRefuseManualOK:
		return "refuse-manual-ok"
	case VerdictRefuseWithReason:
		return "refuse-with-reason"
	case VerdictSkipHook:
		return "skip-hook"
	default:
		return "unknown"
	}
}
