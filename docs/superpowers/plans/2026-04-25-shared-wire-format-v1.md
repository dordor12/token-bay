# shared/ Wire-format v1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the protobuf wire-format v1 spec — `ExhaustionProofV1`, `EnvelopeBody`/`EnvelopeSigned`, `SignedBalanceSnapshot` — plus deterministic-marshal sign/verify helpers in `shared/signing`. Ship the spec amendments (architecture §7.2, `shared/CLAUDE.md`) in the same plan.

**Architecture:** Two-message split for every Ed25519-signed message (Body + Signed{Body, sig}). Determinism via `proto.MarshalOptions{Deterministic: true}` funneled through one helper (`signing.DeterministicMarshal`). Generated `.pb.go` files committed; regeneration is gated behind a `proto-gen` Makefile target and enforced by a `proto-check` CI tripwire. Validation helpers run before signing and after parsing.

**Tech Stack:** Go 1.23+, `google.golang.org/protobuf` (new dep), stdlib `crypto/ed25519`, `github.com/stretchr/testify` for tests. Codegen via `protoc` + `protoc-gen-go`.

**Spec:** `docs/superpowers/specs/shared/2026-04-24-wire-format-v1-design.md`

---

## 1. File map

```
shared/
├── go.mod                                  ← MODIFY: add google.golang.org/protobuf
├── Makefile                                ← MODIFY: add proto-gen + proto-check
├── CLAUDE.md                               ← MODIFY: relax tech-stack rule
├── exhaustionproof/
│   ├── proof.proto                         ← CREATE: schema
│   ├── proof.pb.go                         ← CREATE (generated, committed)
│   ├── exhaustionproof.go                  ← KEEP: Version constants + package doc
│   ├── validate.go                         ← CREATE: ValidateProofV1
│   ├── proof_test.go                       ← CREATE: round-trip tests
│   └── validate_test.go                    ← CREATE: rejection-matrix tests
├── proto/
│   ├── balance.proto                       ← CREATE: schema
│   ├── envelope.proto                      ← CREATE: schema
│   ├── balance.pb.go                       ← CREATE (generated, committed)
│   ├── envelope.pb.go                      ← CREATE (generated, committed)
│   ├── proto.go                            ← KEEP: ProtocolVersion + package doc
│   ├── validate.go                         ← CREATE: ValidateEnvelopeBody
│   ├── balance_test.go                     ← CREATE: round-trip
│   ├── envelope_test.go                    ← CREATE: round-trip + golden fixture
│   └── validate_test.go                    ← CREATE: rejection-matrix tests
└── signing/
    ├── ed25519.go                          ← KEEP: existing Verify
    ├── proto.go                            ← CREATE: DeterministicMarshal + Sign/Verify helpers
    └── proto_test.go                       ← CREATE: helper tests
docs/superpowers/specs/2026-04-22-token-bay-architecture-design.md  ← MODIFY: §7.2 amendment
Makefile (root)                              ← MODIFY: add proto-check passthrough
```

`shared/proto/testdata/` is created by Task 8 for the golden fixture.

## 2. Conventions used in this plan

- All `go test` commands run from `/Users/dor.amid/git/token-bay/shared`. Use absolute paths or `cd` to that directory.
- `PATH="$HOME/.local/share/mise/shims:$PATH"` is needed if Go is managed by mise. Bash steps include it where required.
- `protoc` and `protoc-gen-go` are required only for Task 1 (one-time install) and Tasks 2/4/5 (regenerate). Generated files are committed so other tasks don't need them.
- One commit per task. Conventional-commit prefixes: `feat(shared/<pkg>):`, `chore(shared):`, `docs(shared):`, `spec:`.
- Co-Authored-By footer: `Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>`.

---

## Task 1: Tooling, dependency, CLAUDE.md amendment

**Files:**
- Create: nothing
- Modify: `shared/go.mod`, `shared/Makefile`, `shared/CLAUDE.md`

- [ ] **Step 1: Install protoc and protoc-gen-go (one-time, contributor machine)**

```bash
# macOS
brew install protobuf

# Then (any platform; needs Go on PATH):
PATH="$HOME/.local/share/mise/shims:$PATH" go install google.golang.org/protobuf/cmd/protoc-gen-go@latest

# Verify
which protoc protoc-gen-go
protoc --version          # libprotoc 3.x or 4.x
protoc-gen-go --version   # protoc-gen-go v1.32+ acceptable
```

If `protoc-gen-go` was installed by `go install`, it lives in `$(go env GOBIN)` (default `$HOME/go/bin`). Make sure that directory is on `PATH` before running `make proto-gen`.

- [ ] **Step 2: Add the runtime dependency to shared/go.mod**

```bash
cd /Users/dor.amid/git/token-bay/shared
PATH="$HOME/.local/share/mise/shims:$PATH" go get google.golang.org/protobuf@latest
PATH="$HOME/.local/share/mise/shims:$PATH" go mod tidy
```

Expected: `shared/go.mod` now has `require google.golang.org/protobuf vX.Y.Z` (a direct require). `shared/go.sum` updated.

- [ ] **Step 3: Replace shared/Makefile with proto-aware version**

Replace the entire contents of `shared/Makefile`:

```makefile
.PHONY: test lint check clean build proto-gen proto-check

# Source-relative outputs land .pb.go next to the .proto file.
PROTO_FILES := exhaustionproof/proof.proto proto/balance.proto proto/envelope.proto

test:
	go test -race -coverprofile=coverage.out ./...

lint:
	golangci-lint run ./...

check: test lint

build:
	@echo "shared is a library; nothing to build"

clean:
	rm -f coverage.out

# Regenerate .pb.go files from .proto schemas. Requires protoc + protoc-gen-go on PATH.
proto-gen:
	@command -v protoc >/dev/null 2>&1 || { echo "protoc not found — see plan Task 1 step 1"; exit 1; }
	@command -v protoc-gen-go >/dev/null 2>&1 || { echo "protoc-gen-go not found — see plan Task 1 step 1"; exit 1; }
	protoc -I. \
		--go_out=. --go_opt=paths=source_relative \
		$(PROTO_FILES)

# CI tripwire: regenerate and fail if there is a diff. Catches schemas
# committed without re-running proto-gen.
proto-check: proto-gen
	@if ! git diff --quiet -- $(addsuffix .pb.go,$(basename $(PROTO_FILES))); then \
		echo "ERROR: generated .pb.go files differ from committed; run 'make proto-gen' and commit"; \
		git diff --stat -- $(addsuffix .pb.go,$(basename $(PROTO_FILES))); \
		exit 1; \
	fi
```

- [ ] **Step 4: Update shared/CLAUDE.md tech-stack rule**

Open `shared/CLAUDE.md`. Find the `## Tech stack` section (currently a single line: *"Just Go stdlib + `github.com/stretchr/testify` for tests. That's it."*).

Replace that section with:

```markdown
## Tech stack

- Go stdlib
- `github.com/stretchr/testify` (tests only)
- `google.golang.org/protobuf` (wire-format types — single approved third-party runtime dependency)

No other runtime dependencies.
```

Also add a new rule 6 to the **Non-negotiable rules** section, immediately after rule 5:

```markdown
6. **All signing/verifying of proto messages goes through `shared/signing.DeterministicMarshal`.** This is the single choke point for canonical bytes. Calling `proto.Marshal` directly on something that gets signed bypasses the determinism guarantee — which is precisely the bug Ed25519 verification surfaces in production rather than at code-review time.
```

- [ ] **Step 5: Verify**

```bash
cd /Users/dor.amid/git/token-bay/shared
PATH="$HOME/.local/share/mise/shims:$PATH" go build ./...
make proto-gen   # should run protoc — but no .proto files exist yet so it's a no-op-ish
```

Expected: `go build` passes (the new dep is downloaded but unused). `proto-gen` exits 0 even with no schema files (protoc errors only on missing input files passed explicitly — our PROTO_FILES list is empty by Task 1 standards because the .proto files don't exist yet, so protoc may error). If `proto-gen` errors here, that's expected — Task 2 creates the first .proto file.

- [ ] **Step 6: Commit**

```bash
cd /Users/dor.amid/git/token-bay
git add shared/go.mod shared/go.sum shared/Makefile shared/CLAUDE.md
git commit -m "$(cat <<'EOF'
chore(shared): admit google.golang.org/protobuf for wire-format types

Adds the dependency, the proto-gen + proto-check Makefile targets, and
the CLAUDE.md amendment relaxing "stdlib + testify only" with one
explicit exception. Adds a non-negotiable rule that all sign/verify of
proto messages must go through signing.DeterministicMarshal — the
single canonical-bytes choke point that the wire-format spec depends
on for cross-implementation Ed25519 verification.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: ExhaustionProofV1 schema + round-trip

**Files:**
- Create: `shared/exhaustionproof/proof.proto`
- Create: `shared/exhaustionproof/proof.pb.go` (generated)
- Create: `shared/exhaustionproof/proof_test.go`

- [ ] **Step 1: Write the round-trip test**

Create `shared/exhaustionproof/proof_test.go`:

```go
package exhaustionproof

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestProofV1_RoundTrip(t *testing.T) {
	original := &ExhaustionProofV1{
		StopFailure: &StopFailure{
			Matcher:    "rate_limit",
			At:         1714000000,
			ErrorShape: []byte(`{"type":"rate_limit_error"}`),
		},
		UsageProbe: &UsageProbe{
			At:     1714000005,
			Output: []byte(`{"limit":"hit"}`),
		},
		CapturedAt: 1714000010,
		Nonce:      []byte("0123456789abcdef"),
	}

	first, err := proto.MarshalOptions{Deterministic: true}.Marshal(original)
	require.NoError(t, err)

	var parsed ExhaustionProofV1
	require.NoError(t, proto.Unmarshal(first, &parsed))

	second, err := proto.MarshalOptions{Deterministic: true}.Marshal(&parsed)
	require.NoError(t, err)

	assert.Equal(t, first, second, "Deterministic marshal must be byte-stable across round-trip")
	assert.Equal(t, original.StopFailure.Matcher, parsed.StopFailure.Matcher)
	assert.Equal(t, original.UsageProbe.At, parsed.UsageProbe.At)
	assert.Equal(t, original.Nonce, parsed.Nonce)
}
```

- [ ] **Step 2: Run, confirm FAIL**

```bash
cd /Users/dor.amid/git/token-bay/shared
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./exhaustionproof/...
```

Expected: compilation failure — `ExhaustionProofV1`, `StopFailure`, `UsageProbe` undefined.

- [ ] **Step 3: Write the schema**

Create `shared/exhaustionproof/proof.proto`:

```proto
syntax = "proto3";
package tokenbay.exhaustionproof.v1;

option go_package = "github.com/token-bay/token-bay/shared/exhaustionproof";

// ExhaustionProofV1 is the v1 two-signal rate-limit-exhaustion proof
// attached to every broker_request envelope. Spec:
// docs/superpowers/specs/shared/2026-04-24-wire-format-v1-design.md §5.1.
message ExhaustionProofV1 {
  StopFailure stop_failure = 1;
  UsageProbe  usage_probe  = 2;
  uint64      captured_at  = 3;  // unix seconds — when the proof was assembled
  bytes       nonce        = 4;  // 16 random bytes — proof-level replay protection
}

// StopFailure is the captured StopFailureHookInput payload.
message StopFailure {
  string matcher     = 1;  // v1: MUST be "rate_limit"
  uint64 at          = 2;  // unix seconds
  bytes  error_shape = 3;  // raw JSON Claude Code surfaced — verbatim, no transformation
}

// UsageProbe is the parsed output of `claude /usage` run under PTY.
message UsageProbe {
  uint64 at     = 1;  // unix seconds (when /usage was executed)
  bytes  output = 2;  // raw bytes of /usage TUI output (PTY-captured)
}
```

- [ ] **Step 4: Generate**

```bash
cd /Users/dor.amid/git/token-bay/shared
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" make proto-gen
```

Expected: `shared/exhaustionproof/proof.pb.go` is created (~150–250 LOC, generated).

- [ ] **Step 5: Run, confirm PASS**

```bash
cd /Users/dor.amid/git/token-bay/shared
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./exhaustionproof/...
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
cd /Users/dor.amid/git/token-bay
git add shared/exhaustionproof/proof.proto shared/exhaustionproof/proof.pb.go shared/exhaustionproof/proof_test.go
git commit -m "$(cat <<'EOF'
feat(shared/exhaustionproof): ExhaustionProofV1 protobuf schema

Two-signal rate-limit proof: StopFailure hook payload + /usage TUI
output, plus captured_at + nonce for freshness/replay. Drops the
proof-internal consumer_sig from architecture spec §7.2 — single sig
via the enclosing EnvelopeSigned (amendment ships in Task 9).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: ValidateProofV1

**Files:**
- Create: `shared/exhaustionproof/validate.go`
- Create: `shared/exhaustionproof/validate_test.go`

- [ ] **Step 1: Write the rejection-matrix test**

Create `shared/exhaustionproof/validate_test.go`:

```go
package exhaustionproof

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validProof() *ExhaustionProofV1 {
	return &ExhaustionProofV1{
		StopFailure: &StopFailure{
			Matcher:    "rate_limit",
			At:         1714000000,
			ErrorShape: []byte(`{}`),
		},
		UsageProbe: &UsageProbe{
			At:     1714000010,
			Output: []byte(`limit hit`),
		},
		CapturedAt: 1714000020,
		Nonce:      []byte("0123456789abcdef"),
	}
}

func TestValidateProofV1_HappyPath(t *testing.T) {
	require.NoError(t, ValidateProofV1(validProof()))
}

func TestValidateProofV1_Rejections(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(p *ExhaustionProofV1)
		errFrag string
	}{
		{"nil", func(p *ExhaustionProofV1) { *p = ExhaustionProofV1{} }, "nil"},
		{"missing stop_failure", func(p *ExhaustionProofV1) { p.StopFailure = nil }, "stop_failure"},
		{"missing usage_probe", func(p *ExhaustionProofV1) { p.UsageProbe = nil }, "usage_probe"},
		{"wrong matcher", func(p *ExhaustionProofV1) { p.StopFailure.Matcher = "server_error" }, "matcher"},
		{"empty matcher", func(p *ExhaustionProofV1) { p.StopFailure.Matcher = "" }, "matcher"},
		{"zero stop_failure.at", func(p *ExhaustionProofV1) { p.StopFailure.At = 0 }, "stop_failure.at"},
		{"zero usage_probe.at", func(p *ExhaustionProofV1) { p.UsageProbe.At = 0 }, "usage_probe.at"},
		{"signals 61s apart", func(p *ExhaustionProofV1) { p.UsageProbe.At = p.StopFailure.At + 61 }, "freshness"},
		{"signals 61s apart reversed", func(p *ExhaustionProofV1) { p.StopFailure.At = p.UsageProbe.At + 61 }, "freshness"},
		{"zero captured_at", func(p *ExhaustionProofV1) { p.CapturedAt = 0 }, "captured_at"},
		{"nonce too short", func(p *ExhaustionProofV1) { p.Nonce = []byte("short") }, "nonce"},
		{"nonce too long", func(p *ExhaustionProofV1) { p.Nonce = []byte("01234567890123456789") }, "nonce"},
		{"nonce nil", func(p *ExhaustionProofV1) { p.Nonce = nil }, "nonce"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validProof()
			tc.mutate(p)
			err := ValidateProofV1(p)
			require.Error(t, err, "case %q should fail validation", tc.name)
			assert.Contains(t, err.Error(), tc.errFrag, "error should mention %q", tc.errFrag)
		})
	}
}

func TestValidateProofV1_NilProof(t *testing.T) {
	err := ValidateProofV1(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil")
}
```

- [ ] **Step 2: Run, confirm FAIL**

```bash
cd /Users/dor.amid/git/token-bay/shared
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./exhaustionproof/...
```

Expected: compilation failure — `ValidateProofV1` undefined.

- [ ] **Step 3: Implement**

Create `shared/exhaustionproof/validate.go`:

```go
package exhaustionproof

import (
	"errors"
	"fmt"
)

// proofFreshnessWindow is the maximum allowed gap between the StopFailure
// timestamp and the UsageProbe timestamp. Beyond this, the two signals
// no longer plausibly correlate to the same rate-limit event.
const proofFreshnessWindow = 60 // seconds

// nonceLen is the required length of an ExhaustionProofV1.Nonce.
const nonceLen = 16

// ValidateProofV1 checks an ExhaustionProofV1 against the v1 wire-format rules.
// Returns nil if the proof is well-formed; an error describing the first
// violation otherwise. Callers (sender pre-sign / receiver post-parse) must
// run this before trusting the proof contents.
func ValidateProofV1(p *ExhaustionProofV1) error {
	if p == nil {
		return errors.New("exhaustionproof: nil ExhaustionProofV1")
	}
	if p.StopFailure == nil {
		return errors.New("exhaustionproof: missing stop_failure")
	}
	if p.UsageProbe == nil {
		return errors.New("exhaustionproof: missing usage_probe")
	}
	if p.StopFailure.Matcher != "rate_limit" {
		return fmt.Errorf("exhaustionproof: stop_failure.matcher = %q, want \"rate_limit\"", p.StopFailure.Matcher)
	}
	if p.StopFailure.At == 0 {
		return errors.New("exhaustionproof: stop_failure.at is zero")
	}
	if p.UsageProbe.At == 0 {
		return errors.New("exhaustionproof: usage_probe.at is zero")
	}
	gap := int64(p.StopFailure.At) - int64(p.UsageProbe.At)
	if gap < 0 {
		gap = -gap
	}
	if gap > proofFreshnessWindow {
		return fmt.Errorf("exhaustionproof: signals %ds apart, exceeds freshness window %ds", gap, proofFreshnessWindow)
	}
	if p.CapturedAt == 0 {
		return errors.New("exhaustionproof: captured_at is zero")
	}
	if len(p.Nonce) != nonceLen {
		return fmt.Errorf("exhaustionproof: nonce length %d, want %d", len(p.Nonce), nonceLen)
	}
	return nil
}
```

- [ ] **Step 4: Run, confirm PASS**

```bash
cd /Users/dor.amid/git/token-bay/shared
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./exhaustionproof/...
```

Expected: PASS, all 14 cases (1 happy + 13 rejections + 1 nil top-level).

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/git/token-bay
git add shared/exhaustionproof/validate.go shared/exhaustionproof/validate_test.go
git commit -m "$(cat <<'EOF'
feat(shared/exhaustionproof): ValidateProofV1

Enforces v1 wire-format invariants: matcher == "rate_limit", both
signal timestamps non-zero and within 60s of each other, captured_at
non-zero, nonce 16 bytes. Senders run pre-sign; receivers run
post-parse. Single freshness window constant so future tuning lives
in one place.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: SignedBalanceSnapshot schema + round-trip

**Files:**
- Create: `shared/proto/balance.proto`
- Create: `shared/proto/balance.pb.go` (generated)
- Create: `shared/proto/balance_test.go`

- [ ] **Step 1: Write the round-trip test**

Create `shared/proto/balance_test.go`:

```go
package proto

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestSignedBalanceSnapshot_RoundTrip(t *testing.T) {
	original := &SignedBalanceSnapshot{
		Body: &BalanceSnapshotBody{
			IdentityId:   make32(0xAA),
			Credits:      12345,
			ChainTipHash: make32(0xBB),
			ChainTipSeq:  9876,
			IssuedAt:     1714000000,
			ExpiresAt:    1714000600,
		},
		TrackerSig: make64(0xCC),
	}

	first, err := proto.MarshalOptions{Deterministic: true}.Marshal(original)
	require.NoError(t, err)

	var parsed SignedBalanceSnapshot
	require.NoError(t, proto.Unmarshal(first, &parsed))

	second, err := proto.MarshalOptions{Deterministic: true}.Marshal(&parsed)
	require.NoError(t, err)

	assert.Equal(t, first, second, "Deterministic marshal must be byte-stable")
	assert.Equal(t, original.Body.Credits, parsed.Body.Credits)
	assert.Equal(t, original.TrackerSig, parsed.TrackerSig)
}

// Test helpers — generate fixed-pattern byte slices for fixture data.
func make32(b byte) []byte { return bytes32(b) }
func make64(b byte) []byte { return bytes64(b) }
func bytes32(b byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = b
	}
	return out
}
func bytes64(b byte) []byte {
	out := make([]byte, 64)
	for i := range out {
		out[i] = b
	}
	return out
}
```

- [ ] **Step 2: Run, confirm FAIL**

```bash
cd /Users/dor.amid/git/token-bay/shared
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./proto/...
```

Expected: compilation failure — `SignedBalanceSnapshot`, `BalanceSnapshotBody` undefined.

- [ ] **Step 3: Write the schema**

Create `shared/proto/balance.proto`:

```proto
syntax = "proto3";
package tokenbay.proto.v1;

option go_package = "github.com/token-bay/token-bay/shared/proto";

// BalanceSnapshotBody — tracker-signed credit balance for an identity.
// Spec: docs/superpowers/specs/ledger/2026-04-22-ledger-design.md §3.4.
// Two-message split for canonical signing: tracker signs DeterministicMarshal(body).
message BalanceSnapshotBody {
  bytes  identity_id    = 1;  // 32 bytes — Ed25519 pubkey hash
  int64  credits        = 2;
  bytes  chain_tip_hash = 3;  // 32 bytes
  uint64 chain_tip_seq  = 4;
  uint64 issued_at      = 5;  // unix seconds
  uint64 expires_at     = 6;  // issued_at + 600 (10-minute freshness window)
}

// SignedBalanceSnapshot — wire form. Verifier recovers signed bytes via
// shared/signing.DeterministicMarshal(body).
message SignedBalanceSnapshot {
  BalanceSnapshotBody body        = 1;
  bytes               tracker_sig = 2;  // 64 bytes — Ed25519 signature
}
```

- [ ] **Step 4: Generate**

```bash
cd /Users/dor.amid/git/token-bay/shared
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" make proto-gen
```

Expected: `shared/proto/balance.pb.go` is created.

- [ ] **Step 5: Run, confirm PASS**

```bash
cd /Users/dor.amid/git/token-bay/shared
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./proto/...
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
cd /Users/dor.amid/git/token-bay
git add shared/proto/balance.proto shared/proto/balance.pb.go shared/proto/balance_test.go
git commit -m "$(cat <<'EOF'
feat(shared/proto): SignedBalanceSnapshot protobuf schema

Tracker-signed credit balance per ledger spec §3.4. Two-message split
(BalanceSnapshotBody + SignedBalanceSnapshot) — same canonical-signing
pattern as Envelope. Sign/verify helpers land in Task 7.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: EnvelopeBody + EnvelopeSigned schema + round-trip

**Files:**
- Create: `shared/proto/envelope.proto`
- Create: `shared/proto/envelope.pb.go` (generated)
- Create: `shared/proto/envelope_test.go`

- [ ] **Step 1: Write the round-trip test**

Create `shared/proto/envelope_test.go`:

```go
package proto

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/shared/exhaustionproof"
)

// fixtureEnvelopeBody returns a fully-populated EnvelopeBody with
// deterministic field values — used by round-trip, sign/verify, and the
// golden-fixture tests.
func fixtureEnvelopeBody() *EnvelopeBody {
	return &EnvelopeBody{
		ProtocolVersion: uint32(ProtocolVersion),
		ConsumerId:      bytes32(0x11),
		Model:           "claude-sonnet-4-6",
		MaxInputTokens:  4096,
		MaxOutputTokens: 1024,
		Tier:            PrivacyTier_PRIVACY_TIER_STANDARD,
		BodyHash:        bytes32(0x22),
		ExhaustionProof: &exhaustionproof.ExhaustionProofV1{
			StopFailure: &exhaustionproof.StopFailure{
				Matcher:    "rate_limit",
				At:         1714000000,
				ErrorShape: []byte(`{"type":"rate_limit_error"}`),
			},
			UsageProbe: &exhaustionproof.UsageProbe{
				At:     1714000010,
				Output: []byte(`limit hit`),
			},
			CapturedAt: 1714000020,
			Nonce:      []byte("proof-nonce-1234"), // 16 bytes
		},
		BalanceProof: &SignedBalanceSnapshot{
			Body: &BalanceSnapshotBody{
				IdentityId:   bytes32(0x33),
				Credits:      9999,
				ChainTipHash: bytes32(0x44),
				ChainTipSeq:  100,
				IssuedAt:     1714000000,
				ExpiresAt:    1714000600,
			},
			TrackerSig: bytes64(0x55),
		},
		CapturedAt: 1714000025,
		Nonce:      []byte("envelope-nonce12"), // 16 bytes
	}
}

func TestEnvelopeBody_RoundTrip(t *testing.T) {
	original := fixtureEnvelopeBody()

	first, err := proto.MarshalOptions{Deterministic: true}.Marshal(original)
	require.NoError(t, err)

	var parsed EnvelopeBody
	require.NoError(t, proto.Unmarshal(first, &parsed))

	second, err := proto.MarshalOptions{Deterministic: true}.Marshal(&parsed)
	require.NoError(t, err)

	assert.Equal(t, first, second, "Deterministic marshal must be byte-stable")
	assert.Equal(t, original.Model, parsed.Model)
	assert.Equal(t, original.Tier, parsed.Tier)
	assert.Equal(t, original.ExhaustionProof.StopFailure.Matcher, parsed.ExhaustionProof.StopFailure.Matcher)
	assert.Equal(t, original.BalanceProof.Body.Credits, parsed.BalanceProof.Body.Credits)
}

func TestEnvelopeSigned_RoundTrip(t *testing.T) {
	signed := &EnvelopeSigned{
		Body:        fixtureEnvelopeBody(),
		ConsumerSig: bytes64(0x77),
	}

	first, err := proto.MarshalOptions{Deterministic: true}.Marshal(signed)
	require.NoError(t, err)

	var parsed EnvelopeSigned
	require.NoError(t, proto.Unmarshal(first, &parsed))

	second, err := proto.MarshalOptions{Deterministic: true}.Marshal(&parsed)
	require.NoError(t, err)

	assert.Equal(t, first, second)
	assert.Equal(t, signed.ConsumerSig, parsed.ConsumerSig)
	assert.Equal(t, signed.Body.Model, parsed.Body.Model)
}
```

- [ ] **Step 2: Run, confirm FAIL**

```bash
cd /Users/dor.amid/git/token-bay/shared
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./proto/...
```

Expected: compilation failure — `EnvelopeBody`, `EnvelopeSigned`, `PrivacyTier_PRIVACY_TIER_STANDARD` undefined.

- [ ] **Step 3: Write the schema**

Create `shared/proto/envelope.proto`:

```proto
syntax = "proto3";
package tokenbay.proto.v1;

option go_package = "github.com/token-bay/token-bay/shared/proto";

import "exhaustionproof/proof.proto";
import "proto/balance.proto";

// PrivacyTier classifies a broker_request's confidentiality requirement.
// PRIVACY_TIER_UNSPECIFIED is rejected by ValidateEnvelopeBody.
enum PrivacyTier {
  PRIVACY_TIER_UNSPECIFIED = 0;
  PRIVACY_TIER_STANDARD    = 1;
  PRIVACY_TIER_TEE         = 2;
}

// EnvelopeBody — the signed payload of a broker_request.
// Spec: docs/superpowers/specs/shared/2026-04-24-wire-format-v1-design.md §5.3.
// All consumer-side validation rules in ValidateEnvelopeBody (validate.go).
message EnvelopeBody {
  uint32 protocol_version  = 1;   // matches shared/proto.ProtocolVersion
  bytes  consumer_id       = 2;   // 32 bytes — Ed25519 pubkey hash / IdentityID
  string model             = 3;
  uint64 max_input_tokens  = 4;
  uint64 max_output_tokens = 5;
  PrivacyTier tier         = 6;
  bytes  body_hash         = 7;   // 32 bytes — SHA-256 of full /v1/messages body

  tokenbay.exhaustionproof.v1.ExhaustionProofV1 exhaustion_proof = 8;
  SignedBalanceSnapshot balance_proof = 9;

  uint64 captured_at = 10;        // unix seconds — when envelope was assembled
  bytes  nonce       = 11;        // 16 bytes — envelope-level replay protection
}

// EnvelopeSigned — wire form. Receiver verifies sig via
// shared/signing.VerifyEnvelope.
message EnvelopeSigned {
  EnvelopeBody body         = 1;
  bytes        consumer_sig = 2;  // 64 bytes — Ed25519 over DeterministicMarshal(body)
}
```

- [ ] **Step 4: Generate**

```bash
cd /Users/dor.amid/git/token-bay/shared
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" make proto-gen
```

Expected: `shared/proto/envelope.pb.go` is created.

- [ ] **Step 5: Run, confirm PASS**

```bash
cd /Users/dor.amid/git/token-bay/shared
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./proto/...
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
cd /Users/dor.amid/git/token-bay
git add shared/proto/envelope.proto shared/proto/envelope.pb.go shared/proto/envelope_test.go
git commit -m "$(cat <<'EOF'
feat(shared/proto): EnvelopeBody + EnvelopeSigned protobuf schema

The broker_request payload — all 11 fields per architecture §2.1
step 4a, plus protocol_version and PrivacyTier enum. Two-message
split for canonical Ed25519 signing. Imports ExhaustionProofV1
and SignedBalanceSnapshot.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: ValidateEnvelopeBody

**Files:**
- Create: `shared/proto/validate.go`
- Create: `shared/proto/validate_test.go`

- [ ] **Step 1: Write the rejection-matrix test**

Create `shared/proto/validate_test.go`:

```go
package proto

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateEnvelopeBody_HappyPath(t *testing.T) {
	require.NoError(t, ValidateEnvelopeBody(fixtureEnvelopeBody()))
}

func TestValidateEnvelopeBody_Rejections(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(b *EnvelopeBody)
		errFrag string
	}{
		{"wrong protocol_version", func(b *EnvelopeBody) { b.ProtocolVersion = 99 }, "protocol_version"},
		{"consumer_id too short", func(b *EnvelopeBody) { b.ConsumerId = []byte{1, 2, 3} }, "consumer_id"},
		{"consumer_id too long", func(b *EnvelopeBody) { b.ConsumerId = make([]byte, 64) }, "consumer_id"},
		{"empty model", func(b *EnvelopeBody) { b.Model = "" }, "model"},
		{"body_hash wrong length", func(b *EnvelopeBody) { b.BodyHash = []byte{1, 2} }, "body_hash"},
		{"missing exhaustion_proof", func(b *EnvelopeBody) { b.ExhaustionProof = nil }, "exhaustion_proof"},
		{"missing balance_proof", func(b *EnvelopeBody) { b.BalanceProof = nil }, "balance_proof"},
		{"zero captured_at", func(b *EnvelopeBody) { b.CapturedAt = 0 }, "captured_at"},
		{"nonce too short", func(b *EnvelopeBody) { b.Nonce = []byte("short") }, "nonce"},
		{"unspecified tier", func(b *EnvelopeBody) { b.Tier = PrivacyTier_PRIVACY_TIER_UNSPECIFIED }, "tier"},
		{"unknown tier value", func(b *EnvelopeBody) { b.Tier = PrivacyTier(99) }, "tier"},
		{"invalid exhaustion_proof", func(b *EnvelopeBody) { b.ExhaustionProof.StopFailure.Matcher = "wrong" }, "exhaustion_proof"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := fixtureEnvelopeBody()
			tc.mutate(b)
			err := ValidateEnvelopeBody(b)
			require.Error(t, err, "case %q should fail validation", tc.name)
			assert.Contains(t, err.Error(), tc.errFrag, "error should mention %q", tc.errFrag)
		})
	}
}

func TestValidateEnvelopeBody_NilBody(t *testing.T) {
	err := ValidateEnvelopeBody(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil")
}
```

- [ ] **Step 2: Run, confirm FAIL**

```bash
cd /Users/dor.amid/git/token-bay/shared
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./proto/...
```

Expected: compilation failure — `ValidateEnvelopeBody` undefined.

- [ ] **Step 3: Implement**

Create `shared/proto/validate.go`:

```go
package proto

import (
	"errors"
	"fmt"

	"github.com/token-bay/token-bay/shared/exhaustionproof"
)

// envelopeIDLen is the required length of EnvelopeBody.ConsumerId.
const envelopeIDLen = 32

// envelopeBodyHashLen is the required length of EnvelopeBody.BodyHash.
const envelopeBodyHashLen = 32

// envelopeNonceLen is the required length of EnvelopeBody.Nonce.
const envelopeNonceLen = 16

// ValidateEnvelopeBody enforces v1 wire-format invariants on an EnvelopeBody.
// Senders run pre-sign; receivers (tracker) run post-parse before trusting
// any field. Returns nil if well-formed; an error describing the first
// violation otherwise.
func ValidateEnvelopeBody(b *EnvelopeBody) error {
	if b == nil {
		return errors.New("proto: nil EnvelopeBody")
	}
	if b.ProtocolVersion != uint32(ProtocolVersion) {
		return fmt.Errorf("proto: protocol_version = %d, want %d", b.ProtocolVersion, ProtocolVersion)
	}
	if len(b.ConsumerId) != envelopeIDLen {
		return fmt.Errorf("proto: consumer_id length %d, want %d", len(b.ConsumerId), envelopeIDLen)
	}
	if b.Model == "" {
		return errors.New("proto: model is empty")
	}
	if len(b.BodyHash) != envelopeBodyHashLen {
		return fmt.Errorf("proto: body_hash length %d, want %d", len(b.BodyHash), envelopeBodyHashLen)
	}
	switch b.Tier {
	case PrivacyTier_PRIVACY_TIER_STANDARD, PrivacyTier_PRIVACY_TIER_TEE:
		// ok
	default:
		return fmt.Errorf("proto: tier = %v, must be STANDARD or TEE", b.Tier)
	}
	if b.ExhaustionProof == nil {
		return errors.New("proto: missing exhaustion_proof")
	}
	if err := exhaustionproof.ValidateProofV1(b.ExhaustionProof); err != nil {
		return fmt.Errorf("proto: exhaustion_proof: %w", err)
	}
	if b.BalanceProof == nil {
		return errors.New("proto: missing balance_proof")
	}
	if b.CapturedAt == 0 {
		return errors.New("proto: captured_at is zero")
	}
	if len(b.Nonce) != envelopeNonceLen {
		return fmt.Errorf("proto: nonce length %d, want %d", len(b.Nonce), envelopeNonceLen)
	}
	return nil
}
```

- [ ] **Step 4: Run, confirm PASS**

```bash
cd /Users/dor.amid/git/token-bay/shared
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./proto/...
```

Expected: PASS, all 12 rejection cases + happy path + nil top-level.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/git/token-bay
git add shared/proto/validate.go shared/proto/validate_test.go
git commit -m "$(cat <<'EOF'
feat(shared/proto): ValidateEnvelopeBody

Enforces all field-level invariants from wire-format spec §6: correct
protocol_version, 32-byte consumer_id and body_hash, 16-byte nonce,
non-empty model, valid PrivacyTier (rejecting UNSPECIFIED and unknown
values), and recursive ValidateProofV1 on the embedded exhaustion
proof. Nested validation error wraps the inner error for caller-side
debuggability.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 7: DeterministicMarshal + Sign/Verify helpers

**Files:**
- Create: `shared/signing/proto.go`
- Create: `shared/signing/proto_test.go`

- [ ] **Step 1: Write the helper tests**

Create `shared/signing/proto_test.go`:

```go
package signing

import (
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/exhaustionproof"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// fixtureKeypair derives a deterministic Ed25519 keypair from a fixed seed.
// Used across all signing tests so signatures are reproducible.
func fixtureKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	seed := []byte("token-bay-fixture-seed-v1-000000") // 32 bytes
	require.Len(t, seed, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(seed)
	return priv.Public().(ed25519.PublicKey), priv
}

func fixtureBody() *tbproto.EnvelopeBody {
	return &tbproto.EnvelopeBody{
		ProtocolVersion: uint32(tbproto.ProtocolVersion),
		ConsumerId:      bytes32(0x11),
		Model:           "claude-sonnet-4-6",
		MaxInputTokens:  4096,
		MaxOutputTokens: 1024,
		Tier:            tbproto.PrivacyTier_PRIVACY_TIER_STANDARD,
		BodyHash:        bytes32(0x22),
		ExhaustionProof: &exhaustionproof.ExhaustionProofV1{
			StopFailure: &exhaustionproof.StopFailure{Matcher: "rate_limit", At: 1714000000, ErrorShape: []byte(`{}`)},
			UsageProbe:  &exhaustionproof.UsageProbe{At: 1714000010, Output: []byte(`x`)},
			CapturedAt:  1714000020,
			Nonce:       []byte("proof-nonce-1234"),
		},
		BalanceProof: &tbproto.SignedBalanceSnapshot{
			Body: &tbproto.BalanceSnapshotBody{
				IdentityId: bytes32(0x33), Credits: 99, ChainTipHash: bytes32(0x44),
				ChainTipSeq: 1, IssuedAt: 1714000000, ExpiresAt: 1714000600,
			},
			TrackerSig: bytes64(0x55),
		},
		CapturedAt: 1714000025,
		Nonce:      []byte("envelope-nonce12"),
	}
}

func bytes32(b byte) []byte { return repeat(b, 32) }
func bytes64(b byte) []byte { return repeat(b, 64) }
func repeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

func TestDeterministicMarshal_Stable(t *testing.T) {
	body := fixtureBody()
	a, err := DeterministicMarshal(body)
	require.NoError(t, err)
	b, err := DeterministicMarshal(body)
	require.NoError(t, err)
	assert.Equal(t, a, b, "DeterministicMarshal must be byte-stable across calls")
}

func TestDeterministicMarshal_NilReturnsError(t *testing.T) {
	_, err := DeterministicMarshal(nil)
	require.Error(t, err)
}

func TestSignVerifyEnvelope_RoundTrip(t *testing.T) {
	pub, priv := fixtureKeypair(t)
	body := fixtureBody()

	sig, err := SignEnvelope(priv, body)
	require.NoError(t, err)
	require.Len(t, sig, ed25519.SignatureSize)

	signed := &tbproto.EnvelopeSigned{Body: body, ConsumerSig: sig}
	assert.True(t, VerifyEnvelope(pub, signed))
}

func TestVerifyEnvelope_TamperedBodyFails(t *testing.T) {
	pub, priv := fixtureKeypair(t)
	body := fixtureBody()
	sig, err := SignEnvelope(priv, body)
	require.NoError(t, err)

	body.Model = "claude-sonnet-4-6-tampered"
	signed := &tbproto.EnvelopeSigned{Body: body, ConsumerSig: sig}
	assert.False(t, VerifyEnvelope(pub, signed))
}

func TestVerifyEnvelope_TamperedSigFails(t *testing.T) {
	pub, priv := fixtureKeypair(t)
	body := fixtureBody()
	sig, err := SignEnvelope(priv, body)
	require.NoError(t, err)
	sig[0] ^= 0xFF

	signed := &tbproto.EnvelopeSigned{Body: body, ConsumerSig: sig}
	assert.False(t, VerifyEnvelope(pub, signed))
}

func TestVerifyEnvelope_WrongKeyFails(t *testing.T) {
	_, priv := fixtureKeypair(t)
	body := fixtureBody()
	sig, err := SignEnvelope(priv, body)
	require.NoError(t, err)

	otherSeed := make([]byte, ed25519.SeedSize)
	otherSeed[0] = 0xFF
	otherPriv := ed25519.NewKeyFromSeed(otherSeed)
	otherPub := otherPriv.Public().(ed25519.PublicKey)

	signed := &tbproto.EnvelopeSigned{Body: body, ConsumerSig: sig}
	assert.False(t, VerifyEnvelope(otherPub, signed))
}

func TestVerifyEnvelope_NilSafety(t *testing.T) {
	pub, _ := fixtureKeypair(t)
	assert.False(t, VerifyEnvelope(pub, nil))
	assert.False(t, VerifyEnvelope(pub, &tbproto.EnvelopeSigned{Body: nil, ConsumerSig: bytes64(0x00)}))
	assert.False(t, VerifyEnvelope(nil, &tbproto.EnvelopeSigned{Body: fixtureBody(), ConsumerSig: bytes64(0x00)}))
}

func TestSignEnvelope_BadKeyLength(t *testing.T) {
	_, err := SignEnvelope(ed25519.PrivateKey{1, 2, 3}, fixtureBody())
	require.Error(t, err)
}

func TestSignEnvelope_NilBody(t *testing.T) {
	_, priv := fixtureKeypair(t)
	_, err := SignEnvelope(priv, nil)
	require.Error(t, err)
}

func TestSignVerifyBalanceSnapshot_RoundTrip(t *testing.T) {
	pub, priv := fixtureKeypair(t)
	body := &tbproto.BalanceSnapshotBody{
		IdentityId: bytes32(0xAA), Credits: 500, ChainTipHash: bytes32(0xBB),
		ChainTipSeq: 7, IssuedAt: 1714000000, ExpiresAt: 1714000600,
	}

	sig, err := SignBalanceSnapshot(priv, body)
	require.NoError(t, err)
	signed := &tbproto.SignedBalanceSnapshot{Body: body, TrackerSig: sig}
	assert.True(t, VerifyBalanceSnapshot(pub, signed))

	// Tamper detection.
	body.Credits = 999
	assert.False(t, VerifyBalanceSnapshot(pub, signed))
}

func TestVerifyBalanceSnapshot_NilSafety(t *testing.T) {
	pub, _ := fixtureKeypair(t)
	assert.False(t, VerifyBalanceSnapshot(pub, nil))
	assert.False(t, VerifyBalanceSnapshot(pub, &tbproto.SignedBalanceSnapshot{Body: nil}))
}
```

- [ ] **Step 2: Run, confirm FAIL**

```bash
cd /Users/dor.amid/git/token-bay/shared
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./signing/...
```

Expected: compilation failure — `DeterministicMarshal`, `SignEnvelope`, `VerifyEnvelope`, `SignBalanceSnapshot`, `VerifyBalanceSnapshot` undefined.

- [ ] **Step 3: Implement**

Create `shared/signing/proto.go`:

```go
package signing

import (
	"crypto/ed25519"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// DeterministicMarshal serializes m with proto.MarshalOptions{Deterministic: true}.
// This is the single canonical-bytes choke point for all sign/verify operations
// across the shared/ module — see shared/CLAUDE.md rule 6. Calling proto.Marshal
// directly on a message that gets signed bypasses the determinism guarantee.
func DeterministicMarshal(m proto.Message) ([]byte, error) {
	if m == nil {
		return nil, errors.New("signing: DeterministicMarshal on nil message")
	}
	return proto.MarshalOptions{Deterministic: true}.Marshal(m)
}

// SignEnvelope returns an Ed25519 signature over DeterministicMarshal(body).
// Returns an error on a wrong-length private key or nil body. Pre-condition:
// body has been validated by tbproto.ValidateEnvelopeBody — this helper does
// NOT re-run validation, so callers can choose between fail-fast and
// best-effort signing semantics.
func SignEnvelope(priv ed25519.PrivateKey, body *tbproto.EnvelopeBody) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("signing: private key length %d, want %d", len(priv), ed25519.PrivateKeySize)
	}
	if body == nil {
		return nil, errors.New("signing: SignEnvelope on nil body")
	}
	buf, err := DeterministicMarshal(body)
	if err != nil {
		return nil, fmt.Errorf("signing: marshal envelope body: %w", err)
	}
	return ed25519.Sign(priv, buf), nil
}

// VerifyEnvelope reports whether signed.ConsumerSig is a valid Ed25519 signature
// under pub over DeterministicMarshal(signed.Body). Nil inputs, malformed keys
// or sigs, or marshal failures all return false without panicking.
func VerifyEnvelope(pub ed25519.PublicKey, signed *tbproto.EnvelopeSigned) bool {
	if signed == nil || signed.Body == nil {
		return false
	}
	buf, err := DeterministicMarshal(signed.Body)
	if err != nil {
		return false
	}
	return Verify(pub, buf, signed.ConsumerSig)
}

// SignBalanceSnapshot is the parallel of SignEnvelope for the tracker's
// balance attestation. The tracker calls this; consumers verify via
// VerifyBalanceSnapshot.
func SignBalanceSnapshot(priv ed25519.PrivateKey, body *tbproto.BalanceSnapshotBody) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("signing: private key length %d, want %d", len(priv), ed25519.PrivateKeySize)
	}
	if body == nil {
		return nil, errors.New("signing: SignBalanceSnapshot on nil body")
	}
	buf, err := DeterministicMarshal(body)
	if err != nil {
		return nil, fmt.Errorf("signing: marshal balance snapshot body: %w", err)
	}
	return ed25519.Sign(priv, buf), nil
}

// VerifyBalanceSnapshot — see VerifyEnvelope for nil-safety contract.
func VerifyBalanceSnapshot(pub ed25519.PublicKey, signed *tbproto.SignedBalanceSnapshot) bool {
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
cd /Users/dor.amid/git/token-bay/shared
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./signing/...
```

Expected: PASS — all 11 tests including tampered-body, tampered-sig, wrong-key, nil-safety, bad-key-length.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/git/token-bay
git add shared/signing/proto.go shared/signing/proto_test.go
git commit -m "$(cat <<'EOF'
feat(shared/signing): DeterministicMarshal + envelope/balance sign/verify

Single canonical-bytes choke point (DeterministicMarshal) plus four
helpers wrapping it: SignEnvelope/VerifyEnvelope and the parallel
pair for SignedBalanceSnapshot. All Verify variants fail closed on
nil, wrong-length keys, or marshal errors — no panics. Tests cover
tamper detection on body and sig, wrong-key rejection, nil safety,
and a fixture keypair seeded deterministically so signatures stay
reproducible across runs.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 8: Golden fixture test

**Files:**
- Create: `shared/proto/testdata/envelope_signed.golden.hex`
- Modify: `shared/proto/envelope_test.go` (add golden test)

The golden test is the cross-version protobuf-determinism tripwire: any future library upgrade that changes byte output for an unchanged message fails CI here, *before* it silently breaks a deployed signature.

- [ ] **Step 1: Add the golden test to envelope_test.go**

Append to `shared/proto/envelope_test.go`:

```go
import (
	"crypto/ed25519"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	// keep existing imports
)

const goldenPath = "testdata/envelope_signed.golden.hex"

// goldenKeypair returns the deterministic Ed25519 keypair used to produce
// the golden fixture. Same seed as shared/signing tests so signatures
// reproduce across packages.
func goldenKeypair() (ed25519.PublicKey, ed25519.PrivateKey) {
	seed := []byte("token-bay-fixture-seed-v1-000000") // 32 bytes
	priv := ed25519.NewKeyFromSeed(seed)
	return priv.Public().(ed25519.PublicKey), priv
}

func TestEnvelopeSigned_GoldenBytes(t *testing.T) {
	body := fixtureEnvelopeBody()

	bodyBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(body)
	require.NoError(t, err)

	_, priv := goldenKeypair()
	sig := ed25519.Sign(priv, bodyBytes)

	signed := &EnvelopeSigned{Body: body, ConsumerSig: sig}
	wireBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(signed)
	require.NoError(t, err)

	gotHex := hex.EncodeToString(wireBytes)

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		require.NoError(t, os.MkdirAll(filepath.Dir(goldenPath), 0o755))
		require.NoError(t, os.WriteFile(goldenPath, []byte(gotHex+"\n"), 0o644))
		t.Logf("golden updated at %s", goldenPath)
		return
	}

	wantBytes, err := os.ReadFile(goldenPath)
	require.NoError(t, err, "missing golden file — run: UPDATE_GOLDEN=1 go test ./proto/... -run TestEnvelopeSigned_GoldenBytes")
	wantHex := strings.TrimSpace(string(wantBytes))

	assert.Equal(t, wantHex, gotHex,
		"EnvelopeSigned wire bytes differ from golden. If schema/lib intentionally changed, "+
			"regenerate via UPDATE_GOLDEN=1 and review the diff manually.")
}
```

- [ ] **Step 2: Generate the golden file**

```bash
cd /Users/dor.amid/git/token-bay/shared
PATH="$HOME/.local/share/mise/shims:$PATH" UPDATE_GOLDEN=1 go test -race -count=1 ./proto/... -run TestEnvelopeSigned_GoldenBytes -v
```

Expected: test passes; `shared/proto/testdata/envelope_signed.golden.hex` is created (~one long hex line).

- [ ] **Step 3: Run normally to confirm tripwire works**

```bash
cd /Users/dor.amid/git/token-bay/shared
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./proto/...
```

Expected: PASS, including `TestEnvelopeSigned_GoldenBytes`.

- [ ] **Step 4: Sanity-check the tripwire negative case**

Temporarily edit `fixtureEnvelopeBody` to change `Model` to `"tampered"`, re-run the test:

```bash
cd /Users/dor.amid/git/token-bay/shared
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./proto/... -run TestEnvelopeSigned_GoldenBytes
```

Expected: FAIL with diff. **Then revert the edit** and re-run to confirm PASS.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/git/token-bay
git add shared/proto/envelope_test.go shared/proto/testdata/envelope_signed.golden.hex
git commit -m "$(cat <<'EOF'
test(shared/proto): EnvelopeSigned golden-bytes tripwire

Hex fixture of a populated EnvelopeSigned signed with a deterministic
seed. Any drift in the protobuf library's byte output for unchanged
input fails CI — catches accidental upgrades that break canonical
serialization before they silently break deployed signatures.

Update procedure (when a schema change is intentional):
  UPDATE_GOLDEN=1 go test ./proto/... -run TestEnvelopeSigned_GoldenBytes

Reviewing the regenerated hex is the human checkpoint that confirms
the bytes changed the way we expected.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 9: Architecture spec §7.2 amendment + root proto-check passthrough

**Files:**
- Modify: `docs/superpowers/specs/2026-04-22-token-bay-architecture-design.md`
- Modify: `Makefile` (root)

- [ ] **Step 1: Open architecture spec §7.2**

Locate the `**v1 baseline — two-signal bundle from the Claude Code bridge.**` block. The current schema lists:

```
{
  stop_failure: {
    matcher:     "rate_limit",
    at:          u64,
    error_shape: <plugin-captured>,
  },
  usage_probe: {
    at:          u64,
    output:      <parsed /usage result>,
  },
  captured_at:     u64,
  consumer_nonce:  bytes16,
  consumer_sig:    bytes64
}
```

Remove the `consumer_sig: bytes64` line and rename `consumer_nonce` to `nonce` to match the wire schema.

Also append a new sentence after the schema block:

> *Amended 2026-04-25: ProofV1 has no standalone signature — the enclosing `EnvelopeSigned.consumer_sig` covers the proof's bytes. Standalone proof verification is not exercised by any v1 use case; if a future use case (e.g., gossiped reputation audit) requires it, reintroduce a proof-internal sig in a versioned amendment. Schema lives in `shared/exhaustionproof/proof.proto`.*

- [ ] **Step 2: Add proto-check passthrough to root Makefile**

Open `/Users/dor.amid/git/token-bay/Makefile`. Add a new target after `check`:

```makefile
proto-check:
	@for m in $(MODULES); do \
		if [ -f $$m/Makefile ] && grep -q '^proto-check:' $$m/Makefile; then \
			echo "=== proto-check: $$m ==="; \
			$(MAKE) -C $$m proto-check || exit 1; \
		fi; \
	done
```

And add `proto-check` to the `.PHONY` line.

- [ ] **Step 3: Verify root passthrough works**

```bash
cd /Users/dor.amid/git/token-bay
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" make proto-check
```

Expected: runs `make -C shared proto-check`, regenerates `.pb.go` files into the same bytes as committed, exits 0.

- [ ] **Step 4: Commit**

```bash
cd /Users/dor.amid/git/token-bay
git add docs/superpowers/specs/2026-04-22-token-bay-architecture-design.md Makefile
git commit -m "$(cat <<'EOF'
spec: amend §7.2 — drop ExhaustionProofV1.consumer_sig

Wire-format v1 (shared spec 2026-04-24) ships ProofV1 without a
standalone signature; the enclosing EnvelopeSigned.consumer_sig
covers the proof's bytes. Standalone proof verification has no
v1 use case. Renames consumer_nonce → nonce to match the proto
schema.

Also adds root-level make proto-check that recurses into modules
exposing the target — ready for CI hookup.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 10: Coverage check + tag + push

- [ ] **Step 1: Run shared/ tests with coverage**

```bash
cd /Users/dor.amid/git/token-bay/shared
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -coverprofile=coverage.out ./...
PATH="$HOME/.local/share/mise/shims:$PATH" go tool cover -func=coverage.out | tail -25
```

Expected: ≥ 90% coverage on `shared/exhaustionproof`, `shared/proto`, `shared/signing`. Generated `.pb.go` files dominate LOC but their getters/setters are exercised by the round-trip tests.

If a package falls below 90%, identify uncovered branches via `go tool cover -html=coverage.out` and add tests before tagging.

- [ ] **Step 2: Run root make check**

```bash
cd /Users/dor.amid/git/token-bay
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" make check
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" make proto-check
```

Expected: all module tests pass; lint clean; proto-check no-op (regenerates to identical bytes).

- [ ] **Step 3: Tag**

```bash
cd /Users/dor.amid/git/token-bay
git tag -a shared-wire-format-v1 -m "shared/ wire-format v1 (envelope + proof + balance snapshot + sign/verify helpers)"
```

- [ ] **Step 4: Push**

```bash
git push origin main --follow-tags
```

Expected: 9 commits + 1 annotated tag pushed to `origin/main`.

---

## 3. Out of scope for v1

These ship in subsequent feature plans:

- **`plugin/internal/envelopebuilder`** — assembles `EnvelopeBody` from `ccproxy.EntryMetadata` + identity key. Cannot exist without the schemas this plan ships.
- **`plugin/internal/trackerclient`** — QUIC client + RPC framing for sending envelopes. Heaviest plugin module; its own plan once schemas land.
- **Tracker-side validation pipeline** — replay-window check, balance-freshness check, proof-signal correlation. Lives in `tracker/internal/broker` when written.
- **Real `NetworkRouter`** — uses all of the above to forward `/v1/messages` over the network. The original "next plan" entry from the ccproxy plan; depends on this whole stack.
- **`shared/ledger/` package** — when the ledger feature plan lands, `SignedBalanceSnapshot` may move there, or the existing definition in `shared/proto/` may stay and the ledger package adds Entry / Merkle types. Decision deferred to that plan.

## 4. Open questions

- **`UpdateGolden=1` vs. test-tag.** The plan uses an env var. An alternative is a `// +build update_golden` build tag. Env var keeps the test discoverable in default test runs (where it acts as the verifier); a build tag would hide it. Sticking with env var; revisit if it becomes a footgun.
- **Tracker public key distribution.** `VerifyBalanceSnapshot` needs the tracker's pubkey. Distribution mechanism (bootstrap list signed with project release key, peer-exchange refresh) is the bootstrap/discovery problem from architecture §3.1 — separate plan.
- **Generated-file lint rules.** `golangci-lint` may flag generated `.pb.go` files for naming conventions or shadowing. If `make lint` complains in Task 10 step 2, add `//nolint` directives or a `.golangci.yml` exclude for `*.pb.go`. Worth a follow-up if it bites.

## 5. Self-review

**Spec coverage:**
- §2 scope (ProofV1, Envelope split, BalanceSnapshot, signing helpers, validation, CLAUDE.md amendment, architecture amendment) — Tasks 2/5/4/7/3+6/1/9 respectively. ✓
- §4.1 deterministic marshal choke point — Task 7 with explicit nil rejection + tests. ✓
- §4.2 two-message split — every signed message follows the pattern (Tasks 4, 5). ✓
- §4.3 single-sig — proof.proto has no consumer_sig field (Task 2); architecture spec amended (Task 9). ✓
- §5 schemas — Tasks 2/4/5 implement exactly the field lists in the spec. ✓
- §6 helpers + validators — Tasks 3, 6, 7. Validation rules table maps 1:1 to test cases. ✓
- §7.1 tests — round-trip in 2/4/5; golden in 8; sign/verify + tamper + nil in 7; rejection matrix in 3, 6. ✓
- §7.2 golden refresh protocol — Task 8 step 2 documents `UPDATE_GOLDEN=1`. ✓
- §7.3 ≥90% coverage — Task 10 step 1 enforces. ✓
- §8 file impact — every file modification accounted for. ✓
- §9 amendments — Task 1 (CLAUDE.md), Task 9 (architecture). ✓

**Placeholder scan:** zero "TBD"/"TODO"/"add appropriate"/"similar to". Every code step shows full code. Every command shows expected output. Every test case lists the exact field being mutated and the exact error fragment expected.

**Type consistency:** `ValidateProofV1`, `ValidateEnvelopeBody`, `DeterministicMarshal`, `SignEnvelope`, `VerifyEnvelope`, `SignBalanceSnapshot`, `VerifyBalanceSnapshot`, `EnvelopeBody`, `EnvelopeSigned`, `BalanceSnapshotBody`, `SignedBalanceSnapshot`, `ExhaustionProofV1`, `StopFailure`, `UsageProbe`, `PrivacyTier_PRIVACY_TIER_STANDARD`, `tbproto` import alias — all consistent across tasks. Field names (`Body`, `ConsumerSig`, `TrackerSig`, `ConsumerId`, etc.) match between schema, validators, helpers, and tests.

## 6. Next plans (after this lands)

Per the ccproxy plan's "next plan" cluster, this unblocks:

1. **`plugin/internal/envelopebuilder`** — small, can start immediately after this lands.
2. **`plugin/internal/trackerclient`** — bigger; QUIC + RPC.
3. **`plugin/internal/ccproxy-network`** — the real `NetworkRouter` that ties it all together.
4. **`tracker/internal/broker`** — parses + validates envelopes, runs selection algorithm.
