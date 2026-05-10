package ccbridge

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func userOnly(text string) []Message {
	return []Message{{Role: RoleUser, Content: TextContent(text)}}
}

func TestBuildArgv_HappyPath_IncludesAirtightFlags(t *testing.T) {
	got := BuildArgv(Request{
		Messages: userOnly("hello"),
		Model:    "claude-sonnet-4-6",
	}, "ABCD-SESSION", "the new prompt")

	// Print mode + positional prompt.
	assert.Contains(t, got, "-p")
	assert.Contains(t, got, "the new prompt")

	// Output stream-json + verbose still present (we parse stdout).
	assert.Contains(t, got, "--output-format")
	assert.Contains(t, got, "stream-json")
	assert.Contains(t, got, "--verbose")

	// Stream-json INPUT must NOT be present anymore.
	assert.NotContains(t, got, "--input-format")

	// Airtight tool/MCP/settings flags unchanged.
	assert.Contains(t, got, "--tools")
	assert.Contains(t, got, "--disallowedTools")
	assert.Contains(t, got, "*")
	assert.Contains(t, got, "--mcp-config")
	assert.Contains(t, got, `{"mcpServers":{}}`)
	assert.Contains(t, got, "--strict-mcp-config")
	assert.Contains(t, got, "--settings")
	assert.Contains(t, got, `{"hooks":{}}`)

	// New: resume + no-persist.
	assert.Contains(t, got, "--resume")
	assert.Contains(t, got, "ABCD-SESSION")
	assert.Contains(t, got, "--no-session-persistence")

	// Model passes through.
	assert.Contains(t, got, "--model")
	assert.Contains(t, got, "claude-sonnet-4-6")
}

func TestBuildArgv_PositionalPromptIsLastArg(t *testing.T) {
	got := BuildArgv(Request{
		Messages: userOnly("ignored — only Model matters here"),
		Model:    "claude-haiku-4-5",
	}, "S1", "the actual prompt text")
	require.NotEmpty(t, got)
	assert.Equal(t, "the actual prompt text", got[len(got)-1])
}

func TestBuildArgv_OmitsResumeWhenSessionIDEmpty(t *testing.T) {
	got := BuildArgv(Request{Messages: userOnly("hi"), Model: "claude-haiku-4-5"}, "", "hello")
	assert.NotContains(t, got, "--resume")
	assert.Contains(t, got, "--no-session-persistence")
}

func TestBuildArgv_SystemPrompt_AddedWhenSet(t *testing.T) {
	got := BuildArgv(Request{
		System:   "you are PINGBOT",
		Messages: userOnly("hi"),
		Model:    "claude-sonnet-4-6",
	}, "sid", "prompt-text")
	assert.Contains(t, got, "--system-prompt")
	assert.Contains(t, got, "you are PINGBOT")
}

func TestBuildArgv_NoSystemPrompt_FlagOmitted(t *testing.T) {
	got := BuildArgv(Request{Messages: userOnly("hi"), Model: "claude-sonnet-4-6"}, "sid", "prompt-text")
	for i, a := range got {
		assert.NotEqual(t, "--system-prompt", a, "unexpected --system-prompt at %d", i)
	}
}

func TestBuildArgv_RejectsEmptyModel(t *testing.T) {
	got := BuildArgv(Request{Messages: userOnly("hi"), Model: ""}, "sid", "prompt-text")
	for i, a := range got {
		assert.NotEqual(t, "--model", a, "unexpected --model at %d", i)
	}
}

func TestAirtightFlags_PinnedConstants(t *testing.T) {
	assert.Equal(t, "-p", FlagPrint)
	assert.Equal(t, "--system-prompt", FlagSystemPrompt)
	assert.Equal(t, "--tools", FlagTools)
	assert.Equal(t, "", ToolsNone)
	assert.Equal(t, "--disallowedTools", FlagDisallowedTools)
	assert.Equal(t, "*", DisallowedToolsAll)
	assert.Equal(t, "--mcp-config", FlagMCPConfig)
	assert.Equal(t, `{"mcpServers":{}}`, MCPConfigNull)
	assert.Equal(t, "--strict-mcp-config", FlagStrictMCPConfig)
	assert.Equal(t, "--settings", FlagSettings)
	assert.Equal(t, `{"hooks":{}}`, SettingsNoHooks)
	assert.Equal(t, "user", RoleUser)
	assert.Equal(t, "assistant", RoleAssistant)
	assert.Equal(t, "--resume", FlagResume)
	assert.Equal(t, "--no-session-persistence", FlagNoSessionPersistence)
}
