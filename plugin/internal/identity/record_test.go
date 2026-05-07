package identity

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/ids"
)

func sampleRecord() *Record {
	var idid ids.IdentityID
	for i := range idid {
		idid[i] = 0x11
	}
	var fp [32]byte
	for i := range fp {
		fp[i] = 0x22
	}
	return &Record{
		Version:            1,
		IdentityID:         idid,
		AccountFingerprint: fp,
		Role:               RoleConsumer | RoleSeeder,
		EnrolledAt:         time.Date(2026, 5, 7, 19, 48, 0, 0, time.UTC),
	}
}

func TestSaveRecord_LoadRecord_RoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "identity.json")
	require.NoError(t, SaveRecord(p, sampleRecord()))

	got, err := LoadRecord(p)
	require.NoError(t, err)
	assert.Equal(t, sampleRecord(), got)
}

func TestSaveRecord_AllowsOverwrite(t *testing.T) {
	p := filepath.Join(t.TempDir(), "identity.json")
	require.NoError(t, SaveRecord(p, sampleRecord()))

	r2 := sampleRecord()
	r2.Role = RoleSeeder
	require.NoError(t, SaveRecord(p, r2))

	got, err := LoadRecord(p)
	require.NoError(t, err)
	assert.Equal(t, RoleSeeder, got.Role)
}

func TestSaveRecord_FileMode0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not honor Unix file modes")
	}
	p := filepath.Join(t.TempDir(), "identity.json")
	require.NoError(t, SaveRecord(p, sampleRecord()))
	info, err := os.Stat(p)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestSaveRecord_NormalizesUTC(t *testing.T) {
	r := sampleRecord()
	r.EnrolledAt = time.Date(2026, 5, 7, 12, 0, 0, 0, time.FixedZone("X", 5*3600))
	p := filepath.Join(t.TempDir(), "identity.json")
	require.NoError(t, SaveRecord(p, r))

	got, err := LoadRecord(p)
	require.NoError(t, err)
	assert.True(t, got.EnrolledAt.Equal(r.EnrolledAt))
	assert.Equal(t, time.UTC, got.EnrolledAt.Location())
}

func TestLoadRecord_NotFound(t *testing.T) {
	_, err := LoadRecord(filepath.Join(t.TempDir(), "missing"))
	require.True(t, errors.Is(err, ErrRecordNotFound), "got %v", err)
}

func TestLoadRecord_BadVersion(t *testing.T) {
	p := filepath.Join(t.TempDir(), "identity.json")
	raw := map[string]any{
		"version":             99,
		"identity_id":         hex.EncodeToString(make([]byte, 32)),
		"account_fingerprint": hex.EncodeToString(make([]byte, 32)),
		"role":                1,
		"enrolled_at":         "2026-05-07T19:48:00Z",
	}
	b, _ := json.Marshal(raw)
	require.NoError(t, os.WriteFile(p, b, 0o600))
	_, err := LoadRecord(p)
	require.True(t, errors.Is(err, ErrInvalidRecord), "got %v", err)
}

func TestLoadRecord_BadHexLength(t *testing.T) {
	p := filepath.Join(t.TempDir(), "identity.json")
	raw := map[string]any{
		"version":             1,
		"identity_id":         "abcd",
		"account_fingerprint": hex.EncodeToString(make([]byte, 32)),
		"role":                1,
		"enrolled_at":         "2026-05-07T19:48:00Z",
	}
	b, _ := json.Marshal(raw)
	require.NoError(t, os.WriteFile(p, b, 0o600))
	_, err := LoadRecord(p)
	require.True(t, errors.Is(err, ErrInvalidRecord), "got %v", err)
}
