package identity

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpen_BothFilesPresent(t *testing.T) {
	dir := t.TempDir()
	s, err := Generate()
	require.NoError(t, err)
	require.NoError(t, SaveKey(filepath.Join(dir, "identity.key"), s))

	rec := sampleRecord()
	require.NoError(t, SaveRecord(filepath.Join(dir, "identity.json"), rec))

	gotS, gotR, err := Open(dir)
	require.NoError(t, err)
	assert.Equal(t, s.IdentityID(), gotS.IdentityID())
	assert.Equal(t, rec, gotR)
}

func TestOpen_KeyMissing(t *testing.T) {
	dir := t.TempDir()
	_, _, err := Open(dir)
	require.True(t, errors.Is(err, ErrKeyNotFound), "got %v", err)
}

func TestOpen_RecordMissing(t *testing.T) {
	dir := t.TempDir()
	s, err := Generate()
	require.NoError(t, err)
	require.NoError(t, SaveKey(filepath.Join(dir, "identity.key"), s))

	_, _, err = Open(dir)
	require.True(t, errors.Is(err, ErrRecordNotFound), "got %v", err)
}

func TestOpen_KeyCorrupt(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "identity.key"), []byte("xxx"), 0o600))
	_, _, err := Open(dir)
	require.True(t, errors.Is(err, ErrInvalidKey), "got %v", err)
}

func TestVerifyBinding_Match(t *testing.T) {
	snap := validSnapshot()
	fp, err := AccountFingerprint(snap)
	require.NoError(t, err)
	rec := &Record{Version: 1, AccountFingerprint: fp, EnrolledAt: time.Now().UTC()}
	require.NoError(t, VerifyBinding(rec, snap))
}

func TestVerifyBinding_Mismatch(t *testing.T) {
	snap := validSnapshot()
	rec := &Record{Version: 1, AccountFingerprint: [32]byte{0xFF}, EnrolledAt: time.Now().UTC()}
	err := VerifyBinding(rec, snap)
	require.True(t, errors.Is(err, ErrFingerprintMismatch), "got %v", err)
}

func TestVerifyBinding_IneligibleSnapshot(t *testing.T) {
	snap := validSnapshot()
	snap.LoggedIn = false
	rec := &Record{Version: 1, AccountFingerprint: [32]byte{}, EnrolledAt: time.Now().UTC()}
	err := VerifyBinding(rec, snap)
	require.True(t, errors.Is(err, ErrIneligibleAuth), "got %v", err)
}
