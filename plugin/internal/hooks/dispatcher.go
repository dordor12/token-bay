package hooks

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/rs/zerolog"

	"github.com/token-bay/token-bay/plugin/internal/ratelimit"
)

// ErrUnknownEvent is returned by Handle when eventName is not one of the
// four EventName* constants this package handles. Callers may wrap or
// translate it; downstream sniffing uses errors.Is.
var ErrUnknownEvent = errors.New("hooks: unknown event")

// ErrInvalidConfig is returned by Handle when the Dispatcher is missing
// required fields (currently: Sink). Lets the cmd layer fail-fast with a
// clear message.
var ErrInvalidConfig = errors.New("hooks: invalid dispatcher config")

// Dispatcher routes a parsed hook payload to its Sink and writes a
// Response on the supplied io.Writer. Construct with a non-nil Sink; the
// zero Logger is fine.
type Dispatcher struct {
	// Sink receives parsed payloads. Required.
	Sink Sink

	// Logger is used only for low-volume diagnostic events (sink errors).
	// The zero value is a no-op.
	Logger zerolog.Logger
}

// Handle parses a single hook event of the given name from in, dispatches
// the payload to the configured Sink, and writes a Response to out.
//
// On parse failure or unknown event, Handle returns the error and writes
// nothing to out — the caller decides whether to surface a hook-level
// reply at all (typically: nothing, log to stderr, exit non-zero).
//
// On Sink error, Handle returns the error AND writes an EmptyResponse to
// out: the plugin's design contract is that hook observation must never
// block the host Claude Code turn, even when the supervisor is unhealthy.
func (d *Dispatcher) Handle(ctx context.Context, eventName string, in io.Reader, out io.Writer) error {
	if d.Sink == nil {
		return fmt.Errorf("%w: Sink must be non-nil", ErrInvalidConfig)
	}

	switch eventName {
	case EventNameStopFailure:
		p, err := ratelimit.ParseStopFailurePayload(in)
		if err != nil {
			return err
		}
		return d.finish(out, d.Sink.OnStopFailure(ctx, p))
	case EventNameSessionStart:
		p, err := ParseSessionStart(in)
		if err != nil {
			return err
		}
		return d.finish(out, d.Sink.OnSessionStart(ctx, p))
	case EventNameSessionEnd:
		p, err := ParseSessionEnd(in)
		if err != nil {
			return err
		}
		return d.finish(out, d.Sink.OnSessionEnd(ctx, p))
	case EventNameUserPromptSubmit:
		p, err := ParseUserPromptSubmit(in)
		if err != nil {
			return err
		}
		return d.finish(out, d.Sink.OnUserPromptSubmit(ctx, p))
	default:
		return fmt.Errorf("%w: %q", ErrUnknownEvent, eventName)
	}
}

// finish writes the no-op Response and returns the sink's error. Both
// outcomes are non-fatal to the host: the response is still emitted so
// Claude Code's hook executor sees a clean reply, and the sink error is
// surfaced upward for logging/exit-code policy in the cmd layer.
func (d *Dispatcher) finish(out io.Writer, sinkErr error) error {
	if encErr := EmptyResponse().Encode(out); encErr != nil {
		if sinkErr != nil {
			return errors.Join(sinkErr, fmt.Errorf("hooks: encode response: %w", encErr))
		}
		return fmt.Errorf("hooks: encode response: %w", encErr)
	}
	if sinkErr != nil {
		d.Logger.Error().Err(sinkErr).Msg("hooks: sink returned error")
	}
	return sinkErr
}
