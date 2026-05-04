# `tracker/internal/api` + `tracker/internal/server` — Subsystem Design Spec

| Field | Value |
|---|---|
| Parent | [Regional Tracker](./2026-04-22-tracker-design.md) |
| Peer spec | [Plugin Tracker Client](../plugin/2026-05-02-trackerclient-design.md) |
| Depends on | [Config](./2026-04-25-tracker-internal-config-design.md), [Ledger](../ledger/2026-04-22-ledger-design.md), [STUN/TURN](./2026-05-02-tracker-stunturn-design.md), `tracker/internal/registry` (implemented), `shared/proto/rpc.proto` (lands with the trackerclient PR) |
| Status | Design draft |
| Date | 2026-05-03 |
| Scope | Implements the server-side of the tracker's plugin-facing wire protocol. Two new packages: `internal/server` (QUIC listener, mTLS termination, per-connection supervisor, push-stream initiators, lifecycle) and `internal/api` (`RpcMethod` router + per-RPC handlers + push-stream encoders). Adds four fields to `ServerConfig`. Adds a `token-bay-tracker run` cobra subcommand. Scope-2: every RPC compiles and is unit-testable now; handlers whose subsystems are not yet built (`broker`, `admission`, `federation`, `reputation`) are auto-stubbed to return `RPC_STATUS_INTERNAL "NOT_IMPLEMENTED"`. The admin HTTP API (spec §8) is out of scope for this design. |

## 1. Purpose

Let a real plugin dial the tracker over QUIC, complete an mTLS handshake whose cert pin equals the peer's `IdentityID`, send a framed `RpcRequest`, get a framed `RpcResponse`, and accept server-initiated push streams for offers and settlements — all without any subsystem (broker, admission, federation, reputation) blocking the wire-up.

`internal/server` owns the transport, the per-connection state machine (the spec §3 `listener/` + `session/` combined), and the stream lifecycle for both client- and server-initiated streams.

`internal/api` owns the dispatch surface: a `Router` mapping `RpcMethod → handler`, one handler per RPC, narrow per-handler interfaces declaring the minimum each handler needs from a subsystem, plus encoders/decoders for the two push-stream message pairs.

## 2. Non-goals (explicit)

- **Admin HTTP API** (`/health`, `/stats`, `/peers`, `/identity/<id>`, `/maintenance` — spec §8). Belongs in `internal/admin`, separate plan.
- **Federation peer protocol** (tracker↔tracker). Belongs in `internal/federation`, separate plan; the `transfer_request` handler is stubbed here.
- **Broker selection algorithm**. The `broker_request` handler is stubbed here; broker logic belongs in `internal/broker`, separate plan.
- **Admission gate**. Stubbed in this spec — `enroll` runs the ledger path only.
- **Reputation freeze enforcement**. The handler shape allows it; live enforcement lands when `internal/reputation` does.
- **Metrics collection.** `internal/metrics` is empty; we expose `Server.PeerCount()` and emit structured zerolog events, but install no Prometheus collectors here.
- **In-flight broker request state.** `usage_report` and `settle` are stubbed because they need broker-owned `InflightRequest` (spec §4.2).
- **Connection migration / 0-RTT.** Disabled to match `trackerclient` §5.4.

## 3. Architecture

### 3.1 Layering

```
                          ┌──────────────────────────────────────────┐
                          │  cmd/token-bay-tracker run               │
                          │  (loads *config.Config, builds subsys,   │
                          │   constructs server.Deps, calls Run)     │
                          └────────────────┬─────────────────────────┘
                                           │
                                           ▼
                        ┌──────────────────────────────────────────────┐
                        │  internal/server                             │
                        │   • Server struct + Run(ctx)/Shutdown        │
                        │   • QUIC listener (ALPN "tokenbay/1")        │
                        │   • mTLS termination + SPKI→IdentityID       │
                        │   • Accept loop → spawn *Connection per peer │
                        │   • *Connection: heartbeat reader, push-     │
                        │     stream INITIATORS (Offer / Settlement),  │
                        │     RPC stream acceptor, lifecycle           │
                        │   • Calls api.Dispatcher for each unary RPC  │
                        └────┬─────────────────────────────┬───────────┘
                             │ Router.Dispatch              │ pushAPI.Encode/Decode
                             ▼                              ▲
        ┌──────────────────────────────────────────────────────────────┐
        │  internal/api                                                 │
        │   • Router (RpcMethod → Handler) + Dispatch                   │
        │   • One file per RPC: enroll.go, broker_request.go, …         │
        │   • Each handler declares a NARROW interface for what         │
        │     subsystem(s) it needs (Deps struct on Router)             │
        │   • PushAPI: encoders for OfferPush/SettlementPush, decoders  │
        │     for OfferDecision/SettleAck (no transport ownership)      │
        │   • Stub handlers when matching Deps.X is nil:                │
        │     return RPC_STATUS_INTERNAL "NOT_IMPLEMENTED"              │
        └──────────────────────────────────────────────────────────────┘
                             │
                             ▼ (one narrow interface per handler)
       ┌───────────┬──────────┬───────────┬────────────┬─────────────┐
       │ registry  │ ledger   │ stunturn  │ broker(❌)  │ admission(❌)│
       │ ✅ ready  │ ✅ ready │ ✅ ready  │ stub fake  │ stub gate   │
       └───────────┴──────────┴───────────┴────────────┴─────────────┘
```

### 3.2 Invariants

- `internal/api` imports `shared/proto`, `shared/signing`, `shared/ids`, and only the narrow interfaces it declares itself. **It NEVER imports `internal/server` or any transport package.**
- `internal/server` imports `internal/api` (constructs `api.Router`), `quic-go`, stdlib `crypto/tls` + `crypto/x509` + `crypto/ed25519`, and every concrete subsystem (registry, ledger, stunturn). Server-side mTLS helpers live in `internal/server/tls.go` rather than a shared `idtls/` package; see §10 open questions.
- A `*Connection` exposes its push streams *only* via `Server.PushOfferTo` / `Server.PushSettlementTo`. The connection's owner goroutine is the sole writer to each stream. Outside callers receive a `<-chan *proto.OfferDecision` (or `*SettleAck`) to await the reply.
- Handlers are pure functions of `(ctx, *RequestCtx, request) → (response, error)`. `RequestCtx` carries `PeerIdentityID`, `RemoteAddr`, `Now()` — no `*tls.Conn`, no `quic.Stream`.

### 3.3 Concurrency model

| Goroutine | Lifetime | Purpose |
|---|---|---|
| Accept loop | `Run(ctx)` lifetime | Accepts QUIC connections; spawns one `*Connection` per peer |
| Connection owner | Per peer connection | Owns the `*Connection`; reads `quic.Conn.AcceptStream` to spawn per-RPC reader/dispatcher goroutines; serializes server-initiated push-stream writes |
| RPC reader/dispatcher | Per client-initiated unary stream | Reads one framed `RpcRequest`, calls `Dispatcher.Dispatch`, writes one framed `RpcResponse`, half-closes |
| Heartbeat reader | Per connection | Reads framed `HeartbeatPing`s on the dedicated heartbeat stream; writes `HeartbeatPong`; calls `RegistryService.Heartbeat` |
| Push initiator | Per outstanding offer or settlement | Opens server-initiated stream, writes encoded push, awaits reply, signals caller via channel |

State that crosses goroutines: `Server.connsByPeer` (map guarded by `Server.mu`); `*Connection` fields mutated only by its owner goroutine. `api.Router` is stateless after construction; `Dispatch` is safe under arbitrary concurrency.

## 4. Public Go surface

### 4.1 `internal/server` — public API

```go
package server

// Deps is everything Server needs from the outside world. The cmd binary
// builds this and hands it to New.
type Deps struct {
    Config   *config.Config           // already-validated
    Logger   zerolog.Logger
    Now      func() time.Time         // defaults to time.Now if zero
    Registry *registry.Registry
    Ledger   *ledger.Ledger
    StunTurn *stunturn.Allocator      // exact type follows internal/stunturn
    Reflect  stunturn.Reflector       // for stun_allocate handler

    // API is the dispatch surface. cmd builds it via api.NewRouter(api.Deps{…}).
    // Server holds it as an opaque interface so tests can swap a fake.
    API Dispatcher
}

// Dispatcher is what server needs from api — defined here as the consumer
// (Go "accept interfaces" idiom). api.Router satisfies it.
type Dispatcher interface {
    Dispatch(ctx context.Context, rc *api.RequestCtx, req *proto.RpcRequest) *proto.RpcResponse
    PushAPI() api.PushAPI
}

type Server struct{ /* unexported */ }

func New(d Deps) (*Server, error)

// Run blocks until ctx is cancelled or a fatal listener error occurs.
// It is the long-lived entry point; cmd/run_cmd.go calls it.
func (s *Server) Run(ctx context.Context) error

// Shutdown gracefully drains: stop accepting new connections, wait for in-flight
// RPCs up to ctx.Deadline, then force-close. Idempotent.
func (s *Server) Shutdown(ctx context.Context) error

// PeerCount returns the number of currently-connected peers.
func (s *Server) PeerCount() int

// PushOfferTo enqueues an OfferPush to the given identity. Returns false if
// the peer is not currently connected. The decisionCh receives exactly one
// *proto.OfferDecision then closes (timeout produces OfferDecision{Reject:"timeout"}).
// Used by the (future) broker subsystem.
func (s *Server) PushOfferTo(id ids.IdentityID, push *proto.OfferPush) (decisionCh <-chan *proto.OfferDecision, ok bool)

// PushSettlementTo mirrors PushOfferTo for settlement pushes.
func (s *Server) PushSettlementTo(id ids.IdentityID, push *proto.SettlementPush) (ackCh <-chan *proto.SettleAck, ok bool)
```

`Run` is single-call: a second call returns `ErrAlreadyRunning`. `Shutdown` before `Run` is nil. After `Shutdown`, `PushOfferTo` / `PushSettlementTo` return `(nil, false)`.

### 4.2 `internal/api` — public API

```go
package api

// Deps lists the subsystems an api handler may depend on. Each handler file
// declares a narrow interface for its slice; cmd's hand-wiring at startup
// passes concrete implementations. Nil fields cause the matching RPCs to
// register as ErrNotImplemented stubs.
type Deps struct {
    Logger    zerolog.Logger
    Now       func() time.Time

    Ledger     LedgerService          // declared in balance.go / enroll.go / settle.go
    Registry   RegistryService        // declared in advertise.go
    StunTurn   StunTurnService        // declared in stun_allocate.go / turn_relay_open.go
    Broker     BrokerService          // declared in broker_request.go (Scope-2: nil OK; stub handler)
    Admission  AdmissionService       // declared in enroll.go (Scope-2: nil OK; stub gate-pass)
    Federation FederationService      // declared in transfer_request.go (Scope-2: nil OK)
}

// RequestCtx carries per-call info every handler may need. server.Connection
// constructs one and passes it through Dispatch.
type RequestCtx struct {
    PeerID     ids.IdentityID         // extracted from mTLS SPKI
    RemoteAddr netip.AddrPort         // from QUIC conn
    Now        time.Time              // captured once per call
    Logger     zerolog.Logger         // request-scoped (carries request_id)
}

type Router struct{ /* unexported */ }

func NewRouter(d Deps) (*Router, error)

// Dispatch is the single entry point. It decodes the per-method payload,
// invokes the handler, and serializes the response. It NEVER returns an
// error — protocol errors become *proto.RpcResponse with non-OK status.
// Transport errors are server's concern.
func (r *Router) Dispatch(ctx context.Context, rc *RequestCtx, req *proto.RpcRequest) *proto.RpcResponse

// PushAPI returns helpers for encoding/decoding push-stream messages.
// server uses these — handlers don't.
func (r *Router) PushAPI() PushAPI
```

### 4.3 Push API surface

```go
type PushAPI struct{ /* unexported */ }

// Encoder pairs:
func (p PushAPI) EncodeOfferPush(push *proto.OfferPush) ([]byte, error)
func (p PushAPI) EncodeSettlementPush(push *proto.SettlementPush) ([]byte, error)

// Decoder pairs (call ValidateOfferDecision / ValidateSettleAck internally):
func (p PushAPI) DecodeOfferDecision(b []byte) (*proto.OfferDecision, error)
func (p PushAPI) DecodeSettleAck(b []byte) (*proto.SettleAck, error)
```

### 4.4 Per-handler narrow interfaces (illustrative)

Each handler file declares its own narrow interface naming exactly the methods that handler calls. Where two handlers call the same subsystem, each declares its own interface (Go's structural typing means the same concrete type satisfies both without an adapter). The `Deps` struct holds the union — one field per subsystem, typed as the *handler's own* interface for the canonical handler that uses it; the second handler in `*_test.go` declares its own narrower interface and its real call site uses the same `Deps.X`.

```go
// internal/api/balance.go
type balanceLedger interface {
    SignedBalance(id ids.IdentityID, now time.Time) (*proto.SignedBalanceSnapshot, error)
}

// internal/api/enroll.go
type enrollLedger interface {
    IssueStarterGrant(id ids.IdentityID, now time.Time) (entry []byte, credits uint64, err error)
}
type enrollAdmission interface {
    Admit(ctx context.Context, identity ids.IdentityID, accountFingerprint []byte) error
}

// internal/api/settle.go (stub in Scope-2; interface defined for the future real path)
type settleLedger interface {
    AppendUsage(ctx context.Context, preimage []byte, consumerSig []byte, now time.Time) error
}

// internal/api/advertise.go
type advertiseRegistry interface {
    Advertise(id ids.IdentityID, caps registry.Capabilities, available bool, headroom float64) error
}

// internal/api/stun_allocate.go
type stunService interface {
    ReflectAddr(remote netip.AddrPort) netip.AddrPort
}

// internal/api/turn_relay_open.go
type turnService interface {
    AllocateRelay(sessionID uuid.UUID, peer1, peer2 ids.IdentityID) (relayAddr netip.AddrPort, token []byte, err error)
}

// internal/api/broker_request.go (Scope-2: nil-tolerant)
type brokerService interface {
    Submit(ctx context.Context, env *proto.EnvelopeSigned) (*proto.BrokerResponse, error)
}

// internal/api/transfer_request.go (Scope-2: nil-tolerant)
type federationService interface {
    StartTransfer(ctx context.Context, req *proto.TransferRequest) (*proto.TransferProof, error)
}
```

The `Deps` struct in `router.go` re-declares each interface as exported (`LedgerService` is the union `balanceLedger ∪ enrollLedger ∪ settleLedger`, `RegistryService` is `advertiseRegistry ∪ heartbeatRegistry`, etc.) and `NewRouter` does the type assertions to populate per-handler closures. This keeps `cmd/run_cmd.go` passing one concrete `*ledger.Ledger` rather than separate interface adapters.

Concrete types in `internal/registry`, `internal/ledger`, `internal/stunturn` happen to satisfy these by structural matching. No adapter layer.

### 4.5 Error helpers + status mapping

```go
// Sentinel error constructors used by handlers.
func ErrInvalid(msg string) error
func ErrNoCapacity(msg string) error
func ErrFrozen(msg string) error
func ErrNotFound(msg string) error
func ErrUnauthenticated(msg string) error
func ErrNotImplemented(rpc string) error

// Internal helpers used by Dispatch:
func okResponse(payload []byte) *proto.RpcResponse
func errResponse(status proto.RpcStatus, code, msg string) *proto.RpcResponse
```

Mapping (handler error → `RpcResponse.status` + `RpcError.code`):

| Handler returns | RpcStatus | RpcError.Code |
|---|---|---|
| `nil` (success) | `RPC_STATUS_OK` | — |
| `api.ErrInvalid(msg)` | `RPC_STATUS_INVALID` | `"INVALID"` |
| `api.ErrNoCapacity(msg)` | `RPC_STATUS_NO_CAPACITY` | `"NO_CAPACITY"` |
| `api.ErrFrozen(msg)` | `RPC_STATUS_FROZEN` | `"FROZEN"` |
| `api.ErrNotFound(msg)` | `RPC_STATUS_NOT_FOUND` | `"NOT_FOUND"` |
| `api.ErrUnauthenticated(msg)` | `RPC_STATUS_UNAUTHENTICATED` | `"UNAUTHENTICATED"` |
| `api.ErrNotImplemented(rpc)` (Scope-2 stub) | `RPC_STATUS_INTERNAL` | `"NOT_IMPLEMENTED"` |
| any other `error` | `RPC_STATUS_INTERNAL` | `"INTERNAL"` (message redacted unless LogLevel=debug) |

Maps exactly to the client's sentinel errors so `errors.Is` works across the wire.

### 4.6 Stub-handler contract (Scope-2)

For each RPC whose subsystem isn't built (`broker_request`, `settle`, `usage_report`, `transfer_request`):

- If the corresponding `Deps.X` field is nil at `NewRouter` time, the handler is registered as a **stub** that returns `RPC_STATUS_INTERNAL` with `error.code = "NOT_IMPLEMENTED"` and `error.message = "<rpc> handler awaiting <subsystem> subsystem"`.
- If `Deps.X` is non-nil, the real handler runs.
- A future PR landing `internal/broker` only needs to: build the subsystem and set `Deps.Broker = brokerSvc` in `cmd/run_cmd.go`. Zero changes to `internal/api` source.

## 5. Wire-protocol behavior (server side)

### 5.1 Listener + handshake

- **Transport:** QUIC via `quic-go`, ALPN `"tokenbay/1"`. Locked by trackerclient spec §5.4.
- **TLS:** TLS 1.3, mutual auth. Server presents a self-signed Ed25519 cert built at process start from `config.Server.IdentityKeyPath` + `TLSCertPath`/`TLSKeyPath` (the existing config fields). `tls.Config.ClientAuth = tls.RequireAnyClientCert`; `InsecureSkipVerify = true` because we override `VerifyPeerCertificate` with strictly stronger SPKI pinning.
- **Identity binding:** server-side `VerifyPeerCertificate` extracts the client cert's `SubjectPublicKeyInfo`, SHA-256s it → that 32-byte hash IS the peer's `IdentityID`. No allowlist; identity is intrinsic. `*Connection.peerID` captures it once; handlers see it via `RequestCtx.PeerID`. Mirror of trackerclient §5.4.
- **0-RTT off:** `tls.Config.SessionTicketsDisabled = true`.
- **MaxIncomingStreams:** set from `config.Server.MaxIncomingStreams` (default 1024) — every unary RPC opens its own stream + heartbeat + push streams.

### 5.2 Stream classes (mirror of trackerclient §5.2)

| Class | Initiator | First frame | Per-connection count | Lifetime |
|---|---|---|---|---|
| Heartbeat | Client | `RpcRequest{method=RPC_METHOD_UNSPECIFIED, payload=HeartbeatPing}` | 1 (the first client-initiated bidi stream) | Connected session |
| Unary RPC | Client | `RpcRequest{method=RPC_METHOD_*, payload=<request>}` (method ≥ 1) | many, concurrent | Per call |
| Offers push | Server | `OfferPush` | 1 per outstanding offer | Per offer |
| Settlements push | Server | `SettlementPush` | 1 per outstanding settlement | Per settlement |

**Identification rule (server side):**
- Accept loop reads the first frame off every client-initiated bidi stream.
- If the stream is the **first** the client opened *and* the frame is `RpcRequest{method=0}` → spawn the heartbeat goroutine that owns this stream for the rest of the session.
- Else parse as `RpcRequest`; if `method == 0` on a non-first stream → close with `RPC_STATUS_INVALID` "method 0 reserved for heartbeat channel."
- Else dispatch to `api.Router.Dispatch`, write `RpcResponse`, half-close write side.

### 5.3 Framing

Per-frame layout (verbatim from client spec §5.1):

```
+---------+----------------------------+
| len:u32 | proto bytes (0..MaxFrame)  |   len = big-endian
+---------+----------------------------+
```

- Server enforces `len ≤ config.Server.MaxFrameSize` (default 1 MiB). Oversize frames close the stream with `RPC_STATUS_INVALID` and increment a metric.
- All proto serialization via `shared/signing.DeterministicMarshal` — non-negotiable per `shared/CLAUDE.md` rule #6.
- Each unary RPC: read one framed `RpcRequest`, write one framed `RpcResponse`, server `CloseWrite`s.

### 5.4 Heartbeat

- Server reads `HeartbeatPing{seq, t}`, replies `HeartbeatPong{seq}` immediately.
- Server calls `registry.Heartbeat(peerID, now)` on each ping (this is how the seeder registry stays warm without a separate "I'm alive" RPC).
- Server **does not** drop the connection on missing pings — that's the client's job. The server's only liveness signal is QUIC's idle timeout (`config.Server.IdleTimeoutS`, default 60s).

### 5.5 Push-stream lifecycle (server-initiated)

`Server.PushOfferTo(id, push) → (decisionCh, ok)`:

1. Look up `*Connection` by `peerID == id`. If not connected, return `nil, false`.
2. The connection's owner goroutine (NOT the caller) calls `conn.OpenStreamSync(ctx)` to open a new server-initiated bidi stream.
3. Owner writes the framed `OfferPush` (encoded via `api.PushAPI.EncodeOfferPush`).
4. Owner reads the framed reply, decodes via `api.PushAPI.DecodeOfferDecision`, sends it on `decisionCh`, closes `decisionCh`.
5. Stream closes. If the client never replies before `config.Broker.OfferTimeoutMs`, the owner closes the stream and sends `&proto.OfferDecision{Reject: "timeout"}` on the channel.

Mirror semantics for `PushSettlementTo` with `SettleAck`.

**Why a channel, not a callback:** keeps the owning goroutine the sole writer to the stream, and lets broker wait with `select { case d := <-decisionCh: ... case <-ctx.Done(): ... }`. No mutex on the connection's stream set.

### 5.6 Per-connection goroutines

| Goroutine | Owns | Termination |
|---|---|---|
| **Reader/dispatcher** (per RPC stream) | One client-initiated stream | Returns after writing response or on stream error |
| **Heartbeat** | The heartbeat stream | Returns when stream closes / connection dies |
| **Owner** | The `*Connection` itself + the push-stream queue | Returns when QUIC `Conn.Done()` fires |
| **Per push** | One server-initiated stream (offer or settlement) | Returns when client replies, ctx fires, or push-timeout fires |

Goroutine leak protection: every goroutine's lifetime is bounded by either the connection's `ctx` (derived from `Server.Run`'s ctx) or the per-push `ctx`. `Server.Shutdown` cancels the root ctx → all derived contexts → goroutines exit.

### 5.7 Concurrency-safety contract

- `*Connection` fields mutated only by its owner goroutine. Reads from outside (e.g. `PeerCount()`, `PushOfferTo`) go through `Server.mu`.
- `api.Router` is stateless after construction — `Dispatch` is safe under arbitrary concurrency. Each handler is responsible for its own concurrency contract with its underlying service (e.g. `Ledger.AppendUsage` is already serialized by `Ledger.mu`).

## 6. Source layout

```
tracker/
├── go.mod                                     ← MODIFY (add quic-go, x/sync, google/uuid)
├── go.sum                                     ← regenerated by go mod tidy
│
├── cmd/token-bay-tracker/
│   ├── main.go                                ← MODIFY: register `run` subcommand
│   ├── run_cmd.go                             ← NEW: parses --config, builds subsystems, calls server.Run
│   └── run_cmd_test.go                        ← NEW: cobra wiring test
│
└── internal/
    ├── server/
    │   ├── doc.go                             ← NEW
    │   ├── server.go                          ← NEW: Server, Deps, New, Run, Shutdown
    │   ├── server_test.go
    │   ├── listener.go                        ← NEW: QUIC listener wrap, ALPN, accept loop
    │   ├── listener_test.go
    │   ├── tls.go                             ← NEW: server-side mTLS config + SPKI→IdentityID
    │   ├── tls_test.go
    │   ├── connection.go                      ← NEW: *Connection (per-peer state)
    │   ├── connection_test.go
    │   ├── heartbeat.go                       ← NEW: heartbeat-stream reader
    │   ├── heartbeat_test.go
    │   ├── rpc_stream.go                      ← NEW: client-initiated unary stream acceptor
    │   ├── rpc_stream_test.go
    │   ├── push_offers.go                     ← NEW: server-initiated OfferPush stream lifecycle
    │   ├── push_offers_test.go
    │   ├── push_settlements.go                ← NEW
    │   ├── push_settlements_test.go
    │   ├── shutdown.go                        ← NEW: drain logic
    │   ├── shutdown_test.go
    │   └── testdata/                          ← Ed25519 keypairs for tests
    │
    ├── api/
    │   ├── doc.go                             ← NEW
    │   ├── router.go                          ← NEW: Router, Deps, RequestCtx, Dispatch
    │   ├── router_test.go
    │   ├── errors.go                          ← NEW: sentinel constructors + status mapping
    │   ├── errors_test.go
    │   ├── push_api.go                        ← NEW: PushAPI encoders/decoders
    │   ├── push_api_test.go
    │   ├── enroll.go                          ← NEW: real handler (ledger.IssueStarterGrant; admission optional)
    │   ├── enroll_test.go
    │   ├── broker_request.go                  ← NEW: stub by default
    │   ├── broker_request_test.go
    │   ├── balance.go                         ← NEW: real handler (ledger.SignedBalance)
    │   ├── balance_test.go
    │   ├── settle.go                          ← NEW: stub
    │   ├── settle_test.go
    │   ├── usage_report.go                    ← NEW: stub
    │   ├── usage_report_test.go
    │   ├── advertise.go                       ← NEW: real handler (registry.Advertise)
    │   ├── advertise_test.go
    │   ├── transfer_request.go                ← NEW: stub
    │   ├── transfer_request_test.go
    │   ├── stun_allocate.go                   ← NEW: real handler
    │   ├── stun_allocate_test.go
    │   ├── turn_relay_open.go                 ← NEW: real handler
    │   ├── turn_relay_open_test.go
    │   └── testdata/                          ← golden RpcResponse hex per real handler
    │
    └── (existing) session/                    ← REMOVE: empty dir folded into server/

tracker/test/
├── fakeclient/                                ← NEW: in-process plugin client over loopback
│   └── fakeclient.go
├── component/server_component_test.go         ← NEW: wire round-trip without UDP
└── integration/                               ← NEW: real QUIC + real mTLS  (//go:build integration)
    ├── helpers_test.go                        ← test-server + test-client builders, packet wrappers, goroutine-leak check
    ├── handshake_test.go                      ← TLS / mTLS / ALPN happy + sad paths
    ├── rpc_path_test.go                       ← unary RPC happy + invalid + bad-frame paths
    ├── network_failure_test.go                ← hard kills, idle timeout, packet loss, reorder, address change
    └── push_path_test.go                      ← server-initiated streams: ok, reject, timeout, drain, concurrent
```

## 7. Config additions

Extend `tracker/internal/config.ServerConfig`:

```go
type ServerConfig struct {
    ListenAddr      string `yaml:"listen_addr"`        // REQUIRED (existing)
    IdentityKeyPath string `yaml:"identity_key_path"`  // REQUIRED (existing)
    TLSCertPath     string `yaml:"tls_cert_path"`      // REQUIRED (existing)
    TLSKeyPath      string `yaml:"tls_key_path"`       // REQUIRED (existing)

    // NEW fields:
    MaxFrameSize       int `yaml:"max_frame_size"`         // default 1 << 20  (1 MiB)
    IdleTimeoutS       int `yaml:"idle_timeout_s"`         // default 60       — QUIC idle timeout
    MaxIncomingStreams int `yaml:"max_incoming_streams"`   // default 1024     — per-conn QUIC stream cap
    ShutdownGraceS     int `yaml:"shutdown_grace_s"`       // default 30       — Server.Shutdown drain window
}
```

Validation rules (added to `internal/config/validate.go` §6.2):

- `server.max_frame_size` ≥ 1024
- `server.idle_timeout_s` > 0
- `server.max_incoming_streams` ≥ 16
- `server.shutdown_grace_s` ≥ 0

This is a **cross-cutting change**: the field + default + validation rule + a fixture update in `internal/config/testdata/full.yaml` ship in the same commit as the first server consumer.

## 8. `cmd/token-bay-tracker run` subcommand

```
token-bay-tracker run --config <path>
```

Composition root (`cmd/run_cmd.go`):

```go
func newRunCmd() *cobra.Command {
    var configPath string
    cmd := &cobra.Command{
        Use:   "run",
        Short: "Start the tracker server",
        RunE: func(cmd *cobra.Command, args []string) error {
            cfg, err := config.Load(configPath)
            if err != nil { return err }

            logger := newLogger(cfg.LogLevel)

            store, err := storage.Open(cfg.Ledger.StoragePath)
            if err != nil { return err }
            defer store.Close()

            keyBytes, err := os.ReadFile(cfg.Server.IdentityKeyPath)
            if err != nil { return err }
            trackerKey := ed25519.PrivateKey(keyBytes)

            led, err := ledger.Open(store, trackerKey)
            if err != nil { return err }

            reg, err := registry.New(registry.DefaultShardCount)
            if err != nil { return err }

            allocator := stunturn.NewAllocator(...)
            reflector := stunturn.NewReflector()

            router, err := api.NewRouter(api.Deps{
                Logger:   logger,
                Now:      time.Now,
                Ledger:   led,
                Registry: reg,
                StunTurn: stunturnAdapter{allocator, reflector},
                // Broker / Admission / Federation left nil → ErrNotImplemented stubs
            })
            if err != nil { return err }

            srv, err := server.New(server.Deps{
                Config:   cfg,
                Logger:   logger,
                Now:      time.Now,
                Registry: reg,
                Ledger:   led,
                StunTurn: allocator,
                Reflect:  reflector,
                API:      router,
            })
            if err != nil { return err }

            ctx, stop := signal.NotifyContext(cmd.Context(),
                syscall.SIGINT, syscall.SIGTERM)
            defer stop()

            errCh := make(chan error, 1)
            go func() { errCh <- srv.Run(ctx) }()

            select {
            case err := <-errCh:
                return err
            case <-ctx.Done():
                graceCtx, cancel := context.WithTimeout(context.Background(),
                    time.Duration(cfg.Server.ShutdownGraceS)*time.Second)
                defer cancel()
                return srv.Shutdown(graceCtx)
            }
        },
    }
    cmd.Flags().StringVar(&configPath, "config", "", "Path to tracker.yaml (required)")
    return cmd
}
```

`run_cmd_test.go` covers: the cobra command exists, `--config` is required, invalid config bubbles up via the existing `reportConfigError` path. Real listener startup is tested in `internal/server`, not in `cmd`.

## 9. Graceful shutdown semantics

`Server.Shutdown(ctx)`:

1. **Stop accept loop.** Close the QUIC listener so no new connections arrive. Existing connections continue serving in-flight RPCs.
2. **Cancel root connection ctx.** Each `*Connection` derives its ctx from a `serverCtx` owned by `Server`; cancelling `serverCtx` signals every connection that drain has begun. Heartbeat goroutines exit immediately.
3. **Wait for in-flight RPCs.** A `sync.WaitGroup` counts active RPC dispatch goroutines. Block on `wg.Wait()` *or* `ctx.Done()`, whichever fires first.
4. **Force-close on grace expiry.** When `ctx` fires before `wg.Wait()` returns, call `Conn.Close()` on every connection, log the count of in-flight RPCs cut off, return `ctx.Err()`.
5. **Idempotent.** Second call returns nil immediately.

Push streams get the same treatment: outstanding `decisionCh` / `ackCh` channels receive a "drain" reject (`OfferDecision{Reject: "shutdown"}`) and close, so callers in the (future) broker subsystem unblock.

## 10. Testing

### 10.1 Unit (per-source-file)

`internal/server`:

- `tls_test.go` — server cert builds from Ed25519 priv key; SPKI extraction returns expected `IdentityID`; `VerifyPeerCertificate` accepts well-formed Ed25519 client cert; rejects RSA / empty chain / non-Ed25519.
- `listener_test.go` — listener opens on `:0`, returns its bound port; closes on `Shutdown`; rejects connections after Shutdown.
- `connection_test.go` — `*Connection` constructed with PeerID + RemoteAddr; ctx-cancel cleans up; `mu`-guarded fields race-clean under `-race`.
- `heartbeat_test.go` — read `HeartbeatPing`, write `HeartbeatPong{seq}`, call mock `RegistryService.Heartbeat`; ignores frames with non-zero method on the heartbeat stream; closes when stream ends.
- `rpc_stream_test.go` — read framed `RpcRequest`, call `Dispatcher.Dispatch`, write framed `RpcResponse`, half-close. Truncated read returns stream error. Oversize frame rejected with `RPC_STATUS_INVALID`.
- `push_offers_test.go` — `PushOfferTo` writes encoded `OfferPush`, reads `OfferDecision`, sends on channel, closes channel. Timeout fires `OfferDecision{Reject:"timeout"}`. Connection-not-found returns `(nil, false)`.
- `push_settlements_test.go` — mirror.
- `shutdown_test.go` — second Shutdown is nil; in-flight RPC awaited up to grace; over-grace cuts streams and returns `ctx.Err()`; drain reject delivered to push channels.
- `server_test.go` — `New` rejects nil deps; `Run` requires the listener; `PeerCount` reflects connect/disconnect.

`internal/api`:

- `router_test.go` — `Dispatch` routes by `RpcMethod`; unknown method returns `RPC_STATUS_INVALID "UNKNOWN_METHOD"`; nil `Deps.X` installs `ErrNotImplemented` stubs for the right RPCs (table-driven over the four stubbed RPCs).
- `errors_test.go` — typed-error → status mapping table; `okResponse` payload encodes correctly; `errResponse` redacts message at non-debug log level.
- `push_api_test.go` — encode/decode round-trip for `OfferPush`/`OfferDecision`/`SettlementPush`/`SettleAck`; oversize encode bumps to caller; malformed decode returns `ErrInvalid`.
- One `<rpc>_test.go` per real handler (Scope-2 reals: `enroll`, `balance`, `advertise`, `stun_allocate`, `turn_relay_open`):
  - happy path against a fake service literal
  - service error → matching `RPC_STATUS_*`
  - validation failure on the request payload → `RPC_STATUS_INVALID`
  - golden-bytes assertion on the response payload for one canonical input
- One `<rpc>_test.go` per stub handler (Scope-2 stubs: `broker_request`, `settle`, `usage_report`, `transfer_request`):
  - returns `RPC_STATUS_INTERNAL` with `error.code = "NOT_IMPLEMENTED"`
  - returns the configured "real" handler when the matching `Deps.X` field is non-nil

### 10.2 Component tier — `tracker/test/component/`

Mirror of plugin-side `test/fakeserver/`. A small in-process plugin client that:

- Implements a `Conn` over an in-memory loopback (assumes `shared/transport/loopback/` exists by the time this lands; if it doesn't, copy a ~80-LoC version into `tracker/test/loopback/` with a TODO to dedupe).
- Speaks the wire protocol from §5 verbatim.
- Lets a test write `client.Call(ctx, RPC_METHOD_BALANCE, payload) → response`.
- Exposes `client.OnOfferPush(handler)` and `client.OnSettlementPush(handler)`.

`server_component_test.go` cases:

- All 5 real RPCs round-trip (enroll, balance, advertise, stun_allocate, turn_relay_open).
- All 4 stubbed RPCs return `RPC_STATUS_INTERNAL` "NOT_IMPLEMENTED".
- Heartbeat: fakeclient sends 3 pings, server replies 3 pongs, `registry.Heartbeat` called 3×.
- `PushOfferTo` from a goroutine simulating broker → fakeclient receives `OfferPush`, replies `OfferDecision{Accept}` → `decisionCh` delivers it.
- `PushSettlementTo` → `SettleAck` round-trip.
- 100 parallel `Balance` calls: all complete; one stream per call.
- `Shutdown` with 50 in-flight calls: 49 finish under grace, 1 (sleeping handler injected for the test) is cut and returns `ctx.Err()`.

### 10.3 Integration tier — `tracker/test/integration/`

Real QUIC, real mTLS, real `quic-go`. Build-tagged `//go:build integration`. The test file includes a 100-LoC raw `quic-go` client (importing the real `plugin/internal/trackerclient` is rejected as a too-heavy build dep). Spin up a fresh `Server` on `127.0.0.1:0` per top-level subtest; tear down with `Shutdown(ctx, 5s)`.

Organized as four files for review tractability:

#### 10.3.1 `handshake_test.go` — TLS / mTLS / ALPN happy & sad paths

| Case | Setup | Expectation |
|---|---|---|
| `Handshake_OK_SPKIMatches` | client cert built from key K; endpoint pin = SHA-256(SPKI(K)) | handshake succeeds; `Server.PeerCount() == 1` within 100 ms |
| `Handshake_Fails_SPKIMismatch` | server cert built from key Ks; endpoint pin = SHA-256(SPKI(other)) | handshake error contains "identity"; `PeerCount() == 0`; no `*Connection` created |
| `Handshake_Fails_AlpnMismatch` | client offers ALPN `"wrong/1"` only | handshake error contains "no application protocol"; `PeerCount() == 0` |
| `Handshake_Fails_NonEd25519ClientCert` | client cert is RSA-2048 self-signed | server's `VerifyPeerCertificate` rejects; QUIC handshake fails; logged at info |
| `Handshake_Fails_EmptyChain` | client presents zero certs (`ClientAuth = NoClientCert` requested) | server requires `RequireAnyClientCert`; QUIC handshake fails |
| `Handshake_Fails_TLS12` | client offers `MaxVersion = TLS12` | handshake error: TLS 1.3 required; `PeerCount() == 0` |
| `Handshake_Fails_TruncatedSPKI` | client cert hand-crafted with a 0-byte SubjectPublicKeyInfo (built directly via `x509.Certificate{RawSubjectPublicKeyInfo: nil}` and self-signed) | `VerifyPeerCertificate` returns `"empty SPKI"`; handshake fails |
| `Handshake_Fails_ExpiredCert` | client cert `NotAfter = Now - 1h` | server's verifier ignores Web-PKI validity (we use SPKI-pin) → handshake **succeeds**; assertion documents this intentional behavior |
| `Handshake_Concurrent` | 50 parallel clients with valid distinct keys | all 50 connect within 2 s; `PeerCount() == 50`; race detector clean |

#### 10.3.2 `rpc_path_test.go` — happy & invalid RPC paths over real QUIC

| Case | Expectation |
|---|---|
| `RPC_Balance_OK` | `RPC_METHOD_BALANCE` round-trip; response status `OK`; payload decodes to `SignedBalanceSnapshot` whose tracker_sig verifies |
| `RPC_Advertise_OK` | `Advertise` round-trip; `registry.Get(peerID)` reflects the advertised caps |
| `RPC_StunAllocate_OK` | response payload's `external_addr` equals client's QUIC `RemoteAddr` |
| `RPC_TurnRelayOpen_OK` | response payload contains a parseable `relay_endpoint` and 32-byte token |
| `RPC_Enroll_OK` | starter grant entry verifies under the tracker's public key |
| `RPC_BrokerRequest_NotImplemented` | Scope-2 stub: status `INTERNAL`, code `NOT_IMPLEMENTED` |
| `RPC_Settle_NotImplemented` | same |
| `RPC_UsageReport_NotImplemented` | same |
| `RPC_TransferRequest_NotImplemented` | same |
| `RPC_UnknownMethod` | client sends `RpcMethod=99`; status `INVALID`, code `UNKNOWN_METHOD` |
| `RPC_MalformedPayload` | `Balance` request with garbage proto bytes; status `INVALID`, code `INVALID`; connection stays up (verified by a follow-up successful `Balance` on a fresh stream) |
| `RPC_OversizeFrame` | client writes `len = 2 MiB` (above default `MaxFrameSize`); server closes the stream with `INVALID`/`FRAME_TOO_LARGE`; connection survives; follow-up RPC on a new stream succeeds |
| `RPC_TruncatedFrame` | client writes 4-byte len then closes mid-payload; server closes the stream cleanly; connection survives |
| `RPC_TruncatedHeader` | client writes 2 of 4 length bytes then closes; server closes the stream cleanly; connection survives |
| `RPC_ZeroLenFrame` | client sends `len = 0`; server replies `INVALID`/`INVALID` (empty `RpcRequest` fails validation); connection survives |
| `RPC_MethodZero_NonFirstStream` | client opens stream #2 with `method=0`; server closes with `INVALID`/`METHOD_ZERO_RESERVED`; connection survives |
| `RPC_HeartbeatStream_NonZeroMethod` | client sends a `Balance` frame on the heartbeat stream; server closes the heartbeat stream with `INVALID`; connection ungraceful → owner exits |
| `RPC_StreamLimitExceeded` | client opens `MaxIncomingStreams + 1` streams concurrently; the (limit+1)th open blocks or fails per `quic-go` semantics; verified by stream-id assertion |
| `RPC_ConcurrentBalances_50x` | 50 parallel `Balance` calls on one connection; each gets its own stream; all return OK; race detector clean |
| `RPC_HandlerPanics` | inject a panic into the `enroll` handler via a build-tag–gated test seam; response is `INTERNAL`/`PANIC`; connection survives; subsequent RPC on a new stream succeeds |
| `RPC_ContextCancelled_MidCall` | client cancels the request context after writing the request; client sees `context.Canceled` on read; server's handler completes (server-side has no view of client cancel); follow-up RPC succeeds |

#### 10.3.3 `network_failure_test.go` — connection-level failure modes

| Case | How injected | Expectation |
|---|---|---|
| `Net_HardKill_ClientSide` | client `quic.Conn.CloseWithError` mid-RPC | server's `*Connection` `Done()` fires within 1 s; owner goroutine exits; `PeerCount` decrements; goleak-style assertion that no goroutine outlives the connection |
| `Net_HardKill_ServerSide` | call `Server.Shutdown(immediate)` while one RPC is in flight | in-flight client RPC fails; client receives connection close; server returns `ctx.Err()` from Shutdown |
| `Net_IdleTimeout` | client connects, sends nothing for `IdleTimeoutS + 5` (set `IdleTimeoutS = 2` in this subtest's config) | server tears the connection down via QUIC idle timeout; `PeerCount` returns to 0 |
| `Net_DropDatagrams` | wrap the QUIC `net.PacketConn` with a packet-dropping middleware that drops 100% after T+1s; in-flight RPC mid-write | client times out at QUIC idle; server detects dead connection; in-flight handler completes (server doesn't observe client liveness) |
| `Net_HighLoss_50pct` | wrap `net.PacketConn` to drop 50% of packets randomly; 20 sequential `Balance` calls | every call eventually completes successfully (QUIC retransmits); aggregate latency under 30 s |
| `Net_OutOfOrder` | wrap to reorder packets; 20 sequential `Balance` calls | all succeed (QUIC reassembles) |
| `Net_AddressChange_NotMigrated` | client switches source UDP port mid-session | QUIC connection drops (we disabled migration); server cleans up; client retries on a fresh connection |
| `Net_TLSAlertMidSession` | client sends a malformed QUIC frame after handshake | server tears the connection down; logs include "quic" / "frame" diagnostic; `PeerCount` decrements |
| `Net_DialUnreachable` | dial a closed `127.0.0.1:0` (no server) | client's `Dial` returns within `DialTimeout` with the underlying network error; server side N/A |
| `Net_RefusedAfterShutdown` | shutdown server, then dial | dial fails (listener closed); reasonable error surfaces |

#### 10.3.4 `push_path_test.go` — server-initiated streams under stress

| Case | Setup | Expectation |
|---|---|---|
| `Push_Offer_OK` | client `OnOfferPush` accepts; broker (test goroutine) calls `PushOfferTo` | `decisionCh` delivers `OfferDecision{Accept}` within 100 ms |
| `Push_Offer_Reject` | client handler rejects with `Reject{reason:"busy"}` | `decisionCh` delivers a Reject; server logs at debug |
| `Push_Offer_ClientNeverReplies` | client handler blocks forever | server times out at `OfferTimeoutMs`; `decisionCh` delivers `OfferDecision{Reject:"timeout"}`; stream closed |
| `Push_Offer_ClientCloseMidPush` | client closes the connection after server writes the push but before reading | server sees stream error; `decisionCh` delivers `OfferDecision{Reject:"connection_lost"}` then closes; connection cleanup runs |
| `Push_Offer_ToUnknownPeer` | call `PushOfferTo` for an `IdentityID` no one connected with | returns `(nil, false)` immediately |
| `Push_Offer_AfterShutdown` | shutdown started; call `PushOfferTo` | returns `(nil, false)` |
| `Push_Settlement_OK` | client `OnSettlementPush` returns ack | `ackCh` delivers `*SettleAck` |
| `Push_Settlement_DrainOnShutdown` | one settlement in flight; trigger `Shutdown` | `ackCh` is closed without delivering a value; broker caller distinguishes by the closed-without-receive idiom; receives `<-ackCh` returns the zero value with `ok=false` |
| `Push_Concurrent_50x` | 50 parallel `PushOfferTo` calls to one connected client | client handler invoked 50 times; all `decisionCh`s deliver; stream-id uniqueness asserted on the client side |
| `Push_DuringClientReconnect` | server pushes 5 offers, client kills connection mid-push #3, reconnects | first 2 deliver; #3 yields connection-lost; #4-5 (sent post-disconnect) return `(nil, false)` |

All four files share a `helpers_test.go` with: `newTestServer(t, opts...)`, `dialTestClient(t, srv, opts...)`, `withPacketWrapper(transport, ...)`, and a `goroutineLeakCheck(t)` defer (uses `runtime.NumGoroutine` snapshots, not goleak — keeps the test deps minimal).

`make -C tracker test` runs unit + component + integration; `make -C tracker test-unit` runs only unit + component for fast iteration.

### 10.4 Race + lint + coverage

- `go test -race ./internal/server/... ./internal/api/...` clean — required by `internal/server`'s heavy concurrency.
- `golangci-lint run ./internal/server/... ./internal/api/...` clean.
- Coverage target ≥ 90% on both packages, aggregated via `go test -coverprofile`.

### 10.5 Test fixtures

- `tracker/internal/server/testdata/` — Ed25519 keypairs (server + 2 fake clients) generated once via `go generate` script, committed as raw bytes. Same convention as `internal/ledger/testdata/`.
- `tracker/internal/api/testdata/` — golden `RpcResponse` hex per real handler. Asserts byte-identical canonical encoding; protects against silent proto field reordering.

## 11. Failure handling

| Failure | Behavior |
|---|---|
| Listener bind fails at startup | `Run` returns the error immediately |
| Listener accept loop returns a fatal error | `Run` returns the error; running connections continue until their owners exit |
| Client cert is non-Ed25519 / chain empty | TLS handshake fails; QUIC connection is rejected; no `*Connection` created; logged at info |
| Client cert SPKI matches but is malformed | Same as above |
| Client opens stream and never sends a frame | RPC reader times out at the QUIC idle timeout; stream closes |
| Oversize `len:u32` on a stream | Stream closed with `RPC_STATUS_INVALID "FRAME_TOO_LARGE"` |
| Unknown `RpcMethod` | Stream closed with `RPC_STATUS_INVALID "UNKNOWN_METHOD"` |
| Non-zero method on the heartbeat stream | Stream closed with `RPC_STATUS_INVALID` |
| Method-zero on a non-first stream | Stream closed with `RPC_STATUS_INVALID "METHOD_ZERO_RESERVED"` |
| Handler panics | Recovered by the per-RPC goroutine; logged with stack; response is `RPC_STATUS_INTERNAL "PANIC"` (message redacted unless debug); connection stays up |
| `PushOfferTo` for an unknown peer | Returns `(nil, false)` |
| `PushOfferTo` after Shutdown | Returns `(nil, false)` |
| `decisionCh` reader is GC'd before reply arrives | Owner goroutine drops the decision and logs at debug; stream is closed |
| `Server.Run` called twice | Second call returns `ErrAlreadyRunning` |
| `Shutdown` before `Run` | Returns nil |
| `Shutdown` ctx expires with N in-flight RPCs | Force-closes all connections; logs N; returns `ctx.Err()` |

## 12. Security model

- **mTLS everywhere on the public listener.** The QUIC listener exposes only mTLS-required QUIC; plain QUIC connections are refused at the TLS layer.
- **Identity is intrinsic.** No allowlist of "known" plugin pubkeys. Anyone with an Ed25519 key can complete the handshake; admission to actual capabilities is the (separate) admission subsystem's concern. This handler PR does not add admission gating.
- **No network-side rate limiting in this design.** Per-RPC rate limits live in (a) the reputation subsystem when it lands, and (b) `internal/admission` for `enroll` per spec §9. We do not pre-emptively add rate limiters that would need rewiring later.
- **No secrets in error messages outside debug.** `errResponse` with `RPC_STATUS_INTERNAL` returns `error.message = "internal error"` unless `LogLevel == debug`. Full error text always goes to the structured log.
- **Identity key handling.** `cmd/run_cmd.go` reads the Ed25519 private key from disk via `os.ReadFile`. The key is held in memory for the process lifetime as `ed25519.PrivateKey` (a `[]byte`). Never serialized, logged, or sent over the wire.

## 13. Cross-cutting amendments

- `tracker/internal/config.ServerConfig` gains `MaxFrameSize`, `IdleTimeoutS`, `MaxIncomingStreams`, `ShutdownGraceS` plus their validation rules. Updates land in the same commit as the first `internal/server` consumer.
- `cmd/token-bay-tracker/main.go` registers a new `run` subcommand alongside the existing `version` and `config` subcommands.
- `tracker/internal/session/` (empty dir) is removed; per-connection state lives on `*server.Connection`.
- This design relies on `shared/proto/rpc.proto` (`RpcMethod`, `RpcRequest`, `RpcResponse`, all per-method payloads, push messages, validators). Per the trackerclient design §5.3, that schema is owned by the trackerclient PR and lands in `shared/` first. **Implementation cannot start until that PR is merged.**

## 14. Open questions

- **`shared/idtls/` extraction.** Server-side mTLS helpers live in `internal/server/tls.go` for now (~30 LoC). The plugin already has `internal/idtls/`. When a third consumer arrives (e.g. federation tracker↔tracker mTLS) we extract to `shared/idtls/`. Premature now.
- **Loopback transport sharing.** The plugin's `internal/transport/loopback` and the tracker's component-tier loopback should be one package in `shared/transport/`. The trackerclient plan does not currently extract it. Decision deferred to plan-execution time; if the plugin code hasn't extracted by then, copy 80 LoC into `tracker/test/loopback/` with a TODO.
- **Per-RPC structured request_id.** Handlers receive a `Logger` from `RequestCtx` but no `request_id` is auto-injected. Add when the (separate) `internal/metrics` plan defines its propagation conventions.
- **Push stream backpressure.** If the broker calls `PushOfferTo` faster than a slow client can drain, owner goroutines pile up on `OpenStreamSync`. Bounded `MaxIncomingStreams` on the QUIC level prevents catastrophic blow-up; an explicit per-peer queue cap is deferred until broker observes the issue.
- **Heartbeat-driven `UpdateExternalAddr`.** The spec implies the tracker records the seeder's reflexive address on each heartbeat (§5.4). The heartbeat reader gets `RemoteAddr` from QUIC; whether we proactively call `registry.UpdateExternalAddr` on every ping or only on STUN binding requests is deferred until the broker is wired and we know which it consumes.

## 15. Acceptance criteria

The design is implemented when:

1. `token-bay-tracker run --config <path>` boots successfully against a valid config and binds the listener.
2. A `quic-go`-based test client completes a TLS 1.3 mTLS handshake with ALPN `"tokenbay/1"`; mismatched SPKI / wrong ALPN both fail.
3. The five real handlers (`enroll`, `balance`, `advertise`, `stun_allocate`, `turn_relay_open`) round-trip end-to-end against the real ledger / registry / stunturn implementations in the component tier.
4. The four stub handlers (`broker_request`, `settle`, `usage_report`, `transfer_request`) return `RPC_STATUS_INTERNAL` with `error.code = "NOT_IMPLEMENTED"`.
5. `Server.PushOfferTo` and `Server.PushSettlementTo` deliver a push to a connected client and return the client's reply on the channel.
6. Heartbeat: client pings cause `registry.Heartbeat` to fire; missing pongs are NOT how the server detects death (QUIC idle timeout is).
7. `Server.Shutdown(ctx)` drains in-flight RPCs within `ctx.Deadline` or force-closes; second call is nil; outstanding push channels receive drain rejects.
8. `go test -race -coverprofile=coverage.out ./internal/server/... ./internal/api/...` reports ≥ 90% coverage on both packages.
9. `golangci-lint run ./internal/server/... ./internal/api/...` clean.
10. `internal/api` source has no import of `internal/server`, `quic-go`, or `crypto/tls`.
11. `internal/config/testdata/full.yaml` includes the four new `server.*` fields with valid values; `internal/config` validation rules cover them.
12. The integration tier (build-tagged) passes against real UDP / real `quic-go` for: every handshake case in §10.3.1, every RPC case in §10.3.2, every network-failure case in §10.3.3, every push case in §10.3.4. No goroutine leaks across any subtest (per the `goroutineLeakCheck` defer).

## 16. Future work

- **Admin HTTP API** (spec §8) lands in `internal/admin` with its own design. It will call `Server.Shutdown`, `Server.PeerCount`, and a future `Server.PeerSnapshot()`.
- **Broker** lands in `internal/broker` and replaces the Scope-2 stub for `broker_request` and `usage_report`. It uses `Server.PushOfferTo` and `Server.PushSettlementTo` to drive offer/settlement flows.
- **Admission** lands in `internal/admission` and replaces the optional admission gate in `enroll`.
- **Federation** lands in `internal/federation` and replaces the stub for `transfer_request`. Federation also runs its own peer-tracker connections — likely via a `internal/federation/server` mirroring this design.
- **Metrics** lands in `internal/metrics`; a Prometheus collector subscribes to a small `Server` event bus (peer connect/disconnect, RPC method/duration/status).
- **Per-IP rate limiting on `enroll`** (spec §9 default 1/min/IP) when admission lands.
- **`shared/idtls/` extraction** when a third consumer needs mTLS-with-SPKI-pinning helpers.
