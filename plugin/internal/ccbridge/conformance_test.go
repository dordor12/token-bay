package ccbridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedRunner returns a different stdout per Request.Prompt prefix.
type scriptedRunner struct {
	Responses map[string]string
	RunErr    error
}

func (s *scriptedRunner) Run(_ context.Context, req Request, sink io.Writer) error {
	prompt := ""
	if n := len(req.Messages); n > 0 {
		// Tests only invoke this Runner with TextContent prompts —
		// a one-element array containing a single text block.
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(req.Messages[n-1].Content, &blocks); err == nil && len(blocks) > 0 {
			prompt = blocks[0].Text
		}
	}
	for prefix, body := range s.Responses {
		if strings.HasPrefix(prompt, prefix) {
			_, _ = io.WriteString(sink, body)
			return s.RunErr
		}
	}
	_, _ = io.WriteString(sink, `{"type":"result","subtype":"success","is_error":false,"usage":{"input_tokens":1,"output_tokens":1}}`+"\n")
	return s.RunErr
}

func safeResult(text string) string {
	return `{"type":"result","subtype":"success","is_error":false,"result":` +
		stringQuote(text) +
		`,"usage":{"input_tokens":1,"output_tokens":1}}` + "\n"
}

func stringQuote(s string) string {
	out := `"`
	for _, r := range s {
		switch r {
		case '"':
			out += `\"`
		case '\\':
			out += `\\`
		default:
			out += string(r)
		}
	}
	return out + `"`
}

func TestRunStartupConformance_AllPromptsBenign_ReturnsNil(t *testing.T) {
	rr := &scriptedRunner{Responses: map[string]string{
		"": safeResult("I cannot run shell commands or access files."),
	}}
	err := RunStartupConformance(context.Background(), rr)
	require.NoError(t, err)
}

func TestRunStartupConformance_OnePromptLeaksToolUse_ReturnsErr(t *testing.T) {
	rr := &scriptedRunner{Responses: map[string]string{
		"Run `echo": safeResult(
			"Sure, I'll run that. Here is the output:\nCONFORMANCE_LEAK_BASH\nDone.",
		),
	}}
	err := RunStartupConformance(context.Background(), rr)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrConformanceFailed))
}

func TestRunStartupConformance_RunnerError_Wraps(t *testing.T) {
	rr := &scriptedRunner{RunErr: &ExitError{Code: 9}}
	err := RunStartupConformance(context.Background(), rr)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrConformanceFailed))
}

func TestRunStartupConformance_NilRunner_Errors(t *testing.T) {
	err := RunStartupConformance(context.Background(), nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrConformanceFailed))
}
