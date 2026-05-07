package ccbridge

// Pinned flag strings. Any change MUST ship with an updated conformance
// suite per plugin spec §6.2 + §12.
const (
	FlagPrint           = "-p"
	FlagModel           = "--model"
	FlagOutputFormat    = "--output-format"
	OutputFormatStream  = "stream-json"
	FlagInputFormat     = "--input-format"
	InputFormatStream   = "stream-json"
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
)

// Role values for Message.Role. Mirrors the Anthropic /v1/messages API.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// Message is one turn in the conversation handed to the bridge. v1 is
// text-only — Content is the message text; multimodal (images, etc.)
// is out of scope. Role must be RoleUser or RoleAssistant; the
// system prompt is carried separately on Request.System (per the
// Anthropic /v1/messages shape, system is not a message role).
type Message struct {
	Role    string
	Content string
}

// Request describes a single seeder-side bridge invocation. The bridge
// hands the structured conversation to claude via stream-json input,
// matching the fidelity of a native Anthropic /v1/messages call —
// the model sees the full prior history and generates a response to
// the final user turn.
type Request struct {
	// System is the optional system prompt. Empty means no system
	// prompt. Passed via --system-prompt.
	System string
	// Messages is the conversation history. Must be non-empty and
	// must end with a RoleUser message — that is the turn the model
	// will respond to. Prior assistant turns are preserved as
	// context.
	Messages []Message
	// Model is the Anthropic model id requested by the consumer.
	Model string
}

// BuildArgv returns the argv (excluding the binary path) for a single
// `claude -p` invocation with the airtight flag set. The conversation
// itself flows through subprocess stdin (see ExecRunner), not argv.
func BuildArgv(req Request) []string {
	argv := []string{
		FlagPrint,
		FlagInputFormat, InputFormatStream,
		FlagOutputFormat, OutputFormatStream,
		FlagVerbose,
		FlagTools, ToolsNone,
		FlagDisallowedTools, DisallowedToolsAll,
		FlagMCPConfig, MCPConfigNull,
		FlagStrictMCPConfig,
		FlagSettings, SettingsNoHooks,
	}
	if req.System != "" {
		argv = append(argv, FlagSystemPrompt, req.System)
	}
	if req.Model != "" {
		argv = append(argv, FlagModel, req.Model)
	}
	return argv
}
