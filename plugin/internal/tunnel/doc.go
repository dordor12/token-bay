// Package tunnel is the consumer↔seeder data-path tunnel for the
// Token-Bay plugin. It opens a mutually-authenticated, identity-pinned
// QUIC connection between two peers and exposes a simple
// request-then-streamed-response framing on a single bidirectional
// stream.
//
// # Spec
//
// Plugin design §7 ("Peer tunnel"). Architecture spec §10.3
// ("Seeder ↔ Consumer (data plane)"). The plugin design spec leaves
// "exact protocol TBD" — this package locks in the v1 wire format
// documented under "Wire format" below.
//
// # Pinning
//
// Each peer holds a per-session ephemeral Ed25519 keypair. The TLS
// 1.3 leaf certificate is self-signed with that key. Mutual auth:
// both peers set tls.RequireAnyClientCert + a custom
// VerifyPeerCertificate that compares the peer leaf's
// SubjectPublicKeyInfo bytes to the expected pin. No CA trust; no
// hostname/SAN checks; no certificate transparency. The pin
// (32-byte ed25519.PublicKey) is the only authentication surface.
//
// # ALPN
//
// Single ALPN protocol id "tb-tun/1". Mismatch aborts the handshake.
//
// # Wire format (v1)
//
// One bidirectional QUIC stream per tunnel session. Bytes are:
//
//	consumer → seeder:
//	    [4 bytes BE length] [N bytes request body]
//	seeder → consumer:
//	    [1 byte status] [content bytes ... until peer closes write]
//
// status enum:
//
//	0x00 = OK     // content is a verbatim Anthropic SSE byte stream
//	0x01 = ERROR  // content is a UTF-8 error message (≤ 4 KiB)
//
// The reader bounds the request body at Config.MaxRequestBytes
// (default 1 MiB) and the error message at 4 KiB; oversize is
// ErrFramingViolation.
//
// # Randomness
//
// Production code does not import crypto/rand directly except inside
// selfSignedCert, where the X.509 stdlib needs an io.Reader for the
// serial number and the certificate signature. Tunneling a Config
// io.Reader through that path adds plumbing without buying anything
// — the certificate is ephemeral and validated structurally, not by
// trust roots. Tests use crypto/rand for ed25519.GenerateKey, which
// is also the stdlib idiom.
//
// # Rendezvous mode (opt-in)
//
// When Config.Rendezvous is non-nil, Dial and Listen switch to rendezvous
// mode:
//
//   - Listen binds an ephemeral UDP socket as before, then calls
//     Rendezvous.AllocateReflexive(ctx) and exposes the result via
//     (*Listener).ReflexiveAddr(). The orchestration layer advertises
//     that address through the broker so consumers can dial it during
//     the simultaneous-open phase.
//
//   - Dial binds an ephemeral UDP socket on a private quic.Transport,
//     calls AllocateReflexive (kept for symmetry / forward-compat with
//     broker advertisement), sends a short burst of UDP punch packets
//     to the peer's reflexive address, then attempts a QUIC handshake
//     against that address bounded by Config.HolePunchTimeout. On
//     timeout it calls Rendezvous.OpenRelay(ctx, SessionID) and dials
//     the returned RelayCoords.Endpoint with the same TLS-pinned
//     handshake.
//
// The pin is enforced end-to-end on both paths — the tracker can relay
// bytes but cannot decrypt them. The Token in RelayCoords is held by
// the caller for billing correlation; this package does not transmit it
// on the data channel.
//
// # Out of scope
//
// This package does NOT speak STUN, does NOT implement TURN's RFC 5766
// framing, does NOT call into trackerclient (Rendezvous is the seam),
// and does NOT interpret the seeder's stream-json output. Callers
// supply Rendezvous (or nil for direct mode) and receive bytes;
// orchestration belongs to the sidecar's data-path coordinator
// (separate plan).
package tunnel
