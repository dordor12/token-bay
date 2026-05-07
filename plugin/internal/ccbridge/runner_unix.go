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
//
// The conversation in req.Messages is encoded as line-delimited
// stream-json events and written to the subprocess's stdin in a
// goroutine; stdin is closed once the encoding completes (or fails),
// signalling end-of-input to claude. Stdout streams to sink as bytes
// arrive. On context cancel the entire process group is SIGKILLed.
func (r *ExecRunner) Run(ctx context.Context, req Request, sink io.Writer) error {
	cwd, err := os.MkdirTemp("", "ccbridge-cwd-")
	if err != nil {
		return fmt.Errorf("ccbridge: mkdir temp cwd: %w", err)
	}
	defer func() { _ = os.RemoveAll(cwd) }()

	cmd := exec.CommandContext(ctx, r.resolveBinary(), BuildArgv(req)...)
	cmd.Dir = cwd
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

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("ccbridge: stdin pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return fmt.Errorf("ccbridge: start claude: %w", err)
	}

	// Encode messages → stdin in a goroutine so encoding and stdout
	// streaming can interleave. Close stdin when done so claude knows
	// no more input is coming.
	encDone := make(chan error, 1)
	go func() {
		err := EncodeMessages(stdin, req.Messages)
		closeErr := stdin.Close()
		if err == nil {
			err = closeErr
		}
		encDone <- err
	}()

	runErr := cmd.Wait()
	encErr := <-encDone

	if runErr == nil {
		if encErr != nil {
			return fmt.Errorf("ccbridge: encode stdin: %w", encErr)
		}
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
