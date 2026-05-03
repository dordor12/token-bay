# plugin/internal/tunnel Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement `plugin/internal/tunnel` — a QUIC-based, identity-pinned, mutually-authenticated point-to-point tunnel that the consumer plugin uses to forward an Anthropic `/v1/messages` body to a seeder plugin and stream the SSE response back.

**Architecture:** One bidirectional QUIC stream per tunnel session over `github.com/quic-go/quic-go`. TLS 1.3 with self-signed Ed25519 leaf certs on both sides; peers pin each other by raw Ed25519 SPKI pubkey via a custom `VerifyPeerCertificate` (no CA trust, no name validation). Single ALPN protocol id (`tb-tun/1`) gates compatibility. Wire framing: a 4-byte BE length prefix + request body (consumer → seeder, bounded ≤ 1 MiB), then a 1-byte status + content (seeder → consumer; status 0 = OK followed by raw SSE bytes until close, status 1 = error followed by UTF-8 message until close). UDP socket, ephemeral keys, peer pin, and request id are injected by callers — the tunnel does not generate identity material, does not talk to a tracker, and does not orchestrate hole-punch retries. Pure `io.Reader` for randomness, caller-provided `context.Context` for cancellation, no internal `time.Now()` for clocks where avoidable (cert NotBefore/NotAfter use injected clock).

**Tech Stack:** Go 1.25 stdlib (`context`, `crypto/ed25519`, `crypto/rand`, `crypto/tls`, `crypto/x509`, `crypto/x509/pkix`, `encoding/binary`, `errors`, `fmt`, `io`, `math/big`, `net`, `net/netip`, `sync`, `time`); `github.com/quic-go/quic-go` (transport); `github.com/stretchr/testify` (tests).

**Spec:** Plugin design spec `docs/superpowers/specs/plugin/2026-04-22-plugin-design.md` §7 (peer tunnel) and §2.1 step 9. Architecture spec `docs/superpowers/specs/2026-04-22-token-bay-architecture-design.md` §10.3 (data plane) and §1.3 (data-path choice). Tracker stunturn design `docs/superpowers/specs/tracker/2026-05-02-tracker-stunturn-design.md` §11 ("TURN frame format. Deferred to the data-path plan.").

**Repo path note:** This worktree lives at `/Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel`. The worktree path coincidentally contains the package path; that's the branch name, not nesting. The repo root is the worktree root, so `plugin/internal/tunnel/` (the package being built) is at `/Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin/internal/tunnel/`. All absolute paths in this plan use that prefix.

---

## Scope

### In scope

- `plugin/internal/tunnel` package: QUIC transport, mutual TLS pinning, single-stream framing, `Dial` (consumer) and `Listen`/`Accept` (seeder).
- Bounded request-body framing (≤ 1 MiB hard cap; configurable).
- Status-tagged response stream (OK / error).
- End-to-end loopback tests in-process (one goroutine listens, one dials, both meet on `127.0.0.1:0`).
- Failure-mode tests: bad peer pin, ALPN mismatch, oversized body, context cancellation, half-open peer.

### Out of scope (deferred)

- STUN client. The seeder discovers its reflexive `IP:port` via the tracker's `:3478` STUN reflector (`tracker/internal/stunturn` already implemented in 2026-05-02 plan); the consumer receives a `seeder_addr` from `broker_request`. Both addresses are caller-supplied to `Dial`/`Listen`. STUN-client wiring lives in a follow-up plan (`plugin/internal/stunclient` or similar).
- TURN client / data-path relay framing. Hole-punch retry → tracker-relayed fallback (plugin spec §7.3) belongs to the data-path orchestration plan that wraps this transport. Stunturn spec §11 explicitly defers TURN frame format here; for v1 of *this* plan, callers either dial directly to the seeder's reflexive address or fail.
- Hole-punch coordination (simultaneous-open packet exchange). The QUIC handshake is initiated after hole-punch is presumed to have succeeded; orchestration is a follow-up.
- `internal/ccproxy.NetworkRouter` integration. Today's `NetworkRouter` is a 501 stub (`plugin/internal/ccproxy/router.go:41-50`); replacing it with a real call into this tunnel is a follow-up plan.
- `internal/trackerclient` integration. The trackerclient package is empty (`plugin/internal/trackerclient/.gitkeep`); broker-request RPC, seeder-assignment parsing, and ephemeral key transport are out of scope.
- Long-lived identity binding. The tunnel takes a per-session ephemeral Ed25519 keypair as input; binding it to the consumer/seeder long-lived `identity.IdentityID` happens via the tracker offer wire format (separate plan).
- Connection migration, 0-RTT, retry tokens, datagram extension. Default quic-go config is fine.
- Metrics. The package emits no Prometheus or zerolog calls. Sidecar wiring will add observability around `Dial` / `Accept`.

### Architecture decisions locked here

The plugin-design spec §7 is sparse; the following are first-time decisions made by this plan and will inform a future tunnel subsystem spec:

| # | Decision | Choice | Rationale |
|---|---|---|---|
| D1 | Stream count | One bidirectional QUIC stream per tunnel | Half-duplex framing (consumer writes request, then seeder writes response) maps cleanly to one bidi stream; avoids two-stream race for stream-id ordering. |
| D2 | Request framing | 4-byte BE length prefix + body | Bounded; `binary.BigEndian.Uint32` is stdlib; max 4 GiB header capacity, hard-capped at 1 MiB by the reader. |
| D3 | Response framing | 1-byte status + body bytes until peer closes write | Cheapest possible header; consumer reads status first, then forwards bytes verbatim to the open Claude Code HTTP response. Status enum: `0x00=OK`, `0x01=ERROR`. |
| D4 | TLS leaf cert algorithm | Self-signed X.509 with Ed25519 SPKI | Stdlib supports Ed25519 in TLS 1.3 (RFC 8410); avoids inventing X25519↔Ed25519 binding. |
| D5 | Pinning surface | Compare leaf cert's `SubjectPublicKeyInfo` Ed25519 bytes (`ed25519.PublicKey`, 32 bytes) to expected | The pin is the long-lived data; the cert is ephemeral wrapping. |
| D6 | Cert validity | NotBefore = injected `now − 5 min`, NotAfter = `now + 1 h` | Tunnel sessions are short-lived (single request); 1h covers slow network paths and clock skew. |
| D7 | ALPN | `tb-tun/1` (single protocol) | Mismatch terminates the handshake immediately. Future versions bump the suffix. |
| D8 | Mutual auth | `tls.RequireAnyClientCert` on the listener; both sides set `VerifyPeerCertificate` | Both peers prove possession of the matching ephemeral private key. |
| D9 | Per-tunnel state | One QUIC connection, one bidirectional stream, one `Tunnel` struct wrapping them | A second stream is overkill; if observability later needs a control stream, it can multiplex on the same connection without breaking this API. |
| D10 | Max request body | 1 MiB (1 << 20) | Bigger than any plausible Anthropic `/v1/messages` request given Claude Code's UI; defends against memory blow-up if a peer sends a malicious length prefix. Configurable via `Config.MaxRequestBytes`. |
| D11 | Idle / handshake timeouts | Handshake 5s, idle 30s, max stream open 5s; all caller-overridable via `Config` | Stunturn spec defaults `SessionTTL=30s`; we mirror it. Handshake budget is half the consumer fallback's 2s + headroom. |

These are documented in the package's `doc.go` (Task 2).

---

## File map

```
plugin/internal/tunnel/plugin/                                      (worktree-root + plugin module)
├── go.mod                                                          ← MODIFY (Task 1: add quic-go)
├── go.sum                                                           ← MODIFY (Task 1)
├── CLAUDE.md                                                        ← MODIFY (Task 12: mention tunnel)
└── internal/
    └── tunnel/
        ├── .gitkeep                                                ← REMOVE (Task 2)
        ├── doc.go                                                  ← CREATE (Task 2)
        ├── errors.go                                               ← CREATE (Task 3)
        ├── errors_test.go                                          ← CREATE (Task 3)
        ├── tlspinning.go                                           ← CREATE (Task 4)
        ├── tlspinning_test.go                                      ← CREATE (Task 4)
        ├── frame.go                                                ← CREATE (Task 5)
        ├── frame_test.go                                           ← CREATE (Task 5)
        ├── config.go                                               ← CREATE (Task 6)
        ├── config_test.go                                          ← CREATE (Task 6)
        ├── tunnel.go                                               ← CREATE (Tasks 7-9)
        ├── tunnel_test.go                                          ← CREATE (Tasks 7-9)
        ├── dial.go                                                 ← CREATE (Task 7)
        ├── dial_test.go                                            ← CREATE (Task 7)
        ├── listen.go                                               ← CREATE (Task 8)
        ├── listen_test.go                                          ← CREATE (Task 8)
        └── e2e_test.go                                             ← CREATE (Tasks 10-11)
```

## Conventions used in this plan

- All `go test` / `go build` / `go mod tidy` commands run from `/Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin` unless a step says otherwise.
- One commit per task. Conventional-commit prefixes: `feat(plugin/tunnel):`, `test(plugin/tunnel):`, `chore(plugin):`, `docs(plugin/tunnel):`.
- Co-Authored-By footer on every commit:
  ```
  Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
  ```
- TDD per repo `CLAUDE.md`: failing test first; run it; confirm it fails for the *expected* reason; minimal implementation; run again to confirm green; commit.
- Lint policy: respect `.golangci.yml` (errcheck, gofumpt, gosec, ineffassign, misspell, revive[exported], staticcheck, unused). Every exported symbol gets a doc comment.
- Coverage ≥ 90% per file. Use `go test -race -cover ./internal/tunnel/...` after every task.
- Time handling: pass `time.Time` (or a `func() time.Time` clock) in from callers wherever production code references "now"; cert generation in particular must not call `time.Now()` directly so tests can fix `NotBefore`/`NotAfter`.
- Randomness: callers pass an `io.Reader` (typically `crypto/rand.Reader`); the package never imports `crypto/rand` directly.
- Network addresses: use `net/netip.AddrPort` everywhere a public address is exchanged; convert to `*net.UDPAddr` only at the quic-go boundary.
- Lefthook pre-commit runs `gofumpt`, `go vet`, and `golangci-lint run --new-from-rev=HEAD` per module on staged Go files. Run `gofumpt -l -w <files>` before `git add` to avoid the formatter rewriting your stage.

---

## Task 1: Add `github.com/quic-go/quic-go` to `plugin/go.mod`

**Why this is its own task:** importing quic-go in any subsequent Go file before the require is materialized would fail the build. Materialize the require up-front, then delete the scratch file in the same commit.

**Files:**
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin/go.mod`
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin/go.sum`
- Create then remove: `/Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin/internal/tunnel/scratch_quicgo.go`

- [ ] **Step 1: Create a temporary scratch file that imports quic-go**

```bash
mkdir -p /Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin/internal/tunnel
```

Write `/Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin/internal/tunnel/scratch_quicgo.go`:

```go
// Package tunnel — TEMPORARY scratch file used only to materialize the
// `require github.com/quic-go/quic-go` line in plugin/go.mod. Removed in
// the same task.
package tunnel

import _ "github.com/quic-go/quic-go"
```

- [ ] **Step 2: Run `go mod tidy`**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin
go mod tidy
```

Expected: `plugin/go.mod` gains `require github.com/quic-go/quic-go vX.Y.Z` plus its transitive deps; `plugin/go.sum` gains the checksum lines.

- [ ] **Step 3: Verify build**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin
go build ./...
```

Expected: build succeeds with no output.

- [ ] **Step 4: Remove the scratch file and re-tidy**

```bash
rm /Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin/internal/tunnel/scratch_quicgo.go
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin
go mod tidy
```

Expected: `plugin/go.mod` retains `require github.com/quic-go/quic-go vX.Y.Z` (because the next task will re-import it; `go mod tidy` keeps it because nothing has been added that *removes* the import — but if it's pruned, the next task's failing test will fail at import resolution and Step 1 of Task 2 will re-tidy. That's fine.) Either way, leave the require in. If `go mod tidy` removes it, restore it manually:

```bash
go get github.com/quic-go/quic-go@latest
```

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay
gofumpt -l -w plugin/go.mod plugin/go.sum 2>/dev/null || true
git add plugin/go.mod plugin/go.sum
git commit -m "$(cat <<'EOF'
chore(plugin): add github.com/quic-go/quic-go for tunnel transport

Materializes the QUIC dependency that plugin/internal/tunnel will use
for its data-path transport (plugin design spec §7.1).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: Create the tunnel package skeleton with `doc.go`

**Files:**
- Remove: `/Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin/internal/tunnel/.gitkeep`
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin/internal/tunnel/doc.go`

- [ ] **Step 1: Remove `.gitkeep`**

```bash
rm /Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin/internal/tunnel/.gitkeep
```

- [ ] **Step 2: Write `doc.go`**

Contents:

```go
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
```

- [ ] **Step 3: Verify build**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin
go build ./internal/tunnel/...
```

Expected: build succeeds with no output.

- [ ] **Step 4: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay
gofumpt -l -w plugin/internal/tunnel/doc.go
git add plugin/internal/tunnel/.gitkeep plugin/internal/tunnel/doc.go
git commit -m "$(cat <<'EOF'
docs(plugin/tunnel): scaffold package with doc.go

Removes the .gitkeep placeholder and adds a doc.go that records the
v1 wire format, pinning model, ALPN id, and explicit out-of-scope
boundaries (no STUN, no TURN, no hole-punch orchestration).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: Sentinel errors

**Files:**
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin/internal/tunnel/errors.go`
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin/internal/tunnel/errors_test.go`

- [ ] **Step 1: Write the failing test (`errors_test.go`)**

```go
package tunnel

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSentinelsAreDistinct(t *testing.T) {
	all := []error{
		ErrInvalidConfig,
		ErrPeerPinMismatch,
		ErrALPNMismatch,
		ErrHandshakeFailed,
		ErrFramingViolation,
		ErrRequestTooLarge,
		ErrPeerError,
		ErrTunnelClosed,
	}
	seen := make(map[string]bool, len(all))
	for _, e := range all {
		assert.NotNil(t, e)
		msg := e.Error()
		assert.False(t, seen[msg], "duplicate sentinel message: %q", msg)
		seen[msg] = true
	}
}

func TestErrorsIsWrapped(t *testing.T) {
	wrapped := fmt.Errorf("dial: %w", ErrPeerPinMismatch)
	assert.True(t, errors.Is(wrapped, ErrPeerPinMismatch))
	assert.False(t, errors.Is(wrapped, ErrALPNMismatch))
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin
go test ./internal/tunnel/... -run TestSentinelsAreDistinct -v
```

Expected: FAIL with `undefined: ErrInvalidConfig` etc.

- [ ] **Step 3: Implement `errors.go`**

```go
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
)
```

- [ ] **Step 4: Run tests, expect green**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin
go test -race -cover ./internal/tunnel/... -v
```

Expected: PASS; coverage ≥ 90% on errors.go.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay
gofumpt -l -w plugin/internal/tunnel/errors.go plugin/internal/tunnel/errors_test.go
git add plugin/internal/tunnel/errors.go plugin/internal/tunnel/errors_test.go
git commit -m "$(cat <<'EOF'
feat(plugin/tunnel): sentinel errors

Eight sentinels covering config validation, the two structured
handshake failures (peer pin mismatch, ALPN mismatch), the catch-all
handshake failure, framing violations, oversized requests, structured
peer errors, and the post-Close terminal state.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: TLS pinning helpers

**Files:**
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin/internal/tunnel/tlspinning.go`
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin/internal/tunnel/tlspinning_test.go`

- [ ] **Step 1: Write the failing test (`tlspinning_test.go`)**

```go
package tunnel

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixedClock returns an injected clock for cert NotBefore/NotAfter.
func fixedClock(t *testing.T) func() time.Time {
	t.Helper()
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return now }
}

func TestSelfSignedCert_RoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	cert, err := selfSignedCert(priv, fixedClock(t)())
	require.NoError(t, err)
	require.NotNil(t, cert)
	require.Len(t, cert.Certificate, 1)

	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	require.NoError(t, err)

	got, ok := parsed.PublicKey.(ed25519.PublicKey)
	require.True(t, ok, "leaf pubkey not Ed25519")
	assert.Equal(t, ed25519.PublicKey(pub), got)
}

func TestSelfSignedCert_UsesInjectedClock(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	cert, err := selfSignedCert(priv, now)
	require.NoError(t, err)

	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	require.NoError(t, err)

	assert.True(t, parsed.NotBefore.Before(now) || parsed.NotBefore.Equal(now))
	assert.True(t, parsed.NotAfter.After(now))
	assert.WithinDuration(t, now.Add(time.Hour), parsed.NotAfter, 5*time.Minute)
}

func TestVerifyPeerPin_Match(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	cert, err := selfSignedCert(priv, fixedClock(t)())
	require.NoError(t, err)

	verifier := verifyPeerPin(pub)
	err = verifier([][]byte{cert.Certificate[0]}, nil)
	assert.NoError(t, err)
}

func TestVerifyPeerPin_Mismatch(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	cert, err := selfSignedCert(priv, fixedClock(t)())
	require.NoError(t, err)

	verifier := verifyPeerPin(otherPub)
	err = verifier([][]byte{cert.Certificate[0]}, nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrPeerPinMismatch))
}

func TestVerifyPeerPin_NoCertChain(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	verifier := verifyPeerPin(pub)
	err = verifier(nil, nil)
	assert.True(t, errors.Is(err, ErrPeerPinMismatch))
}

func TestVerifyPeerPin_NonEd25519Cert(t *testing.T) {
	// crypto/x509 self-signed RSA cert (synthesized cheaply via crypto/tls).
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	// Pass garbage cert bytes; ParseCertificate will fail and we expect ErrPeerPinMismatch.
	verifier := verifyPeerPin(pub)
	err = verifier([][]byte{[]byte("not-a-cert")}, nil)
	assert.True(t, errors.Is(err, ErrPeerPinMismatch))
}

func TestNewTLSConfig_DialerSide(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	peerPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	cfg, err := newTLSConfig(priv, peerPub, fixedClock(t)(), false /* listener */)
	require.NoError(t, err)
	require.NotNil(t, cfg)

	assert.Equal(t, []string{alpnProto}, cfg.NextProtos)
	assert.True(t, cfg.InsecureSkipVerify)
	assert.NotNil(t, cfg.VerifyPeerCertificate)
	assert.Len(t, cfg.Certificates, 1)
	_ = pub
}

func TestNewTLSConfig_ListenerSide(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	peerPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	cfg, err := newTLSConfig(priv, peerPub, fixedClock(t)(), true /* listener */)
	require.NoError(t, err)
	assert.Equal(t, tls.RequireAnyClientCert, cfg.ClientAuth)
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin
go test ./internal/tunnel/... -run "TestSelfSignedCert|TestVerifyPeerPin|TestNewTLSConfig" -v
```

Expected: FAIL — `undefined: selfSignedCert`, etc.

- [ ] **Step 3: Implement `tlspinning.go`**

```go
package tunnel

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"time"
)

// alpnProto is the single ALPN id this tunnel speaks.
const alpnProto = "tb-tun/1"

// certValidity is the NotAfter offset for self-signed leaf certs.
const certValidity = time.Hour

// certSkew is the NotBefore back-dating window to absorb clock skew.
const certSkew = 5 * time.Minute

// selfSignedCert produces a one-cert tls.Certificate self-signed by
// priv. Subject CN is the hex-encoded Ed25519 pubkey for trace
// readability; pinning ignores it.
func selfSignedCert(priv ed25519.PrivateKey, now time.Time) (*tls.Certificate, error) {
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%w: priv.Public() is not Ed25519", ErrInvalidConfig)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("%w: serial: %v", ErrInvalidConfig, err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: fmt.Sprintf("tb-tun/%x", pub[:8])},
		NotBefore:    now.Add(-certSkew),
		NotAfter:     now.Add(certValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, fmt.Errorf("%w: create: %v", ErrInvalidConfig, err)
	}
	return &tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  priv,
	}, nil
}

// verifyPeerPin returns a tls.Config.VerifyPeerCertificate callback
// that succeeds iff the peer's leaf cert presents an Ed25519
// SubjectPublicKeyInfo equal to expected.
func verifyPeerPin(expected ed25519.PublicKey) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("%w: peer presented no certificates", ErrPeerPinMismatch)
		}
		leaf, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("%w: parse leaf: %v", ErrPeerPinMismatch, err)
		}
		got, ok := leaf.PublicKey.(ed25519.PublicKey)
		if !ok {
			return fmt.Errorf("%w: leaf pubkey is not Ed25519", ErrPeerPinMismatch)
		}
		if len(got) != ed25519.PublicKeySize || len(expected) != ed25519.PublicKeySize {
			return fmt.Errorf("%w: pubkey size mismatch", ErrPeerPinMismatch)
		}
		// constant-time-ish equality; len-checked above
		var diff byte
		for i := 0; i < ed25519.PublicKeySize; i++ {
			diff |= got[i] ^ expected[i]
		}
		if diff != 0 {
			return fmt.Errorf("%w", ErrPeerPinMismatch)
		}
		return nil
	}
}

// newTLSConfig builds a *tls.Config for either side. listener=true
// switches to the server-side template (RequireAnyClientCert).
func newTLSConfig(priv ed25519.PrivateKey, peerPub ed25519.PublicKey, now time.Time, listener bool) (*tls.Config, error) {
	cert, err := selfSignedCert(priv, now)
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{
		Certificates:          []tls.Certificate{*cert},
		NextProtos:            []string{alpnProto},
		MinVersion:            tls.VersionTLS13,
		MaxVersion:            tls.VersionTLS13,
		InsecureSkipVerify:    true, // pinning replaces CA validation
		VerifyPeerCertificate: verifyPeerPin(peerPub),
	}
	if listener {
		cfg.ClientAuth = tls.RequireAnyClientCert
	}
	return cfg, nil
}
```

- [ ] **Step 4: Run tests, expect green**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin
go test -race -cover ./internal/tunnel/... -v
```

Expected: PASS; tlspinning.go ≥ 90% coverage.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay
gofumpt -l -w plugin/internal/tunnel/tlspinning.go plugin/internal/tunnel/tlspinning_test.go
git add plugin/internal/tunnel/tlspinning.go plugin/internal/tunnel/tlspinning_test.go
git commit -m "$(cat <<'EOF'
feat(plugin/tunnel): Ed25519 self-signed cert + pinning verifier

selfSignedCert builds a one-cert tls.Certificate from an Ed25519
private key with a 1h NotAfter and 5m NotBefore skew (injected
clock). verifyPeerPin produces a tls.Config.VerifyPeerCertificate
that compares the peer leaf's SPKI bytes to an expected pubkey,
returning ErrPeerPinMismatch on any failure (no chain, parse error,
non-Ed25519 leaf, byte mismatch).

newTLSConfig wires both sides: listener=true sets
ClientAuth=RequireAnyClientCert; both pin via VerifyPeerCertificate.
ALPN is fixed at "tb-tun/1".

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: Wire-format framing primitives

**Files:**
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin/internal/tunnel/frame.go`
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin/internal/tunnel/frame_test.go`

- [ ] **Step 1: Write the failing test (`frame_test.go`)**

```go
package tunnel

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteRequest_LengthPrefixed(t *testing.T) {
	var buf bytes.Buffer
	body := []byte("hello world")
	require.NoError(t, writeRequest(&buf, body))

	require.Equal(t, 4+len(body), buf.Len())
	got := binary.BigEndian.Uint32(buf.Bytes()[:4])
	assert.Equal(t, uint32(len(body)), got)
	assert.Equal(t, body, buf.Bytes()[4:])
}

func TestReadRequest_HappyPath(t *testing.T) {
	body := []byte("the quick brown fox")
	var buf bytes.Buffer
	require.NoError(t, writeRequest(&buf, body))

	got, err := readRequest(&buf, 1<<20)
	require.NoError(t, err)
	assert.Equal(t, body, got)
}

func TestReadRequest_TooLarge(t *testing.T) {
	var buf bytes.Buffer
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(1024))
	buf.Write(hdr[:])

	_, err := readRequest(&buf, 512)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrRequestTooLarge))
}

func TestReadRequest_ShortHeader(t *testing.T) {
	r := bytes.NewReader([]byte{0x00, 0x00, 0x00})
	_, err := readRequest(r, 1<<20)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrFramingViolation))
}

func TestReadRequest_TruncatedBody(t *testing.T) {
	var buf bytes.Buffer
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], 16)
	buf.Write(hdr[:])
	buf.Write([]byte("only-7"))

	_, err := readRequest(&buf, 1<<20)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrFramingViolation))
}

func TestWriteResponseStatus_OK(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, writeResponseStatus(&buf, statusOK))
	assert.Equal(t, []byte{0x00}, buf.Bytes())
}

func TestWriteResponseStatus_Error(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, writeResponseError(&buf, "out of credits"))
	require.Equal(t, byte(0x01), buf.Bytes()[0])
	assert.Equal(t, []byte("out of credits"), buf.Bytes()[1:])
}

func TestReadResponseStatus_OK(t *testing.T) {
	r := bytes.NewReader([]byte{0x00, 'a', 'b', 'c'})
	st, err := readResponseStatus(r)
	require.NoError(t, err)
	assert.Equal(t, statusOK, st)
	rest, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, []byte("abc"), rest)
}

func TestReadResponseStatus_Error(t *testing.T) {
	r := bytes.NewReader(append([]byte{0x01}, []byte("rate limited")...))
	st, err := readResponseStatus(r)
	require.NoError(t, err)
	assert.Equal(t, statusError, st)
	rest, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, []byte("rate limited"), rest)
}

func TestReadResponseStatus_UnknownByte(t *testing.T) {
	r := bytes.NewReader([]byte{0x42})
	_, err := readResponseStatus(r)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrFramingViolation))
}

func TestReadResponseStatus_EmptyStream(t *testing.T) {
	r := bytes.NewReader(nil)
	_, err := readResponseStatus(r)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrFramingViolation))
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin
go test ./internal/tunnel/... -run "TestWriteRequest|TestReadRequest|TestWriteResponseStatus|TestReadResponseStatus" -v
```

Expected: FAIL — `undefined: writeRequest`, etc.

- [ ] **Step 3: Implement `frame.go`**

```go
package tunnel

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// status enum on the seeder→consumer half of the bidi stream.
type status byte

const (
	statusOK    status = 0x00
	statusError status = 0x01
)

const (
	requestHeaderLen = 4 // big-endian uint32 length prefix
	defaultMaxBytes  = 1 << 20
)

// writeRequest writes [4 BE length] [body] to w.
func writeRequest(w io.Writer, body []byte) error {
	var hdr [requestHeaderLen]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body))) //nolint:gosec // len bounded by caller
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	_, err := w.Write(body)
	return err
}

// readRequest reads the length prefix and body. Returns ErrRequestTooLarge
// when the prefix exceeds maxBytes (the body is NOT drained); returns
// ErrFramingViolation on short header or truncated body.
func readRequest(r io.Reader, maxBytes int) ([]byte, error) {
	var hdr [requestHeaderLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, fmt.Errorf("%w: header: %v", ErrFramingViolation, err)
	}
	n := int(binary.BigEndian.Uint32(hdr[:]))
	if maxBytes > 0 && n > maxBytes {
		return nil, fmt.Errorf("%w: %d > %d", ErrRequestTooLarge, n, maxBytes)
	}
	if n == 0 {
		return []byte{}, nil
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("%w: body: %v", ErrFramingViolation, err)
	}
	return body, nil
}

// writeResponseStatus writes a single status byte (no payload).
func writeResponseStatus(w io.Writer, s status) error {
	_, err := w.Write([]byte{byte(s)})
	return err
}

// writeResponseError writes [0x01] followed by msg's UTF-8 bytes.
// The caller is expected to close the stream after writing.
func writeResponseError(w io.Writer, msg string) error {
	if _, err := w.Write([]byte{byte(statusError)}); err != nil {
		return err
	}
	if msg == "" {
		return nil
	}
	_, err := w.Write([]byte(msg))
	return err
}

// readResponseStatus consumes the first byte of the seeder→consumer
// half-stream and returns the typed status. Subsequent bytes are the
// caller's to consume.
func readResponseStatus(r io.Reader) (status, error) {
	var b [1]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, fmt.Errorf("%w: empty response stream", ErrFramingViolation)
		}
		return 0, fmt.Errorf("%w: read status: %v", ErrFramingViolation, err)
	}
	switch status(b[0]) {
	case statusOK, statusError:
		return status(b[0]), nil
	default:
		return 0, fmt.Errorf("%w: unknown status byte 0x%02x", ErrFramingViolation, b[0])
	}
}
```

- [ ] **Step 4: Run tests, expect green**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin
go test -race -cover ./internal/tunnel/... -v
```

Expected: PASS; frame.go ≥ 90% coverage.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay
gofumpt -l -w plugin/internal/tunnel/frame.go plugin/internal/tunnel/frame_test.go
git add plugin/internal/tunnel/frame.go plugin/internal/tunnel/frame_test.go
git commit -m "$(cat <<'EOF'
feat(plugin/tunnel): wire-format framing primitives

writeRequest / readRequest implement the consumer→seeder half:
4-byte BE length prefix + body, bounded by maxBytes
(ErrRequestTooLarge) and rejecting short header / truncated body
(ErrFramingViolation).

writeResponseStatus / writeResponseError / readResponseStatus
implement the seeder→consumer status byte (0x00 OK, 0x01 ERROR).
Unknown status bytes are ErrFramingViolation. Subsequent bytes are
the caller's to read until the peer closes write.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: Config struct + validation

**Files:**
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin/internal/tunnel/config.go`
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin/internal/tunnel/config_test.go`

- [ ] **Step 1: Write the failing test (`config_test.go`)**

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin
go test ./internal/tunnel/... -run "TestConfig_" -v
```

Expected: FAIL — `undefined: Config`, etc.

- [ ] **Step 3: Implement `config.go`**

```go
package tunnel

import (
	"crypto/ed25519"
	"fmt"
	"time"
)

// Defaults are documented in doc.go.
const (
	defaultMaxRequestBytes  = 1 << 20      // 1 MiB
	defaultHandshakeTimeout = 5 * time.Second
	defaultIdleTimeout      = 30 * time.Second
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
	return nil
}
```

- [ ] **Step 4: Run tests, expect green**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin
go test -race -cover ./internal/tunnel/... -v
```

Expected: PASS; config.go ≥ 90% coverage.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay
gofumpt -l -w plugin/internal/tunnel/config.go plugin/internal/tunnel/config_test.go
git add plugin/internal/tunnel/config.go plugin/internal/tunnel/config_test.go
git commit -m "$(cat <<'EOF'
feat(plugin/tunnel): Config struct with applyDefaults + validate

Shared by Dialer and Listener. Required fields are EphemeralPriv
(Ed25519 private key, length-checked) and PeerPin (Ed25519 public
key, length-checked). Defaults: MaxRequestBytes=1 MiB,
HandshakeTimeout=5s, IdleTimeout=30s, Now=time.Now.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 7: Tunnel struct + consumer-side `Dial`

**Files:**
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin/internal/tunnel/tunnel.go`
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin/internal/tunnel/dial.go`
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin/internal/tunnel/dial_test.go`

This task introduces the `Tunnel` type carrying the bidi stream and adds `Dial` to open one. The seeder counterpart (`Listen` / `Accept`) is Task 8; we test Dial against an in-process bare quic-go listener that mirrors the seeder behavior just enough for handshake+stream-open.

- [ ] **Step 1: Write the failing test (`dial_test.go`)**

```go
package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loopbackListener spins up a bare quic-go listener with the seeder
// config; the test interacts with it directly via quic-go to exercise
// Dial without needing the full Listen API (Task 8).
func loopbackListener(t *testing.T, seederPriv ed25519.PrivateKey, consumerPub ed25519.PublicKey) (netip.AddrPort, *quic.Listener, func()) {
	t.Helper()
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	require.NoError(t, err)

	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	tlsCfg, err := newTLSConfig(seederPriv, consumerPub, now, true)
	require.NoError(t, err)

	tr := &quic.Transport{Conn: udp}
	ln, err := tr.Listen(tlsCfg, &quic.Config{
		HandshakeIdleTimeout: 5 * time.Second,
		MaxIdleTimeout:       30 * time.Second,
	})
	require.NoError(t, err)

	addrPort := udp.LocalAddr().(*net.UDPAddr).AddrPort()
	cleanup := func() {
		_ = ln.Close()
		_ = tr.Close()
	}
	return addrPort, ln, cleanup
}

func TestDial_HandshakesAndOpensStream(t *testing.T) {
	seederPub, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerPub, consumerPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	addr, ln, cleanup := loopbackListener(t, seederPriv, consumerPub)
	defer cleanup()

	// Seeder side: accept and immediately close to confirm round-trip.
	seederDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, err := ln.Accept(ctx)
		if err != nil {
			seederDone <- err
			return
		}
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			seederDone <- err
			return
		}
		_ = stream.Close()
		_ = conn.CloseWithError(0, "ok")
		seederDone <- nil
	}()

	// Consumer side.
	cfg := Config{
		EphemeralPriv: consumerPriv,
		PeerPin:       seederPub,
		Now:           func() time.Time { return time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC) },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tun, err := Dial(ctx, addr, cfg)
	require.NoError(t, err)
	require.NotNil(t, tun)
	defer tun.Close()

	require.NoError(t, <-seederDone)
}

func TestDial_PinMismatch(t *testing.T) {
	_, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerPub, consumerPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	wrongSeederPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	addr, ln, cleanup := loopbackListener(t, seederPriv, consumerPub)
	defer cleanup()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = ln.Accept(ctx) // ignore — handshake will fail on the consumer side
	}()

	cfg := Config{
		EphemeralPriv: consumerPriv,
		PeerPin:       wrongSeederPub,
		Now:           func() time.Time { return time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC) },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err = Dial(ctx, addr, cfg)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrPeerPinMismatch) || errors.Is(err, ErrHandshakeFailed),
		"got %v", err)
}

func TestDial_ContextCancel(t *testing.T) {
	consumerPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, consumerPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_ = consumerPub

	// Address that nothing listens on.
	addr := netip.MustParseAddrPort("127.0.0.1:1") // privileged port no one binds in tests

	cfg := Config{
		EphemeralPriv: consumerPriv,
		PeerPin:       consumerPub, // any 32-byte pub — handshake never starts
		Now:           func() time.Time { return time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC) },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err = Dial(ctx, addr, cfg)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrHandshakeFailed) || errors.Is(err, context.DeadlineExceeded),
		"got %v", err)
}

func TestDial_BadConfig(t *testing.T) {
	_, err := Dial(context.Background(), netip.MustParseAddrPort("127.0.0.1:1"), Config{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidConfig))
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin
go test ./internal/tunnel/... -run "TestDial_" -v
```

Expected: FAIL — `undefined: Dial`.

- [ ] **Step 3: Implement `tunnel.go`**

```go
package tunnel

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/quic-go/quic-go"
)

// Tunnel is one open consumer↔seeder QUIC connection wrapping one
// bidirectional stream. Methods are safe for concurrent use only in
// the documented half-duplex order: write the request fully before
// reading the response.
type Tunnel struct {
	conn   quic.Connection
	stream quic.Stream
	cfg    Config

	mu     sync.Mutex
	closed bool
}

// Send writes the consumer→seeder request body in length-prefixed form.
// Returns ErrTunnelClosed if Close has been called.
func (t *Tunnel) Send(body []byte) error {
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return ErrTunnelClosed
	}
	return writeRequest(t.stream, body)
}

// Receive returns the seeder's response status and an io.Reader of the
// remaining content bytes (until the peer closes write). status==statusError
// implies the reader yields the UTF-8 error message.
func (t *Tunnel) Receive(ctx context.Context) (status, io.Reader, error) {
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return 0, nil, ErrTunnelClosed
	}
	st, err := readResponseStatus(t.stream)
	if err != nil {
		return 0, nil, err
	}
	return st, t.stream, nil
}

// Close tears down the QUIC connection. Idempotent.
func (t *Tunnel) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()
	if t.stream != nil {
		_ = t.stream.Close()
	}
	if t.conn != nil {
		return t.conn.CloseWithError(0, "tunnel closed")
	}
	return nil
}

// errIs is shorthand used by Dial / Accept implementations.
func errIs(err error, target error) bool { return errors.Is(err, target) }
```

- [ ] **Step 4: Implement `dial.go`**

```go
package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/quic-go/quic-go"
)

// Dial opens a QUIC connection to the seeder at addr and a single
// bidirectional stream. Returns *Tunnel on success.
//
// The handshake is bounded by ctx; pass context.WithTimeout for the
// 2-second budget the consumer fallback path needs (see plugin design
// spec §5.4 — propagation + handshake must fit inside the user-visible
// "next message" latency).
func Dial(ctx context.Context, addr netip.AddrPort, cfg Config) (*Tunnel, error) {
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	tlsCfg, err := newTLSConfig(cfg.EphemeralPriv, cfg.PeerPin, cfg.Now(), false)
	if err != nil {
		return nil, err
	}
	quicCfg := &quic.Config{
		HandshakeIdleTimeout: cfg.HandshakeTimeout,
		MaxIdleTimeout:       cfg.IdleTimeout,
	}

	hsCtx, cancel := context.WithTimeout(ctx, cfg.HandshakeTimeout)
	defer cancel()

	udp := &net.UDPAddr{IP: addr.Addr().AsSlice(), Port: int(addr.Port())}
	conn, err := quic.DialAddr(hsCtx, udp.String(), tlsCfg, quicCfg)
	if err != nil {
		return nil, mapHandshakeErr(err)
	}

	stream, err := conn.OpenStreamSync(hsCtx)
	if err != nil {
		_ = conn.CloseWithError(0, "stream open failed")
		return nil, fmt.Errorf("%w: open stream: %v", ErrHandshakeFailed, err)
	}

	return &Tunnel{conn: conn, stream: stream, cfg: cfg}, nil
}

// mapHandshakeErr inspects err and surfaces ErrPeerPinMismatch / ErrALPNMismatch
// when their underlying signal is recognizable; otherwise wraps ErrHandshakeFailed.
//
// The TLS handshake error message from quic-go embeds the alert string. We pattern-match
// to surface the structured sentinel; the underlying error is preserved via wrapping.
func mapHandshakeErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
	msg := err.Error()
	switch {
	case errors.Is(err, ErrPeerPinMismatch):
		return err
	case strings.Contains(msg, "tunnel: peer pin mismatch"):
		return fmt.Errorf("%w: %v", ErrPeerPinMismatch, err)
	case strings.Contains(msg, "no application protocol"),
		strings.Contains(msg, "ALPN"):
		return fmt.Errorf("%w: %v", ErrALPNMismatch, err)
	default:
		return fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
}

// timeoutBudget returns the soonest deadline between ctx and now+d.
// Helper exported for symmetry with Accept; unused here but kept for the listener.
//
//nolint:unused
func timeoutBudget(ctx context.Context, d time.Duration) time.Time {
	dl, ok := ctx.Deadline()
	if !ok {
		return time.Now().Add(d)
	}
	other := time.Now().Add(d)
	if dl.Before(other) {
		return dl
	}
	return other
}
```

- [ ] **Step 5: Run tests, expect green**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin
go test -race -cover ./internal/tunnel/... -v
```

Expected: PASS; tunnel.go and dial.go ≥ 90% coverage. The pin-mismatch test may surface ErrPeerPinMismatch or ErrHandshakeFailed depending on which side detects first — both branches are accepted by the test.

- [ ] **Step 6: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay
gofumpt -l -w plugin/internal/tunnel/tunnel.go plugin/internal/tunnel/dial.go plugin/internal/tunnel/dial_test.go
git add plugin/internal/tunnel/tunnel.go plugin/internal/tunnel/dial.go plugin/internal/tunnel/dial_test.go
git commit -m "$(cat <<'EOF'
feat(plugin/tunnel): consumer-side Dial + Tunnel struct

Tunnel wraps one quic.Connection + one quic.Stream with Send/Receive/
Close (half-duplex by contract: write request, then read response).

Dial opens a QUIC connection over UDP to the seeder's AddrPort with
the pinned mutual TLS config (Task 4) and an OpenStreamSync for the
bidi stream. Handshake errors are surfaced as ErrPeerPinMismatch /
ErrALPNMismatch when recognizable, otherwise wrapped as
ErrHandshakeFailed. Context cancellation propagates as
ErrHandshakeFailed.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 8: Seeder-side `Listen` / `Accept`

**Files:**
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin/internal/tunnel/listen.go`
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin/internal/tunnel/listen_test.go`

- [ ] **Step 1: Write the failing test (`listen_test.go`)**

```go
package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListen_BadConfig(t *testing.T) {
	_, err := Listen(netip.MustParseAddrPort("127.0.0.1:0"), Config{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidConfig))
}

func TestListen_AcceptHandshake(t *testing.T) {
	seederPub, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerPub, consumerPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	clk := func() time.Time { return time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC) }
	seederCfg := Config{
		EphemeralPriv: seederPriv,
		PeerPin:       consumerPub,
		Now:           clk,
	}
	consumerCfg := Config{
		EphemeralPriv: consumerPriv,
		PeerPin:       seederPub,
		Now:           clk,
	}

	ln, err := Listen(netip.MustParseAddrPort("127.0.0.1:0"), seederCfg)
	require.NoError(t, err)
	defer ln.Close()

	addr := ln.LocalAddr()
	require.True(t, addr.IsValid())
	require.NotEqual(t, uint16(0), addr.Port())

	type acceptResult struct {
		tun *Tunnel
		err error
	}
	accCh := make(chan acceptResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		tun, err := ln.Accept(ctx)
		accCh <- acceptResult{tun, err}
	}()

	dialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	consumerTun, err := Dial(dialCtx, addr, consumerCfg)
	require.NoError(t, err)
	defer consumerTun.Close()

	res := <-accCh
	require.NoError(t, res.err)
	require.NotNil(t, res.tun)
	defer res.tun.Close()
}

func TestListen_AcceptCtxCancel(t *testing.T) {
	_, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	cfg := Config{
		EphemeralPriv: seederPriv,
		PeerPin:       consumerPub,
		Now:           func() time.Time { return time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC) },
	}
	ln, err := Listen(netip.MustParseAddrPort("127.0.0.1:0"), cfg)
	require.NoError(t, err)
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = ln.Accept(ctx)
	require.Error(t, err)
	// Either ctx.DeadlineExceeded surfaces directly or it's wrapped as a tunnel error.
	assert.True(t, errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrHandshakeFailed),
		"got %v", err)
}

func TestListener_LocalAddr_AfterClose(t *testing.T) {
	_, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	cfg := Config{
		EphemeralPriv: seederPriv,
		PeerPin:       consumerPub,
		Now:           func() time.Time { return time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC) },
	}
	ln, err := Listen(netip.MustParseAddrPort("127.0.0.1:0"), cfg)
	require.NoError(t, err)
	require.NoError(t, ln.Close())

	// Second Close is a no-op (idempotent).
	assert.NoError(t, ln.Close())

	// LocalAddr after close returns the last known address.
	_ = ln.LocalAddr()

	// Bad bind also surfaces ErrInvalidConfig.
	_, err = Listen(netip.AddrPort{}, cfg)
	require.Error(t, err)
	_ = net.IPv4zero
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin
go test ./internal/tunnel/... -run "TestListen_|TestListener_" -v
```

Expected: FAIL — `undefined: Listen`.

- [ ] **Step 3: Implement `listen.go`**

```go
package tunnel

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync"

	"github.com/quic-go/quic-go"
)

// Listener is the seeder-side accept loop. One Listener may produce
// many *Tunnel via Accept.
type Listener struct {
	cfg       Config
	transport *quic.Transport
	udp       *net.UDPConn
	ln        *quic.Listener
	addr      netip.AddrPort

	mu     sync.Mutex
	closed bool
}

// Listen binds a UDP socket at bind and starts accepting QUIC connections.
// bind.Port == 0 allocates an ephemeral port; LocalAddr returns the
// resolved AddrPort after Listen returns.
func Listen(bind netip.AddrPort, cfg Config) (*Listener, error) {
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if !bind.Addr().IsValid() {
		return nil, fmt.Errorf("%w: bind address invalid", ErrInvalidConfig)
	}

	udpAddr := &net.UDPAddr{IP: bind.Addr().AsSlice(), Port: int(bind.Port())}
	udp, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, fmt.Errorf("%w: listen udp %s: %v", ErrInvalidConfig, bind, err)
	}

	tlsCfg, err := newTLSConfig(cfg.EphemeralPriv, cfg.PeerPin, cfg.Now(), true)
	if err != nil {
		_ = udp.Close()
		return nil, err
	}
	quicCfg := &quic.Config{
		HandshakeIdleTimeout: cfg.HandshakeTimeout,
		MaxIdleTimeout:       cfg.IdleTimeout,
	}
	tr := &quic.Transport{Conn: udp}
	ln, err := tr.Listen(tlsCfg, quicCfg)
	if err != nil {
		_ = tr.Close()
		_ = udp.Close()
		return nil, fmt.Errorf("%w: quic listen: %v", ErrInvalidConfig, err)
	}

	resolved := udp.LocalAddr().(*net.UDPAddr).AddrPort()

	return &Listener{
		cfg:       cfg,
		transport: tr,
		udp:       udp,
		ln:        ln,
		addr:      resolved,
	}, nil
}

// LocalAddr returns the bound address.
func (l *Listener) LocalAddr() netip.AddrPort { return l.addr }

// Accept returns the next handshaked tunnel. Blocks until a peer
// completes the QUIC + TLS handshake AND opens the bidi stream, or
// ctx fires.
func (l *Listener) Accept(ctx context.Context) (*Tunnel, error) {
	conn, err := l.ln.Accept(ctx)
	if err != nil {
		return nil, mapHandshakeErr(err)
	}
	stream, err := conn.AcceptStream(ctx)
	if err != nil {
		_ = conn.CloseWithError(0, "stream accept failed")
		return nil, fmt.Errorf("%w: accept stream: %v", ErrHandshakeFailed, err)
	}
	return &Tunnel{conn: conn, stream: stream, cfg: l.cfg}, nil
}

// Close tears down the listener. Idempotent.
func (l *Listener) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	l.mu.Unlock()
	_ = l.ln.Close()
	_ = l.transport.Close()
	return l.udp.Close()
}
```

- [ ] **Step 4: Run tests, expect green**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin
go test -race -cover ./internal/tunnel/... -v
```

Expected: PASS; listen.go ≥ 90% coverage.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay
gofumpt -l -w plugin/internal/tunnel/listen.go plugin/internal/tunnel/listen_test.go
git add plugin/internal/tunnel/listen.go plugin/internal/tunnel/listen_test.go
git commit -m "$(cat <<'EOF'
feat(plugin/tunnel): seeder-side Listen + Accept

Listener binds a UDP socket, wraps it in a quic.Transport, and accepts
QUIC connections with the pinned mutual TLS config. Accept blocks until
a peer handshakes AND opens the single bidirectional stream this
protocol expects, then returns a *Tunnel ready for ReadRequest →
WriteResponse.

LocalAddr surfaces the resolved AddrPort after ephemeral-port binding.
Close is idempotent.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 9: Half-duplex helpers on `Tunnel` (request body + response status)

The `Tunnel` struct already has `Send` (calls `writeRequest`) and `Receive` (calls `readResponseStatus` and returns the underlying stream). Now add the symmetric seeder-side helpers and confirm they integrate.

**Files:**
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin/internal/tunnel/tunnel.go`
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin/internal/tunnel/tunnel_test.go` (CREATE)

- [ ] **Step 1: Write the failing test (`tunnel_test.go`)**

```go
package tunnel

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func dialAcceptPair(t *testing.T) (consumer, seeder *Tunnel, cleanup func()) {
	t.Helper()
	seederPub, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerPub, consumerPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	clk := func() time.Time { return time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC) }

	ln, err := Listen(netip.MustParseAddrPort("127.0.0.1:0"), Config{
		EphemeralPriv: seederPriv, PeerPin: consumerPub, Now: clk,
	})
	require.NoError(t, err)

	type res struct {
		tun *Tunnel
		err error
	}
	ch := make(chan res, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		tun, err := ln.Accept(ctx)
		ch <- res{tun, err}
	}()

	dialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	consumer, err = Dial(dialCtx, ln.LocalAddr(), Config{
		EphemeralPriv: consumerPriv, PeerPin: seederPub, Now: clk,
	})
	require.NoError(t, err)

	r := <-ch
	require.NoError(t, r.err)
	seeder = r.tun

	return consumer, seeder, func() {
		_ = consumer.Close()
		_ = seeder.Close()
		_ = ln.Close()
	}
}

func TestTunnel_RequestResponse_OK(t *testing.T) {
	consumer, seeder, cleanup := dialAcceptPair(t)
	defer cleanup()

	go func() {
		body, err := seeder.ReadRequest()
		require.NoError(t, err)
		assert.Equal(t, []byte(`{"model":"opus"}`), body)
		require.NoError(t, seeder.SendOK())
		_, err = seeder.ResponseWriter().Write([]byte("event: message_start\n"))
		require.NoError(t, err)
		_, err = seeder.ResponseWriter().Write([]byte("data: {}\n\n"))
		require.NoError(t, err)
		require.NoError(t, seeder.CloseWrite())
	}()

	require.NoError(t, consumer.Send([]byte(`{"model":"opus"}`)))
	st, r, err := consumer.Receive(context.Background())
	require.NoError(t, err)
	assert.Equal(t, statusOK, st)

	got, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, []byte("event: message_start\ndata: {}\n\n"), got)
}

func TestTunnel_RequestResponse_PeerError(t *testing.T) {
	consumer, seeder, cleanup := dialAcceptPair(t)
	defer cleanup()

	go func() {
		_, err := seeder.ReadRequest()
		require.NoError(t, err)
		require.NoError(t, seeder.SendError("rate-limited"))
		require.NoError(t, seeder.CloseWrite())
	}()

	require.NoError(t, consumer.Send([]byte(`{}`)))
	st, r, err := consumer.Receive(context.Background())
	require.NoError(t, err)
	assert.Equal(t, statusError, st)
	body, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, []byte("rate-limited"), body)
}

func TestTunnel_RequestTooLarge(t *testing.T) {
	consumer, seeder, cleanup := dialAcceptPair(t)
	defer cleanup()
	seeder.cfg.MaxRequestBytes = 16
	consumer.cfg.MaxRequestBytes = 16

	go func() {
		_, err := seeder.ReadRequest()
		// We expect ErrRequestTooLarge.
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrRequestTooLarge), "got %v", err)
	}()

	body := bytes.Repeat([]byte("x"), 17)
	require.NoError(t, consumer.Send(body))
	time.Sleep(100 * time.Millisecond) // let the seeder goroutine record err
}

func TestTunnel_Closed_Send(t *testing.T) {
	consumer, seeder, cleanup := dialAcceptPair(t)
	defer cleanup()
	require.NoError(t, consumer.Close())
	err := consumer.Send([]byte("x"))
	assert.True(t, errors.Is(err, ErrTunnelClosed))
	_ = seeder
}

func TestTunnel_Closed_Receive(t *testing.T) {
	consumer, seeder, cleanup := dialAcceptPair(t)
	defer cleanup()
	require.NoError(t, consumer.Close())
	_, _, err := consumer.Receive(context.Background())
	assert.True(t, errors.Is(err, ErrTunnelClosed))
	_ = seeder
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin
go test ./internal/tunnel/... -run "TestTunnel_" -v
```

Expected: FAIL — `undefined: ReadRequest` / `SendOK` / `SendError` / `ResponseWriter` / `CloseWrite`.

- [ ] **Step 3: Extend `tunnel.go` with the seeder-side helpers**

Append to `tunnel.go`:

```go
// ReadRequest reads the consumer's request body, bounded by
// cfg.MaxRequestBytes. Seeder-side helper.
func (t *Tunnel) ReadRequest() ([]byte, error) {
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return nil, ErrTunnelClosed
	}
	return readRequest(t.stream, t.cfg.MaxRequestBytes)
}

// SendOK writes the success status byte. Subsequent ResponseWriter()
// writes form the SSE body.
func (t *Tunnel) SendOK() error {
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return ErrTunnelClosed
	}
	return writeResponseStatus(t.stream, statusOK)
}

// SendError writes the error status byte and msg. Caller should
// CloseWrite afterwards.
func (t *Tunnel) SendError(msg string) error {
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return ErrTunnelClosed
	}
	return writeResponseError(t.stream, msg)
}

// ResponseWriter exposes the underlying stream for verbatim SSE relay.
// Only valid after SendOK / SendError.
func (t *Tunnel) ResponseWriter() io.Writer { return t.stream }

// CloseWrite signals end-of-response. The peer's Receive reader will
// observe io.EOF on the next Read.
func (t *Tunnel) CloseWrite() error {
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return ErrTunnelClosed
	}
	if t.stream == nil {
		return nil
	}
	return t.stream.Close() // quic-go: closes the send direction
}
```

- [ ] **Step 4: Run tests, expect green**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin
go test -race -cover ./internal/tunnel/... -v
```

Expected: PASS; tunnel.go ≥ 90% coverage.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay
gofumpt -l -w plugin/internal/tunnel/tunnel.go plugin/internal/tunnel/tunnel_test.go
git add plugin/internal/tunnel/tunnel.go plugin/internal/tunnel/tunnel_test.go
git commit -m "$(cat <<'EOF'
feat(plugin/tunnel): seeder-side ReadRequest / SendOK / SendError / ResponseWriter / CloseWrite

Symmetric counterparts to consumer-side Send / Receive. Half-duplex
contract: seeder reads request fully, then writes status byte +
content, then CloseWrite. ResponseWriter exposes the underlying QUIC
stream for verbatim SSE relay (no per-chunk framing inside the OK
half).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 10: End-to-end integration test (loopback round-trip)

Validate the full Dial → request → OK → SSE chunks → close → Receive path on a single goroutine pair, plus the error path. This task adds an additional `e2e_test.go` that documents the expected usage pattern and exercises it as one black-box test.

**Files:**
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin/internal/tunnel/e2e_test.go`

- [ ] **Step 1: Write the failing test (`e2e_test.go`)**

```go
package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestE2E_ConsumerSeederRoundTrip runs a full request/response cycle
// over a real loopback QUIC connection and asserts byte-for-byte
// integrity of both the request and the streamed response.
func TestE2E_ConsumerSeederRoundTrip(t *testing.T) {
	clk := func() time.Time { return time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC) }

	seederPub, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerPub, consumerPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	ln, err := Listen(netip.MustParseAddrPort("127.0.0.1:0"), Config{
		EphemeralPriv: seederPriv, PeerPin: consumerPub, Now: clk,
	})
	require.NoError(t, err)
	defer ln.Close()

	addr := ln.LocalAddr()

	chunks := []string{
		"event: message_start\ndata: {\"type\":\"message_start\"}\n\n",
		"event: content_block_delta\ndata: {\"delta\":{\"text\":\"hello \"}}\n\n",
		"event: content_block_delta\ndata: {\"delta\":{\"text\":\"world\"}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
	requestBody := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`)

	var seederErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		seederTun, err := ln.Accept(ctx)
		if err != nil {
			seederErr = err
			return
		}
		defer seederTun.Close()

		body, err := seederTun.ReadRequest()
		if err != nil {
			seederErr = err
			return
		}
		assert.Equal(t, requestBody, body)

		if err := seederTun.SendOK(); err != nil {
			seederErr = err
			return
		}
		w := seederTun.ResponseWriter()
		for _, c := range chunks {
			if _, err := w.Write([]byte(c)); err != nil {
				seederErr = err
				return
			}
		}
		seederErr = seederTun.CloseWrite()
	}()

	dialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	consumerTun, err := Dial(dialCtx, addr, Config{
		EphemeralPriv: consumerPriv, PeerPin: seederPub, Now: clk,
	})
	require.NoError(t, err)
	defer consumerTun.Close()

	require.NoError(t, consumerTun.Send(requestBody))
	st, r, err := consumerTun.Receive(context.Background())
	require.NoError(t, err)
	assert.Equal(t, statusOK, st)

	got, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, strings.Join(chunks, ""), string(got))

	wg.Wait()
	require.NoError(t, seederErr)
}
```

- [ ] **Step 2: Run test, expect green**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin
go test -race -cover ./internal/tunnel/... -run TestE2E_ -v
```

Expected: PASS — chunks arrive byte-for-byte.

- [ ] **Step 3: Run full test suite**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin
go test -race -cover ./internal/tunnel/... -v
```

Expected: all PASS; total coverage ≥ 90%.

- [ ] **Step 4: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay
gofumpt -l -w plugin/internal/tunnel/e2e_test.go
git add plugin/internal/tunnel/e2e_test.go
git commit -m "$(cat <<'EOF'
test(plugin/tunnel): e2e loopback round-trip

Real QUIC handshake + bidi stream + 4 SSE-style chunks streamed back
verbatim. Asserts byte-for-byte integrity of the consumer→seeder
request body and the seeder→consumer response body. Functions as the
documented usage example for downstream callers (sidecar's
NetworkRouter integration plan).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 11: Negative-path tests (ALPN mismatch, idle timeout, half-open)

This task catches three edge cases not covered by Tasks 7–10.

**Files:**
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin/internal/tunnel/dial_test.go`
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin/internal/tunnel/tunnel_test.go`

- [ ] **Step 1: Add the failing tests (append to `dial_test.go`)**

```go
func TestDial_ALPNMismatch(t *testing.T) {
	seederPub, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerPub, consumerPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	// Seeder side: bare quic-go listener with a different ALPN.
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	require.NoError(t, err)
	t.Cleanup(func() { _ = udp.Close() })

	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	tlsCfg, err := newTLSConfig(seederPriv, consumerPub, now, true)
	require.NoError(t, err)
	tlsCfg.NextProtos = []string{"some-other-proto"}

	tr := &quic.Transport{Conn: udp}
	t.Cleanup(func() { _ = tr.Close() })
	ln, err := tr.Listen(tlsCfg, &quic.Config{HandshakeIdleTimeout: 2 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = ln.Accept(ctx)
	}()

	addr := udp.LocalAddr().(*net.UDPAddr).AddrPort()
	cfg := Config{
		EphemeralPriv: consumerPriv,
		PeerPin:       seederPub,
		Now:           func() time.Time { return now },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err = Dial(ctx, addr, cfg)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrALPNMismatch) || errors.Is(err, ErrHandshakeFailed),
		"got %v", err)
}
```

(Don't forget to add `"github.com/quic-go/quic-go"` to the imports if it's not already there in `dial_test.go`. The earlier `loopbackListener` helper already uses it, so the import exists.)

- [ ] **Step 2: Add half-open Receive test (append to `tunnel_test.go`)**

```go
func TestTunnel_HalfOpenReadEOF(t *testing.T) {
	consumer, seeder, cleanup := dialAcceptPair(t)
	defer cleanup()

	go func() {
		// Seeder reads but never writes a status byte; it just closes
		// its write side immediately.
		_, _ = seeder.ReadRequest()
		_ = seeder.CloseWrite()
	}()

	require.NoError(t, consumer.Send([]byte(`{}`)))
	_, _, err := consumer.Receive(context.Background())
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrFramingViolation), "got %v", err)
}
```

- [ ] **Step 3: Run tests, expect green**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin
go test -race -cover ./internal/tunnel/... -v
```

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay
gofumpt -l -w plugin/internal/tunnel/dial_test.go plugin/internal/tunnel/tunnel_test.go
git add plugin/internal/tunnel/dial_test.go plugin/internal/tunnel/tunnel_test.go
git commit -m "$(cat <<'EOF'
test(plugin/tunnel): ALPN mismatch + half-open Receive

ALPN test: seeder advertises "some-other-proto"; Dial surfaces
ErrALPNMismatch (or ErrHandshakeFailed depending on which TLS layer
detects first).

Half-open test: seeder closes write before sending the status byte;
consumer Receive returns ErrFramingViolation (empty response stream).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 12: Update `plugin/CLAUDE.md` and final lint pass

**Files:**
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin/CLAUDE.md`

- [ ] **Step 1: Add a tunnel entry to the plugin CLAUDE.md "Things that look surprising" section**

Open `plugin/CLAUDE.md`. Under the existing "## Things that look surprising and aren't bugs" section, add:

```markdown
- `internal/tunnel` is a *transport* package, not an orchestrator. It speaks QUIC + identity-pinned mutual TLS over a single bidirectional stream and exposes Dial/Listen+Accept. STUN-client wiring, TURN relay, hole-punch retry, and integration with `internal/ccproxy.NetworkRouter` are deliberately separate plans (see `docs/superpowers/plans/2026-05-03-plugin-tunnel.md` §"Out of scope").
```

- [ ] **Step 2: Run the full test + lint pipeline**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin/internal/tunnel/plugin
go test -race -cover ./internal/tunnel/... -v
golangci-lint run ./internal/tunnel/...
```

Expected: all PASS; lint clean.

- [ ] **Step 3: Run the workspace `make check` to ensure nothing else broke**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay
make check
```

Expected: all tests + lint pass across the workspace.

- [ ] **Step 4: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay
git add plugin/CLAUDE.md
git commit -m "$(cat <<'EOF'
docs(plugin): note internal/tunnel scope in plugin CLAUDE.md

Records that the tunnel package is the transport layer only — STUN,
TURN, hole-punch retry, and ccproxy integration are separate plans.
Helps future contributors not accidentally cross those boundaries.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Acceptance criteria

- All 12 tasks committed in order on the `plugin/internal/tunnel` branch.
- `go test -race -cover ./internal/tunnel/...` passes from `plugin/`.
- Per-file coverage ≥ 90% across `errors.go`, `tlspinning.go`, `frame.go`, `config.go`, `tunnel.go`, `dial.go`, `listen.go`.
- `golangci-lint run ./...` clean against `.golangci.yml`.
- `make check` passes from the repo root.
- Public API surface: `Dial`, `Listen`, `Listener.{Accept, LocalAddr, Close}`, `Tunnel.{Send, Receive, ReadRequest, SendOK, SendError, ResponseWriter, CloseWrite, Close}`, `Config`, the eight sentinel errors. Every exported symbol carries godoc.
- The end-to-end test (`TestE2E_ConsumerSeederRoundTrip`) round-trips a 70-byte request and a 4-chunk SSE response over a real loopback QUIC connection.
- No imports of `crypto/rand` in production code (callers inject randomness via cert generation only — `crypto/rand` is used inside `selfSignedCert` for the X.509 serial because the X.509 stdlib needs an `io.Reader` and tunneling that through Config doesn't earn its keep; this is the documented exception).
- No imports from `plugin/internal/trackerclient`, `plugin/internal/ccproxy`, or `plugin/internal/sidecar` — `tunnel` is a leaf of the plugin-internal dependency graph.

## Self-review checklist

- [x] **Spec coverage:** §7.1 (Setup) → Task 7+8 (Dial/Listen with pinned mutual TLS over UDP). §7.2 (Wire protocol: request body one-shot, response chunks streamed) → Tasks 5 + 9 (frame.go + Tunnel helpers). §7.3 (Failure semantics: hole-punch fail → retry once with TURN) → explicitly out of scope; deferred to data-path orchestration plan. Architecture §10.3 ("TLS 1.3 with consumer+seeder ephemeral keys authenticated against the identities from the tracker offer") → Task 4 (mutual auth + pinning).
- [x] **Placeholder scan:** searched for "TBD", "TODO", "implement later", "fill in details", "Add appropriate error handling", "Similar to Task N" — none present.
- [x] **Type consistency:** `Config` fields named identically across all tasks; `status` enum values (`statusOK`, `statusError`) used consistently in frame.go and tunnel.go; `Tunnel`'s field names (`conn`, `stream`, `cfg`, `closed`, `mu`) match across `tunnel.go` and `dial.go`/`listen.go`.
- [x] **Sentinel-error coverage:** all eight sentinels are tested either directly (`errors_test.go`) or via the integration tests that surface them (`Dial_PinMismatch`, `Dial_ALPNMismatch`, `Tunnel_RequestTooLarge`, `Tunnel_Closed_Send`, `Tunnel_Closed_Receive`, `TestTunnel_HalfOpenReadEOF` exercising `ErrFramingViolation`).

## Execution handoff

Plan complete and saved to `docs/superpowers/plans/2026-05-03-plugin-tunnel.md`. Two execution options:

**1. Subagent-Driven (recommended)** — Dispatch a fresh subagent per task, review between tasks, fast iteration.

**2. Inline Execution** — Execute tasks in this session using `superpowers:executing-plans`, batch execution with checkpoints.

Which approach?
