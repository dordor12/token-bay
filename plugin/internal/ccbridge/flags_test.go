package ccbridge

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func userOnly(text string) []Message {
	return []Message{{Role: RoleUser, Content: text}}
}

func TestBuildArgv_HappyPath_IncludesAirtightFlags(t *testing.T) {
	got := BuildArgv(Request{Messages: userOnly("hello"), Model: "claude-sonnet-4-6"})

	// The flags spec §6.2 says are non-negotiable for safety.
	assert.Contains(t, got, "-p")
	assert.Contains(t, got, "--input-format")
	assert.Contains(t, got, "stream-json")
	assert.Contains(t, got, "--output-format")
	assert.Contains(t, got, "--verbose")
	assert.Contains(t, got, "--model")
	assert.Contains(t, got, "claude-sonnet-4-6")
	assert.Contains(t, got, "--tools")
	assert.Contains(t, got, "--disallowedTools")
	assert.Contains(t, got, "*")
	assert.Contains(t, got, "--mcp-config")
	assert.Contains(t, got, `{"mcpServers":{}}`)
	assert.Contains(t, got, "--strict-mcp-config")
	assert.Contains(t, got, "--settings")
	assert.Contains(t, got, `{"hooks":{}}`)

	// No positional prompt: the conversation goes via stdin (stream-json
	// input), not argv. -p must NOT be followed by a non-flag token.
	for i, a := range got {
		if a == "-p" && i+1 < len(got) {
			next := got[i+1]
			assert.True(t, len(next) >= 2 && next[:2] == "--",
				"unexpected token after -p: %q", next)
		}
	}
}

func TestBuildArgv_SystemPrompt_AddedWhenSet(t *testing.T) {
	got := BuildArgv(Request{
		System:   "you are PINGBOT",
		Messages: userOnly("hi"),
		Model:    "claude-sonnet-4-6",
	})
	assert.Contains(t, got, "--system-prompt")
	assert.Contains(t, got, "you are PINGBOT")
}

func TestBuildArgv_NoSystemPrompt_FlagOmitted(t *testing.T) {
	got := BuildArgv(Request{Messages: userOnly("hi"), Model: "claude-sonnet-4-6"})
	for i, a := range got {
		assert.NotEqual(t, "--system-prompt", a, "unexpected --system-prompt at %d", i)
	}
}

func TestBuildArgv_RejectsEmptyModel(t *testing.T) {
	got := BuildArgv(Request{Messages: userOnly("hi"), Model: ""})
	for i, a := range got {
		assert.NotEqual(t, "--model", a, "unexpected --model at %d", i)
	}
}

func TestAirtightFlags_PinnedConstants(t *testing.T) {
	assert.Equal(t, "-p", FlagPrint)
	assert.Equal(t, "--input-format", FlagInputFormat)
	assert.Equal(t, "stream-json", InputFormatStream)
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
}
