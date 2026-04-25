package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// StubProbeRunner returns canned bytes. Used by parser tests and by
// sidecar tests in sibling packages that want a predictable probe result.
type StubProbeRunner struct {
	Bytes []byte
	Err   error
}

// Probe implements ProbeRunner.
func (s *StubProbeRunner) Probe(_ context.Context) ([]byte, error) {
	return s.Bytes, s.Err
}

// Ensure the two implementations satisfy the interface at compile time.
// ClaudePTYProbeRunner's Probe is defined per-OS (probe_unix.go /
// probe_windows.go) so this assertion holds on every supported platform.
var (
	_ ProbeRunner = (*StubProbeRunner)(nil)
	_ ProbeRunner = (*ClaudePTYProbeRunner)(nil)
)

func TestClaudePTYProbeRunner_Defaults(t *testing.T) {
	r := NewClaudePTYProbeRunner()
	require.NotNil(t, r)
	assert.Equal(t, "claude", r.BinaryPath)
	assert.Equal(t, []string{"/usage"}, r.Args)
	assert.Equal(t, 8*time.Second, r.HardDeadline)
	assert.Equal(t, 2, r.MinTokens)
}
