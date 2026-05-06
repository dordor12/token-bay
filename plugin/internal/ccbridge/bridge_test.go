package ccbridge

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubRunner is a fixture-driven Runner.
type stubRunner struct {
	Stdout []byte
	RunErr error
	Got    Request
}

func (s *stubRunner) Run(_ context.Context, req Request, sink io.Writer) error {
	s.Got = req
	if _, err := sink.Write(s.Stdout); err != nil {
		return err
	}
	return s.RunErr
}

func TestBridgeServe_HappyPath_StreamsAndExtractsUsage(t *testing.T) {
	stub := &stubRunner{Stdout: []byte(
		`{"type":"system","subtype":"init"}` + "\n" +
			`{"type":"result","subtype":"success","is_error":false,"usage":{"input_tokens":11,"output_tokens":22}}` + "\n",
	)}
	b := &Bridge{Runner: stub}

	var sink bytes.Buffer
	usage, err := b.Serve(context.Background(), Request{Prompt: "hi", Model: "claude-sonnet-4-6"}, &sink)
	require.NoError(t, err)
	assert.Equal(t, uint64(11), usage.InputTokens)
	assert.Equal(t, uint64(22), usage.OutputTokens)
	assert.Equal(t, "hi", stub.Got.Prompt)
	assert.Equal(t, "claude-sonnet-4-6", stub.Got.Model)
	assert.Contains(t, sink.String(), `"type":"result"`)
}

func TestBridgeServe_RejectsEmptyPrompt(t *testing.T) {
	b := &Bridge{Runner: &stubRunner{}}
	_, err := b.Serve(context.Background(), Request{Prompt: "", Model: "claude-sonnet-4-6"}, io.Discard)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidRequest))
}

func TestBridgeServe_RejectsEmptyModel(t *testing.T) {
	b := &Bridge{Runner: &stubRunner{}}
	_, err := b.Serve(context.Background(), Request{Prompt: "hi", Model: ""}, io.Discard)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidRequest))
}

func TestBridgeServe_RunnerError_Propagates(t *testing.T) {
	wantErr := &ExitError{Code: 7}
	stub := &stubRunner{Stdout: []byte(`{"type":"system"}` + "\n"), RunErr: wantErr}
	b := &Bridge{Runner: stub}

	var sink bytes.Buffer
	_, err := b.Serve(context.Background(), Request{Prompt: "hi", Model: "claude-sonnet-4-6"}, &sink)
	require.Error(t, err)
	var exit *ExitError
	require.True(t, errors.As(err, &exit))
	assert.Equal(t, 7, exit.Code)
	assert.Contains(t, sink.String(), `"type":"system"`)
}

func TestBridgeServe_NewBridgePanicsOnNilRunner(t *testing.T) {
	assert.Panics(t, func() { _ = NewBridge(nil) })
}
