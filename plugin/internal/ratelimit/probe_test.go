package ratelimit

import (
	"context"
	"errors"
	"os/exec"
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

func TestClaudePTYProbeRunner_MissingBinary_ReturnsError(t *testing.T) {
	r := NewClaudePTYProbeRunner()
	r.BinaryPath = "/definitely/does/not/exist/claude"
	r.HardDeadline = 500 * time.Millisecond

	_, err := r.Probe(context.Background())
	assert.Error(t, err)
}

func TestClaudePTYProbeRunner_EarlyTermOnTokens(t *testing.T) {
	// Use /bin/sh to simulate a PTY-emitting process. We echo exactly
	// two `% used` tokens and then sleep for a long time. The probe should
	// terminate as soon as it sees the two tokens, long before the sleep.
	r := NewClaudePTYProbeRunner()
	r.BinaryPath = "/bin/sh"
	r.Args = []string{"-c", `printf 'Session 37%% used\nWeek 3%% used\n'; sleep 30`}
	r.HardDeadline = 10 * time.Second
	r.MinTokens = 2

	start := time.Now()
	data, err := r.Probe(context.Background())
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Contains(t, string(data), "37% used")
	assert.Contains(t, string(data), "3% used")
	assert.Less(t, elapsed, 5*time.Second, "should have early-terminated on tokens, not waited for sleep")
}

func TestClaudePTYProbeRunner_ContextCancelled_ReturnsError(t *testing.T) {
	// sh sleep that outputs nothing. Ctx deadline should fire.
	r := NewClaudePTYProbeRunner()
	r.BinaryPath = "/bin/sh"
	r.Args = []string{"-c", "sleep 5"}
	r.HardDeadline = 10 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := r.Probe(ctx)
	require.Error(t, err)
	assert.True(t,
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled),
		"expected ctx error, got %v", err)
}

func TestClaudePTYProbeRunner_HardDeadlineExpires(t *testing.T) {
	// sh sleep; no output. HardDeadline should expire before ctx.
	r := NewClaudePTYProbeRunner()
	r.BinaryPath = "/bin/sh"
	r.Args = []string{"-c", "sleep 5"}
	r.HardDeadline = 300 * time.Millisecond

	start := time.Now()
	_, err := r.Probe(context.Background())
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Less(t, elapsed, 2*time.Second)
}

func TestClaudePTYProbeRunner_LiveSmokeTest(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH; skipping live smoke")
	}
	r := NewClaudePTYProbeRunner()
	r.HardDeadline = 10 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	data, err := r.Probe(ctx)
	if err != nil {
		t.Logf("probe returned error (may be deadline-triggered): %v", err)
	}
	assert.NotEmpty(t, data, "live probe should capture at least some TUI bytes")
	t.Logf("live probe captured %d bytes", len(data))
}
