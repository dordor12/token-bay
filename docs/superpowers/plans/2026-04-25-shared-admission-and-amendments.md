# `shared/admission` + Cross-Cutting Spec Amendments — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land the admission-subsystem wire types in `shared/admission/`, the matching `Sign/VerifyCreditAttestation` helpers in `shared/signing`, validation helpers, a byte-stable golden fixture, and the three cross-cutting spec amendments to `docs/superpowers/specs/tracker/2026-04-22-tracker-design.md` called out in admission-design §11. End state: any caller can build a `CreditAttestationBody`, sign it, verify it, validate it, and round-trip the signed form on the wire — and the tracker design spec is internally consistent with the new proto types.

**Architecture:** Two-message split mirroring `EnvelopeBody`/`EnvelopeSigned` and `EntryBody`/`Entry`: an unsigned `CreditAttestationBody` (all signal fields from admission-design §4.1) plus a signed wire form `SignedCreditAttestation { body, tracker_sig }`. The signature is Ed25519 over `signing.DeterministicMarshal(body)` — the single canonical-bytes choke point per `shared/CLAUDE.md` rule 6. `FetchHeadroomRequest`/`FetchHeadroomResponse` are unsigned (they travel over the already-authenticated tracker↔seeder connection per admission-design §4.1). `TickSource` enum lives in the same package because the tracker heartbeat amendment (§11.1) imports it. Validation helpers run pre-sign and post-parse before any field is trusted.

**Tech Stack:** Go 1.23+, `google.golang.org/protobuf` (already a shared dep), stdlib `crypto/ed25519`, `github.com/stretchr/testify`. Codegen via `protoc` + `protoc-gen-go` (already configured in `shared/Makefile`).

**Specs:**
- `docs/superpowers/specs/admission/2026-04-25-admission-design.md` §4.1 (data structures), §6.1 (validation invariants), §6.2 (sign/verify), §6.3 (required tests), §11 (cross-cutting amendments)
- `docs/superpowers/specs/tracker/2026-04-22-tracker-design.md` §2.1, §5.1, §6 (amendment targets)
- `docs/superpowers/specs/shared/2026-04-24-wire-format-v1-design.md` §4.1 (DeterministicMarshal contract), §7 (test-shape requirements)

**Dependency order:** Runs after wire-format v1 (already shipped — `shared/proto/` and `shared/signing` are populated) and the admission spec landing on `main` (commit `c0fad4a`). Tracker control-plane proto (heartbeat, broker_request) does **not** yet exist anywhere in the repo (`tracker/internal/api`, `broker`, `admin`, `server`, `metrics` are all empty `.gitkeep`), so the §11 amendments in this plan are **spec-text-only**: they describe the new fields and oneof variants in prose, and the proto changes themselves materialize when those tracker packages get implemented in a later plan. This is consistent with admission-design §11's "ship in same PR as the admission implementation so the repo is never internally inconsistent" — the *spec* and the *types that exist today* must agree, and they will.

**Branch:** `tracker_admission_shared` off `main` head. Plan 2 (`tracker/internal/admission` core, in-memory) and plan 3 (persistence + admin + acceptance) ship on separate branches off `main` after this one merges.

---

## 1. File map

```
shared/
├── admission/
│   ├── doc.go                                     ← CREATE: package doc + non-negotiables
│   ├── admission.proto                            ← CREATE: TickSource + CreditAttestationBody +
│   │                                                        SignedCreditAttestation + FetchHeadroomRequest +
│   │                                                        FetchHeadroomResponse + PerModelHeadroom
│   ├── admission.pb.go                            ← CREATE (generated, committed)
│   ├── validate.go                                ← CREATE: ValidateCreditAttestationBody +
│   │                                                        ValidateFetchHeadroomRequest +
│   │                                                        ValidateFetchHeadroomResponse
│   ├── admission_test.go                          ← CREATE: round-trip + enum coverage + golden
│   ├── validate_test.go                           ← CREATE: rejection-matrix per §6.1
│   └── testdata/
│       └── credit_attestation.golden.hex          ← CREATE (Task 6)
├── signing/
│   ├── proto.go                                   ← MODIFY: add SignCreditAttestation +
│   │                                                        VerifyCreditAttestation
│   └── proto_test.go                              ← MODIFY: extend helper tests
├── Makefile                                       ← MODIFY: add admission/admission.proto to PROTO_FILES
└── CLAUDE.md                                      ← MODIFY: list shared/admission alongside other proto packages

docs/superpowers/specs/tracker/2026-04-22-tracker-design.md   ← MODIFY: §2.1 heartbeat, §5.1 response, §6 race-list bullet
tracker/CLAUDE.md                                             ← MODIFY: race-list line (admission joins broker + federation)
```

Notes:
- The admission types get their own package (`shared/admission/`) rather than living in `shared/proto/`, matching admission-design §4.1's explicit "Go package `shared/admission`" instruction and paralleling `shared/exhaustionproof/`. The package boundary keeps admission's protobuf import surface separate from envelope/balance/ledger.
- `validate.go` holds three top-level helpers in one file — one per wire message that needs validation. `Sign/Verify` for the attestation lives in `shared/signing/proto.go` alongside the Envelope/Entry/BalanceSnapshot helpers.
- Golden fixture pins the wire bytes for `SignedCreditAttestation` only — the unsigned bodies (`FetchHeadroom*`) get round-trip coverage but no golden hex, matching how envelope/entry/balance handle their unsigned helper messages.
- No tracker proto changes in this plan: §11.1 heartbeat fields and §11.2 broker_request response oneof are described in the tracker spec amendment (Task 9) but the actual tracker `.proto` doesn't exist yet. When it does exist (separate plan), it imports `shared/admission` for `TickSource` and the band/reason enums (which we add in Task 2 alongside the rest of the schema).

## 2. Conventions used in this plan

- All `go test` commands run from `/Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/shared` unless a different `cd` is shown.
- `PATH="$HOME/.local/share/mise/shims:$PATH"` is needed if Go is managed by mise. Bash steps include it where required.
- `protoc` and `protoc-gen-go` are required only for Task 2 (regenerate `admission.pb.go`). Generated files are committed so other tasks don't need them. The wire-format-v1 plan installed both; verify on PATH at the start of Task 2.
- One commit per task. Conventional-commit prefixes: `feat(shared/admission):`, `feat(shared/signing):`, `chore(shared):`, `docs(shared):`, `spec:`.
- Co-Authored-By footer: `Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>`.
- Each red-green commit: failing test first, then minimal impl. Refactor-only commits get `refactor:`.

---

## 3. Wire format design (decisions used by all tasks)

### 3.1 Schema (admission-design §4.1, copied here so the plan is self-contained)

```proto
syntax = "proto3";
package tokenbay.admission.v1;

option go_package = "github.com/token-bay/token-bay/shared/admission";

// TickSource classifies how a seeder produced its headroom_estimate.
// TICK_SOURCE_UNSPECIFIED is rejected by ValidateFetchHeadroomResponse.
// Imported by tracker heartbeat (admission-design §11.1).
enum TickSource {
  TICK_SOURCE_UNSPECIFIED = 0;
  TICK_SOURCE_HEURISTIC   = 1;
  TICK_SOURCE_USAGE_PROBE = 2;
}

// CreditAttestationBody — tracker-signed portable credit-history attestation.
// Spec: docs/superpowers/specs/admission/2026-04-25-admission-design.md §4.1.
// Two-message split for canonical signing: tracker signs DeterministicMarshal(body).
message CreditAttestationBody {
  bytes  identity_id            = 1;   // 32 bytes — consumer Ed25519 pubkey hash
  bytes  issuer_tracker_id      = 2;   // 32 bytes — issuing tracker pubkey hash
  uint32 score                  = 3;   // fixed-point 0..10000 (= 0.0..1.0)

  // Raw signals (so receiving region can re-weight if it wants):
  uint32 tenure_days            = 4;   // capped 365
  uint32 settlement_reliability = 5;   // fixed-point 0..10000
  uint32 dispute_rate           = 6;   // fixed-point 0..10000 (low = good)
  int64  net_credit_flow_30d    = 7;   // signed credit delta over rolling 30d
  int32  balance_cushion_log2   = 8;   // log2(balance/starter_grant), clamped [-8, 8]

  uint64 computed_at            = 9;   // unix seconds; tracker wall-clock
  uint64 expires_at             = 10;  // computed_at + ttl
}

// SignedCreditAttestation — wire form. Verifier recovers signed bytes via
// shared/signing.DeterministicMarshal(body).
message SignedCreditAttestation {
  CreditAttestationBody body        = 1;
  bytes                 tracker_sig = 2;  // 64 bytes — Ed25519 over DeterministicMarshal(body)
}

// FetchHeadroomRequest — tracker-initiated, unsigned (rides the already-
// authenticated tracker↔seeder connection per admission-design §3.2).
message FetchHeadroomRequest {
  uint64 request_nonce = 1;
  string model_filter  = 2;   // optional; "" for any model
}

message FetchHeadroomResponse {
  uint64     request_nonce      = 1;
  uint32     headroom_estimate  = 2;   // fixed-point 0..10000
  TickSource source             = 3;
  uint32     probe_age_s        = 4;
  bool       can_probe_usage    = 5;
  repeated PerModelHeadroom per_model = 6;
}

message PerModelHeadroom {
  string model              = 1;
  uint32 headroom_estimate  = 2;   // fixed-point 0..10000
}
```

### 3.2 Field-length & range invariants (admission-design §6.1)

`ValidateCreditAttestationBody` enforces:
| Field | Invariant |
|---|---|
| `IdentityID` | length == 32 |
| `IssuerTrackerID` | length == 32 |
| `Score` | ≤ 10000 |
| `TenureDays` | ≤ 365 |
| `SettlementReliability` | ≤ 10000 |
| `DisputeRate` | ≤ 10000 |
| `BalanceCushionLog2` | ∈ [-8, 8] |
| `ComputedAt` | > 0 |
| `ExpiresAt` | > `ComputedAt` |
| `ExpiresAt − ComputedAt` | ≤ 7 days (`attestation_max_ttl_seconds` = 604800) |

`ValidateFetchHeadroomResponse` enforces:
| Field | Invariant |
|---|---|
| `Source` | != `TICK_SOURCE_UNSPECIFIED` |
| `HeadroomEstimate` | ≤ 10000 |
| each `PerModel.HeadroomEstimate` | ≤ 10000 |
| each `PerModel.Model` | not empty |

`ValidateFetchHeadroomRequest` enforces:
| Field | Invariant |
|---|---|
| `RequestNonce` | > 0 (zero is the proto-default and indicates "not set") |

`NetCreditFlow30d` is intentionally unbounded (signed int64). `BalanceCushionLog2` is signed because the spec mandates `[-8, 8]` (negative when balance < starter_grant). All other unsigned `uint32` ceilings reflect the fixed-point 10000-scale convention.

### 3.3 Sign/Verify shape (admission-design §6.2)

```go
func SignCreditAttestation(priv ed25519.PrivateKey, body *admission.CreditAttestationBody) ([]byte, error)
func VerifyCreditAttestation(pub ed25519.PublicKey, signed *admission.SignedCreditAttestation) bool
```

Mirrors the existing `SignEnvelope`/`VerifyEnvelope`, `SignBalanceSnapshot`/`VerifyBalanceSnapshot`, `SignEntry`/`VerifyEntry` shape exactly:
- `Sign*` returns an error on wrong-length private key or `nil` body. Pre-condition: body has been validated; the helper does **not** re-run validation.
- `Verify*` reports `bool`; `nil signed`, `nil signed.Body`, malformed key/sig, or marshal failure all return `false` without panicking.
- Both go through `signing.DeterministicMarshal` — the single canonical-bytes choke point.

### 3.4 Required test shapes (admission-design §6.3)

1. **Round-trip** for `CreditAttestationBody`, `SignedCreditAttestation`, `FetchHeadroomRequest`, `FetchHeadroomResponse`, all three `TickSource` enum values.
2. **Byte-stable golden fixture** at `shared/admission/testdata/credit_attestation.golden.hex` — populated body + fixture keypair → specific hex bytes; CI diff-fails on drift.
3. **Sign/verify positive + tampered** — flip body bits, flip sig bits, swap pubkey; all must reject.
4. **Validation rejection matrix** — one row per invariant in §3.2 above.
5. **Nil-safety** — `SignCreditAttestation(priv, nil)` returns error; `VerifyCreditAttestation(pub, nil)` returns false; `VerifyCreditAttestation(pub, &SignedCreditAttestation{Body: nil})` returns false.

Coverage target: ≥ 90% line coverage on `shared/admission` and the `shared/signing` additions.

---

## 4. Tracker spec amendments (admission-design §11) — text-only in this plan

The §11.1 heartbeat-field amendment and §11.2 broker_request response-oneof amendment describe protobuf shapes for messages that **do not yet exist as `.proto` files in this repo**. The tracker spec is the prose source of truth; this plan amends the prose so it matches reality (admission types now exist; tracker control-plane proto comes later).

The §11.3 race-list amendment is a one-line addition to the tracker spec's CI section.

Concrete edits land in Task 9. Until then, every other task in this plan is `shared/`-only.

---

## Task 1: Add `admission/admission.proto` to Makefile PROTO_FILES + create package skeleton

**Files:**
- Create: `shared/admission/doc.go`
- Modify: `shared/Makefile`

- [ ] **Step 1: Verify protoc + protoc-gen-go are on PATH**

```bash
PATH="$HOME/.local/share/mise/shims:$PATH" command -v protoc && protoc --version
PATH="$HOME/.local/share/mise/shims:$PATH" command -v protoc-gen-go && protoc-gen-go --version
```

Expected: both resolve. `protoc` v3.x or v4.x; `protoc-gen-go` v1.32+.

If missing: `brew install protobuf` (macOS) and `PATH="$HOME/.local/share/mise/shims:$PATH" go install google.golang.org/protobuf/cmd/protoc-gen-go@latest`.

- [ ] **Step 2: Add admission proto file to Makefile**

Edit `shared/Makefile`. The `PROTO_FILES` line currently reads:

```makefile
PROTO_FILES := exhaustionproof/proof.proto proto/balance.proto proto/envelope.proto proto/ledger.proto
```

Replace with:

```makefile
PROTO_FILES := exhaustionproof/proof.proto proto/balance.proto proto/envelope.proto proto/ledger.proto admission/admission.proto
```

- [ ] **Step 3: Create the package skeleton**

Create `shared/admission/doc.go`:

```go
// Package admission holds the wire-format types for the tracker admission
// subsystem: portable credit attestations and tracker-initiated headroom
// fetches. Types parallel shared/proto: a Body message plus a Signed wrapper
// for any ed25519-signed payload, with the signature taken over
// DeterministicMarshal(body). Validation helpers in this package run pre-sign
// and post-parse before any field is trusted.
//
// Spec: docs/superpowers/specs/admission/2026-04-25-admission-design.md.
package admission
```

- [ ] **Step 4: Verify the package compiles**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/shared
PATH="$HOME/.local/share/mise/shims:$PATH" go build ./admission/...
```

Expected: success — single-file package with only a doc comment.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission
git add shared/admission/doc.go shared/Makefile
git commit -m "$(cat <<'EOF'
chore(shared/admission): package skeleton + Makefile hookup

Adds the empty shared/admission package and registers admission/admission.proto
with the existing proto-gen target. The .proto schema and generated .pb.go
land in the next commit; this commit just creates the directory and wires
codegen so subsequent tasks can run `make proto-gen` without further plumbing.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: `admission.proto` schema + round-trip test

**Files:**
- Create: `shared/admission/admission.proto`
- Create: `shared/admission/admission.pb.go` (generated)
- Create: `shared/admission/admission_test.go`

- [ ] **Step 1: Write the failing round-trip test**

Create `shared/admission/admission_test.go`:

```go
package admission

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// fixtureCreditAttestationBody returns a fully-populated body with
// deterministic field values — used by round-trip, validator,
// sign/verify, and golden-fixture tests across the shared/ packages.
func fixtureCreditAttestationBody() *CreditAttestationBody {
	return &CreditAttestationBody{
		IdentityId:            make32(0x11),
		IssuerTrackerId:       make32(0x22),
		Score:                 8500,
		TenureDays:            120,
		SettlementReliability: 9700,
		DisputeRate:           50,
		NetCreditFlow_30D:     250000,
		BalanceCushionLog2:    3,
		ComputedAt:            1714000000,
		ExpiresAt:             1714086400,
	}
}

func make32(b byte) []byte { return repeat(b, 32) }
func make64(b byte) []byte { return repeat(b, 64) }
func repeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

func TestCreditAttestationBody_RoundTrip(t *testing.T) {
	original := fixtureCreditAttestationBody()

	first, err := proto.MarshalOptions{Deterministic: true}.Marshal(original)
	require.NoError(t, err)

	var parsed CreditAttestationBody
	require.NoError(t, proto.Unmarshal(first, &parsed))

	second, err := proto.MarshalOptions{Deterministic: true}.Marshal(&parsed)
	require.NoError(t, err)

	assert.Equal(t, first, second, "Deterministic marshal must be byte-stable")
	require.True(t, proto.Equal(original, &parsed), "unmarshal must reproduce original exactly")
}

func TestSignedCreditAttestation_RoundTrip(t *testing.T) {
	signed := &SignedCreditAttestation{
		Body:       fixtureCreditAttestationBody(),
		TrackerSig: make64(0x77),
	}

	first, err := proto.MarshalOptions{Deterministic: true}.Marshal(signed)
	require.NoError(t, err)

	var parsed SignedCreditAttestation
	require.NoError(t, proto.Unmarshal(first, &parsed))

	second, err := proto.MarshalOptions{Deterministic: true}.Marshal(&parsed)
	require.NoError(t, err)

	assert.Equal(t, first, second)
	require.True(t, proto.Equal(signed, &parsed), "unmarshal must reproduce original exactly")
}

func TestFetchHeadroomRequest_RoundTrip(t *testing.T) {
	original := &FetchHeadroomRequest{RequestNonce: 42, ModelFilter: "claude-sonnet-4-6"}

	first, err := proto.MarshalOptions{Deterministic: true}.Marshal(original)
	require.NoError(t, err)

	var parsed FetchHeadroomRequest
	require.NoError(t, proto.Unmarshal(first, &parsed))

	second, err := proto.MarshalOptions{Deterministic: true}.Marshal(&parsed)
	require.NoError(t, err)

	assert.Equal(t, first, second)
	require.True(t, proto.Equal(original, &parsed))
}

func TestFetchHeadroomResponse_RoundTrip(t *testing.T) {
	original := &FetchHeadroomResponse{
		RequestNonce:     42,
		HeadroomEstimate: 7200,
		Source:           TickSource_TICK_SOURCE_USAGE_PROBE,
		ProbeAgeS:        15,
		CanProbeUsage:    true,
		PerModel: []*PerModelHeadroom{
			{Model: "claude-sonnet-4-6", HeadroomEstimate: 7000},
			{Model: "claude-opus-4-7", HeadroomEstimate: 8500},
		},
	}

	first, err := proto.MarshalOptions{Deterministic: true}.Marshal(original)
	require.NoError(t, err)

	var parsed FetchHeadroomResponse
	require.NoError(t, proto.Unmarshal(first, &parsed))

	second, err := proto.MarshalOptions{Deterministic: true}.Marshal(&parsed)
	require.NoError(t, err)

	assert.Equal(t, first, second)
	require.True(t, proto.Equal(original, &parsed))
}

// TestTickSource_AllValuesRoundTrip pins every enum value the schema defines
// (admission-design §6.3 #1).
func TestTickSource_AllValuesRoundTrip(t *testing.T) {
	cases := []TickSource{
		TickSource_TICK_SOURCE_UNSPECIFIED,
		TickSource_TICK_SOURCE_HEURISTIC,
		TickSource_TICK_SOURCE_USAGE_PROBE,
	}
	for _, src := range cases {
		t.Run(src.String(), func(t *testing.T) {
			original := &FetchHeadroomResponse{Source: src, RequestNonce: 1}
			buf, err := proto.MarshalOptions{Deterministic: true}.Marshal(original)
			require.NoError(t, err)
			var parsed FetchHeadroomResponse
			require.NoError(t, proto.Unmarshal(buf, &parsed))
			assert.Equal(t, src, parsed.Source)
		})
	}
}
```

- [ ] **Step 2: Run, confirm FAIL**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/shared
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./admission/...
```

Expected: compilation failure — `CreditAttestationBody`, `SignedCreditAttestation`, `FetchHeadroomRequest`, `FetchHeadroomResponse`, `PerModelHeadroom`, `TickSource_*` undefined.

- [ ] **Step 3: Write the schema**

Create `shared/admission/admission.proto`:

```proto
syntax = "proto3";
package tokenbay.admission.v1;

option go_package = "github.com/token-bay/token-bay/shared/admission";

// TickSource classifies how a seeder produced its headroom_estimate.
// TICK_SOURCE_UNSPECIFIED is rejected by ValidateFetchHeadroomResponse.
// Imported by tracker heartbeat once that proto exists (admission-design
// §11.1).
enum TickSource {
  TICK_SOURCE_UNSPECIFIED = 0;
  TICK_SOURCE_HEURISTIC   = 1;
  TICK_SOURCE_USAGE_PROBE = 2;
}

// CreditAttestationBody — tracker-signed portable credit-history
// attestation. Spec:
// docs/superpowers/specs/admission/2026-04-25-admission-design.md §4.1.
// Two-message split for canonical signing: tracker signs
// DeterministicMarshal(body).
message CreditAttestationBody {
  bytes  identity_id            = 1;   // 32 bytes — consumer Ed25519 pubkey hash
  bytes  issuer_tracker_id      = 2;   // 32 bytes — issuing tracker pubkey hash
  uint32 score                  = 3;   // fixed-point 0..10000 (= 0.0..1.0)

  uint32 tenure_days            = 4;   // capped 365
  uint32 settlement_reliability = 5;   // fixed-point 0..10000
  uint32 dispute_rate           = 6;   // fixed-point 0..10000 (low = good)
  int64  net_credit_flow_30d    = 7;   // signed credit delta over rolling 30d
  int32  balance_cushion_log2   = 8;   // log2(balance/starter_grant), clamped [-8, 8]

  uint64 computed_at            = 9;   // unix seconds; tracker wall-clock
  uint64 expires_at             = 10;  // computed_at + ttl
}

// SignedCreditAttestation — wire form. Verifier recovers signed bytes via
// shared/signing.DeterministicMarshal(body).
message SignedCreditAttestation {
  CreditAttestationBody body        = 1;
  bytes                 tracker_sig = 2;  // 64 bytes — Ed25519 signature
}

// FetchHeadroomRequest — tracker-initiated, unsigned (rides the
// already-authenticated tracker↔seeder connection). Spec §3.2.
message FetchHeadroomRequest {
  uint64 request_nonce = 1;
  string model_filter  = 2;   // optional; "" for any model
}

message FetchHeadroomResponse {
  uint64     request_nonce      = 1;
  uint32     headroom_estimate  = 2;   // fixed-point 0..10000
  TickSource source             = 3;
  uint32     probe_age_s        = 4;
  bool       can_probe_usage    = 5;
  repeated PerModelHeadroom per_model = 6;
}

message PerModelHeadroom {
  string model              = 1;
  uint32 headroom_estimate  = 2;   // fixed-point 0..10000
}
```

- [ ] **Step 4: Regenerate `.pb.go`**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/shared
PATH="$HOME/.local/share/mise/shims:$PATH" make proto-gen
ls admission/admission.pb.go
```

Expected: `admission.pb.go` exists. The CI tripwire (`make proto-check`) will verify the file is in sync with the schema.

- [ ] **Step 5: Run round-trip tests, confirm PASS**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/shared
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./admission/...
```

Expected: all five tests in this task PASS.

- [ ] **Step 6: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission
git add shared/admission/admission.proto shared/admission/admission.pb.go shared/admission/admission_test.go
git commit -m "$(cat <<'EOF'
feat(shared/admission): wire-format schema + round-trip tests

Adds the admission-design §4.1 proto schema:
  - TickSource enum (HEURISTIC | USAGE_PROBE)
  - CreditAttestationBody + SignedCreditAttestation (two-message split,
    Ed25519 over DeterministicMarshal(body))
  - FetchHeadroomRequest / FetchHeadroomResponse / PerModelHeadroom
    (unsigned; ride the already-authenticated tracker↔seeder link)

Round-trip coverage exercises all top-level messages plus every TickSource
enum value (admission-design §6.3 #1). Sign/verify lands in a follow-up.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: `ValidateCreditAttestationBody` + tests

**Files:**
- Create: `shared/admission/validate.go`
- Create: `shared/admission/validate_test.go`

- [ ] **Step 1: Write the failing happy-path + rejection-matrix tests**

Create `shared/admission/validate_test.go`:

```go
package admission

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateCreditAttestationBody_HappyPath(t *testing.T) {
	require.NoError(t, ValidateCreditAttestationBody(fixtureCreditAttestationBody()))
}

func TestValidateCreditAttestationBody_NilBody(t *testing.T) {
	err := ValidateCreditAttestationBody(nil)
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "nil")
}

func TestValidateCreditAttestationBody_Rejections(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*CreditAttestationBody)
		errFrag string
	}{
		{"identity_id short", func(b *CreditAttestationBody) { b.IdentityId = []byte{1, 2, 3} }, "identity_id"},
		{"identity_id long", func(b *CreditAttestationBody) { b.IdentityId = make([]byte, 64) }, "identity_id"},
		{"identity_id nil", func(b *CreditAttestationBody) { b.IdentityId = nil }, "identity_id"},
		{"issuer_tracker_id short", func(b *CreditAttestationBody) { b.IssuerTrackerId = []byte{1, 2, 3} }, "issuer_tracker_id"},
		{"issuer_tracker_id long", func(b *CreditAttestationBody) { b.IssuerTrackerId = make([]byte, 64) }, "issuer_tracker_id"},
		{"issuer_tracker_id nil", func(b *CreditAttestationBody) { b.IssuerTrackerId = nil }, "issuer_tracker_id"},
		{"score over 10000", func(b *CreditAttestationBody) { b.Score = 10001 }, "score"},
		{"tenure_days over 365", func(b *CreditAttestationBody) { b.TenureDays = 366 }, "tenure_days"},
		{"settlement_reliability over 10000", func(b *CreditAttestationBody) { b.SettlementReliability = 10001 }, "settlement_reliability"},
		{"dispute_rate over 10000", func(b *CreditAttestationBody) { b.DisputeRate = 10001 }, "dispute_rate"},
		{"balance_cushion_log2 below -8", func(b *CreditAttestationBody) { b.BalanceCushionLog2 = -9 }, "balance_cushion_log2"},
		{"balance_cushion_log2 above 8", func(b *CreditAttestationBody) { b.BalanceCushionLog2 = 9 }, "balance_cushion_log2"},
		{"computed_at zero", func(b *CreditAttestationBody) { b.ComputedAt = 0 }, "computed_at"},
		{"expires_at not after computed_at (equal)", func(b *CreditAttestationBody) { b.ExpiresAt = b.ComputedAt }, "expires_at"},
		{"expires_at before computed_at", func(b *CreditAttestationBody) { b.ExpiresAt = b.ComputedAt - 1 }, "expires_at"},
		{"ttl exceeds 7 days", func(b *CreditAttestationBody) { b.ExpiresAt = b.ComputedAt + 604801 }, "ttl"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := fixtureCreditAttestationBody()
			tc.mutate(b)
			err := ValidateCreditAttestationBody(b)
			require.Error(t, err, "case %q should fail validation", tc.name)
			assert.Contains(t, err.Error(), tc.errFrag, "error should mention %q", tc.errFrag)
		})
	}
}

func TestValidateCreditAttestationBody_BoundaryValues(t *testing.T) {
	t.Run("score == 10000 ok", func(t *testing.T) {
		b := fixtureCreditAttestationBody()
		b.Score = 10000
		require.NoError(t, ValidateCreditAttestationBody(b))
	})
	t.Run("tenure == 365 ok", func(t *testing.T) {
		b := fixtureCreditAttestationBody()
		b.TenureDays = 365
		require.NoError(t, ValidateCreditAttestationBody(b))
	})
	t.Run("balance_cushion == -8 ok", func(t *testing.T) {
		b := fixtureCreditAttestationBody()
		b.BalanceCushionLog2 = -8
		require.NoError(t, ValidateCreditAttestationBody(b))
	})
	t.Run("balance_cushion == 8 ok", func(t *testing.T) {
		b := fixtureCreditAttestationBody()
		b.BalanceCushionLog2 = 8
		require.NoError(t, ValidateCreditAttestationBody(b))
	})
	t.Run("ttl == 7 days ok", func(t *testing.T) {
		b := fixtureCreditAttestationBody()
		b.ExpiresAt = b.ComputedAt + 604800
		require.NoError(t, ValidateCreditAttestationBody(b))
	})
}
```

- [ ] **Step 2: Run, confirm FAIL**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/shared
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./admission/...
```

Expected: compilation failure — `ValidateCreditAttestationBody` undefined.

- [ ] **Step 3: Write the validator**

Create `shared/admission/validate.go`:

```go
package admission

import (
	"errors"
	"fmt"
)

// identityIDLen is the required length of CreditAttestationBody.IdentityId
// and IssuerTrackerId — Ed25519 pubkey hashes are 32 bytes per
// shared/CLAUDE.md and architecture spec §6.
const identityIDLen = 32

// scoreMax is the fixed-point ceiling for any score / reliability /
// dispute-rate field (= 1.0).
const scoreMax = uint32(10000)

// tenureDaysCap mirrors admission-design §4.1 "capped 365".
const tenureDaysCap = uint32(365)

// balanceCushionMin / balanceCushionMax bound the clamped
// log2(balance/starter_grant) signal.
const (
	balanceCushionMin = int32(-8)
	balanceCushionMax = int32(8)
)

// attestationMaxTTLSeconds bounds expires_at − computed_at (admission-design
// §6.1: 7 days).
const attestationMaxTTLSeconds = uint64(7 * 24 * 60 * 60)

// ValidateCreditAttestationBody enforces admission-design §6.1 invariants on
// a CreditAttestationBody. Senders run pre-sign; receivers (importing tracker)
// run post-parse before trusting any field. Returns nil if well-formed; an
// error describing the first violation otherwise. Does NOT verify the
// tracker_sig — see signing.VerifyCreditAttestation.
func ValidateCreditAttestationBody(b *CreditAttestationBody) error {
	if b == nil {
		return errors.New("admission: nil CreditAttestationBody")
	}
	if len(b.IdentityId) != identityIDLen {
		return fmt.Errorf("admission: identity_id length %d, want %d", len(b.IdentityId), identityIDLen)
	}
	if len(b.IssuerTrackerId) != identityIDLen {
		return fmt.Errorf("admission: issuer_tracker_id length %d, want %d", len(b.IssuerTrackerId), identityIDLen)
	}
	if b.Score > scoreMax {
		return fmt.Errorf("admission: score %d exceeds %d", b.Score, scoreMax)
	}
	if b.TenureDays > tenureDaysCap {
		return fmt.Errorf("admission: tenure_days %d exceeds cap %d", b.TenureDays, tenureDaysCap)
	}
	if b.SettlementReliability > scoreMax {
		return fmt.Errorf("admission: settlement_reliability %d exceeds %d", b.SettlementReliability, scoreMax)
	}
	if b.DisputeRate > scoreMax {
		return fmt.Errorf("admission: dispute_rate %d exceeds %d", b.DisputeRate, scoreMax)
	}
	if b.BalanceCushionLog2 < balanceCushionMin || b.BalanceCushionLog2 > balanceCushionMax {
		return fmt.Errorf("admission: balance_cushion_log2 %d out of [%d, %d]", b.BalanceCushionLog2, balanceCushionMin, balanceCushionMax)
	}
	if b.ComputedAt == 0 {
		return errors.New("admission: computed_at is zero")
	}
	if b.ExpiresAt <= b.ComputedAt {
		return fmt.Errorf("admission: expires_at %d must be > computed_at %d", b.ExpiresAt, b.ComputedAt)
	}
	if b.ExpiresAt-b.ComputedAt > attestationMaxTTLSeconds {
		return fmt.Errorf("admission: ttl %d exceeds max %d", b.ExpiresAt-b.ComputedAt, attestationMaxTTLSeconds)
	}
	return nil
}
```

- [ ] **Step 4: Run, confirm PASS**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/shared
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./admission/...
```

Expected: all tests in this task PASS.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission
git add shared/admission/validate.go shared/admission/validate_test.go
git commit -m "$(cat <<'EOF'
feat(shared/admission): ValidateCreditAttestationBody

Implements the admission-design §6.1 invariants for CreditAttestationBody:
field lengths (32-byte identity hashes), fixed-point ceilings (≤10000),
tenure cap (≤365), balance_cushion clamp ([-8, 8]), monotonic timestamps
(expires_at > computed_at), and TTL ceiling (≤ 7 days).

Rejection matrix covers one row per invariant; boundary tests pin the
inclusive endpoints (score==10000, tenure==365, cushion==±8, ttl==7d).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: `ValidateFetchHeadroomRequest` + tests

**Files:**
- Modify: `shared/admission/validate.go`
- Modify: `shared/admission/validate_test.go`

- [ ] **Step 1: Append failing test cases**

Append to `shared/admission/validate_test.go`:

```go
func TestValidateFetchHeadroomRequest_HappyPath(t *testing.T) {
	require.NoError(t, ValidateFetchHeadroomRequest(&FetchHeadroomRequest{
		RequestNonce: 1,
		ModelFilter:  "claude-sonnet-4-6",
	}))
}

func TestValidateFetchHeadroomRequest_EmptyModelFilterOk(t *testing.T) {
	require.NoError(t, ValidateFetchHeadroomRequest(&FetchHeadroomRequest{
		RequestNonce: 1,
		ModelFilter:  "",
	}))
}

func TestValidateFetchHeadroomRequest_NilBody(t *testing.T) {
	err := ValidateFetchHeadroomRequest(nil)
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "nil")
}

func TestValidateFetchHeadroomRequest_ZeroNonce(t *testing.T) {
	err := ValidateFetchHeadroomRequest(&FetchHeadroomRequest{RequestNonce: 0})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "request_nonce")
}
```

- [ ] **Step 2: Run, confirm FAIL**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/shared
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 -run TestValidateFetchHeadroomRequest ./admission/...
```

Expected: compilation failure — `ValidateFetchHeadroomRequest` undefined.

- [ ] **Step 3: Append the validator**

Append to `shared/admission/validate.go`:

```go
// ValidateFetchHeadroomRequest enforces admission-design §6.1 invariants on a
// FetchHeadroomRequest. RequestNonce must be non-zero (zero is the proto-default
// indicating "not set"). ModelFilter is optional ("" means any model).
func ValidateFetchHeadroomRequest(r *FetchHeadroomRequest) error {
	if r == nil {
		return errors.New("admission: nil FetchHeadroomRequest")
	}
	if r.RequestNonce == 0 {
		return errors.New("admission: request_nonce is zero")
	}
	return nil
}
```

- [ ] **Step 4: Run, confirm PASS**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/shared
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./admission/...
```

Expected: all tests including the four new ones PASS.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission
git add shared/admission/validate.go shared/admission/validate_test.go
git commit -m "$(cat <<'EOF'
feat(shared/admission): ValidateFetchHeadroomRequest

RequestNonce must be non-zero so the proto default doesn't silently mean
"valid request"; ModelFilter is optional.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: `ValidateFetchHeadroomResponse` + tests

**Files:**
- Modify: `shared/admission/validate.go`
- Modify: `shared/admission/validate_test.go`

- [ ] **Step 1: Append failing test cases**

Append to `shared/admission/validate_test.go`:

```go
func fixtureFetchHeadroomResponse() *FetchHeadroomResponse {
	return &FetchHeadroomResponse{
		RequestNonce:     42,
		HeadroomEstimate: 7200,
		Source:           TickSource_TICK_SOURCE_USAGE_PROBE,
		ProbeAgeS:        15,
		CanProbeUsage:    true,
		PerModel: []*PerModelHeadroom{
			{Model: "claude-sonnet-4-6", HeadroomEstimate: 7000},
			{Model: "claude-opus-4-7", HeadroomEstimate: 8500},
		},
	}
}

func TestValidateFetchHeadroomResponse_HappyPath(t *testing.T) {
	require.NoError(t, ValidateFetchHeadroomResponse(fixtureFetchHeadroomResponse()))
}

func TestValidateFetchHeadroomResponse_NoPerModelOk(t *testing.T) {
	r := fixtureFetchHeadroomResponse()
	r.PerModel = nil
	require.NoError(t, ValidateFetchHeadroomResponse(r))
}

func TestValidateFetchHeadroomResponse_NilBody(t *testing.T) {
	err := ValidateFetchHeadroomResponse(nil)
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "nil")
}

func TestValidateFetchHeadroomResponse_Rejections(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*FetchHeadroomResponse)
		errFrag string
	}{
		{"unspecified source", func(r *FetchHeadroomResponse) { r.Source = TickSource_TICK_SOURCE_UNSPECIFIED }, "source"},
		{"unknown source value", func(r *FetchHeadroomResponse) { r.Source = TickSource(99) }, "source"},
		{"headroom over 10000", func(r *FetchHeadroomResponse) { r.HeadroomEstimate = 10001 }, "headroom_estimate"},
		{"per_model headroom over 10000", func(r *FetchHeadroomResponse) {
			r.PerModel[0].HeadroomEstimate = 10001
		}, "per_model"},
		{"per_model empty model name", func(r *FetchHeadroomResponse) {
			r.PerModel[0].Model = ""
		}, "model"},
		{"per_model nil entry", func(r *FetchHeadroomResponse) {
			r.PerModel[0] = nil
		}, "per_model"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := fixtureFetchHeadroomResponse()
			tc.mutate(r)
			err := ValidateFetchHeadroomResponse(r)
			require.Error(t, err, "case %q should fail validation", tc.name)
			assert.Contains(t, err.Error(), tc.errFrag, "error should mention %q", tc.errFrag)
		})
	}
}

func TestValidateFetchHeadroomResponse_BoundaryHeadroom(t *testing.T) {
	r := fixtureFetchHeadroomResponse()
	r.HeadroomEstimate = 10000
	r.PerModel[0].HeadroomEstimate = 10000
	require.NoError(t, ValidateFetchHeadroomResponse(r))
}
```

- [ ] **Step 2: Run, confirm FAIL**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/shared
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 -run TestValidateFetchHeadroomResponse ./admission/...
```

Expected: compilation failure — `ValidateFetchHeadroomResponse` undefined.

- [ ] **Step 3: Append the validator**

Append to `shared/admission/validate.go`:

```go
// ValidateFetchHeadroomResponse enforces admission-design §6.1 invariants on a
// FetchHeadroomResponse. Source must be a known non-UNSPECIFIED value; all
// fixed-point headroom fields are bounded by scoreMax. Per-model entries
// (when present) must have a non-empty model name.
func ValidateFetchHeadroomResponse(r *FetchHeadroomResponse) error {
	if r == nil {
		return errors.New("admission: nil FetchHeadroomResponse")
	}
	switch r.Source {
	case TickSource_TICK_SOURCE_HEURISTIC, TickSource_TICK_SOURCE_USAGE_PROBE:
		// ok
	default:
		return fmt.Errorf("admission: source = %v, must be HEURISTIC or USAGE_PROBE", r.Source)
	}
	if r.HeadroomEstimate > scoreMax {
		return fmt.Errorf("admission: headroom_estimate %d exceeds %d", r.HeadroomEstimate, scoreMax)
	}
	for i, pm := range r.PerModel {
		if pm == nil {
			return fmt.Errorf("admission: per_model[%d] is nil", i)
		}
		if pm.Model == "" {
			return fmt.Errorf("admission: per_model[%d] model is empty", i)
		}
		if pm.HeadroomEstimate > scoreMax {
			return fmt.Errorf("admission: per_model[%d] headroom_estimate %d exceeds %d", i, pm.HeadroomEstimate, scoreMax)
		}
	}
	return nil
}
```

- [ ] **Step 4: Run, confirm PASS**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/shared
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./admission/...
```

Expected: all tests including the new ones PASS.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission
git add shared/admission/validate.go shared/admission/validate_test.go
git commit -m "$(cat <<'EOF'
feat(shared/admission): ValidateFetchHeadroomResponse

Source must be a known non-UNSPECIFIED value; HeadroomEstimate and every
per-model entry's HeadroomEstimate are bounded by scoreMax (10000); per-model
entries must be non-nil with a non-empty model name.

Rejection matrix covers each §6.1 invariant; boundary tests pin the
inclusive endpoint (10000).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: Golden-bytes fixture for `SignedCreditAttestation`

**Files:**
- Create: `shared/admission/testdata/credit_attestation.golden.hex`
- Modify: `shared/admission/admission_test.go`

This task pins the v1 wire bytes for a fully-signed `SignedCreditAttestation`. Any unintended change to schema, codegen library, signing seed, or `DeterministicMarshal` trips this test. Mirrors the entry/envelope golden-fixture pattern.

- [ ] **Step 1: Append the failing golden test**

Append to `shared/admission/admission_test.go`:

```go
import (
	"crypto/ed25519"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)
```

(Add the imports above to the existing import block.)

```go
const admissionGoldenPath = "testdata/credit_attestation.golden.hex"

// fixtureKeypair derives a deterministic Ed25519 keypair from a fixed seed.
// Used by the golden fixture so signatures are reproducible. Mirrors the
// helper of the same name in shared/signing/proto_test.go.
func fixtureKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	seed := []byte("token-bay-fixture-seed-v1-000000") // 32 bytes
	if len(seed) != ed25519.SeedSize {
		t.Fatalf("fixture seed must be %d bytes, got %d", ed25519.SeedSize, len(seed))
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return priv.Public().(ed25519.PublicKey), priv
}

// TestSignedCreditAttestation_GoldenBytes pins the v1 wire bytes for a
// fully-signed attestation. Regenerate with UPDATE_GOLDEN=1 and review the
// diff manually.
func TestSignedCreditAttestation_GoldenBytes(t *testing.T) {
	body := fixtureCreditAttestationBody()

	bodyBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(body)
	require.NoError(t, err)

	_, priv := fixtureKeypair(t)

	signed := &SignedCreditAttestation{
		Body:       body,
		TrackerSig: ed25519.Sign(priv, bodyBytes),
	}
	wireBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(signed)
	require.NoError(t, err)

	gotHex := hex.EncodeToString(wireBytes)

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		require.NoError(t, os.MkdirAll(filepath.Dir(admissionGoldenPath), 0o755))
		require.NoError(t, os.WriteFile(admissionGoldenPath, []byte(gotHex+"\n"), 0o644))
		t.Logf("golden updated at %s", admissionGoldenPath)
		return
	}

	wantBytes, err := os.ReadFile(admissionGoldenPath)
	require.NoError(t, err, "missing golden file — run: UPDATE_GOLDEN=1 go test ./admission/... -run TestSignedCreditAttestation_GoldenBytes")
	wantHex := strings.TrimSpace(string(wantBytes))

	assert.Equal(t, wantHex, gotHex,
		"SignedCreditAttestation wire bytes differ from golden. If schema/lib intentionally changed, "+
			"regenerate via UPDATE_GOLDEN=1 and review the diff manually.")
}
```

- [ ] **Step 2: Run, confirm FAIL with "missing golden file"**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/shared
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 -run TestSignedCreditAttestation_GoldenBytes ./admission/...
```

Expected: FAIL with "missing golden file" (file not yet on disk).

- [ ] **Step 3: Generate the golden file**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/shared
PATH="$HOME/.local/share/mise/shims:$PATH" UPDATE_GOLDEN=1 go test -count=1 -run TestSignedCreditAttestation_GoldenBytes ./admission/...
ls admission/testdata/credit_attestation.golden.hex
```

Expected: file exists, contains a single line of hex (no trailing whitespace beyond `\n`).

- [ ] **Step 4: Run again without UPDATE_GOLDEN, confirm PASS**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/shared
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./admission/...
```

Expected: all tests PASS, including the golden test reading from disk.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission
git add shared/admission/admission_test.go shared/admission/testdata/credit_attestation.golden.hex
git commit -m "$(cat <<'EOF'
test(shared/admission): SignedCreditAttestation golden-bytes tripwire

Pins the v1 wire bytes for a fully-signed attestation against
unintended schema, codegen, or DeterministicMarshal regressions.
Regenerate with UPDATE_GOLDEN=1 and review the diff manually.

Mirrors shared/proto/testdata/entry_signed.golden.hex (ledger Entry)
and the existing envelope golden fixture.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 7: `signing.SignCreditAttestation` + `VerifyCreditAttestation` + tests

**Files:**
- Modify: `shared/signing/proto.go`
- Modify: `shared/signing/proto_test.go`

- [ ] **Step 1: Append failing tests**

Append to `shared/signing/proto_test.go`:

```go
import (
	tbadmission "github.com/token-bay/token-bay/shared/admission"
)
```

(Add to the existing import block alongside the other imports.)

```go
func fixtureAttestationBody() *tbadmission.CreditAttestationBody {
	return &tbadmission.CreditAttestationBody{
		IdentityId:            bytes32(0x11),
		IssuerTrackerId:       bytes32(0x22),
		Score:                 8500,
		TenureDays:            120,
		SettlementReliability: 9700,
		DisputeRate:           50,
		NetCreditFlow_30D:     250000,
		BalanceCushionLog2:    3,
		ComputedAt:            1714000000,
		ExpiresAt:             1714086400,
	}
}

func TestSignVerifyCreditAttestation_RoundTrip(t *testing.T) {
	pub, priv := fixtureKeypair(t)
	body := fixtureAttestationBody()

	sig, err := SignCreditAttestation(priv, body)
	require.NoError(t, err)
	require.Len(t, sig, ed25519.SignatureSize)

	signed := &tbadmission.SignedCreditAttestation{Body: body, TrackerSig: sig}
	assert.True(t, VerifyCreditAttestation(pub, signed))
}

func TestVerifyCreditAttestation_TamperedBodyFails(t *testing.T) {
	pub, priv := fixtureKeypair(t)
	body := fixtureAttestationBody()
	sig, err := SignCreditAttestation(priv, body)
	require.NoError(t, err)

	body.Score = 9999 // post-sign mutation
	signed := &tbadmission.SignedCreditAttestation{Body: body, TrackerSig: sig}
	assert.False(t, VerifyCreditAttestation(pub, signed))
}

func TestVerifyCreditAttestation_TamperedSigFails(t *testing.T) {
	pub, priv := fixtureKeypair(t)
	body := fixtureAttestationBody()
	sig, err := SignCreditAttestation(priv, body)
	require.NoError(t, err)

	sig[0] ^= 0xff
	signed := &tbadmission.SignedCreditAttestation{Body: body, TrackerSig: sig}
	assert.False(t, VerifyCreditAttestation(pub, signed))
}

func TestVerifyCreditAttestation_WrongPubkeyFails(t *testing.T) {
	_, priv := fixtureKeypair(t)
	body := fixtureAttestationBody()
	sig, err := SignCreditAttestation(priv, body)
	require.NoError(t, err)

	otherPub, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	signed := &tbadmission.SignedCreditAttestation{Body: body, TrackerSig: sig}
	assert.False(t, VerifyCreditAttestation(otherPub, signed))
}

func TestSignCreditAttestation_NilBody(t *testing.T) {
	_, priv := fixtureKeypair(t)
	_, err := SignCreditAttestation(priv, nil)
	require.Error(t, err)
}

func TestSignCreditAttestation_WrongPrivKeyLen(t *testing.T) {
	body := fixtureAttestationBody()
	_, err := SignCreditAttestation(make(ed25519.PrivateKey, 16), body)
	require.Error(t, err)
}

func TestVerifyCreditAttestation_NilSigned(t *testing.T) {
	pub, _ := fixtureKeypair(t)
	assert.False(t, VerifyCreditAttestation(pub, nil))
}

func TestVerifyCreditAttestation_NilBody(t *testing.T) {
	pub, _ := fixtureKeypair(t)
	assert.False(t, VerifyCreditAttestation(pub, &tbadmission.SignedCreditAttestation{Body: nil, TrackerSig: bytes64(0x00)}))
}

func TestVerifyCreditAttestation_WrongSigLen(t *testing.T) {
	pub, _ := fixtureKeypair(t)
	signed := &tbadmission.SignedCreditAttestation{Body: fixtureAttestationBody(), TrackerSig: []byte{1, 2, 3}}
	assert.False(t, VerifyCreditAttestation(pub, signed))
}
```

- [ ] **Step 2: Run, confirm FAIL**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/shared
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./signing/...
```

Expected: compilation failure — `SignCreditAttestation`, `VerifyCreditAttestation` undefined.

- [ ] **Step 3: Append the helpers**

Append to `shared/signing/proto.go`:

```go
import (
	tbadmission "github.com/token-bay/token-bay/shared/admission"
)
```

(Add `tbadmission` to the existing import block.)

```go
// SignCreditAttestation returns an Ed25519 signature over
// DeterministicMarshal(body). Mirrors SignEnvelope semantics: returns an
// error on a wrong-length private key or nil body. Pre-condition: body
// has been validated by tbadmission.ValidateCreditAttestationBody — this
// helper does NOT re-run validation.
func SignCreditAttestation(priv ed25519.PrivateKey, body *tbadmission.CreditAttestationBody) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("signing: private key length %d, want %d", len(priv), ed25519.PrivateKeySize)
	}
	if body == nil {
		return nil, errors.New("signing: SignCreditAttestation on nil body")
	}
	buf, err := DeterministicMarshal(body)
	if err != nil {
		return nil, fmt.Errorf("signing: marshal credit attestation body: %w", err)
	}
	return ed25519.Sign(priv, buf), nil
}

// VerifyCreditAttestation reports whether signed.TrackerSig is a valid
// Ed25519 signature under pub over DeterministicMarshal(signed.Body).
// Nil inputs, malformed key/sig, or marshal failure all return false
// without panicking — same nil-safety contract as VerifyEnvelope.
func VerifyCreditAttestation(pub ed25519.PublicKey, signed *tbadmission.SignedCreditAttestation) bool {
	if signed == nil || signed.Body == nil {
		return false
	}
	buf, err := DeterministicMarshal(signed.Body)
	if err != nil {
		return false
	}
	return Verify(pub, buf, signed.TrackerSig)
}
```

- [ ] **Step 4: Run, confirm PASS**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/shared
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./signing/... ./admission/...
```

Expected: all tests PASS in both packages.

- [ ] **Step 5: Coverage check**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/shared
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 -coverprofile=coverage.out.raw ./...
grep -v '\.pb\.go:' coverage.out.raw > coverage.out
PATH="$HOME/.local/share/mise/shims:$PATH" go tool cover -func=coverage.out | grep -E 'admission/|signing/'
rm -f coverage.out coverage.out.raw
```

Expected: every function in `shared/admission/validate.go`, `shared/signing/proto.go` (the new `SignCreditAttestation` / `VerifyCreditAttestation` lines) reports ≥ 90% coverage. Generated `.pb.go` is excluded by the existing Makefile filter.

- [ ] **Step 6: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission
git add shared/signing/proto.go shared/signing/proto_test.go
git commit -m "$(cat <<'EOF'
feat(shared/signing): SignCreditAttestation / VerifyCreditAttestation

Sign helper signs DeterministicMarshal(body); verify helper recovers the
signed bytes the same way and reports a bool without panicking on nil
inputs or malformed keys/sigs.

Mirrors SignEntry/VerifyEntry exactly. All sign/verify of admission types
goes through this single canonical-bytes choke point per shared/CLAUDE.md
rule 6.

Tests: round-trip, tampered body, tampered sig, wrong pubkey, nil body,
wrong-length private key, nil signed, nil signed.Body, wrong-length sig.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 8: `shared/CLAUDE.md` — list `admission` alongside other proto packages

**Files:**
- Modify: `shared/CLAUDE.md`

The existing CLAUDE.md mentions `proto/` and `exhaustionproof/` in rule 5 ("Breaking change checklist") and in the package overview. We add `admission/` to those references so future contributors know the same breaking-change discipline applies.

- [ ] **Step 1: Inspect the current rules**

```bash
grep -n "exhaustionproof\|admission" shared/CLAUDE.md
```

Expected: `exhaustionproof` appears in rule 5; `admission` does not appear yet.

- [ ] **Step 2: Edit rule 5 to include `admission/`**

Find the existing rule 5 block in `shared/CLAUDE.md`. It begins:

```markdown
5. **Breaking change checklist.** Renaming or removing a field in `proto/` or `exhaustionproof/` requires:
```

Replace with:

```markdown
5. **Breaking change checklist.** Renaming or removing a field in `proto/`, `exhaustionproof/`, or `admission/` requires:
```

- [ ] **Step 3: Verify lint passes (no doc-only test changes)**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/shared
PATH="$HOME/.local/share/mise/shims:$PATH" make lint
```

Expected: golangci-lint passes (no Go files were touched; this confirms the doc edit didn't sneak into code).

- [ ] **Step 4: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission
git add shared/CLAUDE.md
git commit -m "$(cat <<'EOF'
docs(shared): list admission/ alongside other proto packages

Adds shared/admission to the breaking-change checklist in rule 5 so
contributors see the same discipline applies (update both consumers in
the same PR; bump module version comment; mark commit with `!` for any
breaking change).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 9: Tracker design spec amendments §11.1 / §11.2 / §11.3 + tracker CLAUDE.md race-list

**Files:**
- Modify: `docs/superpowers/specs/tracker/2026-04-22-tracker-design.md`
- Modify: `tracker/CLAUDE.md`

This task lands the three cross-cutting amendments admission-design §11 requires. The amendments describe new wire shapes that **do not yet exist as `.proto`** in this repo — when tracker control-plane proto materializes (separate plan), it will conform to what we describe here.

The §11.3 race-list amendment lands in **two places**: the tracker spec §6 gets a new bullet declaring the rule (spec is source of truth), and `tracker/CLAUDE.md` (which carries the operational version of the same rule today) gets the new package added to its existing line.

- [ ] **Step 1: Read the current §2.1, §5.1, §6 in tracker spec**

```bash
grep -n "^## 2\.\|^### 2\.1\|^## 5\.\|^### 5\.1\|^## 6\." docs/superpowers/specs/tracker/2026-04-22-tracker-design.md
```

Use the printed line numbers to navigate. Read the relevant sections via `Read` tool to confirm where the amendments fit.

- [ ] **Step 2: Amend §2.1 (heartbeat) — add the three optional fields**

Locate the existing `heartbeat()` description in §2.1 of the tracker spec. After its current field list, append the following amendment block (the 4-backtick wrapper is just so this plan can show the inner ```proto fence verbatim — when copying into the spec, paste the inner content only):

`````markdown
**Amendment (admission-design §11.1).** Heartbeat is extended with three optional fields that transport seeder headroom signal to the admission subsystem (admission-design §3.3):

```proto
// Added to existing tracker heartbeat message (proto numbers ≥100 reserved
// for forward-compatible additions; older seeders that omit these fields
// continue to work — tracker treats absence as "no change, use cached value").
optional uint32                            headroom_estimate = 100;  // fixed-point 0..10000
optional tokenbay.admission.v1.TickSource  source            = 101;  // HEURISTIC | USAGE_PROBE
optional uint32                            probe_age_s       = 102;  // 0 if HEURISTIC
```

`TickSource` is defined in `shared/admission/admission.proto` (this plan, branch `tracker_admission_shared`). The actual heartbeat proto file does not yet exist; this amendment locks the field shape so the eventual heartbeat proto and the existing admission proto are forward-consistent.
`````

- [ ] **Step 3: Amend §5.1 (broker_request response) — convert to oneof**

Locate the existing description of the `broker_request` response in §5.1 (currently returns `seeder_assignment` or `NO_CAPACITY`). After that description, append (4-backtick wrapper rule from Step 2 applies — paste the inner content only into the spec):

`````markdown
**Amendment (admission-design §11.2).** `broker_request` response becomes a `oneof`. Existing alternatives `seeder_assignment` and `no_capacity` remain valid; admission introduces two new alternatives:

```proto
message BrokerRequestResponse {
  oneof outcome {
    SeederAssignment seeder_assignment = 1;  // existing — admitted, broker picked a seeder
    NoCapacity       no_capacity       = 2;  // existing — broker had no eligible seeder
    Queued           queued            = 3;  // new — admission decided to queue
    Rejected         rejected          = 4;  // new — admission decided to reject
  }
}

message Queued {
  bytes        request_id    = 1;  // 16 bytes (UUID)
  PositionBand position_band = 2;
  EtaBand      eta_band      = 3;
}

message Rejected {
  RejectReason reason          = 1;
  uint32       retry_after_s   = 2;  // seconds; clamped [60, 600] with ±50% jitter (admission-design §5.4, §8.5)
}

enum PositionBand {
  POSITION_BAND_UNSPECIFIED = 0;
  POSITION_BAND_1_TO_10     = 1;
  POSITION_BAND_11_TO_50    = 2;
  POSITION_BAND_51_TO_200   = 3;
  POSITION_BAND_200_PLUS    = 4;
}

enum EtaBand {
  ETA_BAND_UNSPECIFIED  = 0;
  ETA_BAND_LT_30S       = 1;
  ETA_BAND_30S_TO_2M    = 2;
  ETA_BAND_2M_TO_5M     = 3;
  ETA_BAND_5M_PLUS      = 4;
}

enum RejectReason {
  REJECT_REASON_UNSPECIFIED      = 0;
  REJECT_REASON_REGION_OVERLOADED = 1;
  REJECT_REASON_QUEUE_TIMEOUT     = 2;
}
```

Receiving plugins MUST handle all four `oneof` cases. The bands replace exact position/eta values to limit information leakage (admission-design §6.5, §8.5). The four messages and three enums above will live in `shared/admission/` (or a sibling tracker-control-plane package) when the corresponding proto file is implemented; the field numbers here are normative and locked by this amendment.
`````

- [ ] **Step 4: Amend tracker spec §6 (concurrency model) — add a race-list bullet**

The tracker spec §6 currently has bullets describing runtime/registry/ledger/broker concurrency, but **does not yet contain an explicit `-race` list**. Add a new bullet at the end of the existing §6 bullet list:

```markdown
- The following packages MUST pass `go test -race` (admission-design §11.3): `tracker/internal/broker`, `tracker/internal/federation`, `tracker/internal/admission`. Failures under `-race` in these packages are always real bugs and block merge.
```

This declares the rule in the spec (source of truth) and names admission alongside the existing two race-mandatory packages. The operational version in `tracker/CLAUDE.md` is updated in the next step so it stays consistent with the spec.

- [ ] **Step 5: Amend `tracker/CLAUDE.md` race-list line**

Open `tracker/CLAUDE.md`. Find the existing line at approximately line 57 (verify with `grep -n "internal/broker" tracker/CLAUDE.md`):

```markdown
- `internal/broker` and `internal/federation` are the most concurrent modules. Run `go test -race` by default; flakes there are always real bugs.
```

Replace with:

```markdown
- `internal/broker`, `internal/federation`, and `internal/admission` are the most concurrent modules. Run `go test -race` by default; flakes there are always real bugs.
```

- [ ] **Step 6: Verify the spec still renders cleanly**

```bash
grep -c "Amendment (admission-design" docs/superpowers/specs/tracker/2026-04-22-tracker-design.md
```

Expected: 2 (one for §11.1, one for §11.2). The §11.3 race-list addition is a one-line bullet inside the spec §6 body without an "Amendment" prefix.

```bash
grep -n "internal/admission" docs/superpowers/specs/tracker/2026-04-22-tracker-design.md tracker/CLAUDE.md
```

Expected: at least one match in tracker spec §6 and one match in `tracker/CLAUDE.md` line ~57.

- [ ] **Step 7: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission
git add docs/superpowers/specs/tracker/2026-04-22-tracker-design.md tracker/CLAUDE.md
git commit -m "$(cat <<'EOF'
spec(tracker): cross-cutting amendments from admission-design §11

§2.1 heartbeat — three new optional fields (headroom_estimate, source,
probe_age_s) at proto numbers 100-102. Backwards-compatible; tracker
treats absence as "no change."

§5.1 broker_request response — oneof with new Queued / Rejected variants
alongside existing SeederAssignment / NoCapacity. Position and eta are
banded (admission-design §6.5) to limit info leakage; retry_after_s is
clamped [60, 600] with ±50% jitter (§5.4, §8.5).

§6 race-list — internal/admission joins internal/broker and
internal/federation on the mandatory `-race` list. Spec §6 gets a new
bullet declaring the rule; tracker/CLAUDE.md (which carries the
operational version of the same rule) is updated to match.

Tracker control-plane proto does not yet exist in the repo; this
amendment locks the field/enum shapes so the eventual proto file is
forward-consistent with shared/admission already on disk.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 10: Repo-root verification — `make check` clean

**Files:** none modified. Final integration check.

- [ ] **Step 1: `make check` from repo root**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission
PATH="$HOME/.local/share/mise/shims:$PATH" make check
```

Expected: every module's tests pass under `-race`; lint passes across all modules.

- [ ] **Step 2: `make proto-check` to confirm the schema is in sync with the generated `.pb.go`**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/shared
PATH="$HOME/.local/share/mise/shims:$PATH" make proto-check
```

Expected: exits 0 — `git diff` on `.pb.go` files is empty after regeneration.

- [ ] **Step 3: Coverage spot-check**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/shared
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 -coverprofile=coverage.out.raw ./admission/... ./signing/...
grep -v '\.pb\.go:' coverage.out.raw > coverage.out
PATH="$HOME/.local/share/mise/shims:$PATH" go tool cover -func=coverage.out | tail -20
rm -f coverage.out coverage.out.raw
```

Expected: `total:` line shows ≥ 90% for the admission + signing additions (admission-design §6.3 coverage target).

- [ ] **Step 4: Push the branch**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission
git push -u origin tracker_admission_shared
```

(Or whatever branch name was used — see Conventions §2.)

- [ ] **Step 5: Open PR — title and body**

```
title: feat(shared/admission): wire-format types + sign/verify + tracker-spec amendments

body:
Implements admission-design §4.1 wire types, §6.1 validation, §6.2 sign/verify,
§6.3 round-trip + golden-bytes + nil-safety tests, plus the §11 cross-cutting
amendments to docs/superpowers/specs/tracker/2026-04-22-tracker-design.md.

Tracker control-plane proto (heartbeat, broker_request) still does not exist
on disk; the §11 amendments describe the eventual shape so future tracker
proto work conforms to what `shared/admission/` already supports.

Plan: docs/superpowers/plans/2026-04-25-shared-admission-and-amendments.md
Spec: docs/superpowers/specs/admission/2026-04-25-admission-design.md

Test plan:
- [ ] make check from repo root, no failures
- [ ] make proto-check, no diff
- [ ] coverage on shared/admission + signing additions ≥ 90%
- [ ] golden-bytes test stable across two consecutive runs
```

---

## Coverage of admission-design §6.3 required tests

| Required (§6.3) | Implemented in |
|---|---|
| #1 Round-trip for all messages + every TickSource value | Task 2 |
| #2 Byte-stable golden fixture | Task 6 |
| #3 Sign/verify positive + tampered (body, sig, pubkey) | Task 7 |
| #4 Validation rejection matrix per §6.1 invariant | Tasks 3, 5 |
| #5 Nil-safety: Sign nil body, Verify nil signed, Verify {Body: nil} | Task 7 |
| ≥ 90% coverage on `shared/admission` + `shared/signing` additions | Task 7 step 5, Task 10 step 3 |

## Coverage of admission-design §11 amendments

| Amendment | Implemented in |
|---|---|
| §11.1 heartbeat headroom_estimate / source / probe_age_s | Task 9 step 2 |
| §11.2 broker_request response oneof — Queued / Rejected + bands + reason | Task 9 step 3 |
| §11.3 internal/admission on mandatory `-race` list | Task 9 step 4 |
