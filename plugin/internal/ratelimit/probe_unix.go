//go:build unix

package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// Probe spawns the configured command under a PTY and returns captured
// output. See ClaudePTYProbeRunner doc for exit conditions.
func (p *ClaudePTYProbeRunner) Probe(ctx context.Context) ([]byte, error) {
	deadline := p.HardDeadline
	if deadline <= 0 {
		deadline = 8 * time.Second
	}
	minTokens := p.MinTokens
	if minTokens <= 0 {
		minTokens = 2
	}

	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	// Use exec.Command + manual PTY wiring. Not CommandContext: its
	// SysProcAttr handling conflicts with pty's need to set Setctty/Ctty.
	// Not pty.Start either: its default SysProcAttr construction on recent
	// Go (1.21+) trips "Setctty set but Ctty not valid in child" because
	// it does not set Ctty to the slave fd explicitly.
	//
	// We replicate pty.Start's logic directly: open a PTY pair, attach the
	// slave as the child's stdio, set Setsid+Setctty+Ctty correctly.
	ptmx, tty, err := pty.Open()
	if err != nil {
		return nil, fmt.Errorf("ratelimit: pty.Open: %w", err)
	}
	defer func() { _ = ptmx.Close() }()

	cmd := exec.Command(p.BinaryPath, p.Args...)
	cmd.Stdin = tty
	cmd.Stdout = tty
	cmd.Stderr = tty
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		// Ctty is an index into the child's file descriptors (the Files
		// it will receive). Stdin=0, Stdout=1, Stderr=2 — any of them
		// points to tty so 0 is a valid selection.
		Ctty: 0,
	}

	if err := cmd.Start(); err != nil {
		_ = tty.Close()
		return nil, fmt.Errorf("ratelimit: cmd.Start %s: %w", p.BinaryPath, err)
	}
	// Close our handle on the slave; the child has its own.
	_ = tty.Close()
	ptyFile := ptmx

	type readResult struct {
		chunk []byte
		err   error
	}
	reads := make(chan readResult, 16)

	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptyFile.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				reads <- readResult{chunk: chunk}
			}
			if err != nil {
				reads <- readResult{err: err}
				return
			}
		}
	}()

	var out []byte
	done := false
	for !done {
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			if len(out) == 0 {
				return nil, ctx.Err()
			}
			return out, ctx.Err()

		case r := <-reads:
			if len(r.chunk) > 0 {
				out = append(out, r.chunk...)
				if countPctUsed(out) >= minTokens {
					done = true
				}
			}
			if r.err != nil {
				if errors.Is(r.err, io.EOF) {
					// Process ended naturally. Return whatever we have.
					done = true
					break
				}
				// Other read error before we had enough signal.
				if !done {
					_ = cmd.Process.Kill()
					_, _ = cmd.Process.Wait()
					if len(out) == 0 {
						return nil, r.err
					}
					return out, r.err
				}
			}
		}
	}

	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	return out, nil
}
