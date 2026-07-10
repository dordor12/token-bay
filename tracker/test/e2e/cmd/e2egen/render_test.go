package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/tracker/internal/config"
)

func testSeeds() (a, b, fed []byte) {
	return bytes.Repeat([]byte{1}, ed25519.SeedSize),
		bytes.Repeat([]byte{2}, ed25519.SeedSize),
		bytes.Repeat([]byte{3}, ed25519.SeedSize)
}

func testOpts(outDir string) genOpts {
	seedA, seedB, seedFed := testSeeds()
	return genOpts{
		OutDir:  outDir,
		SeedA:   seedA,
		SeedB:   seedB,
		SeedFed: seedFed,
		AddrA:   "tracker-a",
		AddrB:   "tracker-b",
	}
}

func TestRender_ProducesValidConfig(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, generate(testOpts(dir)))

	// Identity keys are raw 64-byte Ed25519 private keys.
	kb, err := os.ReadFile(filepath.Join(dir, "identity-a.key"))
	require.NoError(t, err)
	require.Len(t, kb, ed25519.PrivateKeySize)
	info, err := os.Stat(filepath.Join(dir, "identity-a.key"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o644), info.Mode().Perm())

	_, seedB, _ := testSeeds()
	privB := ed25519.NewKeyFromSeed(seedB)
	pubB, ok := privB.Public().(ed25519.PublicKey)
	require.True(t, ok)
	trackerIDB := sha256.Sum256(pubB)

	// config parses + validates via the real loader.
	raw, err := os.ReadFile(filepath.Join(dir, "tracker-a.yaml"))
	require.NoError(t, err)

	cfg, err := config.Parse(bytes.NewReader(raw))
	require.NoError(t, err)
	config.ApplyDefaults(cfg)

	// The generated YAML must carry the literal container path: /data
	// only exists inside the tracker's Docker image (see
	// deployments/docker/Dockerfile), not on the host running this
	// test, so the fs-existence probe in config.Validate's
	// admission.tlog_path check is repointed at a real, on-host
	// directory before validating — everything else is checked as-is.
	require.Equal(t, "/data/admission.tlog", cfg.Admission.TLogPath)
	require.Equal(t, "/data/admission.snapshot", cfg.Admission.SnapshotPathPrefix)
	require.Equal(t, "/data", cfg.DataDir)
	cfg.Admission.TLogPath = filepath.Join(t.TempDir(), "admission.tlog")
	require.NoError(t, config.Validate(cfg))

	// A's peer list contains B (index 0) then the fedactor (index 1),
	// with tracker_id == sha256(B's raw pubkey).
	require.Len(t, cfg.Federation.Peers, 2)
	peerB := cfg.Federation.Peers[0]
	require.Equal(t, hex.EncodeToString(trackerIDB[:]), peerB.TrackerID)
	require.Equal(t, hex.EncodeToString(pubB), peerB.PubKey)
	require.Equal(t, "tracker-b:7443", peerB.Addr)
	require.Equal(t, "B", peerB.Region)

	peerFed := cfg.Federation.Peers[1]
	require.Equal(t, "fedactor:7443", peerFed.Addr)
	require.Equal(t, "FED", peerFed.Region)

	// tracker-a.spki is the hex SHA-256 of tracker A's DER-encoded SPKI.
	privA := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize))
	pubA, ok := privA.Public().(ed25519.PublicKey)
	require.True(t, ok)
	spkiDERA, err := x509.MarshalPKIXPublicKey(pubA)
	require.NoError(t, err)
	wantSPKIHashA := sha256.Sum256(spkiDERA)

	gotSPKIHashA, err := os.ReadFile(filepath.Join(dir, "tracker-a.spki"))
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(wantSPKIHashA[:]), string(gotSPKIHashA))

	// tracker-a.fedid is the hex SHA-256 of tracker A's RAW Ed25519
	// pubkey (the FEDERATION tracker_id, distinct from the mTLS SPKI
	// hash above — see identity's doc comment in render.go).
	wantFedIDHashA := sha256.Sum256(pubA)
	gotFedIDHashA, err := os.ReadFile(filepath.Join(dir, "tracker-a.fedid"))
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(wantFedIDHashA[:]), string(gotFedIDHashA))
	require.NotEqual(t, string(gotFedIDHashA), string(gotSPKIHashA), "fedid and spki must be distinct encodings")

	// tracker-b's config has only A as a peer.
	rawB, err := os.ReadFile(filepath.Join(dir, "tracker-b.yaml"))
	require.NoError(t, err)
	cfgB, err := config.Parse(bytes.NewReader(rawB))
	require.NoError(t, err)
	config.ApplyDefaults(cfgB)
	require.Len(t, cfgB.Federation.Peers, 1)
	require.Equal(t, "A", cfgB.Federation.Peers[0].Region)
	require.Equal(t, "tracker-a:7443", cfgB.Federation.Peers[0].Addr)
}

func TestRender_Deterministic(t *testing.T) {
	dir1 := filepath.Join(t.TempDir(), "run1")
	dir2 := filepath.Join(t.TempDir(), "run2")

	require.NoError(t, generate(testOpts(dir1)))
	require.NoError(t, generate(testOpts(dir2)))

	for _, name := range []string{"tracker-a.yaml", "tracker-b.yaml", "identity-a.key", "identity-b.key", "identity-fed.key", "tracker-a.spki", "tracker-b.spki"} {
		b1, err := os.ReadFile(filepath.Join(dir1, name))
		require.NoError(t, err)
		b2, err := os.ReadFile(filepath.Join(dir2, name))
		require.NoError(t, err)
		require.Truef(t, bytes.Equal(b1, b2), "%s differs across runs with identical seeds", name)
	}
}
