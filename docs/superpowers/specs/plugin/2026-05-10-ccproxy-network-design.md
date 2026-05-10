# Consumer NetworkRouter (`plugin/internal/ccproxy.NetworkRouter`) — Subsystem Design Spec

| Field | Value |
|---|---|
| Parent | [Plugin design](./2026-04-22-plugin-design.md) §5.4, [Architecture](../2026-04-22-token-bay-architecture-design.md) §10.3, [Tunnel design](../../plans/2026-05-03-plugin-tunnel.md) |
| Status | Design draft — 2026-05-10 |
| Date | 2026-05-10 |
| Scope | Consumer-side replacement for the v1 stub `NetworkRouter` in `plugin/internal/ccproxy/router.go`. Wires the in-process ccproxy HTTP server to a real `internal/tunnel.Tunnel`: dial seeder, send length-prefixed `/v1/messages` body, read status byte, stream SSE bytes back to Claude Code's open HTTP response. Adds `SeederAddr` / `SeederPubkey` / `EphemeralPriv` fields to `EntryMetadata` so that future StopFailure-hook orchestration can stage the assignment. Out of scope: who fills those fields. |

## 1. Purpose

Plugin design §5.4 step 5 reads "On seeder assignment: sidecar opens a QUIC tunnel to the seeder ... streams the request body over the tunnel ... relays SSE chunks verbatim to Claude Code's open HTTP response." Today, `ccproxy/router.go:41-50` returns HTTP 501 from `NetworkRouter.Route`. This spec replaces the stub with the real flow.

The router is the consumer-side counterpart to the seeder coordinator described in `2026-05-10-ssetranslate-design.md`. Together they close the consumer→seeder→consumer loop: a rate-limited consumer's `claude` posts `/v1/messages` to the local ccproxy, which (if the session is in `ModeNetwork`) dials a tunnel, ships the body, and copies SSE bytes back into the same HTTP response — indistinguishable from a direct Anthropic call to the consumer's claude.

## 2. Non-goals (explicit)

- The broker handshake. `envelopebuilder` + `trackerclient.BrokerRequest` + the StopFailure hook orchestration that runs them and stashes the resulting `SeederAssignment` in `EntryMetadata` are a separate, future plan. This spec adds the EntryMetadata fields the future plan will populate; it does not call into envelopebuilder or trackerclient.
- Per-request rebrokering. Spec §5.4 envisions a fresh `broker_request` per `/v1/messages` POST. v1 of this router uses whatever assignment is already staged in EntryMetadata (one assignment per ModeNetwork session entry). Per-request rebrokering is a v2 concern; the `PeerDialer` interface defined here is the seam where it would land.
- Counter-signing the tracker's settlement request (§5.4 step 9). Settlement happens on a different RPC channel after the response stream closes; ccproxy does not own that path.
- Audit-log writes per turn (§8). The audit log is opened by the cmd layer and threaded into the sidecar; ccproxy can call into it via an injected sink, but this spec defers the audit-log integration so the router stays focused.
- Seeder-side anything. This spec is consumer-only.

## 3. Architecture

### 3.1 Position in the consumer pipeline

```
                   ┌──────────────────────────────────────────────────────────┐
                   │  Claude Code (consumer)                                  │
                   │   POST /v1/messages → http://127.0.0.1:PORT (ccproxy)    │
                   └────────────────────────────┬─────────────────────────────┘
                                                │
                                                ▼
                   ┌──────────────────────────────────────────────────────────┐
                   │  ccproxy.Server.handleAnthropic                          │
                   │   • lookup session mode                                  │
                   │   • ModeNetwork → NetworkRouter.Route(w, r, meta)        │
                   └────────────────────────────┬─────────────────────────────┘
                                                │
                                                ▼
                   ┌──────────────────────────────────────────────────────────┐
                   │  NetworkRouter.Route                                     │
                   │   1. dialer.Dial(ctx, meta) → PeerConn                   │
                   │   2. peer.Send(reqBody)                                  │
                   │   3. statusOK, errMsg, body, err := peer.Receive(ctx)    │
                   │   4. on OK:    write 200 + headers + io.Copy(w, body)    │
                   │      on error: write 502 + Anthropic-shaped JSON         │
                   │   5. peer.Close                                          │
                   └────────────────────────────┬─────────────────────────────┘
                                                │
                                                ▼
                   ┌──────────────────────────────────────────────────────────┐
                   │  PeerDialer (production: tunnelDialer)                   │
                   │   reads meta.SeederAddr / SeederPubkey / EphemeralPriv   │
                   │   calls tunnel.Dial(...)                                 │
                   └────────────────────────────┬─────────────────────────────┘
                                                │
                                                ▼
                                          (tunnel.Tunnel)
                                                │
                                                ▼
                                            … to seeder
```

### 3.2 Public API additions

```go
package ccproxy

// PeerDialer opens a tunnel to the seeder identified by meta. It is the
// only seam the router uses to reach the network; production wiring
// supplies a tunnelDialer (see network_dialer.go), tests supply fakes.
type PeerDialer interface {
    Dial(ctx context.Context, meta *EntryMetadata) (PeerConn, error)
}

// PeerConn is the half-duplex consumer-side surface of a tunnel session.
// Send the request body once, then Receive returns the status byte and a
// reader for the response stream. Close tears down the underlying tunnel.
type PeerConn interface {
    Send(body []byte) error
    Receive(ctx context.Context) (statusOK bool, errMsg string, body io.Reader, err error)
    Close() error
}

// NetworkRouter forwards ModeNetwork sessions over a tunnel to a seeder.
type NetworkRouter struct {
    Dialer PeerDialer       // required; New() panics if nil at first use
    Logger *zerolog.Logger  // optional; defaults to a no-op logger
    Now    func() time.Time // optional; defaults to time.Now (for log timestamps only)

    // ResponseFlushInterval, when >0, periodically flushes w during the
    // io.Copy loop. 0 → flush on every read (the default; QUIC delivers
    // SSE events token-grained already).
    ResponseFlushInterval time.Duration
}

// EntryMetadata gains three fields. Existing callers leave them zero;
// the StopFailure-hook orchestrator (separate plan) fills them when
// activating ModeNetwork.
type EntryMetadata struct {
    EnteredAt          time.Time
    ExpiresAt          time.Time
    StopFailurePayload *ratelimit.StopFailurePayload
    UsageProbeBytes    []byte
    UsageVerdict       ratelimit.UsageVerdict

    // Seeder routing — populated by the future broker orchestrator.
    SeederAddr    netip.AddrPort
    SeederPubkey  ed25519.PublicKey
    EphemeralPriv ed25519.PrivateKey
}
```

### 3.3 Production `tunnelDialer`

```go
package ccproxy

// tunnelDialer is the production PeerDialer wrapping internal/tunnel.Dial.
type tunnelDialer struct {
    DialTimeout time.Duration // default 5s
    Now         func() time.Time
}

func (d *tunnelDialer) Dial(ctx context.Context, meta *EntryMetadata) (PeerConn, error) {
    if meta == nil { return nil, ErrNoAssignment }
    if !meta.SeederAddr.IsValid() { return nil, ErrNoAssignment }
    if len(meta.SeederPubkey) != ed25519.PublicKeySize { return nil, ErrNoAssignment }
    if len(meta.EphemeralPriv) != ed25519.PrivateKeySize { return nil, ErrNoAssignment }

    cfg := tunnel.Config{
        EphemeralPriv: meta.EphemeralPriv,
        PeerPin:       meta.SeederPubkey,
        Now:           d.Now,
    }
    tun, err := tunnel.Dial(ctx, meta.SeederAddr, cfg)
    if err != nil { return nil, err }
    return &tunnelPeerConn{tun: tun}, nil
}

type tunnelPeerConn struct{ tun *tunnel.Tunnel }
func (p *tunnelPeerConn) Send(body []byte) error { return p.tun.Send(body) }
func (p *tunnelPeerConn) Receive(ctx context.Context) (bool, string, io.Reader, error) {
    st, r, err := p.tun.Receive(ctx)
    if err != nil { return false, "", nil, err }
    if st == tunnel.StatusOK { return true, "", r, nil }
    msg, _ := io.ReadAll(r)
    return false, string(msg), nil, nil
}
func (p *tunnelPeerConn) Close() error { return p.tun.Close() }
```

(Note: `tunnel.StatusOK` exposing the internal `status` enum as a public typed constant is a small additional change to `internal/tunnel`, scoped to the same PR. See §6.)

### 3.4 Route flow (state diagram)

```
                       Route entered
                            │
                            ▼
                ┌─────────────────────────┐
                │ meta == nil ?           │── yes ──► 502 ErrNoAssignment, log "stray ModeNetwork"
                └────────────┬────────────┘
                             │ no
                             ▼
                ┌─────────────────────────┐
                │ read full r.Body        │── err ─► 400 + log
                │ (bounded ≤ 1 MiB)       │
                └────────────┬────────────┘
                             │
                             ▼
                ┌─────────────────────────┐
                │ Dialer.Dial(ctx, meta)  │── err ─► 502 + Anthropic error JSON
                └────────────┬────────────┘
                             │
                             ▼
                ┌─────────────────────────┐
                │ peer.Send(body)         │── err ─► 502; peer.Close
                └────────────┬────────────┘
                             │
                             ▼
                ┌─────────────────────────┐
                │ ok, msg, rdr, err =     │
                │   peer.Receive(ctx)     │
                └────────────┬────────────┘
                             │
              ┌──────────────┴──────────────┐
              │                             │
       err != nil                  ok == false (statusError)
              │                             │
              ▼                             ▼
     502 + log; peer.Close       502 + body=msg; peer.Close
                                            
                                  ok == true
                                            │
                                            ▼
                              ┌──────────────────────────────┐
                              │ w.WriteHeader(200)           │
                              │ w.Header set Content-Type:   │
                              │   text/event-stream          │
                              │ flusher.Flush                │
                              │ io.Copy(w, rdr)              │
                              │  • ignore EOF; ignore        │
                              │    "use of closed conn"      │
                              │ peer.Close                   │
                              └──────────────────────────────┘
```

### 3.5 HTTP response shape

| Outcome | HTTP status | Content-Type | Body |
|---|---|---|---|
| OK + bytes flowing | 200 | `text/event-stream` (set by router before first byte) | raw SSE bytes from tunnel |
| `meta == nil` (stray) | 502 | `application/json` | `{"type":"error","error":{"type":"network_unavailable","message":"Token-Bay session not in network mode"}}` |
| Body read error from claude | 400 | `application/json` | `{"type":"error","error":{"type":"invalid_request","message":"…"}}` |
| Dial failure (`ErrPeerPinMismatch`, `ErrHandshakeFailed`, `ErrInvalidConfig`, `ErrNoAssignment`, network) | 502 | `application/json` | `{"type":"error","error":{"type":"network_unavailable","message":"<short reason, no PII>"}}` |
| Status byte `0x01` (error) | 502 | `application/json` | `{"type":"error","error":{"type":"network_unavailable","message":"<seeder's UTF-8 error, ≤4 KiB>"}}` |
| Mid-stream tunnel close after status OK | (already 200) | (already text/event-stream) | flush whatever was written; close connection. claude observes truncated stream and surfaces a normal turn error. |

### 3.6 Locked design decisions

| # | Decision | Choice | Rationale |
|---|---|---|---|
| D1 | Where the seeder assignment lives | Pre-staged in `EntryMetadata` by the StopFailure-hook orchestrator | Keeps the router thin and the hot path testable. Future per-request rebrokering moves into a different `PeerDialer` impl without touching the router. |
| D2 | `PeerDialer` / `PeerConn` interfaces | Yes | The router never imports `internal/tunnel` directly. Production dialer is in a separate file (`network_dialer.go`). Tests use fakes; no goroutine-leaking real tunnels. |
| D3 | Body size bound | `io.LimitReader(r.Body, 1<<20)` (1 MiB) before forwarding | Mirrors `tunnel.Config.MaxRequestBytes`. Defense against accidental large bodies leaking into a tunnel that would refuse them anyway. |
| D4 | Streaming flush | Default: rely on `http.ResponseWriter`'s implicit flushing + QUIC stream framing. `ResponseFlushInterval > 0` opt-in | Anthropic SSE events are typically <1 KB and arrive token-grained; a per-event explicit flush typically isn't needed for claude to render. Opt-in for diagnostics. |
| D5 | Error body shape | Anthropic-style `{type:"error","error":{type,message}}` | Claude Code surfaces this naturally. Matches existing `NetworkRouter` 501 stub shape. |
| D6 | `Content-Type` header | Always `text/event-stream` on success path | Claude Code's SDK keys streaming behavior off this header. Set before first byte. |
| D7 | Logging | One zerolog event per Route invocation: outcome + ms + bytes-out (no payload). At debug level: dial errors, status byte | Operator visibility without log volume. |
| D8 | Public exposure of `tunnel.StatusOK` | Add a `StatusOK` and `StatusError` typed constant to `internal/tunnel` | The router needs to interpret the byte; a typed constant beats a magic 0x00. Scope creep into tunnel, but tiny. |

## 4. File map

```
plugin/internal/ccproxy/
├── router.go                       — MODIFY: full NetworkRouter + PeerDialer interface
├── router_test.go                  — MODIFY: replace 501-stub test with table-driven cases
├── network_dialer.go               — CREATE: production tunnelDialer + tunnelPeerConn
├── network_dialer_test.go          — CREATE: e2e against a real tunnel.Listener
├── sessionmode.go                  — MODIFY: add SeederAddr/SeederPubkey/EphemeralPriv
└── sessionmode_test.go             — MODIFY: round-trip the new fields

plugin/internal/tunnel/
├── frame.go                        — MODIFY: export StatusOK/StatusError typed constants
└── frame_test.go                   — MODIFY: assert exported constants match wire bytes
```

## 5. Test strategy

### 5.1 Unit tests for `NetworkRouter`

A `fakePeerDialer` and `fakePeerConn` cover every branch in §3.4:

| Case | Expected |
|---|---|
| `meta == nil` | 502, error body, dialer not called |
| `r.Body` read returns error | 400, error body, dialer not called |
| `Dialer.Dial` returns `ErrNoAssignment` | 502, "network_unavailable" |
| `Dialer.Dial` returns `tunnel.ErrPeerPinMismatch` | 502, sanitized message |
| `peer.Send` fails | 502, peer.Close called |
| `peer.Receive` returns `(false, "boom", nil, nil)` | 502, body contains "boom" |
| `peer.Receive` returns `(true, "", reader, nil)`, reader yields 3 SSE events | 200, body byte-equal to reader content, Content-Type set |
| Reader EOFs early (mid-stream cut) | 200, partial bytes flushed, response.Body io.EOF observed by client |

### 5.2 E2E with real tunnel (no real claude)

`network_dialer_test.go`:
1. Spin up a real `tunnel.Listener` on `127.0.0.1:0` with seeder ed25519 keys.
2. Background goroutine: `Accept` → `ReadRequest` → `SendOK` → write canned SSE bytes → `CloseWrite`.
3. Build an `EntryMetadata` carrying the listener's `LocalAddr`, the seeder pubkey, and a fresh consumer ephemeral keypair.
4. Build a `ccproxy.Server` with `WithNetworkRouter(NewNetworkRouter(WithDialer(&tunnelDialer{})))` and `WithSessionStore(...)` containing the staged entry under a known session id.
5. POST `/v1/messages` to the server with `X-Claude-Code-Session-Id: <id>`.
6. Assert response status, Content-Type, and body bytes match the canned SSE.

### 5.3 What this package does NOT test

- The translator (covered in `internal/ssetranslate`).
- Real `claude -p` invocations (covered by the future seeder-coordinator plan's `localintegtest`).
- Broker_request orchestration (separate, future plan).

## 6. Cross-cutting changes outside `internal/ccproxy`

This subsystem touches one external file:

- `plugin/internal/tunnel/frame.go`: export `StatusOK status = 0x00` and `StatusError status = 0x01` as a public type or constants. The existing private enum becomes `tunnel.StatusOK` / `tunnel.StatusError`. No wire change, no test break beyond name updates inside the tunnel package.

This change ships in the same PR as the router, since the router is the only external caller that needs to discriminate on status.

## 7. Open questions

- **Log line content for dial failures.** §3.6/D7 says "no PII". Tunnel error messages from quic-go include peer addrs. We sanitize by mapping known sentinels to short strings; the original error stays at `Debug` level only. Fine for v1.
- **Per-request rebrokering.** §2 defers this to a v2 `PeerDialer` impl. The interface as defined accepts `*EntryMetadata`; a v2 dialer would call `trackerclient.BrokerRequest` inside `Dial` and ignore the staged fields. No interface change required.

## 8. Acceptance criteria

This subsystem is "done" when:

- [ ] `go test ./plugin/internal/ccproxy/...` passes with the race detector clean and no skips.
- [ ] `go test ./plugin/internal/tunnel/...` still passes after the StatusOK/StatusError export.
- [ ] `golangci-lint run ./plugin/internal/ccproxy/... ./plugin/internal/tunnel/...` passes.
- [ ] The 501-stub test in `router_test.go` is removed; table-driven branch coverage from §5.1 takes its place.
- [ ] `network_dialer_test.go` passes against a real `tunnel.Listener` on every supported platform (Unix; Windows pending tunnel/CLAUDE.md note).
- [ ] No production import of `crypto/rand`. No filesystem I/O in the router itself (the dialer is interface-pure; production dialer lives in its own file with explicit deps).
- [ ] No goroutine leaks in tests under `go test -race`.
- [ ] PR is merged green; no localintegtest dependency.
