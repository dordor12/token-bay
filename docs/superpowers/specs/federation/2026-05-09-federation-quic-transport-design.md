# Federation QUIC Peer Transport — Subsystem Design Spec

| Field | Value |
|---|---|
| Parent | [Federation Protocol](2026-04-22-federation-design.md), [Federation Core + Root-Attestation](2026-05-09-tracker-internal-federation-core-design.md) |
| Status | Design draft |
| Date | 2026-05-09 |
| Scope | Real-network production transport for the federation peering plane. Implements `tracker/internal/federation.Transport` over QUIC + mTLS with operator-pinned SPKI hashes. Adds per-peer redial-with-backoff, distinct ALPN, and a separate listener port. The in-process `InprocTransport` remains for unit tests. |

## 1. Purpose

The federation core slice ships an in-process `Transport` so the protocol can be tested end-to-end without a network. This subsystem replaces it for production: two trackers in different processes, on different hosts, can peer over QUIC + mTLS using the operator-pre-shared `FederationConfig.Peers` allowlist for SPKI pinning.

In one sentence: after this slice, two operator-allowlisted trackers running real binaries can peer over the network, exchange `ROOT_ATTESTATION`s, and recover from transient connection drops.

## 2. Non-goals

- Sub-second peer-death detection. We rely on QUIC's `MaxIdleTimeout` to close stale connections; the redial loop reconnects.
- TLS cert chain rotation. The Ed25519 keypair binds a self-signed cert generated per-process; key rotation = restart.
- Auto-discovery of non-allowlisted peers. Federation peers stay operator-managed via config (umbrella spec §10 open question).
- Removing the in-process transport. It is the test default and keeps subsystem-level tests fast and deterministic.
- Cross-region transfer / revocation gossip / peer-exchange / health scoring — those are separate slices.
- Extracting cert helpers to a shared package. The plugin and tracker server each carry near-identical helpers today (`plugin/internal/trackerclient/internal/idtls/`, `tracker/internal/server/tls.go`); duplicating in federation is YAGNI-friendly. Extraction can wait until a fourth caller appears.

## 3. Position in the system

This subsystem is a sibling of the federation core. It depends on:

- `tracker/internal/federation` — the existing `Transport` / `PeerConn` interfaces, `Config`, `AllowlistedPeer`, and the subsystem `Open`/`Close` lifecycle.
- `github.com/quic-go/quic-go` (already a dependency of `tracker/internal/server`).
- `crypto/tls`, `crypto/x509`, `crypto/ed25519`, `crypto/sha256` — stdlib only.
- `tracker/internal/config.FederationConfig` — extended with the new fields below.
- `tracker/internal/federation.Metrics` — new error labels for transport-specific failures.

It is consumed by:

- `cmd/token-bay-tracker/run_cmd.go` — selects `NewQUICTransport(cfg)` over `NewInprocTransport(...)` when `cfg.Federation.ListenAddr` is non-empty. The wiring otherwise stays the same.

## 4. Architecture

### 4.1 File layout

```
tracker/internal/federation/
  tls.go / tls_test.go            ← NEW: cert + TLS config + verifier helpers
  transport_quic.go               ← NEW: QUICTransport + qConn implementing Transport / PeerConn
  transport_quic_test.go          ← NEW: real QUIC over 127.0.0.1:0
  dialer.go / dialer_test.go      ← NEW: per-peer redial with exponential backoff
  config.go                       ← MODIFY: add ListenAddr, IdleTimeoutS, RedialBaseS, RedialMaxS
  subsystem.go                    ← MODIFY: replace one-shot dialOutbound with dialer.Run
                                            (Transport remains pluggable)

tracker/internal/config/
  config.go / apply_defaults.go / validate.go  ← MODIFY: extend FederationConfig + tests

tracker/cmd/token-bay-tracker/
  run_cmd.go                      ← MODIFY: choose QUICTransport vs InprocTransport by config
```

### 4.2 New public types

```go
// QUICTransport implements Transport via quic-go + mTLS, using the SPKI
// hash pin pattern shared with tracker/internal/server and plugin/internal/
// trackerclient/internal/idtls.
type QUICTransport struct { /* … */ }

func NewQUICTransport(cfg QUICConfig) (*QUICTransport, error)

type QUICConfig struct {
    ListenAddr   string             // ":7443"; empty disables Listen
    IdleTimeout  time.Duration      // default 60s
    Cert         tls.Certificate    // built via CertFromIdentity(myPriv)
    HandshakeTO  time.Duration      // dial-side handshake timeout
}
```

`QUICTransport` satisfies `federation.Transport`. Each `Dial` brings up a new QUIC connection, performs cert-pinning verification, opens one bidirectional stream, and returns a `*qConn`. `Listen` accepts QUIC connections, captures the peer SPKI, accepts the first stream, and hands the resulting `*qConn` to the accept callback.

### 4.3 Cert + TLS helpers (in `tls.go`)

Mirrors `tracker/internal/server/tls.go` and `plugin/.../idtls/`:

- `CertFromIdentity(priv ed25519.PrivateKey) (tls.Certificate, error)` — self-signed Ed25519 cert.
- `MakeServerTLSConfig(ourCert, captureClientHash func([32]byte)) *tls.Config` — `RequireAnyClientCert`, ALPN `"tokenbay-fed/1"`, TLS 1.3 only, 0-RTT off, session tickets off.
- `MakeClientTLSConfig(ourCert tls.Certificate, expectedServerSPKI [32]byte) *tls.Config` — pin verifier rejects mismatched SPKI / multi-cert / non-Ed25519.
- `SPKIHashOfCert(cert *x509.Certificate) ([32]byte, error)` — sha256 of `RawSubjectPublicKeyInfo`.
- Sentinel errors: `ErrNoPeerCert`, `ErrTooManyCerts`, `ErrNotEd25519`, `ErrSPKIMismatch`, `ErrEmptySPKI`.

### 4.4 ALPN

`const ALPN = "tokenbay-fed/1"` — distinct from the plugin-facing `"tokenbay/1"`. Two listeners on the same host cannot accidentally cross-serve, even if an operator misconfigures and points federation at the plugin port.

### 4.5 Wire / framing on a QUIC stream

One persistent bidirectional QUIC stream per peer connection. Frames are 4-byte big-endian length prefix + payload, hard-capped at `MaxFrameBytes = 1 MiB` on both sides. `qConn.Send` writes one frame; `qConn.Recv` reads exactly one frame.

This matches `transport_inproc`'s ordering semantics so the federation core code (peer recvLoop, gossip Forward) needs zero changes.

### 4.6 Dialer (`dialer.go`)

```go
type Dialer struct {
    Transport     Transport
    Peer          AllowlistedPeer
    MyTrackerID   ids.TrackerID
    MyPriv        ed25519.PrivateKey
    HandshakeTO   time.Duration
    BackoffBase   time.Duration  // 1s
    BackoffMax    time.Duration  // 30s
    Now           func() time.Time
    OnConnected   func(HandshakeResult, PeerConn)
    OnFailure     func(reason string, err error)
}

// Run is the per-peer goroutine entry point. Runs until ctx is canceled.
// On every iteration: Dial → handshake → if both succeed, hand off via
// OnConnected; OnConnected is expected to block until the connection
// drops, then return so the dialer can reconnect.
func (d *Dialer) Run(ctx context.Context)
```

- Initial wait = `BackoffBase` (1s); doubles on each failure to `BackoffMax` (30s); full-jitter (`wait * rand[0,1)`) applied on every sleep.
- Successful handshake resets the wait to `BackoffBase` for the next cycle.
- `ctx.Done()` exits cleanly at any point in the loop, including mid-sleep.

### 4.7 Subsystem changes (`subsystem.go`)

Replace today's:
```go
for _, p := range cfg.Peers {
    go f.dialOutbound(ctx, p)
}
```

with:
```go
for _, p := range cfg.Peers {
    d := &Dialer{
        Transport:   dep.Transport,
        Peer:        p,
        MyTrackerID: cfg.MyTrackerID,
        MyPriv:      cfg.MyPriv,
        HandshakeTO: cfg.HandshakeTimeout,
        BackoffBase: cfg.RedialBase,
        BackoffMax:  cfg.RedialMax,
        Now:         dep.Now,
        OnConnected: f.attachAndWait,   // replaces attachPeer
        OnFailure:   f.recordDialFailure,
    }
    go d.Run(ctx)
}
```

`attachAndWait` blocks until the conn drops (signaled via `qConn.Done()` or recvLoop exit), then returns so the dialer redials.

### 4.8 Config additions

```go
type FederationConfig struct {
    // existing fields…
    ListenAddr   string `yaml:"listen_addr"`     // e.g. ":7443"; empty = federation network-disabled
    IdleTimeoutS int    `yaml:"idle_timeout_s"`  // default 60
    RedialBaseS  int    `yaml:"redial_base_s"`   // default 1
    RedialMaxS   int    `yaml:"redial_max_s"`    // default 30
}
```

`apply_defaults.go` sets the three int defaults. `validate.go` enforces:
- `IdleTimeoutS` ∈ [1, 600]
- `RedialBaseS` ∈ [1, 60]
- `RedialMaxS` ∈ [`RedialBaseS`, 600]
- `ListenAddr` either empty or a parsable `host:port`.

`run_cmd.go`:
```go
var transport federation.Transport
if cfg.Federation.ListenAddr != "" {
    transport, err = federation.NewQUICTransport(federation.QUICConfig{
        ListenAddr:  cfg.Federation.ListenAddr,
        IdleTimeout: time.Duration(cfg.Federation.IdleTimeoutS) * time.Second,
        Cert:        cert,
        HandshakeTO: time.Duration(cfg.Federation.HandshakeTimeoutS) * time.Second,
    })
    if err != nil {
        return fmt.Errorf("federation transport: %w", err)
    }
} else {
    transport = federation.NewInprocTransport(federation.NewInprocHub(), "self", trackerPub, trackerKey)
}
```

`cert` is built once at startup via `federation.CertFromIdentity(trackerKey)`.

## 5. Connection lifecycle

### 5.1 Dial (active side)

1. `quicgo.DialAddr(ctx, peer.Addr, clientTLS, qcfg)`. `clientTLS` pins `peer.PubKey`'s SHA-256 via `MakeClientTLSConfig`.
2. After handshake, read `connectionState.PeerCertificates[0]`, recompute SPKI hash, compare against expected. On mismatch (defense in depth — pin verifier should already have rejected), close with `ErrSPKIMismatch`.
3. `OpenStreamSync(ctx)` to establish the persistent bidi stream.
4. Return `*qConn` wrapping `conn` + `stream` + `peerSPKI` + a `done` channel that closes when the QUIC conn does.

### 5.2 Accept (passive side)

1. QUIC listener is bound at `Open` time. A goroutine loops on `listener.Accept(ctx)`.
2. Per accepted connection: `AcceptStream(ctx)` for the persistent stream. Capture peer SPKI from cert chain.
3. Build `*qConn`. Pass to the `accept func(PeerConn)` callback supplied by federation `Listen`.
4. The federation core's `acceptInbound` runs the federation handshake (HELLO/PEER_AUTH/PEERING_ACCEPT) on top.

### 5.3 Close

- `qConn.Close()`: `stream.Close()` (half-close write), then `conn.CloseWithError(0, "...")`.
- Idempotent via `sync.Once`.
- `done` channel signals via the wrapped connection's context.

### 5.4 Drop detection + reconnect

- `qConn.Recv` / `qConn.Send` returns an error when the QUIC connection or stream is gone.
- The federation core's recvLoop exits, signaling the Peer is dead.
- The dialer's `OnConnected` callback (`attachAndWait`) blocks until that signal, then returns.
- The dialer loop falls through to backoff + redial.

## 6. Failure handling

| Failure | Behavior |
|---|---|
| Listener bind fails at `Open` | Return error, `Open` fails. |
| Initial `quicgo.DialAddr` fails | Backoff + retry. Metric: `invalid_frames{reason="dial"}`. |
| TLS handshake fails (incl. SPKI mismatch) | Close, backoff + retry. Metric: `invalid_frames{reason="tls_handshake"}` or `"spki_mismatch"`. |
| `OpenStreamSync` fails post-handshake | Close conn, backoff + retry. Metric: `invalid_frames{reason="stream_open"}`. |
| Stream Send/Recv errors mid-flow | Conn closed by core, redial loop reconnects. No special metric (counted as ordinary peer disconnect). |
| Cert verifier sees multi-cert / non-Ed25519 / wrong SPKI | Reject inside `VerifyPeerCertificate`; QUIC handshake fails with that error; same path as TLS handshake fail. |
| Subsystem `Close` while dial mid-flight | `listenCtx` cancellation propagates; dialer exits cleanly without redial. |

## 7. Metrics

New label values for the existing `tokenbay_federation_invalid_frames_total{reason}` counter:
- `dial` — `quicgo.DialAddr` returned error.
- `tls_handshake` — TLS error during dial or accept.
- `spki_mismatch` — peer cert SPKI did not match the operator's allowlisted pin.
- `stream_open` — `OpenStreamSync`/`AcceptStream` returned error.

New gauges (optional, deferred unless needed for ops):
- `tokenbay_federation_redial_backoff_seconds{tracker_id}` — current wait, useful when a peer is flapping.

## 8. Concurrency model

- One Listener goroutine accepting QUIC conns (lives for `listenCtx`'s lifetime).
- One acceptor goroutine per accepted conn, running the federation handshake on top of the freshly-accepted `qConn`.
- One Dialer goroutine per allowlisted peer (`len(cfg.Peers)`).
- Each connected peer continues to own one recvLoop goroutine and one bounded send queue (existing federation core).

`go test -race` requirement (always-`-race` package per umbrella tracker spec §6) applies to every new file. The dialer's `Run` loop, `qConn.Send`/`Recv`, and `Close` paths all need to pass `-race`.

## 9. Testing

### 9.1 Unit

- `tls_test.go`: cert round-trip, SPKI hash determinism, verifier rejects no-cert / two-cert / non-Ed25519 / wrong SPKI / empty SPKI.
- `transport_quic_test.go`: two `QUICTransport` instances on `127.0.0.1:0` (dynamic port), dial+accept round-trip, Send/Recv frame, frame-too-large rejection, peer-cert-pin mismatch rejection.
- `dialer_test.go`: fakeTransport simulates first dial fail then success; assert the dialer waited approximately `BackoffBase` (give or take jitter) before retry; assert reset on success; assert ctx-cancel exits cleanly mid-sleep.

### 9.2 Integration

- New scenario added to `integration_test.go`: two `Federation` instances, both with `cfg.Federation.ListenAddr = ":0"` and a `QUICTransport`. A publishes hour=N; B archives within 5 s. Mirrors the existing in-process scenario.
- Drop-and-recover: bring up A+B, sever B's QUIC conn (close from outside), assert the dialer redials within `BackoffBase + jitter`, assert subsequent publish reaches B.

### 9.3 Acceptance gate

Both unit and integration tests pass `go test -race ./tracker/internal/federation/...`. `make check` is green.

## 10. Open questions

- **TURN-style fallback.** Operators behind hostile NATs can't accept QUIC. v1 assumes federation runs on hosts with public IPs (matches umbrella spec §3 "Long-lived QUIC … connection"). Out of scope.
- **Backoff jitter formula.** Full-jitter (uniform `[0, wait)`) is the default; could revisit to decorrelated jitter if peers are observed to synchronize. Defer to data.
- **Multiple peers behind one operator.** A single operator could allowlist many tracker IDs; today each gets its own Dialer goroutine. Fine for ≤10² peers, revisit if a region scales further.
- **Listener-side backoff.** If `quicgo.Listen` returns an Accept error mid-flight, do we restart the listener or fail open? v1: log + continue (consistent with `tracker/internal/server`'s behavior).

## 11. Acceptance criteria

- Two `Federation` instances can peer over `QUICTransport` on `127.0.0.1:0` and exchange `ROOT_ATTESTATION` end-to-end (new integration test).
- Cert SPKI mismatch is detected by the TLS verifier; the QUIC handshake fails; the dialer reconnects on the next loop.
- A killed connection triggers redial within `RedialBaseS` + jitter; subsequent gossip reaches the peer.
- Subsystem `Close` cancels in-flight dials and accept loops cleanly; no goroutine leaks under `-race`.
- `make check` green.
- The federation subsystem is wired into `cmd/token-bay-tracker/run_cmd.go` so a non-empty `cfg.Federation.ListenAddr` activates QUIC; an empty value falls back to `InprocTransport` (no behavior change for existing tests).

## 12. Subsystem implementation index

- **`tracker/internal/federation` (transport_quic + tls + dialer)** — plan: `docs/superpowers/plans/2026-05-09-federation-quic-transport.md` (to be written via the writing-plans skill).
