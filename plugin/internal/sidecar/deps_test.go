package sidecar

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/auditlog"
	"github.com/token-bay/token-bay/plugin/internal/identity"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
)

func validDeps(t *testing.T) Deps {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := identity.New(priv)
	require.NoError(t, err)

	dir := t.TempDir()
	logger, err := auditlog.Open(filepath.Join(dir, "audit.log"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = logger.Close() })

	return Deps{
		Logger:      zerolog.Nop(),
		Signer:      signer,
		AuditLog:    logger,
		CCProxyAddr: "127.0.0.1:0",
		TrackerEndpoints: []trackerclient.TrackerEndpoint{
			{Addr: "tracker.example:7443", IdentityHash: [32]byte{1}, Region: "test"},
		},
	}
}

func TestDeps_Validate_Happy(t *testing.T) {
	deps := validDeps(t)
	require.NoError(t, deps.Validate())
}

func TestDeps_Validate_NilSigner(t *testing.T) {
	deps := validDeps(t)
	deps.Signer = nil
	err := deps.Validate()
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidDeps))
	require.Contains(t, err.Error(), "Signer")
}

func TestDeps_Validate_NilAuditLog(t *testing.T) {
	deps := validDeps(t)
	deps.AuditLog = nil
	err := deps.Validate()
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidDeps))
	require.Contains(t, err.Error(), "AuditLog")
}

func TestDeps_Validate_EmptyCCProxyAddr(t *testing.T) {
	deps := validDeps(t)
	deps.CCProxyAddr = ""
	err := deps.Validate()
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidDeps))
	require.Contains(t, err.Error(), "CCProxyAddr")
}

func TestDeps_Validate_EmptyTrackerEndpoints(t *testing.T) {
	deps := validDeps(t)
	deps.TrackerEndpoints = nil
	err := deps.Validate()
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidDeps))
	require.Contains(t, err.Error(), "TrackerEndpoints")
}
