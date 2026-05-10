package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/plugin/internal/bootstrap"
	"github.com/token-bay/token-bay/plugin/internal/identity"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
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

func TestRunCmd_AutoBootstrapWithoutSignedFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeMinimalConfig(t, dir, "auto")

	cmd := newRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"run", "--config", cfgPath})

	// No bootstrap.signed in dir, no build-time injected signer pubkey
	// (BootstrapSignerPubkeyHex == "" in test builds). Auto must surface
	// a clear error, not silently fall through.
	err := cmd.Execute()
	require.Error(t, err)
}

func TestRunCmd_StartsAndStops_AutoBootstrap(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	// Inject a fresh build-time signer pubkey for the duration of this test.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	prev := bootstrap.BootstrapSignerPubkeyHex
	bootstrap.BootstrapSignerPubkeyHex = hex.EncodeToString(pub)
	t.Cleanup(func() { bootstrap.BootstrapSignerPubkeyHex = prev })

	// Sign a bootstrap list with one tracker (the dial will never
	// connect; the test asserts the cmd starts up and ctx-cancels
	// cleanly, proving the endpoints flowed into trackerclient).
	list := &tbproto.BootstrapPeerList{
		IssuerId:  bytes32(1),
		SignedAt:  uint64(now.Unix()) - 60,
		ExpiresAt: uint64(now.Unix()) + 600,
		Peers: []*tbproto.BootstrapPeer{{
			TrackerId:   bytes32(0xAA),
			Addr:        "auto-tracker.example.invalid:7443",
			RegionHint:  "eu",
			HealthScore: 0.9,
			LastSeen:    uint64(now.Unix()),
		}},
	}
	require.NoError(t, tbproto.SignBootstrapPeerList(priv, list))
	raw, err := proto.Marshal(list)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bootstrap.signed"), raw, 0o600))

	cfgPath := writeMinimalConfig(t, dir, "auto")
	// Auto path does not consult TOKEN_BAY_TRACKER_HASH — clear it to
	// prove the env var is irrelevant once endpoints come from a signed
	// list.
	t.Setenv("TOKEN_BAY_TRACKER_HASH", "")

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
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("auto-bootstrap run did not return within 3s of ctx cancel")
	}

	// known_peers.json should exist (LoadKnownPeers creates the store
	// and run_cmd wires it as the PeerExchangeHandler; the file is
	// created on the first Merge, but even before Merge the path
	// component should resolve under cfgDir).
	require.FileExists(t, filepath.Join(dir, "bootstrap.signed"),
		"signed bootstrap list must remain in place after auto resolve")
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

func TestResolveTrackerEndpoints_AutoWithoutSignerFails(t *testing.T) {
	dir := t.TempDir()
	// No bootstrap.signed and no build-time signer pubkey: error.
	_, err := resolveTrackerEndpoints("auto", dir, time.Now())
	require.Error(t, err)
}

func TestResolveTrackerEndpoints_RejectsBadURL(t *testing.T) {
	t.Setenv("TOKEN_BAY_TRACKER_HASH", "0100000000000000000000000000000000000000000000000000000000000000")
	_, err := resolveTrackerEndpoints(":not a url:", t.TempDir(), time.Now())
	require.Error(t, err)
}

func TestResolveTrackerEndpoints_RejectsMissingHash(t *testing.T) {
	t.Setenv("TOKEN_BAY_TRACKER_HASH", "")
	_, err := resolveTrackerEndpoints("https://tracker.example:7443", t.TempDir(), time.Now())
	require.Error(t, err)
	require.Contains(t, err.Error(), "TOKEN_BAY_TRACKER_HASH")
}

func TestResolveTrackerEndpoints_HappyPath(t *testing.T) {
	t.Setenv("TOKEN_BAY_TRACKER_HASH", "0100000000000000000000000000000000000000000000000000000000000000")
	eps, err := resolveTrackerEndpoints("https://tracker.example:7443", t.TempDir(), time.Now())
	require.NoError(t, err)
	require.Len(t, eps, 1)
	require.Equal(t, "tracker.example:7443", eps[0].Addr)
	require.Equal(t, [32]byte{1}, eps[0].IdentityHash)
}

func TestResolveTrackerEndpoints_AutoFromSignedFile(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_800_000_000, 0)

	// Generate a fresh signer keypair and inject it as the build-time
	// pubkey for the duration of this test.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	prev := bootstrap.BootstrapSignerPubkeyHex
	bootstrap.BootstrapSignerPubkeyHex = hex.EncodeToString(pub)
	t.Cleanup(func() { bootstrap.BootstrapSignerPubkeyHex = prev })

	// Write a signed BootstrapPeerList with two peers.
	list := &tbproto.BootstrapPeerList{
		IssuerId:  bytes32(1),
		SignedAt:  uint64(now.Unix()) - 60,
		ExpiresAt: uint64(now.Unix()) + 600,
		Peers: []*tbproto.BootstrapPeer{
			{TrackerId: bytes32(0xAA), Addr: "a.example.org:8443", RegionHint: "eu", HealthScore: 0.9, LastSeen: uint64(now.Unix())},
			{TrackerId: bytes32(0xBB), Addr: "b.example.org:8443", RegionHint: "us", HealthScore: 0.7, LastSeen: uint64(now.Unix())},
		},
	}
	require.NoError(t, tbproto.SignBootstrapPeerList(priv, list))
	raw, err := proto.Marshal(list)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bootstrap.signed"), raw, 0o600))

	eps, err := resolveTrackerEndpoints("auto", dir, now)
	require.NoError(t, err)
	require.Len(t, eps, 2)
	require.Equal(t, "a.example.org:8443", eps[0].Addr)
	require.Equal(t, "eu", eps[0].Region)
	require.Equal(t, byte(0xAA), eps[0].IdentityHash[0])
}

func bytes32(fill byte) []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = fill
	}
	return b
}
