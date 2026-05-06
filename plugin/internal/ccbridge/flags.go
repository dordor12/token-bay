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
	FlagMCPConfig       = "--mcp-config"
	MCPConfigNull       = "/dev/null"
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
		FlagDisallowedTools, DisallowedToolsAll,
		FlagMCPConfig, MCPConfigNull,
		FlagSettings, SettingsNoHooks,
	}
	if req.Model != "" {
		argv = append(argv, FlagModel, req.Model)
	}
	return argv
}
