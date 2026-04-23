package ratelimit

import "regexp"

// exhaustedThreshold is the utilization percentage at or above which
// we classify a rate-limit window as exhausted. Math.floor is applied
// by Claude Code before rendering (src/components/Settings/Usage.tsx:42),
// so a real 99.9% shows as `99% used`. A window at 95+ is effectively
// full.
const exhaustedThreshold = 95

// ansiEscapeRE matches CSI (Control Sequence Introducer) sequences,
// OSC (Operating System Command) sequences, and standalone ESC chars
// commonly emitted by terminal TUIs. These interrupt logical tokens in
// the raw PTY byte stream — we strip them before token matching.
//
//	CSI:  ESC [ ... final-byte (in @-~ range)
//	OSC:  ESC ] ... (BEL | ESC \)
//	Bare: ESC followed by a single control character
var ansiEscapeRE = regexp.MustCompile(`\x1b(?:\[[0-9;?]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-Z\\-_])`)

// usagePctUsedRE extracts `N% used` tokens from ANSI-stripped output.
// Allows whitespace (including the `\x1C` cursor-forward character that
// Claude Code's renderer inlines as spacing) between the `%` and `used`.
var usagePctUsedRE = regexp.MustCompile(`(\d+)%[\s\x01-\x1f]*used`)

// ParseUsageProbe extracts `N% used` utilization tokens from raw PTY
// output and classifies per ratelimit plan §1.2:
//   - Any token's percentage ≥ exhaustedThreshold (95) → UsageExhausted
//   - All tokens < 95 AND at least 2 tokens captured   → UsageHeadroom
//   - Fewer than 2 tokens captured (or zero)           → UsageUncertain
//
// Strips ANSI escape sequences before matching so the regex survives
// the cursor-control bytes Claude Code's Ink renderer emits between
// adjacent characters of a single logical token.
//
// Never returns an error.
func ParseUsageProbe(data []byte) UsageVerdict {
	stripped := ansiEscapeRE.ReplaceAll(data, nil)
	matches := usagePctUsedRE.FindAllSubmatch(stripped, -1)
	if len(matches) < 2 {
		return UsageUncertain
	}
	for _, m := range matches {
		if atoiBytes(m[1]) >= exhaustedThreshold {
			return UsageExhausted
		}
	}
	return UsageHeadroom
}

// atoiBytes is a minimal safe parse of ASCII digit bytes to int. Never
// panics; returns 0 on empty or non-digit input. We use this rather
// than strconv.Atoi so the hot path has zero allocations for the tiny
// digit slices we're parsing.
func atoiBytes(b []byte) int {
	n := 0
	for _, c := range b {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int(c-'0')
	}
	return n
}
