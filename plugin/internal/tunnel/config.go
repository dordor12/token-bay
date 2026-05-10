package tunnel

import (
	"crypto/ed25519"
	"fmt"
	"time"
)

// Defaults are documented in doc.go.
const (
	defaultMaxRequestBytes  = 1 << 20 // 1 MiB
	defaultHandshakeTimeout = 5 * time.Second
	defaultIdleTimeout      = 30 * time.Second

	// defaultHolePunchTimeout is the per-attempt budget for the consumer's
	// QUIC handshake against the peer's reflexive address before falling
	// back to the relay path. 3s sits comfortably above typical home-NAT
	// propagation (~150ms) and well below the 2s "next message" budget at
	// the ccproxy layer (which times broker + relay together, not just the
	// punch).
	defaultHolePunchTimeout = 3 * time.Second
)

// Config is shared by Dialer and Listener. Caller-supplied fields
// are validated by applyDefaults+validate before use.
type Config struct {
	// EphemeralPriv is this peer's per-session Ed25519 private key.
	// Required. The matching public key is shipped to the peer
	// out-of-band (tracker offer) and pinned by the peer.
	EphemeralPriv ed25519.PrivateKey

	// PeerPin is the expected Ed25519 public key of the peer leaf
	// cert. Required. Length must be ed25519.PublicKeySize (32).
	PeerPin ed25519.PublicKey

	// MaxRequestBytes caps the consumer→seeder body. 0 → default.
	MaxRequestBytes int

	// HandshakeTimeout caps QUIC + TLS handshake duration.
	// 0 → default.
	HandshakeTimeout time.Duration

	// IdleTimeout closes the QUIC connection after this much idle.
	// 0 → default.
	IdleTimeout time.Duration

	// Now returns the current time for cert NotBefore/NotAfter.
	// nil → time.Now.
	Now func() time.Time

	// Rendezvous, when non-nil, switches Dial and Listen into rendezvous
	// mode. The caller (orchestration layer) implements the interface by
	// adapting *trackerclient.Client. nil → direct mode (existing behavior).
	Rendezvous Rendezvous

	// SessionID is the brokered request id used when Rendezvous.OpenRelay
	// is invoked on hole-punch failure. Ignored when Rendezvous is nil or
	// when the hole-punch attempt succeeds.
	SessionID [16]byte

	// HolePunchTimeout caps the rendezvous-mode QUIC handshake against the
	// peer's reflexive address. On expiry, Dial falls back to the relay
	// path. 0 → defaultHolePunchTimeout. Ignored when Rendezvous is nil.
	HolePunchTimeout time.Duration
}

func (c *Config) applyDefaults() {
	if c.MaxRequestBytes == 0 {
		c.MaxRequestBytes = defaultMaxRequestBytes
	}
	if c.HandshakeTimeout == 0 {
		c.HandshakeTimeout = defaultHandshakeTimeout
	}
	if c.IdleTimeout == 0 {
		c.IdleTimeout = defaultIdleTimeout
	}
	if c.HolePunchTimeout == 0 {
		c.HolePunchTimeout = defaultHolePunchTimeout
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

func (c *Config) validate() error {
	if len(c.EphemeralPriv) != ed25519.PrivateKeySize {
		return fmt.Errorf("%w: EphemeralPriv length %d, want %d",
			ErrInvalidConfig, len(c.EphemeralPriv), ed25519.PrivateKeySize)
	}
	if len(c.PeerPin) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: PeerPin length %d, want %d",
			ErrInvalidConfig, len(c.PeerPin), ed25519.PublicKeySize)
	}
	if c.MaxRequestBytes < 0 {
		return fmt.Errorf("%w: MaxRequestBytes %d < 0",
			ErrInvalidConfig, c.MaxRequestBytes)
	}
	if c.HolePunchTimeout < 0 {
		return fmt.Errorf("%w: HolePunchTimeout %v < 0",
			ErrInvalidConfig, c.HolePunchTimeout)
	}
	return nil
}
