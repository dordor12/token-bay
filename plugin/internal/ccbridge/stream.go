package ccbridge

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Usage is the canonical token count returned by a bridge call.
type Usage struct {
	InputTokens  uint64
	OutputTokens uint64
}

// ErrNoResult is returned when the stream-json output ends without a
// `result` event — the bridge could not extract canonical usage.
// Callers may still have received useful bytes in their sink.
var ErrNoResult = errors.New("ccbridge: stream ended without result event")

// streamResult is the minimal shape we need from a Claude Code
// stream-json `result` event. Unknown fields are ignored.
type streamResult struct {
	Type  string `json:"type"`
	Usage struct {
		InputTokens  uint64 `json:"input_tokens"`
		OutputTokens uint64 `json:"output_tokens"`
	} `json:"usage"`
}

// ParseStreamJSON reads line-delimited JSON from r and forwards every
// byte verbatim to sink. While forwarding, it watches for the trailing
// `result` event and extracts the token counts. Malformed JSON lines
// are forwarded but otherwise ignored.
func ParseStreamJSON(r io.Reader, sink io.Writer) (Usage, error) {
	br := bufio.NewReader(r)
	var (
		usage    Usage
		seenRes  bool
		writeErr error
	)
	for {
		line, readErr := br.ReadBytes('\n')
		if len(line) > 0 && writeErr == nil {
			if _, err := sink.Write(line); err != nil {
				writeErr = fmt.Errorf("ccbridge: write to sink: %w", err)
			}
		}
		if len(line) > 0 {
			body := line
			if body[len(body)-1] == '\n' {
				body = body[:len(body)-1]
			}
			if len(body) > 0 && body[0] == '{' {
				var probe streamResult
				if err := json.Unmarshal(body, &probe); err == nil && probe.Type == "result" {
					usage = Usage{
						InputTokens:  probe.Usage.InputTokens,
						OutputTokens: probe.Usage.OutputTokens,
					}
					seenRes = true
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			if writeErr != nil {
				return usage, writeErr
			}
			return usage, fmt.Errorf("ccbridge: read stream: %w", readErr)
		}
	}
	if writeErr != nil {
		return usage, writeErr
	}
	if !seenRes {
		return usage, ErrNoResult
	}
	return usage, nil
}
