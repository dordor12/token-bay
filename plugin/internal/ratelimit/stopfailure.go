package ratelimit

import (
	"encoding/json"
	"fmt"
	"io"
)

// StopFailurePayload mirrors Claude Code's StopFailureHookInput schema at
// src/entrypoints/sdk/coreSchemas.ts:529-538 combined with BaseHookInputSchema
// at :387-411. Optional fields are pointer-to-string so callers can
// distinguish absent from empty.
type StopFailurePayload struct {
	SessionID            string  `json:"session_id"`
	TranscriptPath       string  `json:"transcript_path"`
	CWD                  string  `json:"cwd"`
	PermissionMode       *string `json:"permission_mode,omitempty"`
	AgentID              *string `json:"agent_id,omitempty"`
	AgentType            *string `json:"agent_type,omitempty"`
	HookEventName        string  `json:"hook_event_name"`
	Error                string  `json:"error"`
	ErrorDetails         *string `json:"error_details,omitempty"`
	LastAssistantMessage *string `json:"last_assistant_message,omitempty"`
}

// ParseStopFailurePayload reads JSON from r and returns the typed payload.
// Returns an error if the JSON is malformed or hook_event_name is not
// literally "StopFailure" (defensive against misuse on other hook events).
func ParseStopFailurePayload(r io.Reader) (*StopFailurePayload, error) {
	var p StopFailurePayload
	if err := json.NewDecoder(r).Decode(&p); err != nil {
		return nil, fmt.Errorf("ratelimit: parse StopFailure payload: %w", err)
	}
	if p.HookEventName != "StopFailure" {
		return nil, fmt.Errorf("ratelimit: expected hook_event_name=StopFailure, got %q", p.HookEventName)
	}
	return &p, nil
}
