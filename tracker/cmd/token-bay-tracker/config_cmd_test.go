package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runConfigValidate exercises the config-validate command and captures
// its exit code via the context-based exit hook.
func runConfigValidate(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := newConfigCmd()
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs(append([]string{"validate"}, args...))

	exit := -1
	exitFn := func(c int) { exit = c }
	cmd.SetContext(withExitFunc(context.Background(), exitFn))
	_ = cmd.Execute()
	return outBuf.String(), errBuf.String(), exit
}

func TestConfigValidate_TempdirYAMLExitsZero(t *testing.T) {
	tmp := t.TempDir()
	yaml := "data_dir: " + tmp + "\n" +
		"server:\n" +
		"  listen_addr: \"0.0.0.0:7777\"\n" +
		"  identity_key_path: " + tmp + "/identity.key\n" +
		"  tls_cert_path: " + tmp + "/cert.pem\n" +
		"  tls_key_path: " + tmp + "/cert.key\n" +
		"ledger:\n" +
		"  storage_path: " + tmp + "/ledger.sqlite\n"
	path := tmp + "/tracker.yaml"
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o644))

	stdout, stderr, code := runConfigValidate(t, "--config", path)

	assert.Equal(t, -1, code, "no exit hook should fire on success; stderr: %s", stderr)
	assert.Contains(t, stdout, "OK")
	assert.Contains(t, stdout, path)
}

func TestConfigValidate_MissingFileExits1(t *testing.T) {
	_, stderr, code := runConfigValidate(t, "--config", "/nonexistent/path.yaml")

	assert.Equal(t, 1, code)
	assert.NotEmpty(t, stderr)
}

func TestConfigValidate_UnknownFieldExits2(t *testing.T) {
	_, stderr, code := runConfigValidate(t, "--config", "../../internal/config/testdata/unknown_field.yaml")

	assert.Equal(t, 2, code)
	assert.Contains(t, strings.ToLower(stderr), "parse")
}

func TestConfigValidate_InvalidExits3(t *testing.T) {
	_, stderr, code := runConfigValidate(t, "--config", "../../internal/config/testdata/invalid_score_weights_sum.yaml")

	assert.Equal(t, 3, code)
	assert.Contains(t, stderr, "admission.score_weights")
	assert.Contains(t, strings.ToLower(stderr), "validation")
}

func TestConfigValidate_NoConfigFlagExits1(t *testing.T) {
	_, stderr, code := runConfigValidate(t)

	assert.Equal(t, 1, code)
	assert.NotEmpty(t, stderr)
}
