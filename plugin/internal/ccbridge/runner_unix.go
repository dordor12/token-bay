//go:build unix

package ccbridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Run implements Runner on Unix platforms.
func (r *ExecRunner) Run(ctx context.Context, req Request, sink io.Writer) error {
	cwd, err := os.MkdirTemp("", "ccbridge-cwd-")
	if err != nil {
		return fmt.Errorf("ccbridge: mkdir temp cwd: %w", err)
	}
	defer func() { _ = os.RemoveAll(cwd) }()

	cmd := exec.CommandContext(ctx, r.resolveBinary(), BuildArgv(req)...)
	cmd.Dir = cwd
	// Detach into a new process group so we can SIGKILL the entire
	// tree on context cancel — a hung claude can spawn helpers we
	// don't want to leak.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return os.ErrProcessDone
	}
	cmd.WaitDelay = 500 * time.Millisecond

	stderr := &capBuffer{cap: r.resolveStderrCap()}
	cmd.Stderr = stderr
	cmd.Stdout = sink

	runErr := cmd.Run()
	if runErr == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return &ExitError{Code: exitErr.ExitCode(), Stderr: stderr.bytes()}
	}
	return fmt.Errorf("ccbridge: run claude: %w", runErr)
}

// capBuffer is a bounded byte buffer used for stderr capture.
type capBuffer struct {
	cap  int
	data []byte
}

func (b *capBuffer) Write(p []byte) (int, error) {
	if len(b.data) >= b.cap {
		return len(p), nil
	}
	room := b.cap - len(b.data)
	if room >= len(p) {
		b.data = append(b.data, p...)
	} else {
		b.data = append(b.data, p[:room]...)
	}
	return len(p), nil
}

func (b *capBuffer) bytes() []byte { return b.data }
