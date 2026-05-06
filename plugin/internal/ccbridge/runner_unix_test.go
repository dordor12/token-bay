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
	writeFakeClaude(t, bin, `#!/bin/sh
echo "$@" | grep -q -- '--disallowedTools' || exit 17
echo "$@" | grep -q -- '--mcp-config' || exit 17
echo "$@" | grep -q -- '{"mcpServers":{}}' || exit 17
echo "$@" | grep -q -- '--strict-mcp-config' || exit 17
echo "$@" | grep -q -- '--settings' || exit 17
echo "$@" | grep -q -- '{"hooks":{}}' || exit 17
echo '{"type":"system","subtype":"init"}'
echo '{"type":"result","subtype":"success","is_error":false,"usage":{"input_tokens":7,"output_tokens":3}}'
`)

	runner := &ExecRunner{BinaryPath: bin}
	var sink bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := runner.Run(ctx, Request{Prompt: "hi", Model: "claude-sonnet-4-6"}, &sink)
	require.NoError(t, err)
	assert.Contains(t, sink.String(), `"type":"result"`)
}

func TestExecRunner_Run_NonZeroExit_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude-fail")
	writeFakeClaude(t, bin, `#!/bin/sh
echo '{"type":"system"}'
exit 9
`)

	runner := &ExecRunner{BinaryPath: bin}
	var sink bytes.Buffer
	err := runner.Run(context.Background(), Request{Prompt: "hi", Model: "claude-sonnet-4-6"}, &sink)
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

	runner := &ExecRunner{BinaryPath: bin}
	var sink bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := runner.Run(ctx, Request{Prompt: "hi", Model: "claude-sonnet-4-6"}, &sink)
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

	runner := &ExecRunner{BinaryPath: bin}
	var sink bytes.Buffer
	require.NoError(t, runner.Run(context.Background(), Request{Prompt: "hi", Model: "claude-sonnet-4-6"}, &sink))
	out := sink.String()
	pwd, _ := os.Getwd()
	assert.NotContains(t, out, `"cwd":"`+pwd+`"`)
	assert.NotEqual(t, "", out)
}
