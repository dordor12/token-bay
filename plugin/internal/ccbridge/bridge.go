package ccbridge

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// ErrInvalidRequest is returned when Bridge.Serve receives a request
// missing required fields. Caller-side concern; never reaches the
// claude subprocess.
var ErrInvalidRequest = errors.New("ccbridge: invalid request")

// Bridge composes a Runner with the stream-json parser. It is the
// public seeder-side surface of this package: the seeder hands over a
// Request and a sink; Bridge writes the synthetic session file, runs
// the bridge subprocess, streams bytes back, and returns the canonical
// Usage from the result event.
//
// Bridge is safe for concurrent use as long as Runner is.
type Bridge struct {
	Runner Runner
}

// NewBridge returns a Bridge with the given Runner. Panics on nil — a
// programmer error caught at startup, not a runtime condition.
func NewBridge(r Runner) *Bridge {
	if r == nil {
		panic("ccbridge: NewBridge called with nil Runner")
	}
	return &Bridge{Runner: r}
}

// Serve runs a single bridge invocation. Bytes from the subprocess's
// stdout are forwarded verbatim to sink as they arrive. On success,
// returns the canonical Usage extracted from the terminal `result`
// event. On Runner failure (non-zero exit, infra error, ctx cancel),
// returns the Runner's error after still flushing whatever the
// subprocess emitted.
//
// The Runner writes req.Messages[:len-1] as a synthetic Claude Code
// session JSONL file under the isolated HOME
// ($HOME/.claude/projects/<sanitized-cwd>/<sessionID>.jsonl) and
// invokes claude with --resume <sessionID> + --no-session-persistence
// + the full airtight flag set. The last user turn in req.Messages
// becomes the positional -p prompt argument. Stream-json stdin is not
// used; the session-file transport is the sole mechanism for conveying
// prior conversation history to the subprocess.
func (b *Bridge) Serve(ctx context.Context, req Request, sink io.Writer) (Usage, error) {
	if err := ValidateMessages(req.Messages); err != nil {
		return Usage{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	if req.Model == "" {
		return Usage{}, fmt.Errorf("%w: empty Model", ErrInvalidRequest)
	}

	pr, pw := io.Pipe()
	parseDone := make(chan struct{})
	var (
		usage    Usage
		parseErr error
	)
	go func() {
		defer close(parseDone)
		usage, parseErr = ParseStreamJSON(pr, sink)
	}()

	runErr := b.Runner.Run(ctx, req, pw)
	_ = pw.Close()
	<-parseDone

	if runErr != nil {
		return usage, runErr
	}
	if parseErr != nil {
		return usage, parseErr
	}
	return usage, nil
}
