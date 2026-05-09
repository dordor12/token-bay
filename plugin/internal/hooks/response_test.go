package hooks

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmptyResponse_EncodesToEmptyJSON(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, EmptyResponse().Encode(&buf))
	// json.Encoder always appends a newline; trim it for the equality check.
	got := bytes.TrimRight(buf.Bytes(), "\n")
	assert.JSONEq(t, `{}`, string(got))
}

func TestResponse_Encode_OmitsZeroValues(t *testing.T) {
	r := Response{}
	var buf bytes.Buffer
	require.NoError(t, r.Encode(&buf))
	assert.JSONEq(t, `{}`, string(bytes.TrimRight(buf.Bytes(), "\n")))
}

func TestResponse_Encode_PopulatedFields(t *testing.T) {
	cont := false
	suppress := true
	stop := "user-cancelled"
	dec := DecisionApprove
	sys := "[token-bay] tracker offline"
	reason := "tracker-down"
	r := Response{
		Continue:       &cont,
		SuppressOutput: &suppress,
		StopReason:     &stop,
		Decision:       &dec,
		SystemMessage:  &sys,
		Reason:         &reason,
	}
	var buf bytes.Buffer
	require.NoError(t, r.Encode(&buf))
	var got map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	assert.Equal(t, false, got["continue"])
	assert.Equal(t, true, got["suppressOutput"])
	assert.Equal(t, "user-cancelled", got["stopReason"])
	assert.Equal(t, "approve", got["decision"])
	assert.Equal(t, "[token-bay] tracker offline", got["systemMessage"])
	assert.Equal(t, "tracker-down", got["reason"])
}

func TestResponse_Encode_HookSpecificOutputRoundTrips(t *testing.T) {
	// SessionStartHookSpecificOutputSchema (coreSchemas.ts:823):
	//   { hookEventName: 'SessionStart', additionalContext?: string, ... }
	r := Response{
		HookSpecificOutput: []byte(`{"hookEventName":"SessionStart","additionalContext":"token-bay active"}`),
	}
	var buf bytes.Buffer
	require.NoError(t, r.Encode(&buf))
	got := decodeResponseMap(t, buf.Bytes())
	hso, ok := got["hookSpecificOutput"].(map[string]any)
	require.True(t, ok, "hookSpecificOutput should be an object")
	assert.Equal(t, "SessionStart", hso["hookEventName"])
	assert.Equal(t, "token-bay active", hso["additionalContext"])
}

func decodeResponseMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	out := map[string]any{}
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func TestDecision_Constants_PinnedToSchema(t *testing.T) {
	// SyncHookJSONOutputSchema constrains decision to enum(['approve', 'block'])
	// at coreSchemas.ts:914. Constants must match those literals exactly.
	assert.Equal(t, Decision("approve"), DecisionApprove)
	assert.Equal(t, Decision("block"), DecisionBlock)
}
