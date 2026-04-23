# Wire-format v1 Design

**Status:** v1 — ready for implementation plan
**Audience:** contributors to `shared/`, `plugin/`, `tracker/`
**Depends on:** architecture spec §2.1 / §7.2, ledger spec §3.4
**Supersedes:** *(none — first concrete schema definition)*

---

## 1. Goal

Define the protobuf wire format for the `broker_request` envelope and its associated signed payloads — the minimum concrete schema the plugin and tracker both need before any of the following can be built:

- `plugin/internal/envelopebuilder` (assembles envelope from ccproxy state)
- `plugin/internal/trackerclient` (sends envelopes over QUIC)
- `tracker/internal/broker` (parses envelopes, validates signatures, brokers requests)

## 2. Scope

**In scope:**

- `shared/exhaustionproof.ProofV1` — typed protobuf message replacing the current package-doc-only stub.
- `shared/proto.EnvelopeBody` + `shared/proto.EnvelopeSigned` — two-message split for Ed25519 signing.
- `shared/proto.BalanceSnapshotBody` + `shared/proto.SignedBalanceSnapshot` — tracker-signed balance proof (§3.4 of the ledger spec), included because it is a required field of every envelope.
- `shared/signing` helpers: `DeterministicMarshal`, `SignEnvelope` / `VerifyEnvelope`, `SignBalanceSnapshot` / `VerifyBalanceSnapshot`.
- Validation helpers (`ValidateEnvelopeBody`, `ValidateProofV1`) that enforce length/required-field invariants before sign-and-send / after parse-and-receive.
- `shared/CLAUDE.md` amendment relaxing the "stdlib + testify only" rule to permit `google.golang.org/protobuf`.
- Architecture spec §7.2 amendment removing `ExhaustionProofV1.consumer_sig` (single-sig via enclosing envelope).

**Out of scope (deferred to separate feature plans):**

- `plugin/internal/envelopebuilder` — assembles `EnvelopeBody` from `ccproxy.EntryMetadata` + identity key.
- Tracker-side validation pipeline (replay window, balance-freshness check, proof-freshness check).
- QUIC transport and RPC framing.
- Ledger's internal storage format (this spec only governs the signed snapshot presented to consumers).
- Seeder-side envelope parsing (seeders never see envelopes directly; they receive `offer(consumer_id, envelope_hash, terms)` from the tracker).

## 3. Constraints

1. **Signed bytes must be byte-identical** across consumer and tracker. Any non-determinism in serialization breaks signature verification.
2. **`shared/` remains a leaf module** — no imports from `plugin/` or `tracker/`; no I/O; pure types + pure functions.
3. **No third-party crypto.** Ed25519 via stdlib.
4. **Tech stack addition:** `google.golang.org/protobuf` is added as the single permitted third-party runtime dependency of `shared/`.
5. **Generated `.pb.go` files are committed.** `make build` must work without `protoc` installed. Schema regeneration is gated behind a `proto-gen` target and enforced by a CI diff check.

## 4. Architecture

### 4.1 Determinism via a single choke point

`google.golang.org/protobuf` produces byte-stable output when marshaling with `proto.MarshalOptions{Deterministic: true}` — fields emitted in ascending tag order, map keys sorted. This is "deterministic given the same library version + schema" and is the v1 canonicalization strategy.

All code that signs a proto message must go through one helper:

```go
func DeterministicMarshal(m proto.Message) ([]byte, error) {
    return proto.MarshalOptions{Deterministic: true}.Marshal(m)
}
```

This makes accidentally-non-deterministic marshals a grep-able policy violation rather than a latent bug.

A byte-stable **golden fixture test** (§7.2) is the tripwire for accidental library upgrades that break determinism.

### 4.2 Two-message split for signing

A Go struct that holds both the signed data and the signature over it creates a chicken-and-egg problem. The pattern adopted throughout:

```proto
message EnvelopeBody {
  // all signed fields
}
message EnvelopeSigned {
  EnvelopeBody body        = 1;
  bytes        consumer_sig = 2;
}
```

- Sender: `sig = ed25519.Sign(priv, DeterministicMarshal(body))`; ship `{body, sig}`.
- Receiver: `ed25519.Verify(pub, DeterministicMarshal(signed.body), signed.consumer_sig)`.

Same pattern applied to `SignedBalanceSnapshot` (tracker signs `BalanceSnapshotBody`).

This is the single canonical signing discipline for all shared-proto messages. New signed messages added in future specs MUST follow the same split.

### 4.3 Single signature, not two

Architecture spec §7.2 originally specified `ExhaustionProofV1.consumer_sig` — a signature over the proof fields, independent of the enclosing envelope's signature. This spec removes that field: the proof is only ever transmitted inside an envelope in this architecture, and the envelope's `consumer_sig` already covers the proof's bytes. The double-sig bought only standalone proof verification, which no use case in this architecture exercises.

Architecture spec §7.2 will be updated in the same commit chain. Both messages retain distinct `captured_at` + `nonce` fields: the proof's pair bounds when the rate-limit evidence was assembled (freshness of evidence), the envelope's pair bounds when the broker-request was built (replay protection on the broker call). They can legitimately differ — user consent, retries — so the tracker validates both independently against its wall clock.

## 5. Schemas

### 5.1 `shared/exhaustionproof/proof.proto`

```proto
syntax = "proto3";
package tokenbay.exhaustionproof.v1;

option go_package = "github.com/token-bay/token-bay/shared/exhaustionproof";

message ExhaustionProofV1 {
  StopFailure stop_failure = 1;
  UsageProbe  usage_probe  = 2;
  uint64      captured_at  = 3;  // unix seconds
  bytes       nonce        = 4;  // 16 random bytes
}

message StopFailure {
  string matcher     = 1;  // v1: MUST be "rate_limit"
  uint64 at          = 2;  // unix seconds
  bytes  error_shape = 3;  // raw JSON from Claude Code's StopFailureHookInput; verbatim
}

message UsageProbe {
  uint64 at     = 1;  // unix seconds (when `claude /usage` was executed)
  bytes  output = 2;  // raw bytes of `claude /usage` TUI output (PTY-captured)
}
```

Constants in the existing `exhaustionproof.go` remain:

```go
const (
    VersionV1 uint8 = 1
    VersionV2 uint8 = 2
)
```

### 5.2 `shared/proto/balance.proto`

```proto
syntax = "proto3";
package tokenbay.proto.v1;

option go_package = "github.com/token-bay/token-bay/shared/proto";

message BalanceSnapshotBody {
  bytes  identity_id    = 1;  // 32
  int64  credits        = 2;
  bytes  chain_tip_hash = 3;  // 32
  uint64 chain_tip_seq  = 4;
  uint64 issued_at      = 5;  // unix seconds
  uint64 expires_at     = 6;  // issued_at + 600 (ledger spec §3.4)
}

message SignedBalanceSnapshot {
  BalanceSnapshotBody body        = 1;
  bytes               tracker_sig = 2;  // 64
}
```

### 5.3 `shared/proto/envelope.proto`

```proto
syntax = "proto3";
package tokenbay.proto.v1;

option go_package = "github.com/token-bay/token-bay/shared/proto";

import "exhaustionproof/proof.proto";
import "proto/balance.proto";

enum PrivacyTier {
  PRIVACY_TIER_UNSPECIFIED = 0;
  PRIVACY_TIER_STANDARD    = 1;
  PRIVACY_TIER_TEE         = 2;
}

message EnvelopeBody {
  uint32 protocol_version  = 1;   // matches shared/proto.ProtocolVersion
  bytes  consumer_id       = 2;   // 32 — Ed25519 pubkey hash / IdentityID
  string model             = 3;   // e.g., "claude-sonnet-4-6"
  uint64 max_input_tokens  = 4;
  uint64 max_output_tokens = 5;
  PrivacyTier tier         = 6;
  bytes  body_hash         = 7;   // 32 — SHA-256 of full Anthropic /v1/messages body

  tokenbay.exhaustionproof.v1.ExhaustionProofV1 exhaustion_proof = 8;
  SignedBalanceSnapshot balance_proof = 9;

  uint64 captured_at = 10;        // unix seconds
  bytes  nonce       = 11;        // 16 random bytes — replay protection
}

message EnvelopeSigned {
  EnvelopeBody body         = 1;
  bytes        consumer_sig = 2;  // 64 — Ed25519 over DeterministicMarshal(body)
}
```

Existing `proto.go` keeps `ProtocolVersion uint16 = 1`; the proto field is `uint32` because protobuf lacks a uint16 type. `ValidateEnvelopeBody` (§6) checks `b.ProtocolVersion == uint32(ProtocolVersion)`.

### 5.4 Protobuf-package vs. Go-package naming

- Protobuf package `tokenbay.proto.v1` — versioned on the wire so a future breaking change can ship as `tokenbay.proto.v2` alongside `v1`.
- Go package `shared/proto` — unversioned; Go-side schema evolution uses renamed types (`EnvelopeBodyV2`) or a new Go package.

## 6. Sign/verify helpers (`shared/signing`)

All new helpers added to `shared/signing`:

```go
// DeterministicMarshal — see §4.1. The canonical entry point for "bytes to sign".
func DeterministicMarshal(m proto.Message) ([]byte, error)

// SignEnvelope returns the Ed25519 signature of DeterministicMarshal(body).
// Pre-condition: ValidateEnvelopeBody(body) == nil. Returns error on nil body or
// wrong-length private key.
func SignEnvelope(priv ed25519.PrivateKey, body *proto.EnvelopeBody) ([]byte, error)

// VerifyEnvelope reports true iff signed.ConsumerSig is a valid Ed25519 signature
// under pub over DeterministicMarshal(signed.Body). Nil inputs, wrong-length keys
// or sigs, or nil body all return false without panic.
func VerifyEnvelope(pub ed25519.PublicKey, signed *proto.EnvelopeSigned) bool

// Parallel helpers for SignedBalanceSnapshot (tracker signs; consumer verifies).
func SignBalanceSnapshot(priv ed25519.PrivateKey, body *proto.BalanceSnapshotBody) ([]byte, error)
func VerifyBalanceSnapshot(pub ed25519.PublicKey, signed *proto.SignedBalanceSnapshot) bool
```

Each helper fails closed — no panics on malformed input, all error paths return a descriptive error or `false`.

Validation helpers live alongside the proto-generated code:

```go
// package shared/proto
func ValidateEnvelopeBody(b *EnvelopeBody) error
// package shared/exhaustionproof
func ValidateProofV1(p *ExhaustionProofV1) error
```

Validation enforced:

- `EnvelopeBody`: `protocol_version == 1`; `len(consumer_id) == 32`; `model` non-empty; `body_hash` length 32; `exhaustion_proof` non-nil; `balance_proof` non-nil; `captured_at` non-zero; `len(nonce) == 16`; tier is one of the defined enum values (not `UNSPECIFIED`).
- `ProofV1`: `stop_failure` and `usage_probe` both non-nil; `stop_failure.matcher == "rate_limit"`; `stop_failure.at` and `usage_probe.at` non-zero; `|stop_failure.at − usage_probe.at| ≤ 60` (two-signal freshness bound); `len(nonce) == 16`; `captured_at` non-zero.

Both sender and receiver call the validator before signing / after parsing. Tracker rejects any envelope that fails validation with a structured error code (`INVALID_ENVELOPE_BODY`, `INVALID_EXHAUSTION_PROOF`) — specific codes are the tracker broker spec's concern, not this one.

## 7. Testing

Every new package gets a test file. Every helper has a test. Per `shared/CLAUDE.md`, tests follow three shapes: construct → canonical-bytes → equality; round-trip serialization; rejection of tampered input.

### 7.1 Required tests

1. **Round-trip per message.** For each of `EnvelopeBody`, `EnvelopeSigned`, `BalanceSnapshotBody`, `SignedBalanceSnapshot`, `ExhaustionProofV1`: build a populated instance, `DeterministicMarshal` → `proto.Unmarshal` → `DeterministicMarshal`, assert bytes equal the first marshal.

2. **Byte-stable golden fixture** (`shared/proto/testdata/envelope_signed.golden.hex`). A hand-crafted `EnvelopeBody` populated with deterministic values (fixed nonce, fixed timestamps, fixture Ed25519 key from `testdata/fixture_keypair.bin`) produces a specific serialized hex string. The test compares the current marshal against the committed hex; any drift fails CI. This is the tripwire for library upgrades that break determinism.

3. **Sign / verify.** Sign a valid envelope → verify → succeed. Flip a bit in body (wrong `model`, wrong `nonce`, wrong `body_hash`) → verify → fail. Flip a bit in sig → verify → fail. Swap pubkey → verify → fail.

4. **Validation-rejection matrix.** Table-driven test with one case per validation rule in §6: empty `consumer_id`, wrong-length `nonce`, missing `exhaustion_proof`, `stop_failure.matcher != "rate_limit"`, `usage_probe.at` more than 60s after `stop_failure.at`, etc. Each case asserts the specific error is returned.

5. **Nil-safety.** `SignEnvelope(priv, nil)` returns an error (no panic). `VerifyEnvelope(pub, nil)` returns false. `VerifyEnvelope(pub, &EnvelopeSigned{Body: nil})` returns false.

### 7.2 Golden fixture refresh protocol

When the schema legitimately changes (new field added, new version):

1. Run `make -C shared proto-gen` to regenerate `.pb.go`.
2. Update the test harness that produces the golden bytes.
3. Run the golden test with `UPDATE_GOLDEN=1` env var to rewrite `testdata/envelope_signed.golden.hex`.
4. Commit both the proto change and the updated fixture in the same commit.

Reviewing the fixture change is the human checkpoint that confirms "the bytes changed the way we expected, and not in some other way."

### 7.3 Coverage target

`shared/proto`, `shared/exhaustionproof`, `shared/signing` all ≥ 90% line coverage. These are pure types + short helpers; anything less signals an untested error branch.

## 8. Impact on existing files

- `shared/proto/proto.go` — retains `ProtocolVersion uint16 = 1` and the package doc; `.pb.go` files added next to it.
- `shared/proto/proto_test.go` — existing test (verifies `ProtocolVersion == 1`) kept; envelope + balance tests added as separate files.
- `shared/exhaustionproof/exhaustionproof.go` — retains `VersionV1` / `VersionV2` constants and the package doc; `.pb.go` added.
- `shared/signing/ed25519.go` — existing `Verify` helper unchanged; new helpers in `proto.go`.
- `shared/go.mod` — adds `google.golang.org/protobuf` dependency.
- `shared/Makefile` — adds `proto-gen` target (requires `protoc` + `protoc-gen-go` on contributor's machine when regenerating).
- Top-level `Makefile` / CI — adds `make proto-check` that runs `proto-gen` and fails if there's a diff.

## 9. Spec amendments shipped by this plan

Two upstream-spec corrections, each a one-line amendment:

1. **Architecture spec §7.2** — remove `consumer_sig: bytes64` from the `ExhaustionProofV1` field list. Add a note: *"ExhaustionProofV1 is signed by the enclosing `EnvelopeSigned.consumer_sig`; it has no standalone signature."*

2. **`shared/CLAUDE.md` rule: "Tech stack."** Change *"Just Go stdlib + `github.com/stretchr/testify` for tests. That's it."* to *"Go stdlib + `github.com/stretchr/testify` for tests + `google.golang.org/protobuf` for wire-format types. No other runtime dependencies."* Add rule 6: *"All signing/verifying of proto messages MUST go through `shared/signing.DeterministicMarshal` so `Deterministic: true` is the single choke point for canonical bytes."*

Both amendments ship in the same PR as the schema implementation so the repo is never internally inconsistent.

## 10. Future work

- **Cross-language conformance suite** (architecture §9): once a non-Go implementation is contemplated, add a byte-level conformance test where the Go implementation emits golden bytes and the other-language implementation verifies them. The golden-fixture test (§7.2) is the starting point.
- **Schema evolution discipline.** v1 uses ascending tag numbers 1–11 with no reserved ranges. Future changes follow standard protobuf rules: never reuse a tag, mark removed fields `reserved`, add-only within a major version.
- **ProofV2**. Once Claude-Code-issued signed attestation lands upstream (architecture §7.2 v2), a new `shared/exhaustionproof.ProofV2` message is added. The envelope grows an `oneof { ExhaustionProofV1 v1; ExhaustionProofV2 v2; } exhaustion_proof` field.
- **TEE tier attestation field.** When the TEE tier is re-designed (architecture §9), the envelope may grow a `tee_attestation` field. Reserve tag 12 for this.
