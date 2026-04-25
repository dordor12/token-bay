package ratelimit

import (
	"context"
	"regexp"
	"time"
)

// ProbeRunner runs `claude /usage` (or an equivalent) under a PTY and
// returns captured bytes. Callers pass the bytes to ParseUsageProbe.
//
// Implementations should exit as soon as they have enough signal —
// typically 2 `% used` tokens — rather than waiting for the full TUI
// render, which can take 15+ seconds.
type ProbeRunner interface {
	Probe(ctx context.Context) ([]byte, error)
}

// ClaudePTYProbeRunner spawns `claude /usage` under a PTY (so Claude Code
// renders its real TUI rather than the non-TTY stub) and reads output
// until one of: MinTokens `% used` matches are visible, the context is
// cancelled, HardDeadline elapses, or the subprocess exits.
//
// The Probe method is implemented per-OS: a real PTY runner on unix
// (probe_unix.go) and a stub returning ErrUnsupportedPlatform on
// windows (probe_windows.go), since `/usage` requires a Unix-style
// pseudo-terminal that the creack/pty library does not provide on
// Windows.
type ClaudePTYProbeRunner struct {
	BinaryPath   string
	Args         []string
	HardDeadline time.Duration
	MinTokens    int
}

// NewClaudePTYProbeRunner returns a runner with sane defaults matching
// plugin spec §5.2: `claude /usage`, 8s deadline, 2 tokens required.
func NewClaudePTYProbeRunner() *ClaudePTYProbeRunner {
	return &ClaudePTYProbeRunner{
		BinaryPath:   "claude",
		Args:         []string{"/usage"},
		HardDeadline: 8 * time.Second,
		MinTokens:    2,
	}
}

// probePctUsedRE counts `N% used` tokens for the early-termination check.
// The parser may use a slightly different pattern; here we only need to
// count them.
var probePctUsedRE = regexp.MustCompile(`(\d+)%[^\w]*used`)

// countPctUsed returns the number of `N% used` tokens in data.
// Used by the probe loop for early-termination.
func countPctUsed(data []byte) int {
	return len(probePctUsedRE.FindAll(data, -1))
}
