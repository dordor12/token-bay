package ccbridge

import (
	"encoding/json"
	"errors"
	"fmt"
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
		if len(m.Content) == 0 {
			return fmt.Errorf("%w: index %d", ErrEmptyContent, i)
		}
		// Reject obviously-malformed payloads up front. Content must
		// be valid JSON; otherwise claude will fail to parse the
		// stream-json line and the error surface is much worse.
		if !json.Valid(m.Content) {
			return fmt.Errorf("%w: index %d: not valid JSON", ErrEmptyContent, i)
		}
	}
	if msgs[len(msgs)-1].Role != RoleUser {
		return ErrLastMessageNotUser
	}
	return nil
}
