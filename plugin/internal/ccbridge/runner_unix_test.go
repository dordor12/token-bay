//go:build unix

package ccbridge

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeFakeClaude writes a small shell script at path that emulates
// `claude -p` for testing.
func writeFakeClaude(t *testing.T, path string, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(body), 0o755))
}

func TestExecRunner_Run_HappyPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude-fake")
	// Fake claude: assert flags are present, read stdin (the encoded
	// messages) and emit a stream-json output that echoes the stdin
	// back inside a result event so the test can verify both flags
	// and stdin made it through.
	writeFakeClaude(t, bin, `#!/bin/sh
echo "$@" | grep -q -- '--input-format' || exit 17
echo "$@" | grep -q -- '--disallowedTools' || exit 17
echo "$@" | grep -q -- '--mcp-config' || exit 17
echo "$@" | grep -q -- '{"mcpServers":{}}' || exit 17
echo "$@" | grep -q -- '--strict-mcp-config' || exit 17
echo "$@" | grep -q -- '--settings' || exit 17
echo "$@" | grep -q -- '{"hooks":{}}' || exit 17
STDIN_BYTES=$(cat)
echo '{"type":"system","subtype":"init"}'
# Plain marker so the test can grep for stdin content. Not valid
# JSON — that's fine; ParseStreamJSON ignores non-{ lines.
printf 'STDIN_RECEIVED=%s\n' "$STDIN_BYTES"
echo '{"type":"result","subtype":"success","is_error":false,"usage":{"input_tokens":7,"output_tokens":3}}'
`)

	runner := &ExecRunner{BinaryPath: bin}
	var sink bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := runner.Run(ctx, Request{
		Messages: []Message{{Role: RoleUser, Content: "hello-anchor"}},
		Model:    "claude-sonnet-4-6",
	}, &sink)
	require.NoError(t, err)
	out := sink.String()
	assert.Contains(t, out, `"type":"result"`)
	// stdin was a stream-json input event with content "hello-anchor".
	assert.Contains(t, out, "STDIN_RECEIVED=")
	assert.Contains(t, out, `"role":"user"`)
	assert.Contains(t, out, `"text":"hello-anchor"`)
}

func TestExecRunner_Run_MultiTurn_StdinHasAllMessages(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude-echo")
	writeFakeClaude(t, bin, `#!/bin/sh
STDIN_BYTES=$(cat)
printf 'STDIN_BYTES=%s\n' "$STDIN_BYTES"
echo '{"type":"result","subtype":"success","is_error":false,"usage":{"input_tokens":1,"output_tokens":1}}'
`)

	runner := &ExecRunner{BinaryPath: bin}
	var sink bytes.Buffer
	err := runner.Run(context.Background(), Request{
		Messages: []Message{
			{Role: RoleUser, Content: "first-user-marker"},
			{Role: RoleAssistant, Content: "first-assistant-marker"},
			{Role: RoleUser, Content: "second-user-marker"},
		},
		Model: "claude-sonnet-4-6",
	}, &sink)
	require.NoError(t, err)
	out := sink.String()
	// All three turns reach the subprocess.
	assert.Contains(t, out, "first-user-marker")
	assert.Contains(t, out, "first-assistant-marker")
	assert.Contains(t, out, "second-user-marker")
	// Roles are correctly tagged.
	assert.Contains(t, out, `"role":"assistant"`)
}

func TestExecRunner_Run_NonZeroExit_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude-fail")
	writeFakeClaude(t, bin, `#!/bin/sh
cat > /dev/null
echo '{"type":"system"}'
exit 9
`)

	runner := &ExecRunner{BinaryPath: bin}
	var sink bytes.Buffer
	err := runner.Run(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Model:    "claude-sonnet-4-6",
	}, &sink)
	require.Error(t, err)
	var exitErr *ExitError
	require.True(t, errors.As(err, &exitErr), "expected ExitError, got %v", err)
	assert.Equal(t, 9, exitErr.Code)
	assert.Contains(t, sink.String(), `"type":"system"`)
}

func TestExecRunner_Run_ContextCancel_KillsProcess(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude-hang")
	writeFakeClaude(t, bin, `#!/bin/sh
cat > /dev/null
echo '{"type":"system"}'
sleep 30
`)

	runner := &ExecRunner{BinaryPath: bin}
	var sink bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := runner.Run(ctx, Request{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Model:    "claude-sonnet-4-6",
	}, &sink)
	elapsed := time.Since(start)
	require.Error(t, err)
	assert.Less(t, elapsed, 5*time.Second, "ctx cancel did not kill subprocess fast enough")
}

func TestExecRunner_Run_FreshTempCWD(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude-pwd")
	writeFakeClaude(t, bin, `#!/bin/sh
cat > /dev/null
PWD_VAL=$(pwd)
printf '{"type":"result","subtype":"success","is_error":false,"usage":{"input_tokens":0,"output_tokens":0},"cwd":"%s"}\n' "$PWD_VAL"
`)

	runner := &ExecRunner{BinaryPath: bin}
	var sink bytes.Buffer
	require.NoError(t, runner.Run(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Model:    "claude-sonnet-4-6",
	}, &sink))
	out := sink.String()
	pwd, _ := os.Getwd()
	assert.NotContains(t, out, `"cwd":"`+pwd+`"`)
	assert.NotEqual(t, "", out)
}
