package ccbridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Errors related to Messages validation.
var (
	// ErrEmptyMessages is returned when Request.Messages has zero entries.
	ErrEmptyMessages = errors.New("ccbridge: Messages must be non-empty")
	// ErrLastMessageNotUser is returned when the final message is not RoleUser.
	// claude responds to user turns; ending on assistant would leave the
	// model with no prompt to answer.
	ErrLastMessageNotUser = errors.New("ccbridge: last Message must have role user")
	// ErrInvalidRole is returned for messages whose Role is not
	// RoleUser or RoleAssistant.
	ErrInvalidRole = errors.New("ccbridge: Message.Role must be user or assistant")
	// ErrEmptyContent is returned for messages with empty Content.
	ErrEmptyContent = errors.New("ccbridge: Message.Content must be non-empty")
)

// ValidateMessages checks the Messages slice for shape violations. It
// is called by Bridge.Serve before subprocess launch so callers see a
// clean error rather than a confusing claude-side stdin parse failure.
func ValidateMessages(msgs []Message) error {
	if len(msgs) == 0 {
		return ErrEmptyMessages
	}
	for i, m := range msgs {
		if m.Role != RoleUser && m.Role != RoleAssistant {
			return fmt.Errorf("%w: index %d role %q", ErrInvalidRole, i, m.Role)
		}
		if m.Content == "" {
			return fmt.Errorf("%w: index %d", ErrEmptyContent, i)
		}
	}
	if msgs[len(msgs)-1].Role != RoleUser {
		return ErrLastMessageNotUser
	}
	return nil
}

// streamJSONInputEvent is the wire shape the claude CLI consumes on
// stdin under --input-format stream-json. Text-only v1 — content is
// always a single text block.
type streamJSONInputEvent struct {
	Type    string                 `json:"type"`
	Message streamJSONInputMessage `json:"message"`
}

type streamJSONInputMessage struct {
	Role    string                 `json:"role"`
	Content []streamJSONInputBlock `json:"content"`
}

type streamJSONInputBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// EncodeMessages writes msgs to w as line-delimited stream-json input
// events suitable for `claude -p --input-format stream-json` stdin.
// Returns the first write error.
func EncodeMessages(w io.Writer, msgs []Message) error {
	enc := json.NewEncoder(w) // appends a trailing newline per Encode call
	for i, m := range msgs {
		ev := streamJSONInputEvent{
			Type: m.Role, // "user" or "assistant" — same wire token as the Role
			Message: streamJSONInputMessage{
				Role: m.Role,
				Content: []streamJSONInputBlock{
					{Type: "text", Text: m.Content},
				},
			},
		}
		if err := enc.Encode(ev); err != nil {
			return fmt.Errorf("ccbridge: encode message %d: %w", i, err)
		}
	}
	return nil
}
