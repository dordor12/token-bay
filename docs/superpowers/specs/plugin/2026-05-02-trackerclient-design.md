# Tracker Client (`plugin/internal/trackerclient`) — Subsystem Design Spec

| Field | Value |
|---|---|
| Parent | [Plugin design](./2026-04-22-plugin-design.md), [Tracker design](../tracker/2026-04-22-tracker-design.md), [Wire-format v1](../shared/2026-04-24-wire-format-v1-design.md) |
| Status | Design draft — 2026-05-02 |
| Date | 2026-05-02 |
| Scope | Plugin-side long-lived client to a regional tracker. Defines the connection lifecycle, transport abstraction, mTLS identity binding, RPC framing, the nine unary RPCs and three server-push streams, the balance-snapshot cache, error taxonomy, and the test pyramid. Cross-cutting: adds `shared/proto/rpc.proto` and adopts `golang.org/x/sync/singleflight`, `github.com/quic-go/quic-go`, and `github.com/google/uuid` as plugin-side runtime dependencies. |

## 1. Purpose

Provide the single Go-facing entry point through which every other plugin module talks to a tracker:

- `ccproxy-network` calls `BrokerRequest` to broker a redirected `/v1/messages` turn.
- `envelopebuilder` consumes a fresh `*SignedBalanceSnapshot` per request, served from this package's TTL cache.
- The seeder offer loop receives `Offer` server-pushes and replies via this package.
- The consumer settlement loop receives `SettlementPush` and counter-signs via `Settle`.
- Slash commands (`/token-bay status`, `/token-bay balance`) read connection state and balance from this package.
- The sidecar's lifecycle owns one `*Client` for the process; reconnect is handled internally and never bubbles up as a caller concern.

## 2. Non-goals (explicit)

- Tracker server-side code. Listener, session, and broker live in `tracker/internal/*`.
- Bootstrap-list parsing or peer-list refresh. This package consumes a `[]TrackerEndpoint` from `Config`. Discovery is `internal/bootstrap`'s problem.
- Anthropic API key handling. The plugin never holds one; this package never sees one.
- 0-RTT, connection migration, or multipath QUIC. v1 disables 0-RTT; the rest are future work.
- Federation RPCs (tracker↔tracker). Not exposed on this surface.
- Caller retry policy. Failed RPCs return typed errors; the caller (sidecar / `ccproxy-network`) decides whether to rebuild and retry.

## 3. Architecture

### 3.1 Layering

```
                            ┌──────────────────────────────────────┐
                            │   sidecar / ccproxy-network          │
                            │   /token-bay status, /balance, etc.  │
                            └──────────────┬───────────────────────┘
                                           │ blocking unary RPCs
                                           │ + handler interfaces
                                           ▼
                          ┌────────────────────────────────────────┐
                          │  trackerclient.Client (public)         │
                          │   • Status / WaitConnected             │
                          │   • Enroll / BrokerRequest / Balance / │
                          │     Settle / UsageReport / Advertise / │
                          │     TransferRequest / StunAllocate /   │
                          │     TurnRelayOpen                      │
                          │   • OfferHandler / SettlementHandler   │
                          │   • BalanceCached (TTL singleflight)   │
                          └──────────────┬─────────────────────────┘
                                         │ Connection.Get(ctx)
                                         ▼
                          ┌────────────────────────────────────────┐
                          │  Connection / Supervisor (unexported)  │
                          │   • dial → handshake → run             │
                          │   • heartbeat / push-stream acceptor   │
                          │   • reconnect with exp-backoff         │
                          └──────────────┬─────────────────────────┘
                                         │
                          ┌──────────────┴─────────────────────────┐
                          ▼                                        ▼
            ┌───────────────────────────┐         ┌─────────────────────────────┐
            │ internal/wire             │         │ internal/idtls              │
            │  • 4-byte LE-prefix codec │         │  • Self-signed Ed25519 cert │
            │  • RpcRequest dispatch    │         │  • SPKI-pinned VerifyPeer   │
            └────────────┬──────────────┘         └─────────────┬───────────────┘
                         │                                      │
                         ▼                                      ▼
            ┌───────────────────────────────────────────────────────────────┐
            │ internal/transport                                            │
            │  • Transport / Stream / Conn interfaces                       │
            │  • quic/   — production driver (quic-go)                      │
            │  • loopback/ — in-memory driver for unit + component tests    │
            └───────────────────────────────────────────────────────────────┘
```

### 3.2 Concurrency model

| Goroutine | Lifetime | Purpose |
|---|---|---|
| Supervisor | Client lifetime | Dial / reconnect / state machine. Owns the current `*Connection`. |
| Heartbeat sender | Per connected session | 15s `HeartbeatPing` cadence over the heartbeat stream. |
| Heartbeat reader | Per connected session | Reads `HeartbeatPong` + opportunistic registry pushes. |
| Offers acceptor | Per connected session, seeder role only | Accepts server-initiated offer streams; dispatches to `OfferHandler`. |
| Settlements acceptor | Per connected session, consumer role only | Accepts server-initiated settlement streams; dispatches to `SettlementHandler`. |
| Background balance refresher | Client lifetime | Proactively refreshes when remaining lifetime ≤ `BalanceRefreshHeadroom`. |
| Per-RPC caller | Per call | Caller's own goroutine; the unary RPC method blocks here on stream I/O. |

State that is shared across goroutines is guarded by a `sync.RWMutex` on the `Client` struct (status, current connection handle, balance cache). Per-connection state lives on `*Connection` and is mutated only by the supervisor.

### 3.3 Transport abstraction

```go
type Transport interface {
    // Dial blocks until either a connected Conn is returned or ctx is done.
    Dial(ctx context.Context, ep TrackerEndpoint, tlsCfg *tls.Config) (Conn, error)
}

type Conn interface {
    // OpenStreamSync opens a fresh client-initiated bidirectional stream.
    OpenStreamSync(ctx context.Context) (Stream, error)
    // AcceptStream blocks until the server opens a bidirectional stream toward us.
    AcceptStream(ctx context.Context) (Stream, error)
    // PeerIdentityID is the SHA-256 of the server's Ed25519 SubjectPublicKeyInfo.
    PeerIdentityID() ids.IdentityID
    // Close terminates the connection and all streams.
    Close() error
    // Done is closed when the connection is no longer usable.
    Done() <-chan struct{}
}

type Stream interface {
    io.ReadWriteCloser
    // CloseWrite signals end-of-write to the peer (idempotent, half-close).
    CloseWrite() error
}
```

Two implementations:

- **`internal/transport/quic`** — production. Wraps `quic-go`. ALPN `"tokenbay/1"`, TLS 1.3, mutual auth, 0-RTT off.
- **`internal/transport/loopback`** — in-memory bidirectional transport for unit/component tests. `Dial` returns a `Conn` whose streams are `net.Pipe`-style paired buffers; identity is asserted via a side-channel for verification tests.

The supervisor and RPC paths only ever see the interface. All wire/protocol behaviour is testable without UDP.

## 4. Public surface

### 4.1 Configuration

```go
type Config struct {
    // Endpoints lists candidate trackers, ordered lowest-RTT first. Required, len ≥ 1.
    Endpoints []TrackerEndpoint

    // Identity provides the consumer's Ed25519 keypair. Required.
    Identity Signer

    // Transport overrides the default QUIC transport. Optional; defaults to the QUIC driver.
    Transport Transport

    // OfferHandler is invoked for each server-pushed Offer. Required iff this client
    // advertises seeder availability.
    OfferHandler OfferHandler

    // SettlementHandler is invoked for each server-pushed SettlementRequest. Required
    // for consumer-role clients (i.e., any client that calls BrokerRequest).
    SettlementHandler SettlementHandler

    Logger zerolog.Logger
    Clock  func() time.Time     // defaults to time.Now
    Rand   io.Reader            // defaults to crypto/rand.Reader

    DialTimeout            time.Duration   // default 5s
    HeartbeatPeriod        time.Duration   // default 15s
    HeartbeatMisses        int             // default 3
    BalanceTTL             time.Duration   // default 10m (matches ledger spec §3.4)
    BalanceRefreshHeadroom time.Duration   // default 2m

    BackoffBase time.Duration              // default 200ms
    BackoffMax  time.Duration              // default 30s
    MaxFrameSize int                       // default 1 << 20  (1 MiB)
}

type TrackerEndpoint struct {
    Addr         string             // host:port (UDP for QUIC)
    IdentityHash [32]byte           // SHA-256 of the tracker's Ed25519 SPKI
    Region       string             // free-form, used in logs only
}

type Signer interface {
    Sign(msg []byte) ([]byte, error)
    PrivateKey() ed25519.PrivateKey   // used to derive the mTLS cert
    IdentityID() ids.IdentityID
}
```

`Signer` is the same interface `envelopebuilder` already defines; this spec re-declares it under `trackerclient` only if `envelopebuilder` won't import `trackerclient`. The implementation will use a single canonical `Signer` location agreed during plan execution (most likely `internal/identity`).

`Validate()` enforces required-field and range invariants and is called by `New`.

### 4.2 Client

```go
type Client struct{ /* unexported */ }

func New(cfg Config) (*Client, error)

// Start launches the supervisor goroutine and returns immediately. The supervisor
// dials the first endpoint, runs the state machine, and reconnects forever until
// Close is called or ctx is cancelled.
func (c *Client) Start(ctx context.Context) error

// Close terminates the connection, drains in-flight RPCs (returning ErrClosed),
// stops the supervisor, and is idempotent.
func (c *Client) Close() error

// Status snapshots the current connection state.
type ConnectionState struct {
    Phase        ConnectionPhase   // Disconnected | Connecting | Connected | Closing | Closed
    Endpoint     TrackerEndpoint
    PeerID       ids.IdentityID
    ConnectedAt  time.Time
    LastError    error
    NextAttemptAt time.Time
}
func (c *Client) Status() ConnectionState

// WaitConnected blocks until the supervisor reports Connected, or ctx is done.
func (c *Client) WaitConnected(ctx context.Context) error
```

### 4.3 Unary RPCs

```go
func (c *Client) Enroll(ctx context.Context, r *EnrollRequest) (*EnrollResponse, error)
func (c *Client) BrokerRequest(ctx context.Context, env *proto.EnvelopeSigned) (*BrokerResponse, error)
func (c *Client) Balance(ctx context.Context, id ids.IdentityID) (*proto.SignedBalanceSnapshot, error)
func (c *Client) BalanceCached(ctx context.Context, id ids.IdentityID) (*proto.SignedBalanceSnapshot, error)
func (c *Client) Settle(ctx context.Context, preimageHash []byte, sig []byte) error
func (c *Client) UsageReport(ctx context.Context, ur *UsageReport) error
func (c *Client) Advertise(ctx context.Context, ad *Advertisement) error
func (c *Client) TransferRequest(ctx context.Context, tr *TransferRequest) (*TransferProof, error)
func (c *Client) StunAllocate(ctx context.Context) (netip.AddrPort, error)
func (c *Client) TurnRelayOpen(ctx context.Context, sessionID uuid.UUID) (*RelayHandle, error)
```

Contract for every unary method:

1. The call blocks until the response is read, ctx is done, or the connection drops.
2. The call is concurrency-safe; concurrent calls open distinct QUIC streams and do not contend on a mutex.
3. On `ctx.Done()`, the call closes its stream and returns `ctx.Err()`.
4. On connection drop mid-RPC, the call returns `ErrConnectionLost`.
5. On a tracker error, the call returns a `*RpcError` (typed status code + message) wrapped with one of the typed sentinel errors below where applicable (e.g. `ErrNoCapacity`, `ErrFrozen`).

### 4.4 Push handlers

```go
type OfferDecision int
const (
    OfferAccept OfferDecision = iota
    OfferReject
)

type OfferHandler interface {
    HandleOffer(ctx context.Context, o *Offer) (OfferDecision, *AcceptDetails, error)
}

type SettlementHandler interface {
    HandleSettlement(ctx context.Context, r *SettlementRequest) (sig []byte, err error)
}
```

The push acceptor goroutine invokes the handler synchronously per push; if the handler returns an error, the supervisor logs it and writes a structured rejection back to the server. Handlers are expected to be cheap (offload to caller-owned worker pools if needed).

### 4.5 Errors

```go
var (
    ErrNotStarted        = errors.New("trackerclient: Start not called")
    ErrAlreadyStarted    = errors.New("trackerclient: already started")
    ErrClosed            = errors.New("trackerclient: client closed")
    ErrConnectionLost    = errors.New("trackerclient: connection lost mid-RPC")
    ErrIdentityMismatch  = errors.New("trackerclient: tracker identity does not match pin")
    ErrInvalidEndpoint   = errors.New("trackerclient: invalid endpoint")
    ErrNoCapacity        = errors.New("trackerclient: tracker reports no capacity")
    ErrFrozen            = errors.New("trackerclient: identity frozen by reputation")
    ErrUnauthenticated   = errors.New("trackerclient: tracker rejected identity")
    ErrFrameTooLarge     = errors.New("trackerclient: framed message exceeds MaxFrameSize")
    ErrInvalidResponse   = errors.New("trackerclient: malformed RpcResponse")
    ErrAlpnMismatch      = errors.New("trackerclient: ALPN negotiation failed")
    ErrNoHandler         = errors.New("trackerclient: server pushed a stream but no handler is registered")
)

type RpcError struct {
    Code    string
    Message string
}

func (e *RpcError) Error() string
```

`errors.Is` / `errors.As` work across the boundary. Tracker-side `RPC_STATUS_*` codes map to the matching sentinel + `*RpcError` wrap; unknown codes map to `ErrInvalidResponse`.

## 5. Wire protocol

### 5.1 Framing

Each frame on a stream:

```
+---------+---------------------------+
| len:u32 | proto bytes (0..MaxFrame) |
+---------+---------------------------+
```

- `len` is big-endian.
- Frames larger than `MaxFrameSize` (default 1 MiB) are rejected with `ErrFrameTooLarge`. This bounds memory per stream and is the same cap the tracker enforces server-side.
- All proto bytes are produced via `signing.DeterministicMarshal`, ensuring byte-identical serialization across implementations and language versions.

### 5.2 Stream classes

| Class | Initiator | Purpose | Lifetime |
|---|---|---|---|
| Heartbeat | Client | Heartbeat ping/pong + opportunistic registry pushes | Connected session |
| Unary RPC | Client | One `RpcRequest` write, one `RpcResponse` read | Per-call |
| Offers push | Server | Tracker pushes `OfferPush`; client writes `OfferDecision` | Per-offer |
| Settlements push | Server | Tracker pushes `SettlementPush`; client writes ack `SettleAck` | Per-settlement |

The heartbeat stream is the **first** client-initiated bidirectional stream after the handshake. The server distinguishes it by its position (first stream) and by the first frame being `RpcMethod = RPC_METHOD_UNSPECIFIED` with a `HeartbeatPing` payload. (Method-zero is reserved for the heartbeat channel; this is documented in `rpc.proto` as "method zero is the heartbeat-channel marker, not an unset value.")

The acceptor for server-initiated streams reads the first frame, dispatches by message type, and either runs the offers protocol or the settlements protocol. Unknown message types are rejected with `ErrNoHandler`.

### 5.3 RPC schema (`shared/proto/rpc.proto`)

Lands in `shared/`, cross-cutting commit per repo rule #4. Messages:

- `RpcRequest{method: RpcMethod, payload: bytes}`
- `RpcResponse{status: RpcStatus, payload: bytes, error: RpcError}`
- `RpcError{code: string, message: string}`
- `EnrollRequest`, `EnrollResponse`
- `BrokerResponse{seeder_addr, seeder_pubkey, reservation_token}`
- `NoCapacity{reason}`
- `UsageReport{request_id, input_tokens, output_tokens, model, seeder_sig}`
- `Advertisement{capabilities, available, headroom}`
- `TransferRequest{identity_id, amount, dest_region, nonce}`
- `TransferProof{...}`
- `StunAllocateRequest`, `StunAllocateResponse{external_addr}`
- `TurnRelayOpenRequest{session_id}`, `TurnRelayOpenResponse{relay_endpoint, token}`
- `HeartbeatPing{seq, t}`, `HeartbeatPong{seq}`
- `OfferPush{consumer_id, envelope_hash, terms}`
- `OfferDecision{accept, ephemeral_pubkey?, reject_reason?}`
- `SettlementPush{entry_preimage}`
- `SettleAck{}`

Validation helpers (`ValidateRpcRequest`, `ValidateRpcResponse`, `ValidateOfferPush`, `ValidateSettlementPush`, etc.) live alongside the generated code in `shared/proto/`. Both client and server call them before signing / after parsing.

### 5.4 Identity binding (mTLS)

Per [AU1] — restated for spec self-containment:

- The plugin's Ed25519 keypair is wrapped in a self-signed X.509 certificate at startup. The cert's only meaningful field is its `SubjectPublicKeyInfo`; serial, subject, and validity are placeholders.
- TLS 1.3 with mutual authentication. ALPN `"tokenbay/1"`. 0-RTT disabled (`SessionTicketsDisabled = true`).
- Both ends override `VerifyPeerCertificate`:
  - Client pins the tracker's SPKI hash against the configured `TrackerEndpoint.IdentityHash`.
  - Server pins by extracting the client's SPKI hash → that *is* `IdentityID`. No allowlist; identity is intrinsic.
- `tls.Config.InsecureSkipVerify = true` — required because the certs are self-signed; the custom `VerifyPeerCertificate` does strictly stronger pinning than the Web-PKI path it disables.
- Post-handshake, the connection's peer identity is captured once into `*Connection`; RPC handlers do not re-authenticate.

The mTLS code is small and lives entirely in `internal/idtls/`, outside the application code path.

## 6. Reconnect, heartbeat, and durability

### 6.1 Reconnect

`Client.Start(ctx)` launches one supervisor goroutine. Loop:

1. Pick the next endpoint from `Config.Endpoints` (round-robin on consecutive failures).
2. `Transport.Dial(ctx)`. On error: increment backoff, wait, retry.
3. On success: open the heartbeat stream, start the offers/settlements acceptors (if handlers are registered), transition `Status.Phase → Connected`, reset backoff.
4. Run until either (a) the connection's `Done` channel closes, (b) heartbeat misses exceed `HeartbeatMisses`, or (c) `ctx` is cancelled.
5. Tear down: close all streams (in-flight RPCs return `ErrConnectionLost`), close the connection, transition `Status.Phase → Connecting`, loop.

**Backoff math:** `delay = min(BackoffMax, BackoffBase << n) * (0.5 + rand·0.5)`. `n` resets to zero after `30s` of continuous Connected uptime.

There is no max-attempts ceiling. The supervisor reconnects indefinitely until `Close` or `ctx` cancellation. Failure is surfaced via `Status()` and structured logs, not by terminating.

### 6.2 Heartbeat

A dedicated bidirectional stream. The client sends `HeartbeatPing{seq, t}` every `HeartbeatPeriod` (15s). The server echoes `HeartbeatPong{seq}`. Three consecutive missed pongs (45s with defaults) → tear down the connection and reconnect.

The server may also push `RegistryUpdate` messages on this stream (out of v1 scope semantics; the client reads-and-logs to keep the stream drained).

### 6.3 Durability across reconnect (R3a — fail-fast)

In-flight RPCs do not survive reconnect. When the connection drops, all blocking RPC callers see their stream error and return `ErrConnectionLost`. Callers decide whether to rebuild and retry.

This is the simplest correct behaviour. Three reasons:

- **Replay protection.** The envelope's `nonce` + `captured_at` (and the balance snapshot's `expires_at`) bind the call to a specific moment. A naïve retry of the *same* envelope will be rejected by the tracker as a duplicate. Callers must rebuild — this is their concern, not ours.
- **No callback/queue invariants to maintain.** Buffer-and-replay (R3c) is a documented anti-pattern: it hides connection failures from callers and breaks freshness invariants the proof layer relies on.
- **Composes with `ctx`.** The caller's context already encodes their patience and retry policy; we don't second-guess it.

## 7. Balance cache

The `BalanceCached(ctx, id)` method is the canonical entry point for `envelopebuilder` and the `/token-bay balance` slash command.

Behaviour:

1. **Hit.** If the cached snapshot exists and `Now < snap.body.expires_at - BalanceRefreshHeadroom`, return it immediately. No stream opened.
2. **Miss / stale.** Call `Balance(ctx, id)` directly, store the result, return it.
3. **Singleflight.** Concurrent stale reads coalesce on `id` via `golang.org/x/sync/singleflight` — only one tracker round-trip outstanding per identity at a time.
4. **Background refresh.** A supervisor goroutine wakes every `BalanceRefreshHeadroom / 2` (≈ 1m default) and refreshes any cached snapshot whose remaining lifetime ≤ `BalanceRefreshHeadroom`. This keeps callers off the hot path during normal operation.
5. **Invalidation on disconnect.** A snapshot is bound to the specific tracker that signed it. On connection drop the cache is cleared so we don't serve a snapshot signed by a tracker we no longer trust transitively.

`Balance(ctx, id)` (uncached) is exposed for callers who explicitly want a fresh snapshot (e.g., the `transfer_request` flow).

## 8. Test pyramid

Discipline matches `shared/`: every helper has a test, every exported method has a test, every error path is asserted.

### 8.1 Unit (the biggest tier)

- `internal/wire/frame_test.go` — round-trip framing; `MaxFrameSize` rejection; truncated reads; partial writes; concurrent stream reuse.
- `internal/wire/codec_test.go` — `RpcMethod` dispatch; `RpcStatus → error` mapping table; nil-safety on every helper.
- `internal/idtls/cert_test.go` — generated cert is parseable, has the expected SPKI, accepts an Ed25519 priv key.
- `internal/idtls/verify_test.go` — `VerifyPeerCertificate` accepts matching SPKI, rejects mismatched SPKI, rejects non-Ed25519 certs, rejects empty cert chains.
- `reconnect_test.go` — backoff math (deterministic with injected RNG); reset-after-uptime threshold; jitter envelope.
- `balance_cache_test.go` — TTL hits/misses; singleflight coalescing; expiry computation; invalidation on disconnect.
- `errors_test.go` — `errors.Is/As` work for every sentinel; `RpcError.Error()` formatting; status-code mapping table.

### 8.2 Component (loopback transport + fakeserver)

`test/fakeserver/` implements the wire protocol against `internal/transport/loopback`. Tests in `*_test.go`:

- All 9 unary RPCs round-trip end-to-end with golden-byte equality on the `RpcRequest`/`RpcResponse` for one known fixture call per RPC.
- Push streams: server initiates an `OfferPush`, client's `OfferHandler` sees it, client writes back an `OfferDecision`; server initiates a `SettlementPush`, client's `SettlementHandler` is invoked and writes a `SettleAck`.
- Heartbeat: client sends three pings, server echoes three pongs, fourth ping is dropped → client tears down within 3× period.
- Reconnect: kill the loopback connection mid-RPC; in-flight RPC returns `ErrConnectionLost`; supervisor reconnects within (backoff + ε); subsequent RPC succeeds.
- Concurrency: 100 parallel `BrokerRequest` calls all complete; each opens a distinct stream; assertions on stream-id uniqueness.

### 8.3 Integration (real QUIC)

`test/integration_test.go` wires `internal/transport/quic` against a `quic-go` server using fixture identity keypairs:

- TLS handshake succeeds with matching SPKI pin.
- TLS handshake fails with `ErrIdentityMismatch` when the pin is wrong (server cert has a different pubkey).
- ALPN mismatch fails with `ErrAlpnMismatch`.
- One full RPC + one push stream round-trip under real UDP / TLS 1.3.
- Mid-RPC server kill produces `ErrConnectionLost` on the in-flight call within 1× heartbeat period; supervisor reconnects when the server returns.

### 8.4 Race + golden + lint

- `go test -race ./plugin/internal/trackerclient/...` is part of `make -C plugin test`.
- Golden fixtures for every new proto message live in `shared/proto/testdata/` per `shared/` discipline.
- `golangci-lint run` clean.

### 8.5 Coverage target

≥ 90% line coverage across the package, aggregated. Connection and supervisor goroutines hit via the loopback tier; transport drivers hit via the integration tier.

## 9. Dependencies introduced

- `github.com/quic-go/quic-go` — transport. Pinned to v2 (latest at design time). Used only inside `internal/transport/quic/`.
- `golang.org/x/sync/singleflight` — balance-cache coalescing. Pure library, no transitive crypto.
- `github.com/google/uuid` — `request_id`, `session_id`. Pure stdlib-only library.

All three are widely used and maintained. None introduces third-party crypto (Ed25519 stays stdlib; `quic-go` uses `crypto/tls`, which uses stdlib crypto).

`shared/`'s leaf-module discipline is preserved: this package adds proto messages but no new runtime imports to `shared/`.

## 10. Open questions

- **Heartbeat-stream identification.** v1 uses "first client-initiated bidi stream after handshake = heartbeat" + a method-zero marker on the first frame. If a future tracker release mandates an explicit ALPN-side stream-id convention, we adopt it without breaking the wire. The marker approach is unambiguous against valid `RpcMethod` values, which start at 1.
- **`Signer` location.** The `Signer` interface is currently re-declared in three packages (`envelopebuilder`, here, eventually `internal/identity`). When `internal/identity` lands, all three converge on its declaration; this spec doesn't force the move because identity is a sibling plan.
- **`netip.AddrPort` vs string for STUN result.** Spec uses `netip.AddrPort` for type safety. The wire format carries it as `string` (host:port) for forward compatibility with TURN-style URIs in v2.
- **0-RTT enablement.** Disabled in v1. Would shave ~1× RTT off reconnect-after-NAT-rebind. Re-evaluate if reconnect latency proves to be a hot path.
- **Connection migration.** quic-go supports it; we don't surface it. The first reconnect after a NAT rebind is observably ≤ 1× heartbeat period, which is fine for v1.

## 11. Failure modes (package-specific)

| Failure | Behavior |
|---|---|
| Endpoint list empty | `New` returns `ErrInvalidEndpoint`. |
| Endpoint cert SPKI mismatch | TLS handshake aborts with `ErrIdentityMismatch`; supervisor backs off and tries the next endpoint. |
| ALPN mismatch | TLS aborts with `ErrAlpnMismatch`; supervisor logs and backs off. Implies plugin/tracker version skew; surfacing the diagnostic is key. |
| Heartbeat stalls | After `HeartbeatMisses` consecutive missed pongs, supervisor tears down and reconnects. In-flight RPCs see `ErrConnectionLost`. |
| Connection drops mid-RPC | All in-flight unary RPCs return `ErrConnectionLost`; push acceptors restart on reconnect. |
| `MaxFrameSize` exceeded | Stream closed with `ErrFrameTooLarge`; the per-RPC error is surfaced to the caller. Connection itself stays up unless this fires repeatedly. |
| Balance snapshot expired before envelopebuilder uses it | Cache returns stale; caller re-fetches via `BalanceCached` which transparently refreshes. The `expires_at - RefreshHeadroom` window prevents this in practice. |
| `OfferHandler` panics | Recovered by the offer-stream goroutine; logged; offer is rejected to the tracker; connection stays up. |
| `Close` called before `Start` | Returns nil. |
| `Start` called twice | Returns `ErrAlreadyStarted`. |
| Tracker reports `RPC_STATUS_FROZEN` | Returns `ErrFrozen`. Caller surfaces to the user; reputation subsystem owns recovery. |

## 12. Acceptance criteria

- `New(cfg)` validates `Config` and rejects malformed endpoints, missing `Identity`, missing handlers when role demands them.
- `Start(ctx)` returns immediately and the supervisor connects to the first reachable endpoint, transitioning `Status` from `Disconnected → Connecting → Connected` within `DialTimeout` against the loopback fakeserver.
- All 9 unary RPCs round-trip against the fakeserver. `RpcRequest` and `RpcResponse` golden bytes match committed fixtures for one canonical call per RPC.
- `BalanceCached` returns without opening a stream on cache hit; concurrent stale reads coalesce to one tracker round-trip; background refresher keeps cached snapshots within `BalanceRefreshHeadroom` of expiry.
- Mid-RPC connection drop produces `ErrConnectionLost` on the in-flight call within 1× `HeartbeatPeriod`.
- Reconnect after a server-side restart resumes service in `≤ BackoffMax + HeartbeatPeriod` wall-clock under `make -C plugin test`.
- mTLS handshake with matching SPKI succeeds; mismatched SPKI fails with `ErrIdentityMismatch`; no Web-PKI roots are consulted.
- ALPN mismatch fails with `ErrAlpnMismatch`.
- `OfferHandler` and `SettlementHandler` round-trips work; absent handlers cause the acceptor to log and reject the push with a structured error.
- `go test -race ./plugin/internal/trackerclient/...` passes.
- `golangci-lint run ./plugin/internal/trackerclient/...` clean.
- Line coverage ≥ 90% aggregated across the package.
- Cross-cutting commit lands `shared/proto/rpc.proto` + generated `.pb.go` + `Validate*` helpers + golden fixtures alongside the plugin code.
- No new runtime dependencies in `shared/go.mod`.
