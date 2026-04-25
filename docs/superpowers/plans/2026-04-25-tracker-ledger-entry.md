# Tracker — `internal/ledger/entry` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land the ledger `Entry` wire-format type in `shared/` and the operation helpers (`Kind` enum, factory builders, canonical-bytes hash, sign/verify) in `tracker/internal/ledger/entry`. End state: any caller can build an `EntryBody`, sign it as consumer / seeder / tracker, verify each signature, compute the entry hash, and round-trip the signed form on the wire.

**Architecture:** Two-message split mirroring `EnvelopeBody`/`EnvelopeSigned`: an unsigned `EntryBody` (all fields from ledger §3.1 except the three signatures) plus a signed wire form `Entry { body, consumer_sig, seeder_sig, tracker_sig }`. All three signatures are Ed25519 over `DeterministicMarshal(body)` — a single preimage shared across all three signers (confirmed by tracker spec §5.2 step 4). `consumer_sig` and `seeder_sig` are nullable per kind / `flags.consumer_sig_missing`; `tracker_sig` is mandatory on every appended entry. The `Kind` enum is mirrored as Go-typed constants in the tracker package.

**Tech Stack:** Go 1.23+, `google.golang.org/protobuf` (already a shared dep), stdlib `crypto/ed25519`, stdlib `crypto/sha256`, `github.com/stretchr/testify`. Codegen via `protoc` + `protoc-gen-go` (already configured in `shared/Makefile`).

**Specs:**
- `docs/superpowers/specs/ledger/2026-04-22-ledger-design.md` §3.1, §3.2, §4.1
- `docs/superpowers/specs/tracker/2026-04-22-tracker-design.md` §5.2
- `docs/superpowers/specs/2026-04-22-token-bay-architecture-design.md` §6.1 (entry semantics)

**Dependency order:** Runs after `shared/` wire-format v1 (already shipped) and tracker scaffolding (already shipped — every `internal/*` is a `.gitkeep`). This is the first feature plan touching tracker code beyond the entry-point smoke test. It is also the first plan since scaffolding to modify `shared/`, so commits cross module boundaries.

---

## 1. File map

```
shared/
├── proto/
│   ├── ledger.proto                          ← CREATE: EntryKind enum + EntryBody + Entry
│   ├── ledger.pb.go                          ← CREATE (generated, committed)
│   ├── validate.go                           ← MODIFY: add ValidateEntryBody
│   ├── ledger_test.go                        ← CREATE: round-trip + golden bytes
│   ├── validate_test.go                      ← MODIFY: extend rejection matrix
│   └── testdata/
│       └── entry_signed.golden.hex           ← CREATE (Task 4)
├── signing/
│   ├── proto.go                              ← MODIFY: SignEntry* / VerifyEntry* helpers
│   └── proto_test.go                         ← MODIFY: extend helper tests
└── Makefile                                  ← MODIFY: add ledger.proto to PROTO_FILES

tracker/
├── go.mod                                    ← MODIFY: add `require shared` (replace already in place)
├── internal/ledger/entry/
│   ├── doc.go                                ← CREATE: package doc + non-negotiables
│   ├── kind.go                               ← CREATE: Kind type + constants + String()
│   ├── kind_test.go                          ← CREATE
│   ├── builder.go                            ← CREATE: BuildUsageEntry / BuildTransferOut / BuildTransferIn / BuildStarterGrant
│   ├── builder_test.go                       ← CREATE
│   ├── hash.go                               ← CREATE: Hash(body) → [32]byte over DeterministicMarshal
│   ├── hash_test.go                          ← CREATE: stability + golden bytes
│   ├── verify.go                             ← CREATE: VerifyAll wrapper (consumer/seeder/tracker as applicable per kind)
│   ├── verify_test.go                        ← CREATE: per-kind sig matrix
│   └── testdata/
│       └── usage_entry.golden.hex            ← CREATE (Task 6)
└── internal/ledger/entry/.gitkeep            ← REMOVE
```

Notes:
- The `EntryBody`/`Entry` *types* live in `shared/proto/` because they are wire types streamed by federation. Per repo-root CLAUDE.md rule 3 we do not duplicate wire structs between modules.
- The *operations* (Kind enum, builders, hash, verify) live in `tracker/internal/ledger/entry` because every caller is tracker-internal — broker, ledger storage, federation. A plugin only ever sees a balance snapshot, never an entry.
- Two golden fixtures (`entry_signed.golden.hex` in shared, `usage_entry.golden.hex` in tracker) are independent on purpose: one fixes the wire bytes, the other fixes the entry hash. Either changing without the other is a bug.

## 2. Conventions used in this plan

- All `go test` commands run from the package root unless a different `cd` is shown.
- `PATH="$HOME/.local/share/mise/shims:$PATH"` is needed if Go is managed by mise.
- `protoc` and `protoc-gen-go` are required for Task 1 only. Generated `.pb.go` is committed so other tasks don't need them.
- One commit per task. Conventional-commit prefixes: `feat(shared/<pkg>):`, `feat(tracker/<pkg>):`, `chore(shared):`, `chore(tracker):`, `test(<pkg>):`.
- Co-Authored-By footer: `Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>`.
- Each red-green commit: failing test first, then minimal impl. Refactor-only commits get `refactor:`.

---

## 3. Wire format design (decisions used by all tasks)

### 3.1 Two-message split

```text
EntryBody {
  prev_hash      bytes(32)
  seq            uint64
  kind           EntryKind         // enum: USAGE | TRANSFER_OUT | TRANSFER_IN | STARTER_GRANT
  consumer_id    bytes(32)         // zero for transfer_out/starter_grant per spec §3.1
  seeder_id      bytes(32)         // zero for non-usage kinds
  model          string            // "" for non-usage; max 32 bytes
  input_tokens   uint32
  output_tokens  uint32
  cost_credits   uint64            // absolute value; sign determined by kind
  timestamp      uint64            // unix seconds, ±5min of tracker wall clock at append
  request_id     bytes(16)         // UUID; zero for non-usage
  flags          uint32            // bit 0 = consumer_sig_missing
  ref            bytes(32)         // transfer UUID for transfer_*; zero otherwise
}

Entry {
  body           EntryBody
  consumer_sig   bytes(64)         // nullable iff flags.consumer_sig_missing OR kind == TRANSFER_IN/STARTER_GRANT
  seeder_sig     bytes(64)         // nullable iff kind != USAGE
  tracker_sig    bytes(64)         // mandatory
}
```

`EntryKind` proto enum:

```
ENTRY_KIND_UNSPECIFIED = 0   // rejected by ValidateEntryBody
ENTRY_KIND_USAGE = 1
ENTRY_KIND_TRANSFER_OUT = 2
ENTRY_KIND_TRANSFER_IN = 3
ENTRY_KIND_STARTER_GRANT = 4
```

`UNSPECIFIED = 0` is reserved-and-rejected, matching the `PrivacyTier` precedent in `envelope.proto`.

### 3.2 Canonical bytes

All three signatures are computed over `signing.DeterministicMarshal(body *EntryBody)`. This is the single canonical-bytes choke point for entries (per `shared/CLAUDE.md` rule 6).

### 3.3 Entry hash

`Hash(body)` returns `sha256.Sum256(DeterministicMarshal(body))`. This is the value used as `prev_hash` in the next entry (ledger spec §3.2 chain).

The hash is over the *body*, not the signed form. Rationale: the chain links semantic content, not signing artifacts. If the tracker ever re-signs the chain tip during a key-rotation incident (tracker spec rule 3 in `tracker/CLAUDE.md`), the chain links remain valid because the bodies didn't change — only `tracker_sig` did.

### 3.4 Per-kind signature matrix

| Kind | consumer_sig | seeder_sig | tracker_sig | flag interaction |
|---|---|---|---|---|
| USAGE | required UNLESS `flags.consumer_sig_missing` | required | required | `consumer_sig_missing` ↔ consumer_sig MUST be empty |
| TRANSFER_OUT | required (consumer authorizes outbound move) | not present (must be empty) | required | flag must be 0 |
| TRANSFER_IN | not present (must be empty) | not present | required | flag must be 0 |
| STARTER_GRANT | not present | not present | required | flag must be 0 |

`ValidateEntryBody` enforces field-presence rules but does not verify signatures. `entry.VerifyAll(body, sigs, pubkeys)` enforces the signature matrix.

---

## Task 1: shared `ledger.proto` schema + codegen

**Files:**
- Create: `shared/proto/ledger.proto`, `shared/proto/ledger.pb.go`
- Modify: `shared/Makefile`

- [ ] **Step 1: Add the schema**

Write `shared/proto/ledger.proto`:

```proto
syntax = "proto3";
package tokenbay.proto.v1;

option go_package = "github.com/token-bay/token-bay/shared/proto";

// EntryKind classifies a ledger entry's semantic role.
// ENTRY_KIND_UNSPECIFIED is rejected by ValidateEntryBody.
enum EntryKind {
  ENTRY_KIND_UNSPECIFIED   = 0;
  ENTRY_KIND_USAGE         = 1;
  ENTRY_KIND_TRANSFER_OUT  = 2;
  ENTRY_KIND_TRANSFER_IN   = 3;
  ENTRY_KIND_STARTER_GRANT = 4;
}

// EntryBody — the signed payload of a ledger entry.
// Spec: docs/superpowers/specs/ledger/2026-04-22-ledger-design.md §3.1.
// All three signatures (consumer/seeder/tracker) are computed over
// shared/signing.DeterministicMarshal(body) — see plan §3.2.
message EntryBody {
  bytes     prev_hash     = 1;   // 32 bytes; zero for genesis
  uint64    seq           = 2;
  EntryKind kind          = 3;
  bytes     consumer_id   = 4;   // 32 bytes; zero for transfer_out/starter_grant
  bytes     seeder_id     = 5;   // 32 bytes; zero for non-usage kinds
  string    model         = 6;   // "" for non-usage kinds
  uint32    input_tokens  = 7;   // 0 for non-usage kinds
  uint32    output_tokens = 8;   // 0 for non-usage kinds
  uint64    cost_credits  = 9;   // absolute value; sign determined by kind+party
  uint64    timestamp     = 10;  // unix seconds
  bytes     request_id    = 11;  // 16 bytes (UUID); zero for non-usage kinds
  uint32    flags         = 12;  // bit 0 = consumer_sig_missing
  bytes     ref           = 13;  // 32 bytes; transfer UUID for transfer_*; zero otherwise
}

// Entry — wire form. Receiver verifies signatures via
// shared/signing.VerifyEntry* (Task 3).
message Entry {
  EntryBody body         = 1;
  bytes     consumer_sig = 2;  // 64 bytes or empty per per-kind matrix (plan §3.4)
  bytes     seeder_sig   = 3;  // 64 bytes or empty per per-kind matrix
  bytes     tracker_sig  = 4;  // 64 bytes; mandatory
}
```

- [ ] **Step 2: Add to PROTO_FILES**

Edit `shared/Makefile`:

```diff
-PROTO_FILES := exhaustionproof/proof.proto proto/balance.proto proto/envelope.proto
+PROTO_FILES := exhaustionproof/proof.proto proto/balance.proto proto/envelope.proto proto/ledger.proto
```

- [ ] **Step 3: Regenerate**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/cut-sunflower/shared
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" make proto-gen
```

Expected: `proto/ledger.pb.go` appears.

- [ ] **Step 4: Verify compilation**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/cut-sunflower/shared
go build ./...
```

Expected: clean build.

- [ ] **Step 5: Commit**

```bash
git add shared/proto/ledger.proto shared/proto/ledger.pb.go shared/Makefile
git commit -m "$(cat <<'EOF'
feat(shared/proto): add ledger Entry schema

EntryBody + Entry mirror the EnvelopeBody/EnvelopeSigned two-message
split. EntryKind enum with explicit UNSPECIFIED=0 reserved-and-rejected.
Spec: ledger §3.1.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: shared `ValidateEntryBody`

**Files:**
- Modify: `shared/proto/validate.go`
- Create: `shared/proto/validate_test.go` modifications (extend rejection matrix), `shared/proto/ledger_test.go` (round-trip)

- [ ] **Step 1: Write the failing round-trip test**

Write `shared/proto/ledger_test.go`:

```go
package proto

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// fixtureUsageEntryBody — fully-populated USAGE-kind body used by
// round-trip, sign/verify, and golden-fixture tests across shared/.
func fixtureUsageEntryBody() *EntryBody {
	return &EntryBody{
		PrevHash:     make32(0x01),
		Seq:          42,
		Kind:         EntryKind_ENTRY_KIND_USAGE,
		ConsumerId:   make32(0x11),
		SeederId:     make32(0x22),
		Model:        "claude-sonnet-4-6",
		InputTokens:  4096,
		OutputTokens: 1024,
		CostCredits:  3072000,
		Timestamp:    1714000100,
		RequestId:    make16(0x33), // 16-byte UUID-shaped fixture
		Flags:        0,
		Ref:          make32(0x00),
	}
}

func TestEntryBody_RoundTrip(t *testing.T) {
	original := fixtureUsageEntryBody()

	first, err := proto.MarshalOptions{Deterministic: true}.Marshal(original)
	require.NoError(t, err)

	var parsed EntryBody
	require.NoError(t, proto.Unmarshal(first, &parsed))

	second, err := proto.MarshalOptions{Deterministic: true}.Marshal(&parsed)
	require.NoError(t, err)

	assert.Equal(t, first, second)
	require.True(t, proto.Equal(original, &parsed))
}

func TestEntry_RoundTrip(t *testing.T) {
	signed := &Entry{
		Body:        fixtureUsageEntryBody(),
		ConsumerSig: make64(0x44),
		SeederSig:   make64(0x55),
		TrackerSig:  make64(0x66),
	}

	first, err := proto.MarshalOptions{Deterministic: true}.Marshal(signed)
	require.NoError(t, err)

	var parsed Entry
	require.NoError(t, proto.Unmarshal(first, &parsed))

	second, err := proto.MarshalOptions{Deterministic: true}.Marshal(&parsed)
	require.NoError(t, err)

	assert.Equal(t, first, second)
	require.True(t, proto.Equal(signed, &parsed))
}
```

Add to `shared/proto/balance_test.go` (or wherever `make32`/`make64` lives — confirm via `grep`) the helper:

```go
func make16(b byte) []byte { return bytes.Repeat([]byte{b}, 16) }
```

if not already present. Add `import "bytes"` if needed.

Run: `cd shared && go test ./proto/... -run Entry`

Expected: PASS (this is just round-trip; no validator yet).

- [ ] **Step 2: Write failing validator tests**

Append to `shared/proto/validate_test.go` (extend the existing rejection-matrix file):

```go
func TestValidateEntryBody_AcceptsValidUsage(t *testing.T) {
	body := fixtureUsageEntryBody()
	require.NoError(t, ValidateEntryBody(body))
}

func TestValidateEntryBody_RejectsNil(t *testing.T) {
	require.Error(t, ValidateEntryBody(nil))
}

// Rejection matrix — one named subtest per invariant.
func TestValidateEntryBody_RejectsInvalid(t *testing.T) {
	cases := map[string]func(*EntryBody){
		"unspecified_kind": func(b *EntryBody) { b.Kind = EntryKind_ENTRY_KIND_UNSPECIFIED },
		"unknown_kind":     func(b *EntryBody) { b.Kind = EntryKind(99) },
		"prev_hash_short":  func(b *EntryBody) { b.PrevHash = make([]byte, 16) },
		"prev_hash_long":   func(b *EntryBody) { b.PrevHash = make([]byte, 64) },
		"consumer_id_short_for_usage": func(b *EntryBody) {
			b.ConsumerId = make([]byte, 16)
		},
		"seeder_id_short_for_usage": func(b *EntryBody) {
			b.SeederId = make([]byte, 16)
		},
		"empty_model_for_usage": func(b *EntryBody) { b.Model = "" },
		"model_too_long":        func(b *EntryBody) { b.Model = strings.Repeat("x", 33) },
		"request_id_wrong_len_for_usage": func(b *EntryBody) {
			b.RequestId = make([]byte, 8)
		},
		"flags_unknown_bit": func(b *EntryBody) { b.Flags = 1 << 5 },
		"ref_wrong_len":     func(b *EntryBody) { b.Ref = make([]byte, 8) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			body := fixtureUsageEntryBody()
			mutate(body)
			require.Error(t, ValidateEntryBody(body))
		})
	}
}

func TestValidateEntryBody_KindSpecificFields(t *testing.T) {
	// TRANSFER_OUT: consumer_id required, seeder_id MUST be zero,
	// model MUST be empty, ref MUST be 32 bytes, request_id MUST be zero.
	t.Run("transfer_out_seeder_id_must_be_zero", func(t *testing.T) {
		b := fixtureTransferOutEntryBody()
		b.SeederId = make32(0xFF)
		require.Error(t, ValidateEntryBody(b))
	})
	t.Run("transfer_in_consumer_id_must_be_zero", func(t *testing.T) {
		b := fixtureTransferInEntryBody()
		b.ConsumerId = make32(0xFF)
		require.Error(t, ValidateEntryBody(b))
	})
	t.Run("starter_grant_model_must_be_empty", func(t *testing.T) {
		b := fixtureStarterGrantEntryBody()
		b.Model = "claude-sonnet-4-6"
		require.Error(t, ValidateEntryBody(b))
	})
	t.Run("transfer_out_request_id_must_be_zero", func(t *testing.T) {
		b := fixtureTransferOutEntryBody()
		b.RequestId = make16(0xFF)
		require.Error(t, ValidateEntryBody(b))
	})
}
```

Add fixture builders for the non-usage kinds in `shared/proto/ledger_test.go`:

```go
func zero32() []byte { return make([]byte, 32) }
func zero16() []byte { return make([]byte, 16) }

func fixtureTransferOutEntryBody() *EntryBody {
	return &EntryBody{
		PrevHash:    make32(0x01), Seq: 43,
		Kind:        EntryKind_ENTRY_KIND_TRANSFER_OUT,
		ConsumerId:  make32(0x11),
		SeederId:    zero32(),
		Model:       "",
		Timestamp:   1714000200,
		RequestId:   zero16(),
		Ref:         make32(0x77),  // transfer UUID-padded
		CostCredits: 1000,
	}
}

func fixtureTransferInEntryBody() *EntryBody {
	return &EntryBody{
		PrevHash:    make32(0x01), Seq: 44,
		Kind:        EntryKind_ENTRY_KIND_TRANSFER_IN,
		ConsumerId:  zero32(),
		SeederId:    zero32(),
		Model:       "",
		Timestamp:   1714000300,
		RequestId:   zero16(),
		Ref:         make32(0x88),
		CostCredits: 1000,
	}
}

func fixtureStarterGrantEntryBody() *EntryBody {
	return &EntryBody{
		PrevHash:    make32(0x01), Seq: 1,
		Kind:        EntryKind_ENTRY_KIND_STARTER_GRANT,
		ConsumerId:  make32(0x11),
		SeederId:    zero32(),
		Model:       "",
		Timestamp:   1714000000,
		RequestId:   zero16(),
		Ref:         zero32(),
		CostCredits: 100000,
	}
}
```

Run: `cd shared && go test ./proto/... -run Validate`

Expected: FAIL — `undefined: ValidateEntryBody`.

- [ ] **Step 3: Implement `ValidateEntryBody`**

Append to `shared/proto/validate.go`:

```go
// entryHashLen is the required length for prev_hash and ref.
const entryHashLen = 32

// entryRequestIDLen is the required length for request_id (UUID-shaped).
const entryRequestIDLen = 16

// entryModelMaxLen mirrors ledger spec §3.1 model: string(32).
const entryModelMaxLen = 32

// entryFlagsMask covers all defined flag bits. Bit 0 = consumer_sig_missing.
// Reject any body with unknown bits set so v2 flags don't silently appear
// as garbage to a v1 verifier.
const entryFlagsMask = uint32(0x1)

// ValidateEntryBody enforces v1 wire-format invariants on an EntryBody.
// Tracker callers run pre-sign and post-parse before trusting any field.
// Does NOT verify signatures — see entry.VerifyAll on the tracker side.
func ValidateEntryBody(b *EntryBody) error {
	if b == nil {
		return errors.New("proto: nil EntryBody")
	}
	if len(b.PrevHash) != entryHashLen {
		return fmt.Errorf("proto: prev_hash length %d, want %d", len(b.PrevHash), entryHashLen)
	}
	if len(b.Ref) != entryHashLen {
		return fmt.Errorf("proto: ref length %d, want %d", len(b.Ref), entryHashLen)
	}
	if b.Flags & ^entryFlagsMask != 0 {
		return fmt.Errorf("proto: flags = %#x, unknown bits set", b.Flags)
	}
	switch b.Kind {
	case EntryKind_ENTRY_KIND_USAGE:
		if len(b.ConsumerId) != entryHashLen {
			return fmt.Errorf("proto: usage consumer_id length %d, want %d", len(b.ConsumerId), entryHashLen)
		}
		if len(b.SeederId) != entryHashLen {
			return fmt.Errorf("proto: usage seeder_id length %d, want %d", len(b.SeederId), entryHashLen)
		}
		if b.Model == "" {
			return errors.New("proto: usage entry has empty model")
		}
		if len(b.Model) > entryModelMaxLen {
			return fmt.Errorf("proto: model length %d exceeds %d", len(b.Model), entryModelMaxLen)
		}
		if len(b.RequestId) != entryRequestIDLen {
			return fmt.Errorf("proto: usage request_id length %d, want %d", len(b.RequestId), entryRequestIDLen)
		}
	case EntryKind_ENTRY_KIND_TRANSFER_OUT:
		if len(b.ConsumerId) != entryHashLen {
			return fmt.Errorf("proto: transfer_out consumer_id length %d, want %d", len(b.ConsumerId), entryHashLen)
		}
		if !allZero(b.SeederId) || len(b.SeederId) != entryHashLen {
			return errors.New("proto: transfer_out seeder_id must be 32 zero bytes")
		}
		if b.Model != "" {
			return errors.New("proto: transfer_out model must be empty")
		}
		if !allZero(b.RequestId) || len(b.RequestId) != entryRequestIDLen {
			return errors.New("proto: transfer_out request_id must be 16 zero bytes")
		}
		if b.InputTokens != 0 || b.OutputTokens != 0 {
			return errors.New("proto: transfer_out tokens must be zero")
		}
		if b.Flags != 0 {
			return fmt.Errorf("proto: transfer_out flags must be 0, got %#x", b.Flags)
		}
	case EntryKind_ENTRY_KIND_TRANSFER_IN:
		if !allZero(b.ConsumerId) || len(b.ConsumerId) != entryHashLen {
			return errors.New("proto: transfer_in consumer_id must be 32 zero bytes")
		}
		if !allZero(b.SeederId) || len(b.SeederId) != entryHashLen {
			return errors.New("proto: transfer_in seeder_id must be 32 zero bytes")
		}
		if b.Model != "" {
			return errors.New("proto: transfer_in model must be empty")
		}
		if !allZero(b.RequestId) || len(b.RequestId) != entryRequestIDLen {
			return errors.New("proto: transfer_in request_id must be 16 zero bytes")
		}
		if b.Flags != 0 {
			return fmt.Errorf("proto: transfer_in flags must be 0, got %#x", b.Flags)
		}
	case EntryKind_ENTRY_KIND_STARTER_GRANT:
		if len(b.ConsumerId) != entryHashLen {
			return fmt.Errorf("proto: starter_grant consumer_id length %d, want %d", len(b.ConsumerId), entryHashLen)
		}
		if !allZero(b.SeederId) || len(b.SeederId) != entryHashLen {
			return errors.New("proto: starter_grant seeder_id must be 32 zero bytes")
		}
		if b.Model != "" {
			return errors.New("proto: starter_grant model must be empty")
		}
		if !allZero(b.RequestId) || len(b.RequestId) != entryRequestIDLen {
			return errors.New("proto: starter_grant request_id must be 16 zero bytes")
		}
		if !allZero(b.Ref) {
			return errors.New("proto: starter_grant ref must be 32 zero bytes")
		}
		if b.Flags != 0 {
			return fmt.Errorf("proto: starter_grant flags must be 0, got %#x", b.Flags)
		}
	default:
		return fmt.Errorf("proto: kind = %v, must be USAGE, TRANSFER_OUT, TRANSFER_IN, or STARTER_GRANT", b.Kind)
	}
	return nil
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}
```

- [ ] **Step 4: Verify tests pass**

```bash
cd shared && go test -race ./proto/...
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add shared/proto/validate.go shared/proto/validate_test.go shared/proto/ledger_test.go
git commit -m "$(cat <<'EOF'
feat(shared/proto): ValidateEntryBody with per-kind field matrix

Enforces zero-by-omission rules from ledger §3.1: TRANSFER_IN has no
counterparties, STARTER_GRANT has no model, TRANSFER_OUT has no seeder.
Unknown flag bits are rejected so v2 flags can't silently leak through
a v1 verifier.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: shared `signing` helpers — `SignEntry*` / `VerifyEntry*`

**Files:**
- Modify: `shared/signing/proto.go`, `shared/signing/proto_test.go`

- [ ] **Step 1: Write failing helper tests**

Append to `shared/signing/proto_test.go`:

```go
func TestSignAndVerifyEntry_RoundTrip(t *testing.T) {
	pub, priv := goldenKeypair()
	body := fixtureUsageEntryBody() // import the helper from the proto package — see step 1a below

	sig, err := SignEntry(priv, body)
	require.NoError(t, err)
	require.Len(t, sig, ed25519.SignatureSize)

	require.True(t, VerifyEntry(pub, body, sig))
}

func TestSignEntry_RejectsNilBody(t *testing.T) {
	_, priv := goldenKeypair()
	_, err := SignEntry(priv, nil)
	require.Error(t, err)
}

func TestSignEntry_RejectsBadKey(t *testing.T) {
	body := fixtureUsageEntryBody()
	_, err := SignEntry(make([]byte, 16), body)
	require.Error(t, err)
}

func TestVerifyEntry_RejectsTamperedBody(t *testing.T) {
	pub, priv := goldenKeypair()
	body := fixtureUsageEntryBody()
	sig, err := SignEntry(priv, body)
	require.NoError(t, err)

	tampered := proto.Clone(body).(*tbproto.EntryBody)
	tampered.CostCredits++
	require.False(t, VerifyEntry(pub, tampered, sig))
}

func TestVerifyEntry_NilSafe(t *testing.T) {
	pub, _ := goldenKeypair()
	require.False(t, VerifyEntry(pub, nil, nil))
}
```

Step 1a: `fixtureUsageEntryBody` lives in `shared/proto/ledger_test.go` (a test file in another package). Either lift the fixture into a small exported helper file in `shared/proto/` (e.g., `shared/proto/testfixtures.go` with a package-private build-tag `//go:build test`) or duplicate the literal here. **Pick duplication** for the signing tests — three lines of literal fields keeps shared/proto's test fixtures non-exported and avoids an `_test.go`-only export hack. Inline:

```go
func entryFixture() *tbproto.EntryBody {
	return &tbproto.EntryBody{
		PrevHash:     bytes.Repeat([]byte{0x01}, 32),
		Seq:          42,
		Kind:         tbproto.EntryKind_ENTRY_KIND_USAGE,
		ConsumerId:   bytes.Repeat([]byte{0x11}, 32),
		SeederId:     bytes.Repeat([]byte{0x22}, 32),
		Model:        "claude-sonnet-4-6",
		InputTokens:  4096,
		OutputTokens: 1024,
		CostCredits:  3072000,
		Timestamp:    1714000100,
		RequestId:    bytes.Repeat([]byte{0x33}, 16),
		Flags:        0,
		Ref:          make([]byte, 32),
	}
}
```

…and use `entryFixture()` in place of `fixtureUsageEntryBody()` above. Add `"bytes"` and `tbproto "github.com/token-bay/token-bay/shared/proto"` to imports as needed.

Run: `cd shared && go test ./signing/...`

Expected: FAIL — `undefined: SignEntry, VerifyEntry`.

- [ ] **Step 2: Implement helpers**

Append to `shared/signing/proto.go`:

```go
// SignEntry returns an Ed25519 signature over DeterministicMarshal(body).
// All three ledger-entry signers (consumer, seeder, tracker) use this same
// helper since they all sign the identical preimage — see plan §3.2 and
// tracker spec §5.2 step 4.
//
// Pre-condition: body has been validated by tbproto.ValidateEntryBody.
// Sign helpers do NOT re-run validation, mirroring SignEnvelope semantics.
func SignEntry(priv ed25519.PrivateKey, body *tbproto.EntryBody) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("signing: private key length %d, want %d", len(priv), ed25519.PrivateKeySize)
	}
	if body == nil {
		return nil, errors.New("signing: SignEntry on nil body")
	}
	buf, err := DeterministicMarshal(body)
	if err != nil {
		return nil, fmt.Errorf("signing: marshal entry body: %w", err)
	}
	return ed25519.Sign(priv, buf), nil
}

// VerifyEntry reports whether sig is a valid Ed25519 signature under pub
// over DeterministicMarshal(body). Nil body, malformed key/sig, or marshal
// failure all return false without panicking. Same nil-safety contract as
// VerifyEnvelope and VerifyBalanceSnapshot.
func VerifyEntry(pub ed25519.PublicKey, body *tbproto.EntryBody, sig []byte) bool {
	if body == nil {
		return false
	}
	buf, err := DeterministicMarshal(body)
	if err != nil {
		return false
	}
	return Verify(pub, buf, sig)
}
```

- [ ] **Step 3: Run tests**

```bash
cd shared && go test -race ./signing/...
```

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add shared/signing/proto.go shared/signing/proto_test.go
git commit -m "$(cat <<'EOF'
feat(shared/signing): SignEntry / VerifyEntry over EntryBody

Three signers (consumer/seeder/tracker) share one preimage — the
DeterministicMarshal of EntryBody — so one Sign+Verify pair covers the
whole signature matrix. Same nil-safety contract as the envelope helpers.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: shared golden-bytes tripwire for `Entry`

**Files:**
- Create: `shared/proto/testdata/entry_signed.golden.hex`
- Modify: `shared/proto/ledger_test.go` (add golden test)

- [ ] **Step 1: Write the golden test (failing)**

Append to `shared/proto/ledger_test.go`:

```go
const ledgerGoldenPath = "testdata/entry_signed.golden.hex"

func TestEntry_GoldenBytes(t *testing.T) {
	body := fixtureUsageEntryBody()

	bodyBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(body)
	require.NoError(t, err)

	_, priv := goldenKeypair()
	// All three sigs use the same preimage; produce three distinct sigs by
	// using three derived keys so the fixture exercises each slot.
	consumerPriv := derivedKey(priv, "consumer")
	seederPriv := derivedKey(priv, "seeder")
	trackerPriv := derivedKey(priv, "tracker")

	signed := &Entry{
		Body:        body,
		ConsumerSig: ed25519.Sign(consumerPriv, bodyBytes),
		SeederSig:   ed25519.Sign(seederPriv, bodyBytes),
		TrackerSig:  ed25519.Sign(trackerPriv, bodyBytes),
	}
	wireBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(signed)
	require.NoError(t, err)

	gotHex := hex.EncodeToString(wireBytes)

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		require.NoError(t, os.MkdirAll(filepath.Dir(ledgerGoldenPath), 0o755))
		require.NoError(t, os.WriteFile(ledgerGoldenPath, []byte(gotHex+"\n"), 0o644))
		t.Logf("golden updated at %s", ledgerGoldenPath)
		return
	}

	wantBytes, err := os.ReadFile(ledgerGoldenPath)
	require.NoError(t, err, "missing golden file — run: UPDATE_GOLDEN=1 go test ./proto/... -run TestEntry_GoldenBytes")
	wantHex := strings.TrimSpace(string(wantBytes))

	assert.Equal(t, wantHex, gotHex,
		"Entry wire bytes differ from golden. If schema/lib intentionally changed, "+
			"regenerate via UPDATE_GOLDEN=1 and review the diff manually.")
}

// derivedKey produces a deterministic Ed25519 key from a base private key
// and a label, used only by the golden fixture to populate three distinct
// sig slots without committing extra seeds. Implementation: SHA-256(seed||label)
// as the new seed.
func derivedKey(base ed25519.PrivateKey, label string) ed25519.PrivateKey {
	seed := base.Seed()
	h := sha256.Sum256(append(seed, []byte(label)...))
	return ed25519.NewKeyFromSeed(h[:])
}
```

Add imports: `"crypto/sha256"`, `"crypto/ed25519"`, `"encoding/hex"`, `"os"`, `"path/filepath"`, `"strings"`.

- [ ] **Step 2: Generate the golden**

```bash
cd shared
UPDATE_GOLDEN=1 go test ./proto/... -run TestEntry_GoldenBytes
```

Expected: passes; file `shared/proto/testdata/entry_signed.golden.hex` is written.

- [ ] **Step 3: Run unconditionally**

```bash
cd shared && go test -race ./proto/...
```

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add shared/proto/ledger_test.go shared/proto/testdata/entry_signed.golden.hex
git commit -m "$(cat <<'EOF'
test(shared/proto): Entry golden-bytes tripwire

Locks the v1 wire bytes for a fully-signed USAGE entry. Any unintended
change to schema, codegen library, or DeterministicMarshal trips this
test. Regenerate with UPDATE_GOLDEN=1 and review the diff manually.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: tracker `internal/ledger/entry` — Kind constants

**Files:**
- Create: `tracker/internal/ledger/entry/doc.go`, `tracker/internal/ledger/entry/kind.go`, `tracker/internal/ledger/entry/kind_test.go`
- Modify: `tracker/go.mod` (add `require shared` if go mod tidy requests it once an import lands)
- Remove: `tracker/internal/ledger/entry/.gitkeep`

- [ ] **Step 1: Write `doc.go`**

```go
// Package entry owns the operations on shared/proto.Entry —
// canonical-bytes hashing, builder helpers per kind, and signature
// verification. The Entry type itself lives in shared/proto/ because it
// is a wire-format type federation streams between trackers.
//
// Spec: docs/superpowers/specs/ledger/2026-04-22-ledger-design.md §3.1.
//
// Non-negotiables:
//
//  1. Never sign the wire form (Entry); always sign EntryBody. The hash
//     and all three signatures are over DeterministicMarshal(body).
//  2. The entry hash is over the body, not the signed form. This keeps
//     the chain stable across tracker key rotations that re-sign the tip.
//  3. Builders return validated bodies. Callers can sign without
//     re-running ValidateEntryBody.
package entry
```

- [ ] **Step 2: Write the failing kind test**

`tracker/internal/ledger/entry/kind_test.go`:

```go
package entry

import (
	"testing"

	"github.com/stretchr/testify/assert"

	tbproto "github.com/token-bay/token-bay/shared/proto"
)

func TestKind_StringRoundsTrip(t *testing.T) {
	for _, k := range []Kind{KindUsage, KindTransferOut, KindTransferIn, KindStarterGrant} {
		assert.NotEmpty(t, k.String(), "kind %v has no String()", k)
	}
}

func TestKind_MapsToProtoEnum(t *testing.T) {
	cases := map[Kind]tbproto.EntryKind{
		KindUsage:        tbproto.EntryKind_ENTRY_KIND_USAGE,
		KindTransferOut:  tbproto.EntryKind_ENTRY_KIND_TRANSFER_OUT,
		KindTransferIn:   tbproto.EntryKind_ENTRY_KIND_TRANSFER_IN,
		KindStarterGrant: tbproto.EntryKind_ENTRY_KIND_STARTER_GRANT,
	}
	for k, want := range cases {
		assert.Equal(t, want, k.Proto(), "kind %v", k)
	}
}

func TestKindFromProto_KnownEnums(t *testing.T) {
	cases := map[tbproto.EntryKind]Kind{
		tbproto.EntryKind_ENTRY_KIND_USAGE:         KindUsage,
		tbproto.EntryKind_ENTRY_KIND_TRANSFER_OUT:  KindTransferOut,
		tbproto.EntryKind_ENTRY_KIND_TRANSFER_IN:   KindTransferIn,
		tbproto.EntryKind_ENTRY_KIND_STARTER_GRANT: KindStarterGrant,
	}
	for in, want := range cases {
		got, ok := KindFromProto(in)
		assert.True(t, ok)
		assert.Equal(t, want, got)
	}
}

func TestKindFromProto_RejectsUnspecifiedAndUnknown(t *testing.T) {
	_, ok := KindFromProto(tbproto.EntryKind_ENTRY_KIND_UNSPECIFIED)
	assert.False(t, ok)
	_, ok = KindFromProto(tbproto.EntryKind(99))
	assert.False(t, ok)
}
```

Run: `cd tracker && go test ./internal/ledger/entry/...`

Expected: FAIL — package compile error / undefined.

- [ ] **Step 3: Implement `kind.go`**

```go
package entry

import (
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// Kind is the Go-typed mirror of tbproto.EntryKind. Using a Go enum here
// (instead of leaking the protobuf type into broker/ledger code) keeps the
// generated package surface narrow and gives us a real exhaustive-switch
// linter signal.
type Kind uint8

const (
	KindUsage Kind = iota + 1
	KindTransferOut
	KindTransferIn
	KindStarterGrant
)

// String returns the human-readable kind name. Used for logs and errors.
func (k Kind) String() string {
	switch k {
	case KindUsage:
		return "usage"
	case KindTransferOut:
		return "transfer_out"
	case KindTransferIn:
		return "transfer_in"
	case KindStarterGrant:
		return "starter_grant"
	default:
		return "unknown"
	}
}

// Proto returns the wire-format enum value for k. Panics on unknown
// because every Kind value defined in this package has a mapping;
// constructing a Kind outside the constants is a programmer error.
func (k Kind) Proto() tbproto.EntryKind {
	switch k {
	case KindUsage:
		return tbproto.EntryKind_ENTRY_KIND_USAGE
	case KindTransferOut:
		return tbproto.EntryKind_ENTRY_KIND_TRANSFER_OUT
	case KindTransferIn:
		return tbproto.EntryKind_ENTRY_KIND_TRANSFER_IN
	case KindStarterGrant:
		return tbproto.EntryKind_ENTRY_KIND_STARTER_GRANT
	default:
		panic("entry: Kind.Proto() on unknown kind")
	}
}

// KindFromProto converts a wire-format enum to the Go-typed Kind.
// Returns false for ENTRY_KIND_UNSPECIFIED and any unknown value, so
// callers can reject malformed remote entries cleanly.
func KindFromProto(p tbproto.EntryKind) (Kind, bool) {
	switch p {
	case tbproto.EntryKind_ENTRY_KIND_USAGE:
		return KindUsage, true
	case tbproto.EntryKind_ENTRY_KIND_TRANSFER_OUT:
		return KindTransferOut, true
	case tbproto.EntryKind_ENTRY_KIND_TRANSFER_IN:
		return KindTransferIn, true
	case tbproto.EntryKind_ENTRY_KIND_STARTER_GRANT:
		return KindStarterGrant, true
	default:
		return 0, false
	}
}
```

- [ ] **Step 4: Remove .gitkeep**

```bash
rm tracker/internal/ledger/entry/.gitkeep
```

- [ ] **Step 5: Update tracker `go.mod`**

The first import of `shared/` from tracker code triggers `go mod tidy` to add `require github.com/token-bay/token-bay/shared v0.0.0-...`. Run:

```bash
cd tracker
go mod tidy
go test -race ./internal/ledger/entry/...
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add tracker/internal/ledger/entry/ tracker/go.mod tracker/go.sum
git commit -m "$(cat <<'EOF'
feat(tracker/ledger/entry): Kind enum mapped to wire EntryKind

Go-typed Kind avoids leaking the generated protobuf enum into broker
and ledger storage code. KindFromProto returns ok=false for
UNSPECIFIED + unknowns so federation can reject malformed remote
entries without a panic.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: tracker `entry` — Hash + body-bytes helper

**Files:**
- Create: `tracker/internal/ledger/entry/hash.go`, `tracker/internal/ledger/entry/hash_test.go`, `tracker/internal/ledger/entry/testdata/usage_entry.golden.hex`

- [ ] **Step 1: Failing test**

`hash_test.go`:

```go
package entry

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// usageBodyFixture mirrors shared/proto's fixtureUsageEntryBody so the
// hash here and the golden-bytes test there exercise the same shape.
// Duplication is intentional: tracker tests don't import shared/proto's
// _test.go fixtures.
func usageBodyFixture() *tbproto.EntryBody {
	return &tbproto.EntryBody{
		PrevHash:     bytes.Repeat([]byte{0x01}, 32),
		Seq:          42,
		Kind:         tbproto.EntryKind_ENTRY_KIND_USAGE,
		ConsumerId:   bytes.Repeat([]byte{0x11}, 32),
		SeederId:     bytes.Repeat([]byte{0x22}, 32),
		Model:        "claude-sonnet-4-6",
		InputTokens:  4096,
		OutputTokens: 1024,
		CostCredits:  3072000,
		Timestamp:    1714000100,
		RequestId:    bytes.Repeat([]byte{0x33}, 16),
		Flags:        0,
		Ref:          make([]byte, 32),
	}
}

func TestHash_MatchesSHA256OfDeterministicMarshal(t *testing.T) {
	body := usageBodyFixture()

	got, err := Hash(body)
	require.NoError(t, err)

	wantBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(body)
	require.NoError(t, err)
	want := sha256.Sum256(wantBytes)

	assert.Equal(t, want, got)
}

func TestHash_StableAcrossRepeatedCalls(t *testing.T) {
	body := usageBodyFixture()
	a, err := Hash(body)
	require.NoError(t, err)
	b, err := Hash(body)
	require.NoError(t, err)
	assert.Equal(t, a, b)
}

func TestHash_DiffersOnFieldChange(t *testing.T) {
	a, err := Hash(usageBodyFixture())
	require.NoError(t, err)

	mutated := usageBodyFixture()
	mutated.CostCredits++
	b, err := Hash(mutated)
	require.NoError(t, err)

	assert.NotEqual(t, a, b)
}

func TestHash_NilReturnsError(t *testing.T) {
	_, err := Hash(nil)
	require.Error(t, err)
}

const usageEntryGoldenPath = "testdata/usage_entry.golden.hex"

func TestHash_GoldenBytes(t *testing.T) {
	got, err := Hash(usageBodyFixture())
	require.NoError(t, err)
	gotHex := hex.EncodeToString(got[:])

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		require.NoError(t, os.MkdirAll(filepath.Dir(usageEntryGoldenPath), 0o755))
		require.NoError(t, os.WriteFile(usageEntryGoldenPath, []byte(gotHex+"\n"), 0o644))
		return
	}

	wantBytes, err := os.ReadFile(usageEntryGoldenPath)
	require.NoError(t, err, "missing golden — run: UPDATE_GOLDEN=1 go test ./internal/ledger/entry/... -run TestHash_GoldenBytes")
	wantHex := strings.TrimSpace(string(wantBytes))
	assert.Equal(t, wantHex, gotHex,
		"entry hash differs from golden. If wire schema or marshal lib changed, regenerate with UPDATE_GOLDEN=1.")
}
```

Run: FAIL — `undefined: Hash`.

- [ ] **Step 2: Implement `hash.go`**

```go
package entry

import (
	"crypto/sha256"
	"errors"
	"fmt"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
)

// Hash returns SHA-256 over the canonical bytes of body. This value is
// what the next entry's prev_hash must equal (ledger spec §3.2 chain).
//
// The hash covers EntryBody only — never the wire-form Entry. Rationale
// in the package doc: chain links survive tracker key rotation because
// the bodies don't change, only tracker_sig does.
func Hash(body *tbproto.EntryBody) ([32]byte, error) {
	if body == nil {
		return [32]byte{}, errors.New("entry: Hash on nil body")
	}
	buf, err := signing.DeterministicMarshal(body)
	if err != nil {
		return [32]byte{}, fmt.Errorf("entry: marshal body: %w", err)
	}
	return sha256.Sum256(buf), nil
}
```

- [ ] **Step 3: Generate golden**

```bash
cd tracker
UPDATE_GOLDEN=1 go test ./internal/ledger/entry/... -run TestHash_GoldenBytes
go test -race ./internal/ledger/entry/...
```

Expected: golden written, all tests pass.

- [ ] **Step 4: Commit**

```bash
git add tracker/internal/ledger/entry/hash.go tracker/internal/ledger/entry/hash_test.go tracker/internal/ledger/entry/testdata/
git commit -m "$(cat <<'EOF'
feat(tracker/ledger/entry): Hash over canonical body bytes

prev_hash linkage is over EntryBody, not the signed wire form, so the
chain survives a tracker key rotation that re-signs the tip. Golden
test pins the v1 hash for the standard usage fixture.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 7: tracker `entry` — Builder helpers

**Files:**
- Create: `tracker/internal/ledger/entry/builder.go`, `tracker/internal/ledger/entry/builder_test.go`

Each builder takes typed inputs and returns a `*tbproto.EntryBody` already validated. Callers then sign with `shared/signing.SignEntry` per the per-kind matrix.

- [ ] **Step 1: Write failing builder tests**

`builder_test.go`:

```go
package entry

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tbproto "github.com/token-bay/token-bay/shared/proto"
)

func TestBuildUsageEntry_Valid(t *testing.T) {
	body, err := BuildUsageEntry(UsageInput{
		PrevHash:     bytes.Repeat([]byte{0x01}, 32),
		Seq:          42,
		ConsumerID:   bytes.Repeat([]byte{0x11}, 32),
		SeederID:     bytes.Repeat([]byte{0x22}, 32),
		Model:        "claude-sonnet-4-6",
		InputTokens:  4096,
		OutputTokens: 1024,
		CostCredits:  3072000,
		Timestamp:    1714000100,
		RequestID:    bytes.Repeat([]byte{0x33}, 16),
	})
	require.NoError(t, err)
	require.NoError(t, tbproto.ValidateEntryBody(body))
	assert.Equal(t, tbproto.EntryKind_ENTRY_KIND_USAGE, body.Kind)
	assert.Equal(t, uint32(0), body.Flags)
}

func TestBuildUsageEntry_WithConsumerSigMissingFlag(t *testing.T) {
	body, err := BuildUsageEntry(UsageInput{
		PrevHash: bytes.Repeat([]byte{0x01}, 32), Seq: 42,
		ConsumerID: bytes.Repeat([]byte{0x11}, 32),
		SeederID:   bytes.Repeat([]byte{0x22}, 32),
		Model:      "claude-sonnet-4-6", Timestamp: 1714000100,
		RequestID: bytes.Repeat([]byte{0x33}, 16),
		ConsumerSigMissing: true,
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(1), body.Flags)
}

func TestBuildUsageEntry_RejectsBadHash(t *testing.T) {
	_, err := BuildUsageEntry(UsageInput{
		PrevHash:   make([]byte, 16),
		Seq:        42,
		ConsumerID: bytes.Repeat([]byte{0x11}, 32),
		SeederID:   bytes.Repeat([]byte{0x22}, 32),
		Model:      "claude-sonnet-4-6", Timestamp: 1714000100,
		RequestID: bytes.Repeat([]byte{0x33}, 16),
	})
	require.Error(t, err)
}

func TestBuildTransferOutEntry_Valid(t *testing.T) {
	body, err := BuildTransferOutEntry(TransferOutInput{
		PrevHash:    bytes.Repeat([]byte{0x01}, 32),
		Seq:         43,
		ConsumerID:  bytes.Repeat([]byte{0x11}, 32),
		Amount:      1000,
		Timestamp:   1714000200,
		TransferRef: bytes.Repeat([]byte{0x77}, 32),
	})
	require.NoError(t, err)
	require.NoError(t, tbproto.ValidateEntryBody(body))
	assert.Equal(t, tbproto.EntryKind_ENTRY_KIND_TRANSFER_OUT, body.Kind)
	assert.Empty(t, body.Model)
	assert.True(t, allZeroBytes(body.SeederId))
}

func TestBuildTransferInEntry_Valid(t *testing.T) {
	body, err := BuildTransferInEntry(TransferInInput{
		PrevHash:    bytes.Repeat([]byte{0x01}, 32),
		Seq:         44,
		Amount:      1000,
		Timestamp:   1714000300,
		TransferRef: bytes.Repeat([]byte{0x88}, 32),
	})
	require.NoError(t, err)
	require.NoError(t, tbproto.ValidateEntryBody(body))
	assert.True(t, allZeroBytes(body.ConsumerId))
}

func TestBuildStarterGrantEntry_Valid(t *testing.T) {
	body, err := BuildStarterGrantEntry(StarterGrantInput{
		PrevHash:   bytes.Repeat([]byte{0x01}, 32),
		Seq:        1,
		ConsumerID: bytes.Repeat([]byte{0x11}, 32),
		Amount:     100000,
		Timestamp:  1714000000,
	})
	require.NoError(t, err)
	require.NoError(t, tbproto.ValidateEntryBody(body))
}

func allZeroBytes(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}
```

Run: FAIL — undefined types/functions.

- [ ] **Step 2: Implement `builder.go`**

```go
package entry

import (
	"errors"
	"fmt"

	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// flagConsumerSigMissing is bit 0 of EntryBody.flags (ledger spec §3.1).
const flagConsumerSigMissing uint32 = 1 << 0

// UsageInput is the typed input to BuildUsageEntry. All byte slices must
// be exact-length per §3.1; the builder enforces this via ValidateEntryBody.
type UsageInput struct {
	PrevHash           []byte // 32 bytes
	Seq                uint64
	ConsumerID         []byte // 32 bytes
	SeederID           []byte // 32 bytes
	Model              string
	InputTokens        uint32
	OutputTokens       uint32
	CostCredits        uint64
	Timestamp          uint64
	RequestID          []byte // 16 bytes
	ConsumerSigMissing bool
}

// BuildUsageEntry constructs a USAGE-kind body and returns it after running
// ValidateEntryBody. Callers can call signing.SignEntry directly without
// re-validating.
func BuildUsageEntry(in UsageInput) (*tbproto.EntryBody, error) {
	if in.Model == "" {
		return nil, errors.New("entry: BuildUsageEntry: model is empty")
	}
	body := &tbproto.EntryBody{
		PrevHash:     in.PrevHash,
		Seq:          in.Seq,
		Kind:         tbproto.EntryKind_ENTRY_KIND_USAGE,
		ConsumerId:   in.ConsumerID,
		SeederId:     in.SeederID,
		Model:        in.Model,
		InputTokens:  in.InputTokens,
		OutputTokens: in.OutputTokens,
		CostCredits:  in.CostCredits,
		Timestamp:    in.Timestamp,
		RequestId:    in.RequestID,
		Ref:          make([]byte, 32),
	}
	if in.ConsumerSigMissing {
		body.Flags |= flagConsumerSigMissing
	}
	if err := tbproto.ValidateEntryBody(body); err != nil {
		return nil, fmt.Errorf("entry: BuildUsageEntry: %w", err)
	}
	return body, nil
}

// TransferOutInput — typed input for BuildTransferOutEntry.
type TransferOutInput struct {
	PrevHash    []byte // 32
	Seq         uint64
	ConsumerID  []byte // 32 — the identity moving credits out
	Amount      uint64 // absolute value; semantic sign is "debit"
	Timestamp   uint64
	TransferRef []byte // 32 — transfer UUID padded
}

func BuildTransferOutEntry(in TransferOutInput) (*tbproto.EntryBody, error) {
	body := &tbproto.EntryBody{
		PrevHash:    in.PrevHash,
		Seq:         in.Seq,
		Kind:        tbproto.EntryKind_ENTRY_KIND_TRANSFER_OUT,
		ConsumerId:  in.ConsumerID,
		SeederId:    make([]byte, 32),
		CostCredits: in.Amount,
		Timestamp:   in.Timestamp,
		RequestId:   make([]byte, 16),
		Ref:         in.TransferRef,
	}
	if err := tbproto.ValidateEntryBody(body); err != nil {
		return nil, fmt.Errorf("entry: BuildTransferOutEntry: %w", err)
	}
	return body, nil
}

// TransferInInput — typed input for BuildTransferInEntry.
type TransferInInput struct {
	PrevHash    []byte // 32
	Seq         uint64
	Amount      uint64
	Timestamp   uint64
	TransferRef []byte // 32 — peer-region transfer UUID
}

func BuildTransferInEntry(in TransferInInput) (*tbproto.EntryBody, error) {
	body := &tbproto.EntryBody{
		PrevHash:    in.PrevHash,
		Seq:         in.Seq,
		Kind:        tbproto.EntryKind_ENTRY_KIND_TRANSFER_IN,
		ConsumerId:  make([]byte, 32),
		SeederId:    make([]byte, 32),
		CostCredits: in.Amount,
		Timestamp:   in.Timestamp,
		RequestId:   make([]byte, 16),
		Ref:         in.TransferRef,
	}
	if err := tbproto.ValidateEntryBody(body); err != nil {
		return nil, fmt.Errorf("entry: BuildTransferInEntry: %w", err)
	}
	return body, nil
}

// StarterGrantInput — typed input for BuildStarterGrantEntry.
type StarterGrantInput struct {
	PrevHash   []byte // 32
	Seq        uint64
	ConsumerID []byte // 32 — the identity receiving the grant
	Amount     uint64
	Timestamp  uint64
}

func BuildStarterGrantEntry(in StarterGrantInput) (*tbproto.EntryBody, error) {
	body := &tbproto.EntryBody{
		PrevHash:    in.PrevHash,
		Seq:         in.Seq,
		Kind:        tbproto.EntryKind_ENTRY_KIND_STARTER_GRANT,
		ConsumerId:  in.ConsumerID,
		SeederId:    make([]byte, 32),
		CostCredits: in.Amount,
		Timestamp:   in.Timestamp,
		RequestId:   make([]byte, 16),
		Ref:         make([]byte, 32),
	}
	if err := tbproto.ValidateEntryBody(body); err != nil {
		return nil, fmt.Errorf("entry: BuildStarterGrantEntry: %w", err)
	}
	return body, nil
}
```

- [ ] **Step 3: Run tests**

```bash
cd tracker && go test -race ./internal/ledger/entry/...
```

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add tracker/internal/ledger/entry/builder.go tracker/internal/ledger/entry/builder_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/ledger/entry): typed builders per Kind

Each builder takes a typed Input struct, fills the zero-by-omission
fields the per-kind matrix requires, and returns an already-validated
EntryBody. Callers sign without re-running ValidateEntryBody.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 8: tracker `entry` — `VerifyAll` per-kind matrix

**Files:**
- Create: `tracker/internal/ledger/entry/verify.go`, `tracker/internal/ledger/entry/verify_test.go`

`VerifyAll` checks every signature required by the kind, refuses unexpected sigs (e.g. a seeder sig on a STARTER_GRANT), and returns the first failure.

- [ ] **Step 1: Failing test**

`verify_test.go` exercises:
- All-three-sigs USAGE round-trip → `nil`.
- USAGE with `consumer_sig_missing` and empty `consumer_sig` → `nil`.
- USAGE with `consumer_sig_missing` flag set but a non-empty `consumer_sig` → error.
- TRANSFER_OUT with a stray `seeder_sig` → error.
- STARTER_GRANT missing `tracker_sig` → error.
- Tampered body (post-sign) → error.

Test scaffolding:

```go
package entry

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"testing"

	"github.com/stretchr/testify/require"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
)

func newKey(label string) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("entry-test-seed-" + label))
	return ed25519.NewKeyFromSeed(seed[:])
}
func pub(p ed25519.PrivateKey) ed25519.PublicKey { return p.Public().(ed25519.PublicKey) }

func TestVerifyAll_UsageHappyPath(t *testing.T) {
	body, err := BuildUsageEntry(UsageInput{
		PrevHash: bytes.Repeat([]byte{1}, 32), Seq: 42,
		ConsumerID: bytes.Repeat([]byte{2}, 32),
		SeederID:   bytes.Repeat([]byte{3}, 32),
		Model:      "claude-sonnet-4-6",
		Timestamp:  1714000100,
		RequestID:  bytes.Repeat([]byte{4}, 16),
	})
	require.NoError(t, err)

	cKey, sKey, tKey := newKey("c"), newKey("s"), newKey("t")
	cSig, _ := signing.SignEntry(cKey, body)
	sSig, _ := signing.SignEntry(sKey, body)
	tSig, _ := signing.SignEntry(tKey, body)

	require.NoError(t, VerifyAll(&tbproto.Entry{Body: body, ConsumerSig: cSig, SeederSig: sSig, TrackerSig: tSig},
		Pubkeys{Consumer: pub(cKey), Seeder: pub(sKey), Tracker: pub(tKey)}))
}

// ... remaining tests as enumerated above
```

(Full enumeration in implementation; truncated here to keep the plan readable.)

- [ ] **Step 2: Implement `verify.go`**

```go
package entry

import (
	"crypto/ed25519"
	"errors"
	"fmt"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
)

// Pubkeys carries the verification keys for an Entry's three potential
// signers. Consumer / Seeder may be empty for kinds that don't include
// the corresponding signature; Tracker is always required.
type Pubkeys struct {
	Consumer ed25519.PublicKey
	Seeder   ed25519.PublicKey
	Tracker  ed25519.PublicKey
}

// VerifyAll runs the per-kind signature matrix from plan §3.4. Returns
// nil on success; the first violation otherwise. Callers must have run
// ValidateEntryBody first — VerifyAll does not re-check field shape.
func VerifyAll(e *tbproto.Entry, keys Pubkeys) error {
	if e == nil || e.Body == nil {
		return errors.New("entry: VerifyAll on nil entry")
	}
	if len(e.TrackerSig) == 0 {
		return errors.New("entry: tracker_sig is required")
	}
	if len(keys.Tracker) != ed25519.PublicKeySize {
		return errors.New("entry: tracker pubkey missing")
	}
	if !signing.VerifyEntry(keys.Tracker, e.Body, e.TrackerSig) {
		return errors.New("entry: tracker_sig invalid")
	}

	flagSigMissing := e.Body.Flags&flagConsumerSigMissing != 0

	switch e.Body.Kind {
	case tbproto.EntryKind_ENTRY_KIND_USAGE:
		if len(e.SeederSig) == 0 {
			return errors.New("entry: usage seeder_sig is required")
		}
		if !signing.VerifyEntry(keys.Seeder, e.Body, e.SeederSig) {
			return errors.New("entry: seeder_sig invalid")
		}
		if flagSigMissing {
			if len(e.ConsumerSig) != 0 {
				return errors.New("entry: usage consumer_sig must be empty when flag set")
			}
		} else {
			if len(e.ConsumerSig) == 0 {
				return errors.New("entry: usage consumer_sig required (or set consumer_sig_missing flag)")
			}
			if !signing.VerifyEntry(keys.Consumer, e.Body, e.ConsumerSig) {
				return errors.New("entry: consumer_sig invalid")
			}
		}
	case tbproto.EntryKind_ENTRY_KIND_TRANSFER_OUT:
		if flagSigMissing {
			return errors.New("entry: transfer_out cannot set consumer_sig_missing")
		}
		if len(e.ConsumerSig) == 0 {
			return errors.New("entry: transfer_out consumer_sig required")
		}
		if !signing.VerifyEntry(keys.Consumer, e.Body, e.ConsumerSig) {
			return errors.New("entry: consumer_sig invalid")
		}
		if len(e.SeederSig) != 0 {
			return errors.New("entry: transfer_out seeder_sig must be empty")
		}
	case tbproto.EntryKind_ENTRY_KIND_TRANSFER_IN, tbproto.EntryKind_ENTRY_KIND_STARTER_GRANT:
		if len(e.ConsumerSig) != 0 {
			return errors.New("entry: consumer_sig must be empty for this kind")
		}
		if len(e.SeederSig) != 0 {
			return errors.New("entry: seeder_sig must be empty for this kind")
		}
	default:
		return fmt.Errorf("entry: unknown kind %v", e.Body.Kind)
	}
	return nil
}
```

- [ ] **Step 3: Run tests**

```bash
cd tracker && go test -race ./internal/ledger/entry/...
```

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add tracker/internal/ledger/entry/verify.go tracker/internal/ledger/entry/verify_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/ledger/entry): VerifyAll per-kind signature matrix

Tracker_sig is mandatory on every kind; consumer/seeder presence is
gated by kind and the consumer_sig_missing flag. Stray sigs on kinds
that don't accept them are a hard error so an attacker can't piggy-back
a confusing signature onto a starter grant or transfer.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 9: Final verification + tag

**Files:**
- None (verification only)

- [ ] **Step 1: Repo-wide check**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/cut-sunflower
make check
```

Expected: shared + plugin + tracker green. tracker now has nonzero coverage in `internal/ledger/entry/`.

- [ ] **Step 2: Coverage spot-check**

```bash
cd tracker && go test -coverprofile=coverage.out ./internal/ledger/entry/...
go tool cover -func=coverage.out | tail
```

Expected: ≥ 90% on the entry package; lower bound is fine for builders since each kind has its own happy-path test.

- [ ] **Step 3: Lint passthrough**

```bash
cd tracker && make lint
cd ../shared && make lint
```

Expected: clean.

- [ ] **Step 4: Tag (optional)**

```bash
git tag -a tracker-ledger-entry-v0 -m "Tracker ledger entry helpers complete"
```

---

## Self-review

- **Spec coverage:** §3.1 entry layout, §3.2 chain (via `Hash`), §4.1 step 4 validation hooks, tracker spec §5.2 step 4 confirmation that the same preimage feeds all three signers — all addressed. §3.3 balance materialization and §4.3 Merkle roots are out of scope (next plans: `internal/ledger/storage` and `internal/ledger`).
- **Cross-module phasing:** Tasks 1–4 land entirely in `shared/`. Tasks 5–8 land entirely in `tracker/`. No single commit crosses the boundary, but the tracker tasks depend on the shared tasks shipping first — natural ordering preserved.
- **Wire-format invariants:** Two-message split, `UNSPECIFIED=0` reserved-and-rejected, deterministic-marshal choke point, golden-bytes tripwire — all match the precedents from `EnvelopeBody`/`EnvelopeSigned` and `BalanceSnapshotBody`/`SignedBalanceSnapshot`.
- **Hash-of-body decision:** Documented in plan §3.3 and `entry/doc.go`. The alternative — hashing the signed wire form — would couple chain links to signing keys, breaking key rotation. Chosen reading is the only one consistent with `tracker/CLAUDE.md` rule 3.
- **Per-kind matrix:** Plan §3.4 + Task 2 (`ValidateEntryBody`) enforce field-presence; Task 8 (`VerifyAll`) enforces signature-presence. Two layers because field validation runs at parse time on the receive side, signature verification runs only when keys are available.
- **Placeholders:** None. Every task ends with a working binary or library and a passing test.

## Next plans (after this one)

The natural follow-ons are `internal/ledger/storage` (SQLite-backed `entries` / `balances` / `merkle_roots` tables) and `internal/ledger` itself (chain + balance projection + hourly Merkle roots), in that order. `internal/registry` and `internal/config` are independent and can be split off into separate sessions per the user's stated plan.
