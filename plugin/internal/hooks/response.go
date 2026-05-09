package hooks

import (
	"encoding/json"
	"io"
)

// Decision is the synchronous-output decision enum from
// SyncHookJSONOutputSchema (coreSchemas.ts:914) — "approve" or "block".
// Other values are not valid wire output and the host Claude Code will
// reject them.
type Decision string

// Decision values from SyncHookJSONOutputSchema.
const (
	DecisionApprove Decision = "approve"
	DecisionBlock   Decision = "block"
)

// Response mirrors SyncHookJSONOutputSchema (coreSchemas.ts:907-936). All
// fields are pointers so a zero Response encodes to `{}` — the no-op output
// every plugin hook should emit unless it explicitly wants to influence the
// host's behavior. Field tags match the camelCase wire form.
type Response struct {
	Continue       *bool     `json:"continue,omitempty"`
	SuppressOutput *bool     `json:"suppressOutput,omitempty"`
	StopReason     *string   `json:"stopReason,omitempty"`
	Decision       *Decision `json:"decision,omitempty"`
	SystemMessage  *string   `json:"systemMessage,omitempty"`
	Reason         *string   `json:"reason,omitempty"`
}

// EmptyResponse returns a zero-value Response. Useful as the default
// "do nothing" reply from a hook handler.
func EmptyResponse() Response {
	return Response{}
}

// Encode writes r as a single JSON object to w. Uses encoding/json's
// default newline-terminated encoder behavior.
func (r Response) Encode(w io.Writer) error {
	return json.NewEncoder(w).Encode(r)
}
