package bootstrap_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/plugin/internal/bootstrap"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

func makeSignedListBytes(t *testing.T, signedAt, expiresAt uint64, peers []*tbproto.BootstrapPeer) ([]byte, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	list := &tbproto.BootstrapPeerList{
		IssuerId:  bytesOfLen(32, 1),
		SignedAt:  signedAt,
		ExpiresAt: expiresAt,
		Peers:     peers,
	}
	require.NoError(t, tbproto.SignBootstrapPeerList(priv, list))
	raw, err := proto.Marshal(list)
	require.NoError(t, err)
	return raw, pub
}

func TestResolveAutoEndpoints_FromSignedFile(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_800_000_000, 0)
	raw, pub := makeSignedListBytes(t,
		uint64(now.Unix())-60,
		uint64(now.Unix())+600,
		samplePeers(),
	)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bootstrap.signed"), raw, 0o600))

	eps, err := bootstrap.ResolveAutoEndpoints(bootstrap.AutoResolveConfig{
		CfgDir:       dir,
		SignerPubkey: pub,
		Now:          now,
		MaxEndpoints: 5,
	})
	require.NoError(t, err)
	require.Len(t, eps, 2)
	require.Equal(t, "tracker-a.example.org:8443", eps[0].Addr)
}

func TestResolveAutoEndpoints_FallbackToBuiltin(t *testing.T) {
	dir := t.TempDir() // no bootstrap.signed present
	now := time.Unix(1_800_000_000, 0)
	raw, pub := makeSignedListBytes(t,
		uint64(now.Unix())-60,
		uint64(now.Unix())+600,
		samplePeers(),
	)

	eps, err := bootstrap.ResolveAutoEndpoints(bootstrap.AutoResolveConfig{
		CfgDir:        dir,
		SignerPubkey:  pub,
		BuiltinSigned: raw,
		Now:           now,
		MaxEndpoints:  5,
	})
	require.NoError(t, err)
	require.Len(t, eps, 2)
	require.Equal(t, "tracker-a.example.org:8443", eps[0].Addr)
}

func TestResolveAutoEndpoints_PrefersFileOverBuiltin(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_800_000_000, 0)

	// File: one peer at tracker-file
	fileRaw, pub := makeSignedListBytes(t,
		uint64(now.Unix())-60,
		uint64(now.Unix())+600,
		[]*tbproto.BootstrapPeer{{
			TrackerId:   bytesOfLen(32, 0xCC),
			Addr:        "tracker-file.example.org:8443",
			HealthScore: 0.9,
			LastSeen:    uint64(now.Unix()),
		}},
	)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bootstrap.signed"), fileRaw, 0o600))

	// Built-in: same signer, different peer.
	builtin, err := proto.Marshal(&tbproto.BootstrapPeerList{
		IssuerId:  bytesOfLen(32, 2),
		SignedAt:  uint64(now.Unix()) - 60,
		ExpiresAt: uint64(now.Unix()) + 600,
		Peers: []*tbproto.BootstrapPeer{{
			TrackerId:   bytesOfLen(32, 0xDD),
			Addr:        "tracker-builtin.example.org:8443",
			HealthScore: 0.9,
			LastSeen:    uint64(now.Unix()),
		}},
	})
	require.NoError(t, err)
	// Note: builtin is unsigned here; the test asserts file wins regardless.

	eps, err := bootstrap.ResolveAutoEndpoints(bootstrap.AutoResolveConfig{
		CfgDir:        dir,
		SignerPubkey:  pub,
		BuiltinSigned: builtin,
		Now:           now,
		MaxEndpoints:  5,
	})
	require.NoError(t, err)
	require.Len(t, eps, 1)
	require.Equal(t, "tracker-file.example.org:8443", eps[0].Addr,
		"file source must win over built-in")
}

func TestResolveAutoEndpoints_NoSourceFails(t *testing.T) {
	dir := t.TempDir() // no bootstrap.signed
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	_, err = bootstrap.ResolveAutoEndpoints(bootstrap.AutoResolveConfig{
		CfgDir:       dir,
		SignerPubkey: pub,
		Now:          time.Unix(1_800_000_000, 0),
		MaxEndpoints: 5,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "bootstrap")
}

func TestResolveAutoEndpoints_FileCorruptedSurfaces(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_800_000_000, 0)
	_, pub := makeSignedListBytes(t,
		uint64(now.Unix())-60,
		uint64(now.Unix())+600,
		samplePeers(),
	)
	// Write a corrupted file.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bootstrap.signed"), []byte("garbage"), 0o600))

	_, err := bootstrap.ResolveAutoEndpoints(bootstrap.AutoResolveConfig{
		CfgDir:       dir,
		SignerPubkey: pub,
		Now:          now,
		MaxEndpoints: 5,
	})
	require.Error(t, err, "corrupted file should not silently fall back to built-in")
}

func TestResolveAutoEndpoints_TopNCap(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_800_000_000, 0)
	many := make([]*tbproto.BootstrapPeer, 10)
	for i := range many {
		id := make([]byte, 32)
		id[0] = byte(i + 1)
		id[31] = 1
		many[i] = &tbproto.BootstrapPeer{
			TrackerId:   id,
			Addr:        "t.example.org:8443",
			HealthScore: float64(10-i) / 10.0,
			LastSeen:    uint64(now.Unix()),
		}
	}
	raw, pub := makeSignedListBytes(t,
		uint64(now.Unix())-60,
		uint64(now.Unix())+600,
		many,
	)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bootstrap.signed"), raw, 0o600))

	eps, err := bootstrap.ResolveAutoEndpoints(bootstrap.AutoResolveConfig{
		CfgDir:       dir,
		SignerPubkey: pub,
		Now:          now,
		MaxEndpoints: 3,
	})
	require.NoError(t, err)
	require.Len(t, eps, 3, "MaxEndpoints must truncate")
}
