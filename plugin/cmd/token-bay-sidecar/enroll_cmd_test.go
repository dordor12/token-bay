package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/auditlog"
	"github.com/token-bay/token-bay/plugin/internal/ccproxy"
	"github.com/token-bay/token-bay/plugin/internal/identity"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
	"github.com/token-bay/token-bay/shared/ids"
)

// stubAuthProber returns a fixed AuthState. Production would use
// ccproxy.NewClaudeAuthProber.
type stubAuthProber struct {
	state *ccproxy.AuthState
	err   error
}

func (s *stubAuthProber) Probe(_ context.Context) (*ccproxy.AuthState, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.state, nil
}

// fakeEnrollClient captures the EnrollRequest and returns a canned response.
type fakeEnrollClient struct {
	got       *trackerclient.EnrollRequest
	respond   *trackerclient.EnrollResponse
	respondEr error
}

func (f *fakeEnrollClient) Enroll(_ context.Context, r *trackerclient.EnrollRequest) (*trackerclient.EnrollResponse, error) {
	cp := *r
	f.got = &cp
	if f.respondEr != nil {
		return nil, f.respondEr
	}
	return f.respond, nil
}

func newHappyAuthProber() *stubAuthProber {
	return &stubAuthProber{state: &ccproxy.AuthState{
		LoggedIn:    true,
		AuthMethod:  "claude.ai",
		APIProvider: "firstParty",
		OrgID:       "cd2c6c26-fdea-44af-b14f-11e283737e33",
	}}
}

func newGrantingClient(t *testing.T, credits uint64) *fakeEnrollClient {
	t.Helper()
	var trackerID [32]byte
	copy(trackerID[:], "tracker-id-XXXXXXXXXXXXXXXXXXXXX")
	return &fakeEnrollClient{
		respond: &trackerclient.EnrollResponse{
			IdentityID:            ids.IdentityID(trackerID),
			StarterGrantCredits:   credits,
			StarterGrantEntryBlob: []byte("starter-grant-entry-blob"),
		},
	}
}

func TestEnrollCmd_HappyPath_PersistsKeyFingerprintAndGrant(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.log")
	keyPath := filepath.Join(dir, "identity.key")
	enrollmentPath := filepath.Join(dir, "enrollment.json")

	auth := newHappyAuthProber()
	client := newGrantingClient(t, 50)

	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)

	err := runEnroll(context.Background(), enrollOptions{
		ConfigDir:    dir,
		AuditLogPath: auditPath,
		Tracker:      "https://tracker.example:7443",
		TrackerHash:  [32]byte{0xAB},
		Region:       "eu-central-1",
		Role:         identity.RoleConsumer,
		AuthProber:   auth,
		NewClient:    constClientFactory(client),
		Now:          func() time.Time { return now },
		Stderr:       &bytes.Buffer{},
	})
	require.NoError(t, err)

	// (a) keypair exists at expected path with 0600.
	info, err := os.Stat(keyPath)
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o600), info.Mode().Perm(), "key file perms")
	assert.Equal(t, int64(ed25519.SeedSize), info.Size(), "key file size")
	signer, err := identity.LoadKey(keyPath)
	require.NoError(t, err)

	// (b) tracker received an Enroll call with the right pubkey + fingerprint.
	require.NotNil(t, client.got, "tracker.Enroll never called")
	assert.Equal(t, ed25519.PublicKey(signer.PublicKey()), client.got.IdentityPubkey)
	wantFingerprint := sha256.Sum256([]byte("cd2c6c26-fdea-44af-b14f-11e283737e33"))
	assert.Equal(t, wantFingerprint, client.got.AccountFingerprint)
	assert.Equal(t, identity.RoleConsumer, client.got.Role)
	// Signature must verify against the captured pubkey.
	preimage, err := identity.EnrollPreimage(client.got.Nonce, client.got.AccountFingerprint, client.got.IdentityPubkey)
	require.NoError(t, err)
	assert.True(t, ed25519.Verify(client.got.IdentityPubkey, preimage, client.got.Sig))

	// (c) starter grant landed in the audit log.
	var grantSeen int
	for rec, rerr := range auditlog.Read(auditPath) {
		require.NoError(t, rerr)
		cr, ok := rec.(auditlog.ConsumerRecord)
		if !ok {
			continue
		}
		if cr.CostCredits == -50 && cr.ServedLocally {
			grantSeen++
			assert.Equal(t, now, cr.Timestamp.UTC())
		}
	}
	assert.Equal(t, 1, grantSeen, "exactly one starter-grant audit record expected")

	// (d) identity.json + enrollment.json sidecar persisted with right data.
	_, rec, err := identity.Open(dir)
	require.NoError(t, err)
	assert.Equal(t, identity.RoleConsumer, rec.Role)
	assert.Equal(t, wantFingerprint, rec.AccountFingerprint)
	assert.Equal(t, now, rec.EnrolledAt.UTC())

	em, err := loadEnrollmentMeta(enrollmentPath)
	require.NoError(t, err)
	// enrollment.json's tracker_id is the configured tracker SPKI hash
	// (= TOKEN_BAY_TRACKER_HASH), not the IdentityID the tracker returns
	// from Enroll — that one lives in identity.json.
	assert.Equal(t, [32]byte{0xAB}, em.TrackerID)
	assert.Equal(t, "eu-central-1", em.Region)
	assert.Equal(t, uint64(50), em.StarterGrantCredits)
	assert.Equal(t, now, em.EnrolledAt.UTC())
}

func TestEnrollCmd_RefusesBedrock(t *testing.T) {
	dir := t.TempDir()
	auth := &stubAuthProber{state: &ccproxy.AuthState{
		LoggedIn:    true,
		AuthMethod:  "claude.ai",
		APIProvider: "bedrock",
		OrgID:       "doesnt-matter",
	}}

	err := runEnroll(context.Background(), enrollOptions{
		ConfigDir:    dir,
		AuditLogPath: filepath.Join(dir, "audit.log"),
		Tracker:      "https://t.example:1",
		TrackerHash:  [32]byte{1},
		Role:         identity.RoleConsumer,
		AuthProber:   auth,
		NewClient:    constClientFactory(&fakeEnrollClient{}),
		Now:          time.Now,
		Stderr:       &bytes.Buffer{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Bedrock")

	// No keypair should have been written.
	_, err = os.Stat(filepath.Join(dir, "identity.key"))
	assert.True(t, errors.Is(err, fs.ErrNotExist))
}

func TestEnrollCmd_RefusesNotLoggedIn(t *testing.T) {
	dir := t.TempDir()
	auth := &stubAuthProber{state: &ccproxy.AuthState{LoggedIn: false}}

	err := runEnroll(context.Background(), enrollOptions{
		ConfigDir:    dir,
		AuditLogPath: filepath.Join(dir, "audit.log"),
		Tracker:      "https://t.example:1",
		TrackerHash:  [32]byte{1},
		Role:         identity.RoleConsumer,
		AuthProber:   auth,
		NewClient:    constClientFactory(&fakeEnrollClient{}),
		Now:          time.Now,
		Stderr:       &bytes.Buffer{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not logged in")
}

func TestEnrollCmd_ProberError_SurfacesClaudeNotInstalled(t *testing.T) {
	dir := t.TempDir()
	auth := &stubAuthProber{err: errors.New(`exec: "claude": executable file not found in $PATH`)}

	err := runEnroll(context.Background(), enrollOptions{
		ConfigDir:    dir,
		AuditLogPath: filepath.Join(dir, "audit.log"),
		Tracker:      "https://t.example:1",
		TrackerHash:  [32]byte{1},
		Role:         identity.RoleConsumer,
		AuthProber:   auth,
		NewClient:    constClientFactory(&fakeEnrollClient{}),
		Now:          time.Now,
		Stderr:       &bytes.Buffer{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "claude")
}

func TestEnrollCmd_RefusesIfAlreadyEnrolledWithoutFlag(t *testing.T) {
	dir := t.TempDir()
	// Pre-seed an existing key.
	preSigner, err := identity.Generate()
	require.NoError(t, err)
	require.NoError(t, identity.SaveKey(filepath.Join(dir, "identity.key"), preSigner))

	auth := newHappyAuthProber()
	client := newGrantingClient(t, 50)

	err = runEnroll(context.Background(), enrollOptions{
		ConfigDir:    dir,
		AuditLogPath: filepath.Join(dir, "audit.log"),
		Tracker:      "https://t.example:1",
		TrackerHash:  [32]byte{1},
		Role:         identity.RoleConsumer,
		AuthProber:   auth,
		NewClient:    constClientFactory(client),
		Now:          time.Now,
		Stderr:       &bytes.Buffer{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already")
	assert.Nil(t, client.got, "tracker should not have been called")

	// Pre-existing key untouched.
	postSigner, err := identity.LoadKey(filepath.Join(dir, "identity.key"))
	require.NoError(t, err)
	assert.Equal(t, preSigner.PublicKey(), postSigner.PublicKey())
}

func TestEnrollCmd_ReEnrollRotatesKey(t *testing.T) {
	dir := t.TempDir()
	preSigner, err := identity.Generate()
	require.NoError(t, err)
	require.NoError(t, identity.SaveKey(filepath.Join(dir, "identity.key"), preSigner))

	auth := newHappyAuthProber()
	client := newGrantingClient(t, 50)

	err = runEnroll(context.Background(), enrollOptions{
		ConfigDir:    dir,
		AuditLogPath: filepath.Join(dir, "audit.log"),
		Tracker:      "https://t.example:1",
		TrackerHash:  [32]byte{1},
		Region:       "us-east-1",
		Role:         identity.RoleConsumer,
		AuthProber:   auth,
		NewClient:    constClientFactory(client),
		ReEnroll:     true,
		Now:          time.Now,
		Stderr:       &bytes.Buffer{},
	})
	require.NoError(t, err)

	postSigner, err := identity.LoadKey(filepath.Join(dir, "identity.key"))
	require.NoError(t, err)
	assert.NotEqual(t, preSigner.PublicKey(), postSigner.PublicKey(), "re-enroll must rotate the keypair")
	require.NotNil(t, client.got)
	assert.Equal(t, ed25519.PublicKey(postSigner.PublicKey()), client.got.IdentityPubkey)
}

func TestEnrollCmd_RegisteredOnRoot(t *testing.T) {
	root := newRootCmd()
	enrollCmd, _, err := root.Find([]string{"enroll"})
	require.NoError(t, err)
	require.NotNil(t, enrollCmd)
	assert.Equal(t, "enroll", enrollCmd.Name())
}
