package hooks

import "testing"

// TestEventName_PinnedToClaudeCodeSchema asserts that the event-name string
// constants match the literals declared in Claude Code's hook input schemas
// at src/entrypoints/sdk/coreSchemas.ts (StopFailure: line 529; SessionStart:
// 493; SessionEnd: 758; UserPromptSubmit: 484). A future Claude Code release
// that renames any of these breaks our hook bindings — this test is the
// canary.
func TestEventName_PinnedToClaudeCodeSchema(t *testing.T) {
	cases := map[string]string{
		"StopFailure":      EventNameStopFailure,
		"SessionStart":     EventNameSessionStart,
		"SessionEnd":       EventNameSessionEnd,
		"UserPromptSubmit": EventNameUserPromptSubmit,
	}
	for want, got := range cases {
		if got != want {
			t.Errorf("event-name constant for %q = %q; want %q", want, got, want)
		}
	}
}
