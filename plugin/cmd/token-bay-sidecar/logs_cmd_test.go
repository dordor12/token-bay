package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/auditlog"
)

func writeAuditLogConfig(t *testing.T, dir, auditPath string) string {
	t.Helper()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := "role: both\ntracker: https://x.example:7443\naudit_log_path: " + auditPath + "\n"
	require.NoError(t, os.WriteFile(cfgPath, []byte(body), 0o600))
	return cfgPath
}

func runLogsCmd(t *testing.T, dir string, args ...string) (string, string, error) {
	t.Helper()
	cmd := newRootCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(append([]string{"logs", "--config", filepath.Join(dir, "config.yaml")}, args...))
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

func TestLogsCmd_TailsAuditLog_RendersHumanReadableLines(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.log")
	writeAuditLogConfig(t, dir, auditPath)

	al, err := auditlog.Open(auditPath)
	require.NoError(t, err)
	require.NoError(t, al.LogConsumer(auditlog.ConsumerRecord{
		RequestID:     "req-1",
		ServedLocally: true,
		CostCredits:   10,
		Timestamp:     time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC),
	}))
	require.NoError(t, al.LogTransfer(auditlog.TransferRecord{
		RequestID:    "transfer:abc",
		SourceRegion: "eu",
		DestRegion:   "us",
		Amount:       100,
		Outcome:      auditlog.TransferOutcomeSuccess,
		Timestamp:    time.Date(2026, 5, 24, 12, 1, 0, 0, time.UTC),
	}))
	require.NoError(t, al.Close())

	out, _, err := runLogsCmd(t, dir)
	require.NoError(t, err)
	assert.Contains(t, out, "req-1")
	assert.Contains(t, out, "transfer:abc")
}

func TestLogsCmd_RespectsTailFlag(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.log")
	writeAuditLogConfig(t, dir, auditPath)

	al, err := auditlog.Open(auditPath)
	require.NoError(t, err)
	for i := range 10 {
		require.NoError(t, al.LogConsumer(auditlog.ConsumerRecord{
			RequestID:     "r-" + string(rune('0'+i)),
			ServedLocally: true,
			Timestamp:     time.Now().UTC(),
		}))
	}
	require.NoError(t, al.Close())

	out, _, err := runLogsCmd(t, dir, "--tail", "3")
	require.NoError(t, err)
	lines := strings.Count(strings.TrimSpace(out), "\n") + 1
	assert.Equal(t, 3, lines, "want 3 tail lines, got output: %s", out)
}

func TestLogsCmd_MissingAuditLog_NonZeroExit(t *testing.T) {
	dir := t.TempDir()
	writeAuditLogConfig(t, dir, filepath.Join(dir, "does-not-exist.log"))

	_, errOut, err := runLogsCmd(t, dir)
	require.Error(t, err)
	assert.NotEmpty(t, errOut+err.Error())
}

func TestLogsCmd_NoSidecarDependency(t *testing.T) {
	// The audit log file is the source of truth; logs must read directly,
	// not via the sidecar. So this should succeed even with no sidecar.url.
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.log")
	writeAuditLogConfig(t, dir, auditPath)

	al, err := auditlog.Open(auditPath)
	require.NoError(t, err)
	require.NoError(t, al.LogConsumer(auditlog.ConsumerRecord{
		RequestID: "while-down", ServedLocally: true, Timestamp: time.Now().UTC(),
	}))
	require.NoError(t, al.Close())

	out, _, err := runLogsCmd(t, dir)
	require.NoError(t, err)
	assert.Contains(t, out, "while-down")
}
