# tracker/internal/stunturn — Subsystem Design Spec

| Field | Value |
|---|---|
| Parent | [Regional Tracker](./2026-04-22-tracker-design.md) |
| Status | Design draft |
| Date | 2026-05-02 |
| Scope | The `stunturn` Go package: a STUN binding-response reflector (wire-format codec via `github.com/pion/stun/v2`) and an in-process TURN session allocator. Pure logic. No sockets, no goroutines, no metrics. |

## 1. Purpose

Help a consumer plugin and a seeder plugin open a peer-to-peer UDP tunnel for the per-request data path, and provide a relay fallback when hole-punching fails. Two services:

- **STUN** (RFC 5389 binding request/response): tells a node what public `IP:port` the tracker observed it from. Used by both peers to discover their reflexive addresses for hole-punching.
- **TURN-style relay** (custom framing, deferred): when hole-punching fails, the tracker becomes the relay between the two peers. This module manages the *session state* — token issuance, expiry, per-seeder bandwidth caps. It does not speak RFC 5766 and does not own the UDP socket.

The tracker is the control plane (tracker spec §1, plugin spec §1.3); `stunturn` carries no bulk traffic by design and exists only to make peer-to-peer feasible.

## 2. Scope

### 2.1 In scope

- Thin wrappers over `github.com/pion/stun/v2` for: binding-request decode (validate + extract 12-byte transaction ID) and binding-response encode (one `XOR-MAPPED-ADDRESS` attribute, IPv4 or IPv6 derived from `netip.AddrPort`). Wire-format compliance is delegated to the library; the wrappers convert between our `[12]byte` / `netip.AddrPort` types and pion's `stun.Message`.
- A pure `Reflect(txID, observed)` function over `netip.AddrPort`.
- An `Allocator` exposing `Allocate / Resolve / ResolveAndCharge / Charge / Release / Sweep` with:
  - 16-byte opaque session tokens, generated from an injected `io.Reader`.
  - Per-seeder token-bucket bandwidth cap driven by `STUNTURNConfig.TURNRelayMaxKbps`.
  - Idle-session expiry via `SessionTTL` and an explicit `Sweep(now)` method.
  - Caller-injected clock (`time.Time` arguments; no internal `time.Now`).
- Sentinel errors checkable via `errors.Is`.

### 2.2 Out of scope

- Hand-rolled STUN wire format. We import `github.com/pion/stun/v2` (pure-Go, MIT, no cgo) for encode/decode. The repo's "no third-party crypto" rule (root `CLAUDE.md` §1) does not apply — STUN is framing, not crypto.
- RFC 5389 `MESSAGE-INTEGRITY` / `FINGERPRINT` / `SOFTWARE` / long-term credentials. Available in pion but not used by the wrappers; the binding-request validator rejects messages that carry comprehension-required attrs we don't expect.
- RFC 5766 TURN (Allocate/Send/Data/ChannelBind methods, channel numbers, lifetime extensions). Tracker spec §10 leaves real TURN tied to the future bandwidth-credit concept; this module ships only the session-state primitives.
- The UDP read/write loops on `:3478` and `:3479`. Those live in `internal/server` (consistent with `registry` / `ledger/storage` having no networking).
- IPv4↔IPv6 candidate gathering / pairing (ICE-style). Out of scope for v1.
- Prometheus metrics. Listeners and RPC handlers in `internal/server` and `internal/api` emit metrics; `stunturn` is pure logic.
- Persistence. Allocator state is in-memory only; tracker restart drops every session (sessions naturally re-initiate on the next data-path setup).

## 3. Module boundary

### 3.1 Where stunturn fits

```
                consumer plugin                  seeder plugin
                      │                               │
                      │ TLS/QUIC control connection   │
                      ▼                               ▼
                ┌───────────────────────────────────────────────┐
                │              tracker process                   │
                │                                                │
                │   internal/server  (TLS + UDP listeners)       │
                │       │           │                            │
                │       ▼           ▼                            │
                │   internal/api    UDP read loops               │
                │       │           (:3478 STUN, :3479 TURN)     │
                │       ▼           │                            │
                │   internal/stunturn ◄─── pure functions ───────│
                │   (codec, reflect, allocator)                  │
                └───────────────────────────────────────────────┘
```

### 3.2 What lives where

| Concern | Module |
|---|---|
| `net.ListenUDP(":3478")`, read loop, `WriteToUDPAddrPort` | `internal/server` |
| `net.ListenUDP(":3479")`, read loop, peer-to-peer copy | `internal/server` |
| Decoding the inbound STUN packet, building the response bytes | **`internal/stunturn`** |
| Looking up TURN session by token, charging bytes, expiring idle | **`internal/stunturn`** |
| `stun_allocate()` and `turn_relay_open()` RPCs over the control connection | `internal/api` (calls into `stunturn.Reflect` / `Allocator.Allocate`) |
| Recording the reflexive address into the registry | `internal/server` (after `Reflect`, calls `registry.UpdateExternalAddr`) |
| Per-seeder reputation freezes that should tear down sessions | `internal/reputation` (calls `Allocator.Release`) |

`stunturn` imports nothing from other tracker modules. External deps: `github.com/pion/stun/v2` (codec only). Otherwise: stdlib (`net/netip`, `sync`, `time`, `io`, `errors`, `fmt`, `encoding/hex`) and `shared/ids` for `IdentityID`. The caller injects `crypto/rand.Reader` via `AllocatorConfig.Rand`; this package does not import `crypto/rand` directly.

## 4. Public surface

```go
package stunturn

// --- STUN wrappers around pion/stun/v2 (no state) ---

// DecodeBindingRequest unmarshals p via pion/stun, validates it is a
// STUN binding request, and returns the 12-byte transaction ID.
// Returns ErrInvalidPacket (with a wrapped underlying error from pion or
// from local validation) on any failure: malformed bytes, wrong message
// type, wrong magic cookie, or unknown comprehension-required attribute.
func DecodeBindingRequest(p []byte) (txID [12]byte, err error)

// EncodeBindingResponse returns wire-ready bytes for a STUN binding
// success response carrying exactly one XOR-MAPPED-ADDRESS attribute.
// Resulting length is 32 bytes for IPv4 and 44 bytes for IPv6. Panics
// if observed.IsValid() == false (caller responsibility; the listener
// already validated it).
func EncodeBindingResponse(txID [12]byte, observed netip.AddrPort) []byte

// --- Reflector (one-line wrapper for the listener path) ---

type ReflectResult struct {
    Observed netip.AddrPort
    Response []byte // wire-ready binding-response bytes (empty if observed invalid)
}

// Reflect returns a binding response payload for the observed address.
// Returns an empty Response when observed.IsValid() == false; caller
// should drop the packet.
func Reflect(txID [12]byte, observed netip.AddrPort) ReflectResult

// --- Allocator ---

type AllocatorConfig struct {
    MaxKbpsPerSeeder int           // > 0
    SessionTTL       time.Duration // > 0; idle-expiry threshold
    Now              func() time.Time
    Rand             io.Reader
}

type Allocator struct{ /* opaque */ }

// NewAllocator validates cfg and returns an empty allocator.
// Returns ErrInvalidConfig (with a wrapped reason) on any malformed field.
func NewAllocator(cfg AllocatorConfig) (*Allocator, error)

type Token [16]byte
func (t Token) String() string // hex; never special-cases zero

type Session struct {
    Token       Token
    SessionID   uint64
    ConsumerID  ids.IdentityID
    SeederID    ids.IdentityID
    RequestID   [16]byte
    AllocatedAt time.Time
    LastActive  time.Time
}

// Allocate creates a new TURN session. Returns ErrDuplicateRequest if
// requestID is already live; ErrRandFailed if the injected Rand fails.
func (a *Allocator) Allocate(consumer, seeder ids.IdentityID, requestID [16]byte, now time.Time) (Session, error)

// Resolve looks up a session without updating LastActive. Returns
// (Session{}, false) if the token is unknown. Returned Session is a copy.
func (a *Allocator) Resolve(tok Token, now time.Time) (Session, bool)

// ResolveAndCharge atomically (under one mutex acquisition) looks up the
// session, refreshes the per-seeder bucket, debits n bytes, updates
// LastActive, and returns the session. The hot path called per UDP datagram.
//
// Errors:
//   ErrUnknownToken     — token not in allocator (or expired and reaped)
//   ErrSessionExpired   — LastActive older than SessionTTL; entry deleted
//   ErrThrottled        — bucket has < n bytes available (NOT debited)
func (a *Allocator) ResolveAndCharge(tok Token, n int, now time.Time) (Session, error)

// Charge debits n bytes from the seeder's bucket without touching session
// state. n <= 0 is a no-op (returns nil). The seeder's bucket is lazy-
// initialized on first use; calling Charge for a seeder who has never had
// Allocate called succeeds.
func (a *Allocator) Charge(seederID ids.IdentityID, n int, now time.Time) error

// Release deletes the session by SessionID. Idempotent — a no-op if the
// session is unknown. The seeder's bucket is left in place (it is shared
// across that seeder's sessions and self-decays).
func (a *Allocator) Release(sessionID uint64)

// Sweep removes every session whose LastActive is older than SessionTTL.
// Returns the count removed. Intended to be called from a single goroutine
// on a periodic timer; safe for concurrent calls but wasteful.
func (a *Allocator) Sweep(now time.Time) int
```

### 4.1 Sentinel errors

```go
var (
    ErrInvalidConfig    = errors.New("stunturn: invalid allocator config")
    ErrInvalidPacket    = errors.New("stunturn: invalid stun packet")
    ErrUnknownToken     = errors.New("stunturn: unknown session token")
    ErrSessionExpired   = errors.New("stunturn: session expired")
    ErrThrottled        = errors.New("stunturn: seeder throttled")
    ErrDuplicateRequest = errors.New("stunturn: duplicate request id")
    ErrRandFailed       = errors.New("stunturn: token randomness failed")
)
```

`ErrInvalidConfig` is the only sentinel that wraps an underlying reason (`fmt.Errorf("%w: …", ErrInvalidConfig)`); hot-path errors are bare to keep the data path allocation-free.

## 5. Data structures

### 5.1 STUN message layout

We do not own the wire format. `pion/stun/v2` parses and emits RFC 5389 binding messages. Our wrappers only:

- Construct/inspect a `stun.Message` with `Type`, `TransactionID`, and one `stun.XORMappedAddress` attribute.
- Map `netip.AddrPort` ↔ pion's `(IP net.IP, Port int)` pair.
- Reject inbound messages whose `Type` is not `(Method=stun.MethodBinding, Class=stun.ClassRequest)` or which carry comprehension-required attrs we don't expect.

Reference: RFC 5389 §6 (header), §15.2 (XOR-MAPPED-ADDRESS), §7.3.1 (comprehension-required range `0x0000`–`0x7FFF`).

### 5.2 Allocator internal state

```go
type allocator struct {
    cfg AllocatorConfig

    mu      sync.Mutex
    nextID  uint64
    byToken map[Token]*sessionEntry
    bySID   map[uint64]*sessionEntry
    byReq   map[[16]byte]*sessionEntry
    buckets map[ids.IdentityID]*tokenBucket
}

type sessionEntry struct {
    session Session
}

type tokenBucket struct {
    capacityBytes float64 // = MaxKbpsPerSeeder * 1024 / 8
    refillPerSec  float64 // = capacityBytes (1s of burst)
    available     float64
    lastRefill    time.Time
}
```

Three indexes share the same `*sessionEntry` value. A panicking helper asserts all three stay in sync; mismatch means programmer error inside this package.

### 5.3 Token bucket

- `capacityBytes = MaxKbpsPerSeeder * 1024 / 8`. At default 1024 kbps this is 131,072 bytes.
- `refillPerSec = capacityBytes`. One full second of burst, refilled at the same steady-state rate.
- Refill is event-driven, not timer-driven: `available = min(capacity, available + elapsed * refillPerSec)`, computed at every `Charge` / `ResolveAndCharge`.
- Lazy init: `buckets[seederID]` is created on first Charge or first Allocate for that seeder; pre-existing buckets are reused across all that seeder's sessions.
- Cleanup: buckets are not swept. They are ~80 bytes each, keyed by stable `IdentityID`. If we ever need to evict, do it in v2 driven from registry deregistration.

## 6. Algorithms

### 6.1 Reflect

```
Reflect(txID, observed):
    if !observed.IsValid(): return ReflectResult{}
    return ReflectResult{Observed: observed, Response: EncodeBindingResponse(txID, observed)}
```

`EncodeBindingResponse` constructs a `stun.Message{Type: {Method: MethodBinding, Class: ClassSuccessResponse}, TransactionID: txID}`, calls `(&stun.XORMappedAddress{IP: observed.Addr().AsSlice(), Port: int(observed.Port())}).AddTo(m)`, calls `m.Encode()`, and returns `m.Raw`. The pion library handles the XOR-MAPPED-ADDRESS encoding (XORs against magic cookie for IPv4 and against `cookie || transaction_id` for IPv6 per RFC 5389 §15.2).

### 6.2 Allocate

```
mu.Lock(); defer mu.Unlock()
if _, dup := byReq[requestID]; dup: return Session{}, ErrDuplicateRequest
var tokBuf [16]byte
if _, err := io.ReadFull(cfg.Rand, tokBuf[:]); err != nil:
    return Session{}, fmt.Errorf("%w: %v", ErrRandFailed, err)
tok := Token(tokBuf)
nextID++
sid := nextID
entry := &sessionEntry{ session: Session{
    Token: tok, SessionID: sid,
    ConsumerID: consumer, SeederID: seeder,
    RequestID: requestID,
    AllocatedAt: now, LastActive: now,
}}
byToken[tok] = entry
bySID[sid]   = entry
byReq[requestID] = entry
ensureBucket(seeder, now)        // lazy init if absent
return entry.session, nil        // return a copy
```

If the random reader produces a token that already exists in `byToken` (collision), we return `ErrRandFailed`; with `crypto/rand` and a 16-byte token the probability is ~2^-128 and treating it as a hard error is simpler than retrying.

### 6.3 Resolve

Read-only. Single-mutex acquisition. Returns a copy of `Session` or `(Session{}, false)`. Does NOT touch `LastActive` and does NOT delete expired entries (caller asked for a peek, not liveness).

### 6.4 ResolveAndCharge

```
mu.Lock(); defer mu.Unlock()
entry, ok := byToken[tok]
if !ok: return Session{}, ErrUnknownToken
if now.Sub(entry.session.LastActive) > cfg.SessionTTL:
    deleteIndexes(entry)
    return Session{}, ErrSessionExpired
b := buckets[entry.session.SeederID]   // always present after Allocate
refill(b, now)
if n > 0 && b.available < float64(n): return Session{}, ErrThrottled
if n > 0: b.available -= float64(n)
entry.session.LastActive = now
return entry.session, nil
```

`n <= 0` skips the bucket check and debit but still updates `LastActive` (the listener saw a packet — the session is alive even if the framing was a 0-byte keepalive).

### 6.5 Charge

```
mu.Lock(); defer mu.Unlock()
if n <= 0: return nil
b := ensureBucket(seederID, now)
refill(b, now)
if b.available < float64(n): return ErrThrottled
b.available -= float64(n)
return nil
```

### 6.6 Release

```
mu.Lock(); defer mu.Unlock()
entry, ok := bySID[sessionID]
if !ok: return
deleteIndexes(entry)
```

`deleteIndexes` removes the entry from `byToken`, `bySID`, `byReq`. Idempotent and panic-free for missing keys.

### 6.7 Sweep

```
mu.Lock(); defer mu.Unlock()
n := 0
for tok, entry := range byToken:
    if now.Sub(entry.session.LastActive) > cfg.SessionTTL:
        deleteIndexes(entry); n++
return n
```

O(N) over live sessions. Not atomic with `Allocate` — a session created in the same window will be reaped on the next sweep, which is the intended GC behavior. Buckets untouched.

### 6.8 Token bucket refill

```
refill(b, now):
    elapsed := now.Sub(b.lastRefill).Seconds()
    if elapsed <= 0: return        // clock backwards / same instant: no-op
    b.available = min(b.capacityBytes, b.available + elapsed * b.refillPerSec)
    b.lastRefill = now
```

## 7. Concurrency model

### 7.1 Single mutex

Every public method on `*Allocator` holds a single `sync.Mutex`. Critical sections are O(1) map lookups + small arithmetic; ~hundreds of nanoseconds per call. Expected scale is ≤ 10² concurrent TURN sessions (≤ 20% of the brokered request mix per tracker spec §11) at ≤ 10⁴ packets/s in aggregate, which is < 1% of the contended-mutex regime on a single core. STUN binding requests don't touch the allocator — `Reflect` is stateless — so STUN traffic doesn't contend on this lock at all.

### 7.2 Documented upgrade path

If the allocator ever shows up under contention, shard by `IdentityID` (the seeder), embed the shard index in the high byte of `Token`, and walk shards in `Sweep`. This trades ~+150 LOC and a documented lock-ordering rule for parallel scaling. No work to do today; `doc.go` carries a forward reference.

## 8. Error semantics

### 8.1 Sentinel catalog

See §4.1. Every sentinel is exported and checked via `errors.Is`. Hot-path errors (`ErrThrottled`, `ErrUnknownToken`, `ErrSessionExpired`) are bare; `ErrInvalidConfig` is the only one that wraps a reason.

### 8.2 Caller behavior

| Caller call | Error | Listener / RPC handler should |
|---|---|---|
| `DecodeBindingRequest` | `ErrInvalidPacket` | Drop, no reply, increment metric. |
| `NewAllocator` | `ErrInvalidConfig` | Fail tracker startup; surface field reason. |
| `Allocate` | `ErrDuplicateRequest` | Look up existing session; treat as success. |
| `Allocate` | `ErrRandFailed` | Fail RPC; alert; randomness fault is a host issue. |
| `ResolveAndCharge` | `ErrUnknownToken` | Drop packet. |
| `ResolveAndCharge` | `ErrSessionExpired` | Drop packet; session already deleted by allocator. |
| `ResolveAndCharge` | `ErrThrottled` | Drop packet; rely on application-level rate adaptation. |
| `Resolve` | `(_, false)` | Same as `ErrUnknownToken`. |
| `Charge` | `ErrThrottled` | Drop packet (no session to clean up; bucket-only call). |

### 8.3 Caller-misuse contract

- Zero-value `IdentityID` for consumer/seeder is accepted (allocator does not validate identity content; broker did).
- `Charge(_, n, _)` with `n ≤ 0` is a no-op returning nil.
- `Release(unknownSID)` is a no-op.
- `Sweep` from multiple goroutines is correct but wasteful.
- Concurrent `Allocate(sameRequestID)` — exactly one returns nil; the rest get `ErrDuplicateRequest`.

### 8.4 No panics from the public API

Public methods return errors rather than panicking — except `EncodeBindingResponse` panics on an invalid `observed` (the listener already validated it; passing `netip.AddrPort{}` is a programmer error). Internal invariant violations panic loudly as tripwires.

## 9. Configuration

### 9.1 STUNTURNConfig fields

`MaxKbpsPerSeeder` is sourced from `config.STUNTURN.TURNRelayMaxKbps` (default 1024 kbps; see `tracker/internal/config/config.go`).

### 9.2 SessionTTL

Default: **30 seconds** of idle. Justification:

- Spec §5.3 lists `stream_idle_s` (60s) as the consumer-side detection of a broken stream. The TURN session should expire *before* that, so the listener can release allocator state before the broker times out the request.
- 30s is short enough that spam-allocated sessions self-clean within a sweep cycle; long enough that legitimate streaming requests with brief silences (e.g., model thinking time on a long Anthropic completion) don't get reaped.
- Configurable per tracker via the same `STUNTURNConfig` block. Adding a `session_ttl_seconds` field to `STUNTURNConfig` is part of this work.

## 10. Testing strategy

### 10.1 Codec wrappers

We do not re-test pion's RFC compliance. Wrapper tests verify only the glue:

- **Encode→pion decode round-trip.** Encode a binding response with our wrapper; parse the bytes with `stun.Message{}.UnmarshalBinary` and `XORMappedAddress.GetFrom`; assert recovered IP/port equals input. One IPv4, one IPv6.
- **Length sanity.** IPv4 response is 32 bytes; IPv6 response is 44 bytes; transaction ID round-trips.
- **Negative cases.** `DecodeBindingRequest` returns `ErrInvalidPacket` for: a 19-byte input (short header), a binding *response* (`ClassSuccessResponse`), bytes whose pion `UnmarshalBinary` fails, and a binding request carrying an unknown comprehension-required attribute (constructed via pion: add an attr with type `0x0001`).
- **Pion-encoded request is accepted.** Build a binding request via pion (`stun.Build(stun.NewTransactionIDSetter(txID), stun.BindingRequest)`), marshal, decode with our wrapper; assert `txID` matches.

### 10.2 Reflector
Pass-through tests; one per address family; one for invalid input.

### 10.3 Allocator
Construction failures (each invalid config field), happy-path `Allocate / Resolve / ResolveAndCharge / Release / Sweep`, dedupe-by-RequestID, lazy bucket init, refill math (including clock-backwards), idle expiry, post-Release lookup behavior.

### 10.4 Concurrency
Race-detector-gated stress: N goroutines × Allocate→RAC×K→Release; another goroutine Sweeping. Final state invariant: zero live sessions, no map corruption, no panics. Plus a `TestConcurrent_DuplicateRequestRace` that asserts exactly one winner.

### 10.5 Coverage and lint

- `go test -race -cover ./internal/stunturn/...` ≥ 90% per file.
- `golangci-lint run ./...` clean against the existing `.golangci.yml`.

## 11. Open questions

- **SessionTTL default.** §9.2 proposes 30s. Empirical work in the broker integration test will tell us whether that's right; revisit before the data-path plan lands.
- **Lazy bucket init for unknown seeders.** Current decision (§4): allowed. Revisit if the reputation subsystem exposes a "this identity is gone" hook that should hard-fail Charge to surface bugs sooner.
- **Token collision policy.** Current: hard `ErrRandFailed`. Probability ~2^-128 with `crypto/rand`. If we ever swap to a smaller token (e.g., 8 bytes) revisit.
- **TURN frame format.** Deferred to the data-path plan. The allocator's API (Token in / bytes out) doesn't constrain framing.

## 12. Acceptance criteria

- Module builds with no changes outside `tracker/internal/stunturn/`, `tracker/internal/config/` (adding `SessionTTLSeconds`), and the spec/plan docs.
- All tests pass under `go test -race` on the per-task quality gate.
- ≥ 90% line coverage per file.
- `golangci-lint run ./...` clean.
- Wrapper-emitted responses round-trip through `pion/stun/v2`'s decoder for IPv4 and IPv6 (§10.1).
- `DecodeBindingRequest` accepts pion-built binding requests and rejects the negative cases listed in §10.1.
- All exported symbols carry godoc comments.
- `doc.go` carries a one-paragraph forward reference to the §7.2 sharding upgrade path.
