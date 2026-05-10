package tunnel

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validConfig(t *testing.T) Config {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	peerPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return Config{
		EphemeralPriv: priv,
		PeerPin:       peerPub,
		Now:           func() time.Time { return time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC) },
	}
}

func TestConfig_DefaultsApplied(t *testing.T) {
	cfg := validConfig(t)
	cfg.applyDefaults()
	assert.Equal(t, defaultMaxRequestBytes, cfg.MaxRequestBytes)
	assert.Equal(t, defaultHandshakeTimeout, cfg.HandshakeTimeout)
	assert.Equal(t, defaultIdleTimeout, cfg.IdleTimeout)
}

func TestConfig_DefaultsRespectExplicitValues(t *testing.T) {
	cfg := validConfig(t)
	cfg.MaxRequestBytes = 999
	cfg.HandshakeTimeout = 7 * time.Second
	cfg.IdleTimeout = 9 * time.Second
	cfg.applyDefaults()
	assert.Equal(t, 999, cfg.MaxRequestBytes)
	assert.Equal(t, 7*time.Second, cfg.HandshakeTimeout)
	assert.Equal(t, 9*time.Second, cfg.IdleTimeout)
}

func TestConfig_Validate_HappyPath(t *testing.T) {
	cfg := validConfig(t)
	require.NoError(t, cfg.validate())
}

func TestConfig_Validate_BadEphemeralPriv(t *testing.T) {
	cfg := validConfig(t)
	cfg.EphemeralPriv = nil
	err := cfg.validate()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidConfig))
}

func TestConfig_Validate_BadPeerPin(t *testing.T) {
	cfg := validConfig(t)
	cfg.PeerPin = ed25519.PublicKey([]byte("too-short"))
	err := cfg.validate()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidConfig))
}

func TestConfig_Validate_NegativeMaxRequestBytes(t *testing.T) {
	cfg := validConfig(t)
	cfg.MaxRequestBytes = -1
	err := cfg.validate()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidConfig))
}

func TestConfig_Validate_DefaultsNowToTimeNow(t *testing.T) {
	cfg := validConfig(t)
	cfg.Now = nil
	cfg.applyDefaults()
	assert.NotNil(t, cfg.Now)
	gap := time.Since(cfg.Now())
	if gap < 0 {
		gap = -gap
	}
	assert.Less(t, gap, time.Second)
}

func TestConfig_RendezvousDefaults(t *testing.T) {
	cfg := validConfig(t)
	cfg.Rendezvous = noopRendezvous{}
	cfg.applyDefaults()
	require.NoError(t, cfg.validate())
	assert.Equal(t, defaultHolePunchTimeout, cfg.HolePunchTimeout)
}

func TestConfig_RendezvousNilSessionIDOK(t *testing.T) {
	// SessionID is allowed to be all-zero; it is the caller's responsibility
	// to set it to the brokered request id when relay fallback might fire.
	cfg := validConfig(t)
	cfg.Rendezvous = noopRendezvous{}
	cfg.HolePunchTimeout = 1 * time.Second
	cfg.applyDefaults()
	require.NoError(t, cfg.validate())
}

func TestConfig_HolePunchTimeoutNegative(t *testing.T) {
	cfg := validConfig(t)
	cfg.HolePunchTimeout = -1 * time.Second
	err := cfg.validate()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidConfig))
}
