package ccbridge

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateMessages_RejectsInvalidJSONContent(t *testing.T) {
	err := ValidateMessages([]Message{
		{Role: RoleUser, Content: json.RawMessage("not json")},
	})
	require.Error(t, err)
}

func TestValidateMessages_AcceptsArrayContent(t *testing.T) {
	err := ValidateMessages([]Message{
		{Role: RoleUser, Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"x","content":"y"}]`)},
	})
	require.NoError(t, err)
}
