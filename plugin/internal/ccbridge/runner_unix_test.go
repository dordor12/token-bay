//go:build unix

package ccbridge

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testClientKey = ed25519.PublicKey(bytes.Repeat([]byte{0x77}, ed25519.PublicKeySize))

// writeFakeClaude writes a small shell script at path that emulates
// `claude -p` for testing.
func writeFakeClaude(t *testing.T, path string, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(body), 0o755))
}

func TestExecRunner_Run_HappyPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude-fake")
	writeFakeClaude(t, bin, `#!/bin/sh
echo "$@" | grep -q -- '--resume' || exit 17
echo "$@" | grep -q -- '--no-session-persistence' || exit 17
echo "$@" | grep -q -- '--disallowedTools' || exit 17
echo "$@" | grep -q -- '--mcp-config' || exit 17
echo "$@" | grep -q -- '--settings' || exit 17
echo "$@" | grep -q -- '{"hooks":{}}' || exit 17
echo "$@" | grep -q -- '--input-format' && exit 18  # MUST NOT be present
test "$HOME" != "$REAL_HOME" || exit 19
test -d "$HOME/.claude/projects" || exit 20
SID=$(echo "$@" | sed -E 's/.*--resume ([0-9a-f-]+).*/\1/')
PROJ=$(ls "$HOME/.claude/projects" | head -1)
SESSFILE="$HOME/.claude/projects/$PROJ/$SID.jsonl"
test -f "$SESSFILE" || exit 21
LINES=$(wc -l < "$SESSFILE" | tr -d ' ')
echo '{"type":"system","subtype":"init"}'
printf 'SESSION_RECORDS=%s\n' "$LINES"
echo '{"type":"result","subtype":"success","is_error":false,"usage":{"input_tokens":7,"output_tokens":3}}'
`)
	t.Setenv("REAL_HOME", os.Getenv("HOME"))

	runner := &ExecRunner{BinaryPath: bin, SeederRoot: t.TempDir()}
	var sink bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := runner.Run(ctx, Request{
		Messages: []Message{
			{Role: RoleUser, Content: TextContent("first")},
			{Role: RoleAssistant, Content: TextContent("ack")},
			{Role: RoleUser, Content: TextContent("the new question")},
		},
		Model:        "claude-sonnet-4-6",
		ClientPubkey: testClientKey,
	}, &sink)
	require.NoError(t, err)
	out := sink.String()
	assert.Contains(t, out, `"type":"result"`)
	assert.Contains(t, out, "SESSION_RECORDS=2") // 2 history records, last user is the prompt arg
}

func TestExecRunner_Run_NonZeroExit_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude-fail")
	writeFakeClaude(t, bin, `#!/bin/sh
echo '{"type":"system"}'
exit 9
`)

	runner := &ExecRunner{BinaryPath: bin, SeederRoot: t.TempDir()}
	var sink bytes.Buffer
	err := runner.Run(context.Background(), Request{
		Messages:     []Message{{Role: RoleUser, Content: TextContent("hi")}},
		Model:        "claude-sonnet-4-6",
		ClientPubkey: testClientKey,
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
echo '{"type":"system"}'
sleep 30
`)

	runner := &ExecRunner{BinaryPath: bin, SeederRoot: t.TempDir()}
	var sink bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := runner.Run(ctx, Request{
		Messages:     []Message{{Role: RoleUser, Content: TextContent("hi")}},
		Model:        "claude-sonnet-4-6",
		ClientPubkey: testClientKey,
	}, &sink)
	elapsed := time.Since(start)
	require.Error(t, err)
	assert.Less(t, elapsed, 5*time.Second, "ctx cancel did not kill subprocess fast enough")
}

func TestExecRunner_Run_FreshTempCWD(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude-pwd")
	writeFakeClaude(t, bin, `#!/bin/sh
PWD_VAL=$(pwd)
printf '{"type":"result","subtype":"success","is_error":false,"usage":{"input_tokens":0,"output_tokens":0},"cwd":"%s"}\n' "$PWD_VAL"
`)

	runner := &ExecRunner{BinaryPath: bin, SeederRoot: t.TempDir()}
	var sink bytes.Buffer
	require.NoError(t, runner.Run(context.Background(), Request{
		Messages:     []Message{{Role: RoleUser, Content: TextContent("hi")}},
		Model:        "claude-sonnet-4-6",
		ClientPubkey: testClientKey,
	}, &sink))
	out := sink.String()
	pwd, _ := os.Getwd()
	assert.NotContains(t, out, `"cwd":"`+pwd+`"`)
	assert.NotEqual(t, "", out)
}
