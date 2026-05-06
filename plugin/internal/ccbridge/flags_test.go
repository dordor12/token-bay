package ccbridge

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildArgv_HappyPath_IncludesAirtightFlags(t *testing.T) {
	got := BuildArgv(Request{Prompt: "hello", Model: "claude-sonnet-4-6"})

	// The flags spec §6.2 says are non-negotiable for safety.
	assert.Contains(t, got, "-p")
	assert.Contains(t, got, "hello")
	assert.Contains(t, got, "--model")
	assert.Contains(t, got, "claude-sonnet-4-6")
	assert.Contains(t, got, "--output-format")
	assert.Contains(t, got, "stream-json")
	assert.Contains(t, got, "--verbose")
	assert.Contains(t, got, "--tools")
	assert.Contains(t, got, "--disallowedTools")
	assert.Contains(t, got, "*")
	assert.Contains(t, got, "--mcp-config")
	assert.Contains(t, got, `{"mcpServers":{}}`)
	assert.Contains(t, got, "--strict-mcp-config")
	assert.Contains(t, got, "--settings")
	assert.Contains(t, got, `{"hooks":{}}`)
}

func TestBuildArgv_PromptIsPositional(t *testing.T) {
	got := BuildArgv(Request{Prompt: "hi", Model: "claude-sonnet-4-6"})
	idx := -1
	for i, a := range got {
		if a == "-p" {
			idx = i
			break
		}
	}
	require.Greater(t, idx, -1, "missing -p")
	require.Less(t, idx+1, len(got), "no token after -p")
	assert.Equal(t, "hi", got[idx+1])
}

func TestBuildArgv_RejectsEmptyModel(t *testing.T) {
	got := BuildArgv(Request{Prompt: "hi", Model: ""})
	for i, a := range got {
		assert.NotEqual(t, "--model", a, "unexpected --model at %d", i)
	}
}

func TestAirtightFlags_PinnedConstants(t *testing.T) {
	assert.Equal(t, "--tools", FlagTools)
	assert.Equal(t, "", ToolsNone)
	assert.Equal(t, "--disallowedTools", FlagDisallowedTools)
	assert.Equal(t, "*", DisallowedToolsAll)
	assert.Equal(t, "--mcp-config", FlagMCPConfig)
	assert.Equal(t, `{"mcpServers":{}}`, MCPConfigNull)
	assert.Equal(t, "--strict-mcp-config", FlagStrictMCPConfig)
	assert.Equal(t, "--settings", FlagSettings)
	assert.Equal(t, `{"hooks":{}}`, SettingsNoHooks)
}
