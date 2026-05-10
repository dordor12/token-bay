package hooks

import (
	"context"
	"sync"

	"github.com/token-bay/token-bay/plugin/internal/ratelimit"
)

// Sink is the side-effect collaborator the Dispatcher calls into for each
// observed hook event. Implementations are responsible for whatever
// downstream coordination is required (writing a pending-fallback ticket,
// updating availability state, forwarding via IPC to the long-lived sidecar,
// etc.). The Dispatcher returns the sink's error verbatim alongside writing
// an empty Response — sink failure does not block the host Claude Code.
//
// Each method receives a context so a sink may abort fast (e.g. a queued IPC
// write that the supervisor refuses post-shutdown).
type Sink interface {
	OnStopFailure(ctx context.Context, p *ratelimit.StopFailurePayload) error
	OnSessionStart(ctx context.Context, p *SessionStartPayload) error
	OnSessionEnd(ctx context.Context, p *SessionEndPayload) error
	OnUserPromptSubmit(ctx context.Context, p *UserPromptSubmitPayload) error
}

// NopSink is the trivial Sink implementation: every method returns nil and
// records nothing. Useful for the cmd-layer's safe default before the
// supervisor wiring is in place.
type NopSink struct{}

// OnStopFailure satisfies Sink.
func (*NopSink) OnStopFailure(context.Context, *ratelimit.StopFailurePayload) error {
	return nil
}

// OnSessionStart satisfies Sink.
func (*NopSink) OnSessionStart(context.Context, *SessionStartPayload) error { return nil }

// OnSessionEnd satisfies Sink.
func (*NopSink) OnSessionEnd(context.Context, *SessionEndPayload) error { return nil }

// OnUserPromptSubmit satisfies Sink.
func (*NopSink) OnUserPromptSubmit(context.Context, *UserPromptSubmitPayload) error { return nil }

// RecordingSink is a Sink that appends every observed payload to per-event
// slices. Concurrent-safe via an internal mutex. If Err is non-nil, every
// method returns it (after recording the payload) so tests can drive
// sink-error code paths.
type RecordingSink struct {
	mu sync.Mutex

	// Err, if non-nil, is returned from every method after the payload is
	// recorded. Lets tests drive the dispatcher's error-propagation path.
	Err error

	StopFailures      []*ratelimit.StopFailurePayload
	SessionStarts     []*SessionStartPayload
	SessionEnds       []*SessionEndPayload
	UserPromptSubmits []*UserPromptSubmitPayload
}

// NewRecordingSink returns an empty RecordingSink.
func NewRecordingSink() *RecordingSink {
	return &RecordingSink{}
}

// OnStopFailure records p and returns Err (or nil).
func (s *RecordingSink) OnStopFailure(ctx context.Context, p *ratelimit.StopFailurePayload) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.StopFailures = append(s.StopFailures, p)
	s.mu.Unlock()
	return s.Err
}

// OnSessionStart records p and returns Err (or nil).
func (s *RecordingSink) OnSessionStart(ctx context.Context, p *SessionStartPayload) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.SessionStarts = append(s.SessionStarts, p)
	s.mu.Unlock()
	return s.Err
}

// OnSessionEnd records p and returns Err (or nil).
func (s *RecordingSink) OnSessionEnd(ctx context.Context, p *SessionEndPayload) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.SessionEnds = append(s.SessionEnds, p)
	s.mu.Unlock()
	return s.Err
}

// OnUserPromptSubmit records p and returns Err (or nil).
func (s *RecordingSink) OnUserPromptSubmit(ctx context.Context, p *UserPromptSubmitPayload) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.UserPromptSubmits = append(s.UserPromptSubmits, p)
	s.mu.Unlock()
	return s.Err
}
