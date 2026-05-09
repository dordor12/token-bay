package hooks

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/ratelimit"
)

// Compile-time assertion: both bundled implementations satisfy Sink.
var (
	_ Sink = (*NopSink)(nil)
	_ Sink = (*RecordingSink)(nil)
)

func TestNopSink_AllMethods_NilError(t *testing.T) {
	s := &NopSink{}
	ctx := context.Background()
	require.NoError(t, s.OnStopFailure(ctx, &ratelimit.StopFailurePayload{}))
	require.NoError(t, s.OnSessionStart(ctx, &SessionStartPayload{}))
	require.NoError(t, s.OnSessionEnd(ctx, &SessionEndPayload{}))
	require.NoError(t, s.OnUserPromptSubmit(ctx, &UserPromptSubmitPayload{}))
}

func TestRecordingSink_RecordsInOrder(t *testing.T) {
	s := NewRecordingSink()
	ctx := context.Background()

	require.NoError(t, s.OnSessionStart(ctx, &SessionStartPayload{SessionID: "s-1", Source: "startup"}))
	require.NoError(t, s.OnUserPromptSubmit(ctx, &UserPromptSubmitPayload{SessionID: "s-1", Prompt: "hi"}))
	require.NoError(t, s.OnStopFailure(ctx, &ratelimit.StopFailurePayload{SessionID: "s-1", Error: ratelimit.ErrorRateLimit}))
	require.NoError(t, s.OnSessionEnd(ctx, &SessionEndPayload{SessionID: "s-1", Reason: "logout"}))

	require.Len(t, s.SessionStarts, 1)
	assert.Equal(t, "s-1", s.SessionStarts[0].SessionID)
	require.Len(t, s.UserPromptSubmits, 1)
	assert.Equal(t, "hi", s.UserPromptSubmits[0].Prompt)
	require.Len(t, s.StopFailures, 1)
	assert.Equal(t, ratelimit.ErrorRateLimit, s.StopFailures[0].Error)
	require.Len(t, s.SessionEnds, 1)
	assert.Equal(t, "logout", s.SessionEnds[0].Reason)
}

func TestRecordingSink_PropagatesProgrammedError(t *testing.T) {
	want := errors.New("sink boom")
	s := NewRecordingSink()
	s.Err = want

	ctx := context.Background()
	assert.ErrorIs(t, s.OnStopFailure(ctx, &ratelimit.StopFailurePayload{}), want)
	assert.ErrorIs(t, s.OnSessionStart(ctx, &SessionStartPayload{}), want)
	assert.ErrorIs(t, s.OnSessionEnd(ctx, &SessionEndPayload{}), want)
	assert.ErrorIs(t, s.OnUserPromptSubmit(ctx, &UserPromptSubmitPayload{}), want)

	// Event is still recorded even when sink returns programmed error —
	// matches the "fire-and-record" semantics tests downstream depend on.
	assert.Len(t, s.StopFailures, 1)
	assert.Len(t, s.SessionStarts, 1)
	assert.Len(t, s.SessionEnds, 1)
	assert.Len(t, s.UserPromptSubmits, 1)
}

func TestRecordingSink_RespectsContextCancellation(t *testing.T) {
	s := NewRecordingSink()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := s.OnStopFailure(ctx, &ratelimit.StopFailurePayload{})
	assert.ErrorIs(t, err, context.Canceled)
	// Cancelled events are NOT recorded — they never executed the sink body.
	assert.Empty(t, s.StopFailures)
}
