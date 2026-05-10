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

func TestSaveSignedBootstrapList_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bootstrap.signed")
	now := time.Unix(1_800_000_000, 0)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	list := &tbproto.BootstrapPeerList{
		IssuerId:  bytesOfLen(32, 0xAA),
		SignedAt:  uint64(now.Unix()) - 60,
		ExpiresAt: uint64(now.Unix()) + 600,
		Peers:     samplePeers(),
	}
	require.NoError(t, tbproto.SignBootstrapPeerList(priv, list))
	raw, err := proto.Marshal(list)
	require.NoError(t, err)

	// First write — new file.
	require.NoError(t, bootstrap.SaveSignedBootstrapList(path, raw))

	// Round-trip via ParseSignedBootstrapList.
	endpoints, err := bootstrap.ParseSignedBootstrapList(path, pub, now)
	require.NoError(t, err)
	require.Len(t, endpoints, 2)
}

func TestSaveSignedBootstrapList_OverwriteAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bootstrap.signed")

	require.NoError(t, os.WriteFile(path, []byte("OLD"), 0o600))
	require.NoError(t, bootstrap.SaveSignedBootstrapList(path, []byte("NEW")))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, []byte("NEW"), got)

	// No leftover tempfile in the directory.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "bootstrap.signed", entries[0].Name())
}

func TestSaveSignedBootstrapList_RejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bootstrap.signed")
	require.Error(t, bootstrap.SaveSignedBootstrapList(path, nil))
	require.Error(t, bootstrap.SaveSignedBootstrapList(path, []byte{}))
}

func TestSaveSignedBootstrapList_CreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "bootstrap.signed")
	require.NoError(t, bootstrap.SaveSignedBootstrapList(path, []byte("ok")))
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, []byte("ok"), got)
}
