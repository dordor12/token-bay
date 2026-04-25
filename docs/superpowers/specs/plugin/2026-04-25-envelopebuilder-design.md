# Plugin envelope + exhaustion-proof builders — Design Spec

**Status:** v1 — ready for implementation plan
**Audience:** contributors to `plugin/internal/`
**Depends on:** plugin spec §5.4 / §5.6, wire-format spec (`shared/2026-04-24-wire-format-v1-design.md`), exhaustion-proof spec
**Supersedes:** *(none — first spec for these packages)*

---

## 1. Goal

Define two sibling packages under `plugin/internal/` that, together, take the consumer-side fallback inputs (cached fallback ticket + the in-flight `/v1/messages` request + a tracker-issued balance snapshot) and produce a fully-validated, signed `*EnvelopeSigned` ready for the trackerclient to ship.

These are the two missing pieces between the wire-format types in `shared/` (now landed) and a real `NetworkRouter` that can actually call `tracker.broker_request`.

## 2. Scope

**In scope:**

- New package `plugin/internal/exhaustionproofbuilder` — assembles `*ExhaustionProofV1` from a typed `ProofInput` struct, validates via `ValidateProofV1`.
- New package `plugin/internal/envelopebuilder` — assembles `*EnvelopeSigned` from a typed `RequestSpec`, an `*ExhaustionProofV1`, and a `*SignedBalanceSnapshot`; validates body; signs via a `Signer` interface.
- Sentinel errors and `errors.Is`-discriminable error surface for both packages.
- Adapter conventions for the call site (ccproxy-network, future plan): how `EntryMetadata` becomes `ProofInput`; how the `/v1/messages` body becomes `RequestSpec`. Documented but not implemented here — this spec covers only the two builder packages.
- First plugin-side import of `shared/`: populates the dangling `replace` in `plugin/go.mod`.

**Out of scope (separate feature plans):**

- `plugin/internal/identity` — the production `Signer` implementation (key file, optional keychain). Tests use a fake.
- `plugin/internal/trackerclient` — the QUIC client that supplies the balance-snapshot cache + ships envelopes.
- `plugin/internal/ccproxy-network` (real `NetworkRouter`) — the request handler that parses `/v1/messages`, looks up session mode, calls both builders in order, and forwards the result.
- Body parsing of `/v1/messages` JSON (lives in ccproxy-network).
- Tracker-side validation of incoming envelopes.

## 3. Constraints

1. **No direct imports from peer plugin-internal packages.** Both builders import only `shared/...` + Go stdlib. They do NOT import `plugin/internal/ratelimit` or `plugin/internal/ccproxy`. The caller adapts those types into the builder's typed inputs.
2. **No I/O.** No file reads, no network calls, no stdout. Pure data transforms + a `Signer` callback.
3. **Validate-before-sign.** A signed envelope produced by this codebase MUST pass `ValidateEnvelopeBody`; a proof produced by this codebase MUST pass `ValidateProofV1`. The builders enforce this internally — callers cannot opt out.
4. **Determinism for tests.** Time and randomness sources are injectable function-typed fields; production uses `time.Now` and `crypto/rand.Read`.
5. **Concurrency-safe by construction.** Builders hold no mutable per-request state. Long-lived deps (`Signer`, `Now`, `RandRead`) are read-only. The sidecar instantiates each Builder once at startup and reuses it across requests.

## 4. Architecture

### 4.1 Two peers, not a chain

Both builders sit at the same layer of the dependency graph. Neither imports the other. The caller composes them:

```go
proof, err := proofBuilder.Build(proofIn)        // step A
if err != nil { return wrapAnthropicError(err) }
env, err := envBuilder.Build(spec, proof, balance)  // step B
if err != nil { return wrapAnthropicError(err) }
```

Why peers and not envelope-builds-proof: the two have different inputs (a fallback ticket vs. an HTTP request + balance snapshot), different validators, and live in separate proto packages. Peer composition keeps each unit testable in isolation and avoids the envelope-builder having to know what a `StopFailurePayload` is.

### 4.2 Validate-before-sign

Both `shared/proto.ValidateEnvelopeBody` and `shared/exhaustionproof.ValidateProofV1` already exist and are exhaustive. The builders run them after assembly and before any signing. A malformed envelope therefore cannot leave the plugin under a valid signature — the consumer's identity cannot accidentally vouch for invalid data. Failure surfaces as `ErrValidation` wrapping the validator's structured error.

### 4.3 `Signer` interface — not a private key

`envelopebuilder` does not import `shared/signing`. It receives a `Signer` interface that owns Ed25519. The production implementation lives in the future `internal/identity` package and uses `shared/signing.SignEnvelope` internally; tests use a fake. This decouples the builder from the keying mechanism (file vs. OS keychain vs. hardware) and lets builder tests run without touching keys.

### 4.4 Adapter pattern at the call site

The builders take typed input structs (`ProofInput`, `RequestSpec`) defined in their own packages. The caller (future ccproxy-network) translates from its own types:

- `EntryMetadata + /usage probe timestamp → ProofInput` — small adapter function in ccproxy-network.
- `*http.Request + body bytes + config → RequestSpec` — same.

This inverts the natural direction (caller depends on builder; builder is unaware of caller) and breaks any potential cycle.

## 5. Package layout

```
plugin/internal/
  exhaustionproofbuilder/
    exhaustionproofbuilder.go       -- ProofInput, Builder, NewBuilder, Build()
    exhaustionproofbuilder_test.go
    doc.go                          -- package doc

  envelopebuilder/
    envelopebuilder.go              -- RequestSpec, Signer, Builder, NewBuilder, Build()
    envelopebuilder_test.go
    doc.go
```

Each package is single-file production code at this size (target ~150–200 lines each). If either grows past ~300 lines, split into `builder.go` + `types.go` + `errors.go` at that point.

## 6. Public API

### 6.1 `exhaustionproofbuilder`

```go
package exhaustionproofbuilder

import (
    "time"

    "github.com/token-bay/token-bay/shared/exhaustionproof"
)

// ProofInput carries the raw materials the proof needs. Caller (ccproxy-network)
// adapts EntryMetadata + the /usage probe timestamp into this struct.
type ProofInput struct {
    StopFailureMatcher    string    // expected: "rate_limit"
    StopFailureAt         time.Time
    StopFailureErrorShape []byte    // raw JSON from StopFailureHookInput
    UsageProbeAt          time.Time
    UsageProbeOutput      []byte    // raw PTY-captured /usage bytes
}

// Builder is stateless beyond its long-lived deps. Safe for concurrent use.
type Builder struct {
    Now      func() time.Time              // default: time.Now
    RandRead func([]byte) (int, error)     // default: crypto/rand.Read
}

// NewBuilder returns a Builder with production defaults.
func NewBuilder() *Builder

// Build assembles, validates, and returns a *ExhaustionProofV1.
//
// Errors (errors.Is-discriminable):
//   - ErrInvalidInput: required ProofInput field empty/zero (wraps a field name)
//   - ErrRandFailed:   RandRead returned an error
//   - ErrValidation:   ValidateProofV1 rejected the assembled message
func (b *Builder) Build(in ProofInput) (*exhaustionproof.ExhaustionProofV1, error)
```

Build sequence:
1. Reject obviously-empty fields up front (`StopFailureMatcher == ""`, `StopFailureAt.IsZero()`, `UsageProbeAt.IsZero()`, `len(StopFailureErrorShape) == 0`, `len(UsageProbeOutput) == 0`) → `ErrInvalidInput`. Policy note: `ValidateProofV1` does not check the byte-length of `error_shape` / `output`; the builder enforces non-empty on those defensively because in normal operation they are always populated by the StopFailure hook payload and the PTY probe. An empty byte field signals a caller bug.
2. Generate `nonce = make([]byte, 16)` via `RandRead`. On error → `ErrRandFailed`.
3. Assemble `*ExhaustionProofV1` with `captured_at = Now().Unix()`.
4. `ValidateProofV1(proof)` → on error, wrap as `ErrValidation`.
5. Return the validated proof.

### 6.2 `envelopebuilder`

```go
package envelopebuilder

import (
    "time"

    "github.com/token-bay/token-bay/shared/exhaustionproof"
    "github.com/token-bay/token-bay/shared/ids"
    tbproto "github.com/token-bay/token-bay/shared/proto"
)

// RequestSpec is the per-request slice of the envelope, supplied by the caller
// after parsing /v1/messages and resolving the privacy tier from config.
type RequestSpec struct {
    Model           string
    MaxInputTokens  uint64
    MaxOutputTokens uint64
    Tier            tbproto.PrivacyTier
    BodyHash        []byte               // SHA-256, len 32
}

// Signer abstracts the consumer's identity. Production impl lives in
// plugin/internal/identity; tests use a fake. Concurrency: implementations
// must be safe for concurrent use (the builder calls Sign per-request).
type Signer interface {
    Sign(body *tbproto.EnvelopeBody) ([]byte, error)
    IdentityID() ids.IdentityID
}

// Builder is stateless beyond its long-lived deps. Safe for concurrent use.
type Builder struct {
    Signer   Signer                          // required; NewBuilder rejects nil
    Now      func() time.Time                // default: time.Now
    RandRead func([]byte) (int, error)       // default: crypto/rand.Read
}

// NewBuilder returns a Builder with production defaults. Panics on nil Signer.
// Asymmetric with the package's general "fail closed, no panics" rule by
// design: a nil Signer at construction is a programmer error caught at
// startup, never a runtime condition. All Build()-time error paths still fail
// closed via sentinels.
func NewBuilder(s Signer) *Builder

// Build assembles, validates, signs, and returns *EnvelopeSigned.
//
// Errors (errors.Is-discriminable):
//   - ErrNilProof:     proof argument is nil
//   - ErrNilBalance:   balance argument is nil
//   - ErrInvalidSpec:  RequestSpec field invalid (wraps a field name)
//   - ErrRandFailed:   RandRead returned an error
//   - ErrValidation:   ValidateEnvelopeBody rejected the assembled message
//   - ErrSign:         Signer.Sign returned an error
func (b *Builder) Build(
    spec RequestSpec,
    proof *exhaustionproof.ExhaustionProofV1,
    balance *tbproto.SignedBalanceSnapshot,
) (*tbproto.EnvelopeSigned, error)
```

Build sequence:
1. Reject `proof == nil` → `ErrNilProof`; `balance == nil` → `ErrNilBalance`.
2. Reject obviously-bad `spec` fields up front (empty `Model`, `len(BodyHash) != 32`, `Tier == PRIVACY_TIER_UNSPECIFIED`) → `ErrInvalidSpec`.
3. Generate `nonce` via `RandRead`. On error → `ErrRandFailed`.
4. Assemble `EnvelopeBody`:
   - `protocol_version = uint32(tbproto.ProtocolVersion)`
   - `consumer_id = b.Signer.IdentityID().Bytes()[:]`
   - spec fields verbatim
   - `exhaustion_proof = proof`, `balance_proof = balance`
   - `captured_at = Now().Unix()`, `nonce = nonce`
5. `ValidateEnvelopeBody(body)` → on error, wrap as `ErrValidation`.
6. `sig, err := b.Signer.Sign(body)` → on error, wrap as `ErrSign`.
7. Return `&EnvelopeSigned{Body: body, ConsumerSig: sig}`.

### 6.3 Sentinel errors

Each package defines its sentinels at the top of its main file. Example, `exhaustionproofbuilder`:

```go
var (
    ErrInvalidInput = errors.New("exhaustionproofbuilder: invalid input")
    ErrRandFailed   = errors.New("exhaustionproofbuilder: random read failed")
    ErrValidation   = errors.New("exhaustionproofbuilder: validation failed")
)
```

`envelopebuilder` exports `ErrNilProof`, `ErrNilBalance`, `ErrInvalidSpec`, `ErrRandFailed`, `ErrValidation`, `ErrSign` with the same `package: detail` message convention.

Internal wrapping uses `fmt.Errorf("%w: <field-or-detail>", ErrX)`. Callers discriminate via `errors.Is(err, ErrX)`.

## 7. Data flow

End-to-end trace, plugin-side, from "/v1/messages arrives at ccproxy" through "envelope ready to ship":

```
Claude Code  ──POST /v1/messages──▶  ccproxy.Server (network mode)
                                         │
                                         ▼
                          ┌──────────────────────────────┐
                          │  ccproxy request handler     │
                          │  (future: NetworkRouter)     │
                          └──────────────────────────────┘
                                         │
              ┌──────────────────────────┴──────────────────────────┐
              │                                                     │
              ▼                                                     ▼
     parse /v1/messages JSON                        SessionModeStore.GetMode(sid)
     compute body_hash = SHA-256(raw)                       │
     resolve tier (config / per-request)                    ▼
              │                                    *EntryMetadata (ticket)
              │                                             │
              ▼                                             ▼
        RequestSpec                            ProofInput (adapter fn)
              │                                             │
              │                                             ▼
              │                          exhaustionproofbuilder.Build(in)
              │                                             │
              │                                             │ ValidateProofV1
              │                                             ▼
              │                                  *ExhaustionProofV1 (validated)
              │       ┌─────────────────────────────────────┘
              │       │
              ▼       ▼
     envelopebuilder.Build(spec, proof, balance)
        │       │
        │       │   *SignedBalanceSnapshot   ◀── trackerclient cache (future)
        │       │
        │       ▼
        │   assemble EnvelopeBody (consumer_id from Signer.IdentityID,
        │                          captured_at = Now, nonce = 16 random)
        │       │
        │       ▼
        │   ValidateEnvelopeBody
        │       │
        │       ▼
        │   Signer.Sign(body)  ──▶ identity.Signer (future) ──▶ ed25519
        │       │
        ▼       ▼
   *EnvelopeSigned  ──▶ trackerclient.BrokerRequest (future plan)
```

`Build()` is called once per request. `Now` and `RandRead` fire at one point each per builder invocation. No re-entrancy on the same Builder for one request. Multiple concurrent requests instantiate distinct local `EnvelopeBody` values; the only shared state (`Signer`, `Now`, `RandRead`) is read-only.

`envelope.nonce` and `proof.nonce` are independent 16-byte values from independent `RandRead` calls (one per builder). The two messages also have independent `captured_at` timestamps — the proof's bounds when the rate-limit evidence was assembled, the envelope's bounds when the broker-request was built. Per wire-format spec §4.3, this is intentional: they can legitimately differ (user consent, retries) and the tracker validates each independently.

### 7.1 Failure surfaces in the caller

The future ccproxy-network handler translates builder errors into Anthropic-shaped HTTP error responses (per plugin spec §5.4 step 4 / step 10):

| Error class | HTTP status | Reasoning |
|---|---|---|
| `ErrInvalidInput` / `ErrInvalidSpec` / `ErrValidation` | 502 | Internal bug — ccproxy fed malformed data. Should never reach prod; fail loud. |
| `ErrNilProof` | 502 | Same — ticket cache invariant violated. |
| `ErrNilBalance` | 503 | Tracker-balance cache stale or empty. Hint: "tracker-balance-stale". |
| `ErrRandFailed` | 500 | Kernel/OS entropy source failure — exotic, treat as machine-level fault. |
| `ErrSign` | 500 | Identity backend unavailable (keychain locked, file unreadable). |

Mapping is the caller's responsibility, not the builder's.

## 8. Testing

Mirrors `shared/CLAUDE.md` patterns: construct → canonical-bytes → equality; round-trip; rejection of tampered/invalid input. Tests live in the same directory as the code, as `*_test.go` files.

### 8.1 `exhaustionproofbuilder` test inventory

1. **Happy path.** Build with a fully-populated `ProofInput`, fixed `Now`, deterministic `RandRead` returning 16 zero bytes. Assert returned proof matches expectations field-by-field and passes `ValidateProofV1` independently.
2. **Validation-rejection matrix** — table-driven, one row per rule in `ValidateProofV1`:
   - `StopFailureMatcher != "rate_limit"`
   - `StopFailureAt` zero
   - `UsageProbeAt` zero
   - `|StopFailureAt − UsageProbeAt| > 60s`
   - `StopFailureErrorShape` nil
   - `UsageProbeOutput` nil
   Each asserts `errors.Is(err, ErrValidation)` (or `ErrInvalidInput` for the up-front cases) and that the wrapped error names the offending field.
3. **`RandRead` failure** — inject a `RandRead` returning an error. Assert `errors.Is(err, ErrRandFailed)`; no proof returned.
4. **Determinism check.** Same inputs + same fixed `Now` and `RandRead` produce byte-identical `proto.MarshalOptions{Deterministic: true}.Marshal(proof)` across two `Build()` calls.

### 8.2 `envelopebuilder` test inventory

1. **Happy path.** Construct with a fake `Signer` (returns a known 64-byte sig + fixed `IdentityID`), fixed `Now`, deterministic `RandRead`. Build with a valid `RequestSpec` + a hand-constructed `*ExhaustionProofV1` (passing `ValidateProofV1`) + a hand-constructed `*SignedBalanceSnapshot`. Assert returned envelope's body has the expected fields, `ConsumerSig == fakeSigner.lastSig`, and body passes `ValidateEnvelopeBody`.
2. **Round-trip + signature verify.** Use a real-Ed25519 test Signer that wraps `signing.SignEnvelope` with a generated keypair. Build → `proto.MarshalOptions{Deterministic: true}.Marshal` → `proto.Unmarshal` → `signing.VerifyEnvelope(pub, signed)` returns true.
3. **Tamper detection.** Build → flip one byte in `Body.Model` → `VerifyEnvelope` returns false. Sanity check that the test Signer is doing real Ed25519, not mock-stamping.
4. **Nil/invalid-input matrix** — one row each: `proof` nil; `balance` nil; `spec.BodyHash` wrong length; `spec.Model` empty; `spec.Tier == PRIVACY_TIER_UNSPECIFIED`. Each asserts a specific sentinel via `errors.Is`.
5. **Signer failure.** Fake Signer returns an error from `Sign`. Assert `errors.Is(err, ErrSign)` and no envelope returned.
6. **`RandRead` failure.** Inject a `RandRead` returning an error. Assert `errors.Is(err, ErrRandFailed)`; `Signer.Sign` is never called (assert via the fake Signer's call counter).
7. **Determinism check.** Same inputs (including same fixed `Now` output and `RandRead` byte sequence), two `Build()` calls → byte-identical `DeterministicMarshal(envelope.Body)` output. Tripwire for accidental non-determinism in field ordering.
8. **Concurrency check.** 50 goroutines call `Build()` on the same Builder with distinct inputs and distinct fake-Signer responses; all complete without races (`-race`) and produce distinct envelopes.

Test fakes (a `fakeSigner` struct with controllable behavior, a deterministic `RandRead`) live in `envelopebuilder_test.go` in the same package — no need for an `internal/testing` package at this size.

### 8.3 Coverage target

≥ 90% line coverage in both packages — same bar as `shared/`. Reasonable because both are short pure-ish helpers with explicit error branches.

### 8.4 Test fixtures

No on-disk golden hex for these packages. The wire-format plan (`shared/proto/testdata/envelope_signed.golden.hex`) already owns the cross-version `EnvelopeSigned` byte tripwire. The Builders' tests use `proto.Equal` and field-level assertions. If a future divergence needs catching that the shared/proto golden doesn't cover, we revisit.

## 9. Impact on existing files

- `plugin/go.mod` — adds first `require github.com/token-bay/token-bay/shared` (the `replace` directive is already present and dangling per the comment in `go.mod`). `go work sync` from repo root.
- `plugin/internal/envelopebuilder/` — currently empty; populated.
- `plugin/internal/exhaustionproofbuilder/` — currently empty; populated.
- No changes to `shared/`, `tracker/`, or any other plugin-internal package.

## 10. Open questions

- **Should `RequestSpec` carry the raw body for downstream debug logging?** Currently no — `BodyHash` is sufficient for the envelope. Re-visit if the audit-log plan wants the raw body and discovers it's no longer in scope by the time it gets called.
- **Should the proof builder accept `time.Time` zero as a sentinel for "use Now()"?** Currently no — caller is expected to pass actual timestamps from the ticket, and a zero is a programmer error caught by `ErrInvalidInput`. Cleaner contract; revisit if the call-site adapter ends up making this awkward.
- **Should `Signer` expose the public key, not just the IdentityID hash?** The envelope only carries `consumer_id` (the hash). If a future spec needs the raw pubkey on the wire (e.g., for a key-discovery RPC), the interface grows; for now, `IdentityID()` is enough.

## 11. Future work

- **`internal/identity` Signer implementation** — file-backed by default, optional OS keychain. Separate plan; trivial to wire once it lands.
- **`internal/trackerclient`** — owns the balance-snapshot cache. The cache passes a `*SignedBalanceSnapshot` into the envelope builder per-request.
- **`internal/ccproxy-network`** — the real `NetworkRouter` that calls both builders in order. The end-to-end `/v1/messages` → broker-request happy path lights up when this lands.
- **Audit-log integration** — both builders are silent today; the audit-log spec may want to log build outcomes. Trivial post-hoc addition (caller logs after `Build()` returns).

## 12. Acceptance criteria

- [ ] `plugin/internal/exhaustionproofbuilder` package exists with the public API in §6.1.
- [ ] `plugin/internal/envelopebuilder` package exists with the public API in §6.2.
- [ ] All sentinel errors in §6.3 are exported and `errors.Is`-discriminable.
- [ ] Test inventory in §8.1 and §8.2 passes under `go test -race`.
- [ ] Both packages ≥ 90% line coverage.
- [ ] `make -C plugin test` passes.
- [ ] `make -C plugin lint` passes.
- [ ] `plugin/go.mod` has a real `require` line for `shared/`; `go work sync` is clean.
- [ ] Neither package imports any other `plugin/internal/...` package.
