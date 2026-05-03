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
// # Out of scope
//
// This package does NOT speak STUN, does NOT speak TURN, does NOT
// orchestrate hole-punch retries, does NOT call into trackerclient,
// and does NOT interpret the seeder's stream-json output. Callers
// supply a netip.AddrPort and receive bytes; orchestration belongs
// to the sidecar's data-path coordinator (separate plan).
package tunnel
