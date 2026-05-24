package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/auditlog"
	"github.com/token-bay/token-bay/plugin/internal/identity"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// fakeTransferClient stubs the TransferClient seam runTransfer uses.
// It captures the TransferRequest payload it receives, returns a canned
// balance snapshot for BalanceCached, and either returns a canned proof
// or a canned error from TransferRequest.
type fakeTransferClient struct {
	balance    *tbproto.SignedBalanceSnapshot
	balanceErr error

	transferGot   *trackerclient.TransferRequest
	transferProof *trackerclient.TransferProof
	transferErr   error
	transferCalls int
}

func (f *fakeTransferClient) BalanceCached(_ context.Context, _ ids.IdentityID) (*tbproto.SignedBalanceSnapshot, error) {
	if f.balanceErr != nil {
		return nil, f.balanceErr
	}
	return f.balance, nil
}

func (f *fakeTransferClient) TransferRequest(_ context.Context, tr *trackerclient.TransferRequest) (*trackerclient.TransferProof, error) {
	f.transferCalls++
	cp := *tr
	f.transferGot = &cp
	if f.transferErr != nil {
		return nil, f.transferErr
	}
	return f.transferProof, nil
}

// snapshotWithCredits is a minimal SignedBalanceSnapshot for tests; the
// transfer cmd reads only Body.Credits from it.
func snapshotWithCredits(c int64) *tbproto.SignedBalanceSnapshot {
	return &tbproto.SignedBalanceSnapshot{
		Body: &tbproto.BalanceSnapshotBody{Credits: c},
	}
}

// seedEnrolledIdentity writes the identity.key, identity.json, and
// enrollment.json files runTransfer expects under dir, using region as
// the source region. Mirrors what enroll_cmd writes on success.
func seedEnrolledIdentity(t *testing.T, dir string, region string) *identity.Signer {
	t.Helper()
	signer, err := identity.Generate()
	require.NoError(t, err)
	require.NoError(t, identity.SaveKey(filepath.Join(dir, "identity.key"), signer))

	rec := &identity.Record{
		Version:    1,
		IdentityID: signer.IdentityID(),
		Role:       identity.RoleConsumer,
		EnrolledAt: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
	}
	require.NoError(t, identity.SaveRecord(filepath.Join(dir, "identity.json"), rec))

	em := &enrollmentMeta{
		Version:    enrollmentSchemaVersion,
		TrackerID:  [32]byte{0xAB},
		Region:     region,
		EnrolledAt: rec.EnrolledAt,
	}
	require.NoError(t, saveEnrollmentMeta(filepath.Join(dir, enrollmentFilename), em))
	return signer
}

func newConstTransferClientFactory(c TransferClient) func(context.Context, *identity.Signer) (TransferClient, func(), error) {
	return func(_ context.Context, _ *identity.Signer) (TransferClient, func(), error) {
		return c, func() {}, nil
	}
}

// findTransferRecords scans the audit log for TransferRecord entries.
// A missing audit-log file means no records were written, not a test
// failure — returns an empty slice in that case.
func findTransferRecords(t *testing.T, path string) []auditlog.TransferRecord {
	t.Helper()
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	var out []auditlog.TransferRecord
	for rec, err := range auditlog.Read(path) {
		require.NoError(t, err)
		if tr, ok := rec.(auditlog.TransferRecord); ok {
			out = append(out, tr)
		}
	}
	return out
}

func TestTransferCmd_HappyPath_DebitsAndAuditAndStdout(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.log")

	seedEnrolledIdentity(t, dir, "eu-central-1")

	chainTip := [32]byte{0x11, 0x22, 0x33, 0x44}
	client := &fakeTransferClient{
		balance: snapshotWithCredits(1000),
		transferProof: &trackerclient.TransferProof{
			SourceChainTipHash: chainTip,
			SourceSeq:          77,
			TrackerSig:         bytes.Repeat([]byte{0x55}, 64),
		},
	}

	now := time.Date(2026, 5, 10, 12, 30, 0, 0, time.UTC)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	err := runTransfer(context.Background(), transferOptions{
		ConfigDir:    dir,
		AuditLogPath: auditPath,
		DestRegion:   "us-east-1",
		Amount:       250,
		NewClient:    newConstTransferClientFactory(client),
		Now:          func() time.Time { return now },
		Stdout:       stdout,
		Stderr:       stderr,
	})
	require.NoError(t, err)

	// (a) tracker received a TransferRequest with the right fields.
	require.NotNil(t, client.transferGot, "TransferRequest never called")
	assert.Equal(t, uint64(250), client.transferGot.Amount)
	assert.Equal(t, "us-east-1", client.transferGot.DestRegion)
	assert.NotEqual(t, [32]byte{}, client.transferGot.Nonce, "nonce must be non-zero")
	assert.NotZero(t, client.transferGot.Timestamp, "timestamp must be non-zero")

	// (b) audit log has exactly one TransferRecord with success outcome.
	got := findTransferRecords(t, auditPath)
	require.Len(t, got, 1)
	rec := got[0]
	assert.Equal(t, auditlog.TransferOutcomeSuccess, rec.Outcome)
	assert.Equal(t, "eu-central-1", rec.SourceRegion)
	assert.Equal(t, "us-east-1", rec.DestRegion)
	assert.Equal(t, uint64(250), rec.Amount)
	require.NotNil(t, rec.SourceChainTipHash)
	assert.Equal(t, chainTip, *rec.SourceChainTipHash)
	assert.Equal(t, uint64(77), rec.SourceSeq)
	assert.True(t, rec.Timestamp.Equal(now))
	assert.True(t, strings.HasPrefix(rec.RequestID, "transfer:"))

	// (c) stdout summary is human-readable and mentions key fields.
	out := stdout.String()
	assert.Contains(t, out, "us-east-1")
	assert.Contains(t, out, "250")
	// Per task: success message documents that destination application is async.
	assert.Contains(t, out, "Destination region will credit shortly")
}

func TestTransferCmd_InsufficientBalance_RefusesBeforeRPC(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.log")
	seedEnrolledIdentity(t, dir, "eu-central-1")

	client := &fakeTransferClient{
		balance: snapshotWithCredits(50),
	}

	err := runTransfer(context.Background(), transferOptions{
		ConfigDir:    dir,
		AuditLogPath: auditPath,
		DestRegion:   "us-east-1",
		Amount:       100,
		NewClient:    newConstTransferClientFactory(client),
		Now:          time.Now,
		Stdout:       io.Discard,
		Stderr:       &bytes.Buffer{},
	})
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "insufficient")

	assert.Equal(t, 0, client.transferCalls, "RPC must not be invoked when local balance check fails")
	// No audit entry on local pre-check failure (only tracker-side rejections audit).
	assert.Empty(t, findTransferRecords(t, auditPath))
}

func TestTransferCmd_TrackerRejectionFrozen_AuditsRejection(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.log")
	seedEnrolledIdentity(t, dir, "eu-central-1")

	client := &fakeTransferClient{
		balance:     snapshotWithCredits(1000),
		transferErr: trackerclient.ErrFrozen,
	}

	err := runTransfer(context.Background(), transferOptions{
		ConfigDir:    dir,
		AuditLogPath: auditPath,
		DestRegion:   "us-east-1",
		Amount:       250,
		NewClient:    newConstTransferClientFactory(client),
		Now:          func() time.Time { return time.Date(2026, 5, 10, 13, 0, 0, 0, time.UTC) },
		Stdout:       io.Discard,
		Stderr:       &bytes.Buffer{},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, trackerclient.ErrFrozen)

	assert.Equal(t, 1, client.transferCalls)
	got := findTransferRecords(t, auditPath)
	require.Len(t, got, 1)
	assert.Equal(t, auditlog.TransferOutcomeRejected, got[0].Outcome)
	assert.NotEmpty(t, got[0].OutcomeReason)
	assert.Nil(t, got[0].SourceChainTipHash, "no chain tip on rejection")
	assert.Equal(t, uint64(0), got[0].SourceSeq)
}

func TestTransferCmd_TrackerUnavailable_NoAuditEntry(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.log")
	seedEnrolledIdentity(t, dir, "eu-central-1")

	// Factory itself fails to open the connection.
	factory := func(_ context.Context, _ *identity.Signer) (TransferClient, func(), error) {
		return nil, nil, errors.New("trackerclient: connect refused")
	}

	err := runTransfer(context.Background(), transferOptions{
		ConfigDir:    dir,
		AuditLogPath: auditPath,
		DestRegion:   "us-east-1",
		Amount:       250,
		NewClient:    factory,
		Now:          time.Now,
		Stdout:       io.Discard,
		Stderr:       &bytes.Buffer{},
	})
	require.Error(t, err)
	// No partial audit entry when the tracker was never reachable.
	assert.Empty(t, findTransferRecords(t, auditPath))
}

func TestTransferCmd_DryRun_NoRPC(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.log")
	seedEnrolledIdentity(t, dir, "eu-central-1")

	client := &fakeTransferClient{
		balance: snapshotWithCredits(1000),
	}

	stdout := &bytes.Buffer{}
	err := runTransfer(context.Background(), transferOptions{
		ConfigDir:    dir,
		AuditLogPath: auditPath,
		DestRegion:   "us-east-1",
		Amount:       250,
		DryRun:       true,
		NewClient:    newConstTransferClientFactory(client),
		Now:          time.Now,
		Stdout:       stdout,
		Stderr:       &bytes.Buffer{},
	})
	require.NoError(t, err)

	assert.Equal(t, 0, client.transferCalls, "dry-run must not invoke the RPC")
	assert.Empty(t, findTransferRecords(t, auditPath), "dry-run must not write an audit entry")

	out := stdout.String()
	assert.Contains(t, strings.ToLower(out), "would send")
	assert.Contains(t, out, "us-east-1")
	assert.Contains(t, out, "250")
}

func TestTransferCmd_AmountZero_Rejected(t *testing.T) {
	dir := t.TempDir()
	seedEnrolledIdentity(t, dir, "eu-central-1")

	err := runTransfer(context.Background(), transferOptions{
		ConfigDir:    dir,
		AuditLogPath: filepath.Join(dir, "audit.log"),
		DestRegion:   "us-east-1",
		Amount:       0,
		NewClient:    newConstTransferClientFactory(&fakeTransferClient{}),
		Now:          time.Now,
		Stdout:       io.Discard,
		Stderr:       &bytes.Buffer{},
	})
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "amount")
}

func TestTransferCmd_MissingDestRegion_Rejected(t *testing.T) {
	dir := t.TempDir()
	seedEnrolledIdentity(t, dir, "eu-central-1")

	err := runTransfer(context.Background(), transferOptions{
		ConfigDir:    dir,
		AuditLogPath: filepath.Join(dir, "audit.log"),
		DestRegion:   "",
		Amount:       100,
		NewClient:    newConstTransferClientFactory(&fakeTransferClient{}),
		Now:          time.Now,
		Stdout:       io.Discard,
		Stderr:       &bytes.Buffer{},
	})
	require.Error(t, err)
}

func TestTransferCmd_DestEqualsSource_Rejected(t *testing.T) {
	dir := t.TempDir()
	seedEnrolledIdentity(t, dir, "eu-central-1")

	err := runTransfer(context.Background(), transferOptions{
		ConfigDir:    dir,
		AuditLogPath: filepath.Join(dir, "audit.log"),
		DestRegion:   "eu-central-1",
		Amount:       100,
		NewClient:    newConstTransferClientFactory(&fakeTransferClient{}),
		Now:          time.Now,
		Stdout:       io.Discard,
		Stderr:       &bytes.Buffer{},
	})
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "region")
}

func TestTransferCmd_NoIdentity_Rejected(t *testing.T) {
	dir := t.TempDir() // no enroll seed

	err := runTransfer(context.Background(), transferOptions{
		ConfigDir:    dir,
		AuditLogPath: filepath.Join(dir, "audit.log"),
		DestRegion:   "us-east-1",
		Amount:       100,
		NewClient:    newConstTransferClientFactory(&fakeTransferClient{}),
		Now:          time.Now,
		Stdout:       io.Discard,
		Stderr:       &bytes.Buffer{},
	})
	require.Error(t, err)
}

func TestTransferCmd_RegisteredOnRoot(t *testing.T) {
	root := newRootCmd()
	transferCmd, _, err := root.Find([]string{"transfer"})
	require.NoError(t, err)
	require.NotNil(t, transferCmd)
	assert.Equal(t, "transfer", transferCmd.Name())
}
