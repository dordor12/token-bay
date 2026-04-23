package ratelimit

const (
	// maxSignalDriftSec is the allowed gap between the StopFailure event and
	// the /usage probe. Plugin spec §5.3 requires the probe to follow
	// StopFailure closely enough that both measure the same session.
	maxSignalDriftSec = 60

	// maxSignalAgeSec bounds how old either signal can be at verdict time.
	// The sidecar holds pending tickets for short windows; beyond this we
	// treat both signals as stale.
	maxSignalAgeSec = 120
)

// ApplyVerdictMatrix combines the trigger, the StopFailure error (when
// trigger is hook), and the /usage verdict into a GateVerdict per plugin
// spec §5.2.
//
// When trigger is hook and hookErr is not "rate_limit", the gate returns
// VerdictSkipHook — the plugin is not equipped to handle other error types.
// hookErr is ignored when trigger is TriggerManual.
//
// "Uncertain" safety direction: for TriggerHook, an external strong signal
// (a real 429) is present, so an uncertain /usage probe does not block
// proceeding. For TriggerManual, no external signal exists — uncertain
// means refuse to spend credits.
func ApplyVerdictMatrix(t Trigger, hookErr string, u UsageVerdict) GateVerdict {
	if t == TriggerHook {
		if hookErr != ErrorRateLimit {
			return VerdictSkipHook
		}
		switch u {
		case UsageExhausted, UsageUncertain:
			return VerdictProceed
		case UsageHeadroom:
			return VerdictRefuseManualOK
		default:
			return VerdictRefuseManualOK
		}
	}
	// TriggerManual
	switch u {
	case UsageExhausted:
		return VerdictProceed
	case UsageHeadroom, UsageUncertain:
		return VerdictRefuseWithReason
	default:
		return VerdictRefuseWithReason
	}
}

// CheckFreshness verifies that both signal timestamps are within
// maxSignalDriftSec of each other and within maxSignalAgeSec of now.
// Timestamps are unix seconds.
func CheckFreshness(stopFailureAt, usageProbeAt, now int64) bool {
	drift := stopFailureAt - usageProbeAt
	if drift < 0 {
		drift = -drift
	}
	if drift > maxSignalDriftSec {
		return false
	}
	if now-stopFailureAt > maxSignalAgeSec {
		return false
	}
	if now-usageProbeAt > maxSignalAgeSec {
		return false
	}
	return true
}
