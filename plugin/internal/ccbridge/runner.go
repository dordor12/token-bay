package ccbridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Runner runs a single bridge invocation. Implementations stream the
// subprocess's stdout to sink as bytes are read; non-zero exits return
// *ExitError. Implementations must be safe for concurrent use.
type Runner interface {
	Run(ctx context.Context, req Request, sink io.Writer) error
}

// ExitError reports a non-zero exit from the bridge subprocess. It is
// distinct from sink-write errors and context cancellations so callers
// can differentiate "claude exited with status N" from infrastructure
// failures.
type ExitError struct {
	Code   int
	Stderr []byte
}

func (e *ExitError) Error() string {
	if len(e.Stderr) == 0 {
		return fmt.Sprintf("ccbridge: claude exited with code %d", e.Code)
	}
	return fmt.Sprintf("ccbridge: claude exited with code %d: %s", e.Code, string(e.Stderr))
}

// ErrUnsupportedPlatform is returned by ExecRunner.Run on platforms
// without process-group support (currently: Windows). Plugin spec §10
// notes Windows seeder support is deferred.
var ErrUnsupportedPlatform = errors.New("ccbridge: bridge subprocess management unsupported on this platform")

// ExecRunner is the default Runner. It exec.Commands BinaryPath with
// argv from BuildArgv, in a per-client persistent directory, captures
// stderr for diagnostic context on non-zero exit, and streams stdout
// to sink as bytes arrive.
type ExecRunner struct {
	BinaryPath     string
	MaxStderrBytes int
	cachedVersion  string
	// SeederRoot is the parent directory under which per-client
	// session folders are created. Each client folder doubles as
	// the subprocess HOME so claude finds the synthetic session
	// file at <SeederRoot>/<ClientHash(pubkey)>/.claude/projects/
	// <sanitized-cwd>/<sessionID>.jsonl. Defaults to
	// filepath.Join(os.TempDir(), "ccbridge-seeder") when empty.
	SeederRoot string
}

// resolveSeederRoot returns the effective root, applying the
// default when SeederRoot is unset.
func (r *ExecRunner) resolveSeederRoot() string {
	if r.SeederRoot != "" {
		return r.SeederRoot
	}
	return filepath.Join(os.TempDir(), "ccbridge-seeder")
}

func (r *ExecRunner) resolveBinary() string {
	if r.BinaryPath == "" {
		return "claude"
	}
	return r.BinaryPath
}

func (r *ExecRunner) resolveStderrCap() int {
	if r.MaxStderrBytes <= 0 {
		return 8 * 1024
	}
	return r.MaxStderrBytes
}
