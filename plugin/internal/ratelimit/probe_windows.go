//go:build windows

package ratelimit

import (
	"context"
	"errors"
)

// ErrUnsupportedPlatform is returned by Probe on platforms where the
// PTY-based `/usage` capture is not implemented. The `/usage` TUI
// requires a Unix-style pseudo-terminal: stdout must pass isatty() so
// Claude Code renders its real local-jsx UI rather than the non-TTY
// stub. Windows uses ConPTY (a different API) which the creack/pty
// dependency does not target, so the probe is unavailable there.
//
// Callers should treat this as a graceful "no probe data" signal and
// fall back to whatever default behavior they use when the probe is
// unavailable, rather than as a hard failure.
var ErrUnsupportedPlatform = errors.New("ratelimit: PTY probe not supported on this platform")

// Probe is a stub on Windows — see ErrUnsupportedPlatform.
func (p *ClaudePTYProbeRunner) Probe(_ context.Context) ([]byte, error) {
	return nil, ErrUnsupportedPlatform
}
