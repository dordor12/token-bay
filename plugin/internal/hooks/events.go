package hooks

import (
	"encoding/json"
	"fmt"
	"io"
)

// SessionStartPayload mirrors SessionStartHookInputSchema (coreSchemas.ts:493)
// composed with BaseHookInputSchema (:387). Source is one of: "startup",
// "resume", "clear", "compact" — left as a string so a future enum value
// doesn't break parsing; the sidecar may inspect it for richer behavior.
type SessionStartPayload struct {
	SessionID      string  `json:"session_id"`
	TranscriptPath string  `json:"transcript_path"`
	CWD            string  `json:"cwd"`
	PermissionMode *string `json:"permission_mode,omitempty"`
	AgentID        *string `json:"agent_id,omitempty"`
	HookEventName  string  `json:"hook_event_name"`
	Source         string  `json:"source"`
	AgentType      *string `json:"agent_type,omitempty"`
	Model          *string `json:"model,omitempty"`
}

// SessionEndPayload mirrors SessionEndHookInputSchema (coreSchemas.ts:758).
// Reason is one of EXIT_REASONS ("clear", "resume", "logout",
// "prompt_input_exit", "other", "bypass_permissions_disabled"); kept as
// string for forward-compat. AgentType comes from BaseHookInputSchema.
type SessionEndPayload struct {
	SessionID      string  `json:"session_id"`
	TranscriptPath string  `json:"transcript_path"`
	CWD            string  `json:"cwd"`
	PermissionMode *string `json:"permission_mode,omitempty"`
	AgentID        *string `json:"agent_id,omitempty"`
	AgentType      *string `json:"agent_type,omitempty"`
	HookEventName  string  `json:"hook_event_name"`
	Reason         string  `json:"reason"`
}

// UserPromptSubmitPayload mirrors UserPromptSubmitHookInputSchema
// (coreSchemas.ts:484). AgentType comes from BaseHookInputSchema.
type UserPromptSubmitPayload struct {
	SessionID      string  `json:"session_id"`
	TranscriptPath string  `json:"transcript_path"`
	CWD            string  `json:"cwd"`
	PermissionMode *string `json:"permission_mode,omitempty"`
	AgentID        *string `json:"agent_id,omitempty"`
	AgentType      *string `json:"agent_type,omitempty"`
	HookEventName  string  `json:"hook_event_name"`
	Prompt         string  `json:"prompt"`
}

// ParseSessionStart decodes r and verifies hook_event_name == "SessionStart".
func ParseSessionStart(r io.Reader) (*SessionStartPayload, error) {
	var p SessionStartPayload
	if err := json.NewDecoder(r).Decode(&p); err != nil {
		return nil, fmt.Errorf("hooks: parse SessionStart payload: %w", err)
	}
	if p.HookEventName != EventNameSessionStart {
		return nil, fmt.Errorf("hooks: expected hook_event_name=%s, got %q", EventNameSessionStart, p.HookEventName)
	}
	return &p, nil
}

// ParseSessionEnd decodes r and verifies hook_event_name == "SessionEnd".
func ParseSessionEnd(r io.Reader) (*SessionEndPayload, error) {
	var p SessionEndPayload
	if err := json.NewDecoder(r).Decode(&p); err != nil {
		return nil, fmt.Errorf("hooks: parse SessionEnd payload: %w", err)
	}
	if p.HookEventName != EventNameSessionEnd {
		return nil, fmt.Errorf("hooks: expected hook_event_name=%s, got %q", EventNameSessionEnd, p.HookEventName)
	}
	return &p, nil
}

// ParseUserPromptSubmit decodes r and verifies hook_event_name ==
// "UserPromptSubmit".
func ParseUserPromptSubmit(r io.Reader) (*UserPromptSubmitPayload, error) {
	var p UserPromptSubmitPayload
	if err := json.NewDecoder(r).Decode(&p); err != nil {
		return nil, fmt.Errorf("hooks: parse UserPromptSubmit payload: %w", err)
	}
	if p.HookEventName != EventNameUserPromptSubmit {
		return nil, fmt.Errorf("hooks: expected hook_event_name=%s, got %q", EventNameUserPromptSubmit, p.HookEventName)
	}
	return &p, nil
}
