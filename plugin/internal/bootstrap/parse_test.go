package bootstrap_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/plugin/internal/bootstrap"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// makeSignedList builds a marshaled, signed BootstrapPeerList with `peers`
// and writes it to `path`. Returns the list (post-sign) and the signer
// public key used.
func makeSignedList(t *testing.T, path string, signedAt, expiresAt uint64, peers []*tbproto.BootstrapPeer) (ed25519.PublicKey, ed25519.PrivateKey) {
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

	data, err := proto.Marshal(list)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return pub, priv
}

func bytesOfLen(n int, fill byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = fill
	}
	return b
}

func samplePeers() []*tbproto.BootstrapPeer {
	return []*tbproto.BootstrapPeer{
		{
			TrackerId:   bytesOfLen(32, 0xAA),
			Addr:        "tracker-a.example.org:8443",
			RegionHint:  "eu-central-1",
			HealthScore: 0.9,
			LastSeen:    1_700_000_000,
		},
		{
			TrackerId:   bytesOfLen(32, 0xBB),
			Addr:        "tracker-b.example.org:8443",
			RegionHint:  "us-east-1",
			HealthScore: 0.7,
			LastSeen:    1_700_000_000,
		},
	}
}

func TestParseSignedBootstrapList_OK(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bootstrap.signed")
	now := time.Unix(1_800_000_000, 0)
	pub, _ := makeSignedList(t, path,
		uint64(now.Unix())-60,
		uint64(now.Unix())+600,
		samplePeers(),
	)

	endpoints, err := bootstrap.ParseSignedBootstrapList(path, pub, now)
	require.NoError(t, err)
	require.Len(t, endpoints, 2)
	require.Equal(t, "tracker-a.example.org:8443", endpoints[0].Addr)
	require.Equal(t, "eu-central-1", endpoints[0].Region)
	require.Equal(t, byte(0xAA), endpoints[0].IdentityHash[0])
}

func TestParseSignedBootstrapList_CorruptedSignature(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bootstrap.signed")
	now := time.Unix(1_800_000_000, 0)
	pub, _ := makeSignedList(t, path,
		uint64(now.Unix())-60,
		uint64(now.Unix())+600,
		samplePeers(),
	)

	// Flip the very last byte of the file (Sig field is at the end of the
	// marshaled message because it's the highest-numbered field).
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	data[len(data)-1] ^= 0xFF
	require.NoError(t, os.WriteFile(path, data, 0o600))

	_, err = bootstrap.ParseSignedBootstrapList(path, pub, now)
	require.Error(t, err)
	require.ErrorIs(t, err, tbproto.ErrBootstrapPeerListBadSig)
}

func TestParseSignedBootstrapList_WrongSigner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bootstrap.signed")
	now := time.Unix(1_800_000_000, 0)
	_, _ = makeSignedList(t, path,
		uint64(now.Unix())-60,
		uint64(now.Unix())+600,
		samplePeers(),
	)

	// Verify against an unrelated pubkey.
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	_, err = bootstrap.ParseSignedBootstrapList(path, otherPub, now)
	require.Error(t, err)
	require.ErrorIs(t, err, tbproto.ErrBootstrapPeerListBadSig)
}

func TestParseSignedBootstrapList_PastExpiry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bootstrap.signed")
	now := time.Unix(1_800_000_000, 0)
	pub, _ := makeSignedList(t, path,
		uint64(now.Unix())-3600,
		uint64(now.Unix())-1800, // 30 min ago, well outside skew tolerance
		samplePeers(),
	)

	_, err := bootstrap.ParseSignedBootstrapList(path, pub, now)
	require.Error(t, err)
	require.True(t, errors.Is(err, bootstrap.ErrBootstrapListExpired),
		"want ErrBootstrapListExpired, got %v", err)
}

func TestParseSignedBootstrapList_FileMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "absent.signed")
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	_, err = bootstrap.ParseSignedBootstrapList(path, pub, time.Unix(1_800_000_000, 0))
	require.Error(t, err)
	require.True(t, errors.Is(err, os.ErrNotExist), "want os.ErrNotExist, got %v", err)
}

func TestParseSignedBootstrapList_NotProto(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "garbage.signed")
	require.NoError(t, os.WriteFile(path, []byte("not a proto message at all"), 0o600))

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	_, err = bootstrap.ParseSignedBootstrapList(path, pub, time.Unix(1_800_000_000, 0))
	require.Error(t, err)
}

func TestParseSignedBootstrapList_NilPubkey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bootstrap.signed")
	now := time.Unix(1_800_000_000, 0)
	_, _ = makeSignedList(t, path,
		uint64(now.Unix())-60,
		uint64(now.Unix())+600,
		samplePeers(),
	)

	_, err := bootstrap.ParseSignedBootstrapList(path, nil, now)
	require.Error(t, err)
}
