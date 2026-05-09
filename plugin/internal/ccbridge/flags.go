package ccbridge

import (
	"crypto/ed25519"
	"encoding/json"
)

// Pinned flag strings. Any change MUST ship with an updated conformance
// suite per plugin spec §6.2 + §12.
const (
	FlagPrint           = "-p"
	FlagModel           = "--model"
	FlagOutputFormat    = "--output-format"
	OutputFormatStream  = "stream-json"
	FlagVerbose         = "--verbose"
	FlagSystemPrompt    = "--system-prompt"
	FlagDisallowedTools = "--disallowedTools"
	DisallowedToolsAll  = "*"
	// FlagTools is the explicit allow-list. The spec template uses
	// --disallowedTools "*" but current Claude Code (2.1.x) does not
	// honor "*" as a wildcard — only enumerated names. --tools ""
	// (empty allow-list) is the airtight equivalent and is the
	// load-bearing flag for the safety argument.
	FlagTools     = "--tools"
	ToolsNone     = ""
	FlagMCPConfig = "--mcp-config"
	// MCPConfigNull is a valid-JSON empty MCP server map. Spec §6.2 used
	// "/dev/null" as the conceptual null config but current Claude Code
	// validates the file as JSON and rejects the literal device file.
	// {"mcpServers":{}} is the minimal payload the validator accepts.
	MCPConfigNull = `{"mcpServers":{}}`
	// FlagStrictMCPConfig restricts MCP servers to those declared by
	// FlagMCPConfig (i.e., none, given MCPConfigNull). Without this,
	// Claude Code merges in user settings.json's MCP servers.
	FlagStrictMCPConfig = "--strict-mcp-config"
	FlagSettings        = "--settings"
	SettingsNoHooks     = `{"hooks":{}}`
	// FlagResume + sessionID makes claude load history from
	// $HOME/.claude/projects/<sanitized-cwd>/<sessionID>.jsonl. The
	// bridge writes that file synthetically before exec; loading
	// goes through the same loadConversationForResume code path
	// real session resumes use, preserving tool blocks and content
	// shape verbatim.
	FlagResume = "--resume"
	// FlagNoSessionPersistence stops claude from appending its own
	// records to the session file we wrote. The bridge controls the
	// file's contents end-to-end; suppressing writes keeps it that
	// way and avoids accumulating cruft in the per-client folder.
	FlagNoSessionPersistence = "--no-session-persistence"
)

// Role values for Message.Role. Mirrors the Anthropic /v1/messages API.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// Message is one turn in the conversation handed to the bridge. Role
// must be RoleUser or RoleAssistant; the system prompt is carried
// separately on Request.System (per the Anthropic /v1/messages shape,
// system is not a message role).
//
// Content is the JSON value the message's "content" field will carry
// on the wire. It can be either:
//   - a JSON string (plain text turn) — produced by TextContent, or
//   - a JSON array of content blocks ([{"type":"text",...}, {"type":
//     "tool_use",...}, {"type":"tool_result",...}, ...]) — required
//     to faithfully transmit a conversation that has used tools.
//
// WriteSessionFile writes Content verbatim into each session-file
// record's "message.content" field, so callers control the exact
// shape claude sees on resume.
type Message struct {
	Role    string
	Content json.RawMessage
}

// TextContent returns the json.RawMessage encoding of a plain text
// turn — a one-element array containing a single text content block:
//
//	[{"type":"text","text":"<s>"}]
//
// Claude's stream-json input parser expects message.content to be an
// array (it calls .some() on it), so TextContent wraps the text in
// the canonical content-block array shape rather than emitting a
// raw JSON string. Use it for the common text-only case:
//
//	{Role: RoleUser, Content: TextContent("hello")}
//
// For tool_use / tool_result / mixed-block turns, build the JSON
// array yourself and assign it directly to Content.
func TextContent(s string) json.RawMessage {
	block := struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{Type: "text", Text: s}
	b, err := json.Marshal([]any{block})
	if err != nil {
		// Marshalling this fixed shape can't fail; if it does,
		// return nil so ValidateMessages surfaces a clean error.
		return nil
	}
	return b
}

// Request describes a single seeder-side bridge invocation. The bridge
// writes Messages[:len-1] as a synthetic Claude Code session JSONL
// file under $HOME/.claude/projects/<sanitized-cwd>/<sessionID>.jsonl
// and invokes claude with --resume <sessionID> plus the last user
// turn as a positional -p argument. Loading on the claude side goes
// through the same loadConversationForResume path real resumes use,
// so tool blocks and content-block shape are preserved verbatim.
type Request struct {
	// System is the optional system prompt. Empty means no system
	// prompt. Passed via --system-prompt.
	System string
	// Messages is the conversation history. Must be non-empty and
	// must end with a RoleUser message — that final user turn becomes
	// the positional -p prompt; prior turns become the resumed
	// session file.
	Messages []Message
	// Model is the Anthropic model id requested by the consumer.
	Model string
	// ClientPubkey identifies which P2P consumer this request is
	// being served on behalf of. Used to derive the per-client
	// storage folder under SeederRoot. Required for production;
	// some unit tests can pass a synthetic 32-zero-byte key.
	ClientPubkey ed25519.PublicKey
}

// BuildArgv returns the argv (excluding the binary path) for a single
// `claude -p` invocation with the airtight flag set. sessionID is the
// session-file UUID to resume; pass "" to skip --resume (useful for
// initial-prompt invocations with no prior history). prompt is the
// positional argument claude treats as the new user turn.
func BuildArgv(req Request, sessionID, prompt string) []string {
	argv := []string{
		FlagPrint,
		FlagOutputFormat, OutputFormatStream,
		FlagVerbose,
		FlagTools, ToolsNone,
		FlagDisallowedTools, DisallowedToolsAll,
		FlagMCPConfig, MCPConfigNull,
		FlagStrictMCPConfig,
		FlagSettings, SettingsNoHooks,
		FlagNoSessionPersistence,
	}
	if sessionID != "" {
		argv = append(argv, FlagResume, sessionID)
	}
	if req.System != "" {
		argv = append(argv, FlagSystemPrompt, req.System)
	}
	if req.Model != "" {
		argv = append(argv, FlagModel, req.Model)
	}
	// Positional prompt LAST — claude treats the trailing positional
	// arg as the value of -p when omitted from --print's optional arg.
	argv = append(argv, prompt)
	return argv
}
