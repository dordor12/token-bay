package ccbridge

// Pinned flag strings. Any change MUST ship with an updated conformance
// suite per plugin spec §6.2 + §12.
const (
	FlagPrompt          = "-p"
	FlagModel           = "--model"
	FlagOutputFormat    = "--output-format"
	OutputFormatStream  = "stream-json"
	FlagVerbose         = "--verbose"
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

// Request describes a single seeder-side bridge invocation. The bridge
// does not interpret the conversation context — the caller hands over a
// flat prompt string.
type Request struct {
	Prompt string
	Model  string
}

// BuildArgv returns the argv (excluding the binary path) for a single
// `claude -p` invocation with the airtight flag set.
func BuildArgv(req Request) []string {
	argv := []string{
		FlagPrompt, req.Prompt,
		FlagOutputFormat, OutputFormatStream,
		FlagVerbose,
		FlagTools, ToolsNone,
		FlagDisallowedTools, DisallowedToolsAll,
		FlagMCPConfig, MCPConfigNull,
		FlagStrictMCPConfig,
		FlagSettings, SettingsNoHooks,
	}
	if req.Model != "" {
		argv = append(argv, FlagModel, req.Model)
	}
	return argv
}
