# Tracker STUN/TURN data plane + NAT-simulated e2e — Design

**Date:** 2026-07-11
**Status:** Approved (design phase)
**Builds on:**
- `docs/superpowers/specs/tracker/2026-05-02-tracker-stunturn-design.md` (the pure `stunturn` package: Reflect + Allocator, already implemented, ~97% unit-covered)
- `docs/superpowers/specs/2026-07-11-tracker-e2e-testcontainers-concurrency-design.md` (the testcontainers harness, now merged)

## Goal

Make the tracker's STUN/TURN **data plane** real, and prove it end-to-end against a simulated NAT'd network.

Today `stunturn` is pure logic (Reflect, Allocator) with no sockets, and `internal/server` opens a UDP socket **only for QUIC** (`:7777`, the control plane). The `:3478` STUN and `:3479` TURN-relay read/write loops the stunturn spec explicitly assigns to `internal/server` were never built. Consequence: a consumer behind a restrictive NAT **cannot open a data-path tunnel** — hole-punch discovery (STUN) and relay fallback (TURN) do not work. The plugin's `TURN_RELAY_OPEN` control RPC exists and is tested (e2e scenario 23), but nothing carries bytes. This is a deploy blocker for the P2P tunnel, which is the product's data path.

Delivered in **two phases** so the tracker gap lands and is verifiable before the larger plugin+NAT work:

- **Phase 1 — Tracker data plane.** Build the `:3478`/`:3479` UDP loops in `internal/server`, wire them in `run_cmd`, and test them with a direct host-side STUN/relay test client.
- **Phase 2 — Plugin Rendezvous + NAT e2e.** Implement the plugin `Rendezvous` client and the token-framing relay transport, and add a libp2p-style NAT-router topology to the testcontainers harness so a NAT'd consumer relays real tunnel bytes through the tracker.

## Non-goals

- RFC 5766 TURN (Allocate/Send/Data/ChannelBind, channel numbers, lifetime). We ship the custom token-framed relay the stunturn spec describes, not standards TURN.
- Simulating **symmetric or full-cone** NAT. Docker's MASQUERADE is port-restricted-cone; that is sufficient to force the relay path and exercise reflexive addressing. Full-cone/symmetric would need kernel modules (`netfilter-full-cone-nat`) — out of scope.
- Testing hole-punch **success** across NAT-type matrices (the client-side ICE optimization). Our deploy-critical path is the **relay fallback** when direct fails; that is what Phase 2 asserts.
- In-process virtual-network testing (Tailscale `natlab` / pion `vnet`). The `stunturn` logic is already unit-covered; the untested gap is the **real kernel sockets + real deployment**, which only a container topology exercises. See Research.
- IPv6 / ICE candidate pairing. IPv4/UDP only, as in the stunturn spec.

## Research: how similar systems test NAT-related behavior

Two schools, plus the reasoning for our choice.

**In-process virtual network.** Tailscale `natlab` (`tstest/natlab`) simulates whole networks in-memory (no VMs/root) and models EndpointIndependent (full-cone), AddressDependent (restricted-cone), and AddressAndPortDependent (symmetric) NAT, plus a synthetic internet. pion `transport/vnet` does the same for `pion/stun|turn|ice`. Fast, deterministic, and the only cheap way to cover symmetric NAT — but it tests protocol/client logic against a fake `PacketConn`, not real kernel sockets or the real deployment.

**Docker NAT-router topology.** libp2p's hole-punch CI uses 5 containers / 3 networks per test: peer containers whose default route is their own NAT-router container doing SNAT out to a shared WAN where the relay/STUN server lives, with unique per-test subnets for parallelism. `enobufs/nat-traversal-test-using-docker` uses a WAN/MIDBOX/APP three-layer. Ready-made router images exist (`kmanna/docker-nat-router`, `bobfraser1/alpine-router`): Alpine + `NET_ADMIN` + `ip_forward=1` + `iptables -t nat -A POSTROUTING -o <ext> -j MASQUERADE`. Docker `--internal` networks get no MASQUERADE rule → no route out, which is how two peers are made mutually unreachable so relay is exercised. Limitation: Docker MASQUERADE is port-restricted-cone only.

**Choice.** Token-Bay's `stunturn` is pure logic already unit-tested at ~97%; the deploy gap is the real `net.ListenUDP` loops + real UDP relay in a real NAT topology, which in-process testing cannot cover. The Docker NAT-router topology tests exactly the unproven part and matches the "real NAT + firewalls" goal, and the merged testcontainers harness makes dynamic networks + privileged router containers easy. We therefore use the Docker topology for e2e and keep the existing pure-logic unit tests as-is.

Sources: Tailscale natlab (pkg.go.dev/tailscale.com/tstest/natlab); pion/transport/vnet; libp2p unified testing (libp2p.io/blog/new-unified-testing); enobufs/nat-traversal-test-using-docker; kmanna/docker-nat-router.

## Phase 1 — Tracker STUN/TURN data plane

### 1.1 New unit: `internal/server/udp.go`

Owns the two UDP data-plane loops. Isolated from QUIC (`listener.go`) and from `stunturn` (which stays pure). Inputs: the `*stunturn.Allocator`, the `STUNTURNConfig` addresses, a logger, and a metrics sink. Started and stopped by `run_cmd`.

**STUN loop (`:3478`, `STUNTURNConfig.STUNListenAddr`):**
```
for datagram, src := ReadFromUDPAddrPort(stunSock):
    txID, ok := stunturn.DecodeBindingRequest(datagram)   // rejects non-binding / unexpected comprehension-required attrs
    if !ok: metric(stun_invalid); continue
    resp := stunturn.Reflect(txID, src).ResponsePayload     // src IS the reflexive (post-NAT) address
    WriteToUDPAddrPort(resp, src)
    metric(stun_reflected)
```
Recording the reflexive address into the registry (`UpdateExternalAddr`) is deferred — the seeder's advertised address already works through the QUIC path; STUN here serves the *peers'* hole-punch discovery.

**TURN relay loop (`:3479`, `STUNTURNConfig.TURNListenAddr`):**

Relay datagrams are framed `[16-byte session token][opaque payload]` in **both** directions. The loop keeps a data-plane binding map (owned here, not in the pure allocator):
```
type binding struct { a, b netip.AddrPort; lastSeen time.Time }
bindings map[stunturn.Token]*binding   // guarded by a mutex

for datagram, src := ReadFromUDPAddrPort(turnSock):
    if len(datagram) < 16: metric(turn_malformed); continue
    tok := Token(datagram[:16]); payload := datagram[16:]
    sess, err := allocator.ResolveAndCharge(tok, len(datagram), now())   // validates session + per-seeder kbps bucket
    if err != nil: metric by errors.Is (ErrUnknownSession | ErrThrottled); continue
    dst := learnAndPeer(bindings, tok, src)    // first two distinct srcs bind a/b; a 3rd is dropped
    if !dst.IsValid(): metric(turn_awaiting_peer); continue
    WriteToUDPAddrPort(datagram, dst)          // forward token+payload verbatim
    metric(turn_relayed_bytes += len)
```
`learnAndPeer`: on an unknown token, create a binding with side `a = src`. On a second distinct src, set `b`. Return the *other* side for a known src; return invalid (buffer/drop) until both sides are known; drop a third distinct src (a token binds exactly two peers). `ResolveAndCharge` failing on `ErrUnknownSession` is the auth gate — possession of a live token is the capability.

**Reaper:** a single goroutine on a ticker calls `allocator.Sweep(now)` and prunes `bindings` whose `lastSeen` predates `SessionTTL`. (One reaper; the existing `stunturn` sweeper wiring in `maintenance.go` is reused/extended rather than duplicated.)

### 1.2 Wiring (`cmd/token-bay-tracker/run_cmd.go`)

Open both UDP sockets (`net.ListenUDP`, addresses from `STUNTURNConfig`), construct the `server/udp.go` component with the existing allocator, start its loops in the run group, and close on shutdown. Disabled cleanly if either addr is empty.

### 1.3 Config, metrics, ports

- Config already has `STUNTURNConfig{STUNListenAddr ":3478", TURNListenAddr ":3479", TURNRelayMaxKbps, SessionTTLSeconds}`. No new config.
- Prometheus metrics (emitted in `server/udp.go`): `stun_requests_total{outcome}`, `turn_datagrams_total{outcome}`, `turn_relayed_bytes_total`, `turn_throttle_drops_total`, `turn_active_bindings`.
- `compose.e2e.yaml` tracker-a exposes the two UDP ports for the host-side Phase-1 test (e.g. `7778:3478/udp`, `7779:3479/udp`); the e2egen config points STUN/TURN at `:3478`/`:3479`.

### 1.4 Phase-1 e2e (direct test client)

A build-tagged scenario drives the tracker host-side, no plugin tunnel:
- **STUN:** a host UDP socket sends a `pion/stun` binding request to the mapped `:3478`; assert a well-formed binding response decodes to a valid `XOR-MAPPED-ADDRESS`. (Assert validity, not a specific IP — the reflexive value is whatever the mapped-port path presents; the round-trip through the real codec + real socket is the point.)
- **TURN relay:** enroll a consumer, win an assignment, `TURN_RELAY_OPEN` → token. Two host UDP sockets act as the two peers: each sends `[token][payload]` to the mapped `:3479`; assert peerA↔peerB cross-delivery, that an **unknown token is dropped**, and that flooding past `TURNRelayMaxKbps` produces **throttle drops** (observed via the metric).
- A small `driver` helper encapsulates the STUN request build/parse and the token framing.

Exercises `DecodeBindingRequest`, `EncodeBindingResponse`, `Reflect`, `ResolveAndCharge`, `Charge`, and the forwarding loop against real sockets.

## Phase 2 — Plugin Rendezvous + NAT e2e

### 2.1 Plugin `Rendezvous` implementation (`plugin/internal/rendezvous`)

Implements the existing `tunnel.Rendezvous` interface:
- `AllocateReflexive(ctx) (netip.AddrPort, error)` — send an RFC 5389 binding request to the tracker's `:3478` (address from enrollment/config) over a UDP socket, parse the reflexive `AddrPort`.
- `OpenRelay(ctx, sessionID) (RelayCoords, error)` — call the `TURN_RELAY_OPEN` RPC via `trackerclient`, return `RelayCoords{Endpoint, Token}` (endpoint parsing already exists as `ParseRelayCoords`).

### 2.2 Token-framing relay transport

Today `holepunch.go`'s relay fallback dials the relay endpoint with **raw QUIC** — no session token — so the tracker cannot route it. Add a `net.PacketConn` wrapper that, when the tunnel takes the relay path, prefixes `[16-byte token]` on every send to the relay endpoint and strips it on receive. `quic-go` runs unmodified over this wrapper; the pinned TLS handshake is unchanged (the tracker relays ciphertext, cannot decrypt). Wired into `holepunch.go` between `OpenRelay` and `dialOverTransport`.

### 2.3 E2E actors

The seeder/consumer actors gain a config flag to build the tunnel with the real `Rendezvous` (STUN + relay) instead of today's direct `tunnel.Dial`. Default off (existing scenarios unaffected); the NAT scenario turns it on.

### 2.4 NAT topology (libp2p-style, via `driver.Stack`)

```
        ┌──────────── wan (shared bridge) ────────────┐
        │   tracker-a  (:3478 STUN, :3479 TURN, QUIC)  │
        └───▲──────────────────────────────────────▲──┘
       routerA (MASQUERADE)                    routerB (MASQUERADE)
            │  NET_ADMIN, ip_forward=1              │
     ┌── site-A (--internal) ──┐          ┌── site-B (--internal) ──┐
     │      consumer           │          │        seeder            │
```

New `driver.Stack` capability (`StartNATSite`):
- Create an `--internal` site network (no route out) and attach the actor container to it as its only network, with its default route set to the router.
- Start an Alpine iptables router container (image built from a tiny `Dockerfile.natrouter`, or a pinned public router image) attached to both the site network and `wan`, with `NET_ADMIN` (via `HostConfigModifier` cap-add), `ip_forward=1`, and a `POSTROUTING -o <wan-if> -j MASQUERADE` rule.
- The tracker-a container is reachable on `wan`; consumer↔tracker and seeder↔tracker traverse the routers (SNAT — the tracker sees each router's WAN IP as the reflexive address); **consumer↔seeder is unroutable** (isolated internal nets, port-restricted-cone) → hole-punch fails → **relay via tracker**.
- Ports/aliases are dynamic (testcontainers); teardown terminates routers + networks with the stack.

### 2.5 Phase-2 e2e (relay-fallback)

A NAT'd consumer serves a request through a NAT'd seeder with real tunnel bytes over the tracker's `:3479` relay:
- Assert the served SSE body arrives (relay carried the bytes end-to-end).
- Assert the relay's `turn_relayed_bytes_total` moved (bytes actually transited the tracker).
- Assert (negative) that a direct hole-punch was attempted first and failed (from the consumer actor's tunnel logs / a probe counter), so the relay path — not an accidental direct path — was exercised.
- Settlement completes normally (the data path is transparent to the ledger).

## Error handling

- **STUN:** malformed / non-binding / unexpected-attr datagrams are counted and dropped, never answered. A slow/blocked write never blocks the read loop (best-effort `WriteToUDPAddrPort`).
- **TURN:** unknown/expired token → drop (`ErrUnknownSession`); over-rate → drop (`ErrThrottled`), never queue; malformed (<16 bytes) → drop; a third peer address for a token → drop. All counted.
- **Relay transport (plugin):** a relay-mode datagram whose token doesn't match the session is dropped by the receiver; QUIC's own retransmit handles loss. `OpenRelay` failure or invalid endpoint surfaces `ErrRelayFailed` (existing).
- **NAT router (test infra):** a router that fails to bring up its MASQUERADE rule fails the scenario fast with its logs; teardown is idempotent.

## Testing strategy

| Layer | Test |
|---|---|
| `stunturn` pure logic | existing unit tests (~97%), unchanged |
| `server/udp.go` loops | Go unit tests over an in-proc `net.PacketConn` pair (framing, learn-and-peer, throttle drop, unknown token, reaper) |
| Phase-1 data plane | host-side e2e scenario (STUN round-trip, relay cross-delivery, throttle, unknown token) |
| Plugin `Rendezvous` + relay transport | plugin unit tests (STUN client parse, token frame/deframe) |
| Phase-2 end-to-end | NAT-router e2e scenario (relay fallback carries real bytes; direct fails first) |

## Decomposition into implementation plans

Two plans, in order:
1. **Plan A — Phase 1** (`internal/server/udp.go` + `run_cmd` wiring + config/metrics/ports + `server/udp` unit tests + Phase-1 host-side e2e). Self-contained; lands the tracker data-plane gap and is fully verifiable on the existing harness.
2. **Plan B — Phase 2** (plugin `rendezvous` package + token-framing relay transport + actor flag + `driver.Stack.StartNATSite` + `Dockerfile.natrouter` + Phase-2 NAT e2e scenario). Depends on Plan A.
