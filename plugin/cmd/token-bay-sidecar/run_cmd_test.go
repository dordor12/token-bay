package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/identity"
	"github.com/token-bay/token-bay/shared/ids"
)

func writeMinimalConfig(t *testing.T, dir, tracker string) string {
	t.Helper()
	t.Setenv("TOKEN_BAY_TRACKER_HASH", "0100000000000000000000000000000000000000000000000000000000000000")

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := identity.New(priv)
	require.NoError(t, err)

	require.NoError(t, identity.SaveKey(filepath.Join(dir, "identity.key"), signer))
	rec := &identity.Record{
		Version:            1,
		IdentityID:         ids.IdentityID(sha256.Sum256(signer.PublicKey())),
		AccountFingerprint: [32]byte{42},
		Role:               1,
		EnrolledAt:         time.Now().UTC(),
	}
	require.NoError(t, identity.SaveRecord(filepath.Join(dir, "identity.json"), rec))

	cfgPath := filepath.Join(dir, "config.yaml")
	body := "role: both\n" +
		"tracker: " + tracker + "\n" +
		"audit_log_path: " + filepath.Join(dir, "audit.log") + "\n"
	require.NoError(t, os.WriteFile(cfgPath, []byte(body), 0o600))
	return cfgPath
}

func TestRunCmd_RequiresConfigFlag(t *testing.T) {
	cmd := newRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"run"})

	err := cmd.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), "config")
}

func TestRunCmd_RejectsTrackerAuto(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeMinimalConfig(t, dir, "auto")

	cmd := newRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"run", "--config", cfgPath})

	err := cmd.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), "auto")
}

func TestRunCmd_StartsAndStops(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeMinimalConfig(t, dir, "https://tracker.example:7443")

	cmd := newRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"run", "--config", cfgPath})

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	cmd.SetContext(ctx)

	errCh := make(chan error, 1)
	go func() { errCh <- cmd.Execute() }()

	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		// ctx-cancel returns nil from sidecar.Run; cmd.Execute may pass
		// it through unchanged.
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("run did not return within 3s of ctx cancel")
	}
}

func TestResolveTrackerEndpoints_RejectsAuto(t *testing.T) {
	_, err := resolveTrackerEndpoints("auto")
	require.Error(t, err)
	require.Contains(t, err.Error(), "auto")
}

func TestResolveTrackerEndpoints_RejectsBadURL(t *testing.T) {
	t.Setenv("TOKEN_BAY_TRACKER_HASH", "0100000000000000000000000000000000000000000000000000000000000000")
	_, err := resolveTrackerEndpoints(":not a url:")
	require.Error(t, err)
}

func TestResolveTrackerEndpoints_RejectsMissingHash(t *testing.T) {
	t.Setenv("TOKEN_BAY_TRACKER_HASH", "")
	_, err := resolveTrackerEndpoints("https://tracker.example:7443")
	require.Error(t, err)
	require.Contains(t, err.Error(), "TOKEN_BAY_TRACKER_HASH")
}

func TestResolveTrackerEndpoints_HappyPath(t *testing.T) {
	t.Setenv("TOKEN_BAY_TRACKER_HASH", "0100000000000000000000000000000000000000000000000000000000000000")
	eps, err := resolveTrackerEndpoints("https://tracker.example:7443")
	require.NoError(t, err)
	require.Len(t, eps, 1)
	require.Equal(t, "tracker.example:7443", eps[0].Addr)
	require.Equal(t, [32]byte{1}, eps[0].IdentityHash)
}
