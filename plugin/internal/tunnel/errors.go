package tunnel

import "errors"

// Sentinel errors. Discriminate via errors.Is.
var (
	// ErrInvalidConfig is returned by NewDialer / NewListener when
	// Config has a malformed field. The wrapped error names the field.
	ErrInvalidConfig = errors.New("tunnel: invalid config")

	// ErrPeerPinMismatch is returned during the TLS handshake when the
	// peer's leaf certificate does not present the expected Ed25519
	// public key. Surfaces from the QUIC handshake as a wrapped error.
	ErrPeerPinMismatch = errors.New("tunnel: peer pin mismatch")

	// ErrALPNMismatch is returned during the TLS handshake when the
	// negotiated ALPN protocol is not "tb-tun/1".
	ErrALPNMismatch = errors.New("tunnel: alpn mismatch")

	// ErrHandshakeFailed wraps any non-pin / non-ALPN handshake failure
	// (TLS error, network error during handshake, context cancellation
	// during handshake).
	ErrHandshakeFailed = errors.New("tunnel: handshake failed")

	// ErrFramingViolation is returned when the peer sent bytes that do
	// not match the v1 wire format: short header, unknown status byte,
	// truncated body, etc.
	ErrFramingViolation = errors.New("tunnel: framing violation")

	// ErrRequestTooLarge is returned by ReadRequest when the length
	// prefix exceeds Config.MaxRequestBytes. The reader does NOT
	// consume the oversized body; the caller closes the stream.
	ErrRequestTooLarge = errors.New("tunnel: request too large")

	// ErrPeerError is returned by ReadResponseStatus when the peer
	// signaled a status of 0x01 (ERROR). The wrapped error carries
	// the UTF-8 message body (truncated to 4 KiB).
	ErrPeerError = errors.New("tunnel: peer error")

	// ErrTunnelClosed is returned by Send/Receive after Close has been
	// called or after the underlying QUIC connection has terminated.
	ErrTunnelClosed = errors.New("tunnel: closed")

	// ErrHolePunchFailed is returned by Dial when a rendezvous-mode dial
	// could not establish a direct UDP path to the peer's reflexive address
	// before the configured HolePunchTimeout. The wrapped error is the last
	// underlying QUIC handshake error.
	ErrHolePunchFailed = errors.New("tunnel: hole-punch failed")

	// ErrRelayFailed is returned by Dial when the tracker's relay coords
	// could not be obtained (Rendezvous.OpenRelay error) or the QUIC
	// handshake against the relay endpoint failed.
	ErrRelayFailed = errors.New("tunnel: relay dial failed")
)
