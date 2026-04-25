# Plugin envelope + exhaustion-proof builders Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship two sibling packages — `plugin/internal/exhaustionproofbuilder` and `plugin/internal/envelopebuilder` — that take fallback-ticket data + a parsed `/v1/messages` request + a tracker-issued balance snapshot and produce a fully-validated, signed `*EnvelopeSigned` ready for the future trackerclient to ship. First plugin-side import of `shared/`.

**Architecture:** Two peer packages (no mutual imports). Inputs are typed structs (`ProofInput`, `RequestSpec`); identity is via a `Signer` interface (production impl in future `internal/identity`, tests use a fake). Both Builders validate-before-sign via `shared/exhaustionproof.ValidateProofV1` / `shared/proto.ValidateEnvelopeBody`. Time and randomness are injected via function-typed fields (`Now`, `RandRead`) defaulting to `time.Now` and `crypto/rand.Read`.

**Tech Stack:** Go 1.23, `google.golang.org/protobuf` (transitively via `shared/`), stdlib `crypto/rand` + `crypto/ed25519`, `github.com/stretchr/testify`.

**Spec:** `docs/superpowers/specs/plugin/2026-04-25-envelopebuilder-design.md`

---

## 1. File map

Created in this plan:

| Path | Purpose |
|---|---|
| `plugin/internal/exhaustionproofbuilder/doc.go` | Package documentation comment |
| `plugin/internal/exhaustionproofbuilder/exhaustionproofbuilder.go` | `ProofInput`, sentinels, `Builder`, `NewBuilder`, `Build` |
| `plugin/internal/exhaustionproofbuilder/exhaustionproofbuilder_test.go` | All tests |
| `plugin/internal/envelopebuilder/doc.go` | Package documentation comment |
| `plugin/internal/envelopebuilder/envelopebuilder.go` | `RequestSpec`, `Signer`, sentinels, `Builder`, `NewBuilder`, `Build` |
| `plugin/internal/envelopebuilder/envelopebuilder_test.go` | All tests |

Modified:

| Path | Change |
|---|---|
| `plugin/go.mod` | First `require` for `github.com/token-bay/token-bay/shared` (the `replace` already exists). Added by `go mod tidy` once Task 1 introduces the import. |
| `plugin/go.sum` | Auto-populated by `go mod tidy`. |

---

## 2. Conventions used in this plan

- All Go commands run from `/Users/dor.amid/.superset/worktrees/token-bay/receptive-arch/plugin` unless stated. Use absolute paths in commands.
- `PATH="$HOME/.local/share/mise/shims:$PATH"` may be required if Go is managed by mise. Bash steps include it.
- One commit per task. Conventional-commit prefixes: `feat(plugin/<pkg>):`, `test(plugin/<pkg>):`, `chore(plugin):`.
- Co-Authored-By footer (every commit): `Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>`.
- Module path: `github.com/token-bay/token-bay/plugin`. Shared module: `github.com/token-bay/token-bay/shared`.
- Test helpers (fakes, fixed `RandRead`, etc.) live in the same `_test.go` file as the tests that use them — no separate testing helpers package.

---

## Task 1: `exhaustionproofbuilder` — full package + first plugin→shared import

**Files:**
- Create: `plugin/internal/exhaustionproofbuilder/doc.go`
- Create: `plugin/internal/exhaustionproofbuilder/exhaustionproofbuilder.go`
- Create: `plugin/internal/exhaustionproofbuilder/exhaustionproofbuilder_test.go`
- Modify: `plugin/go.mod`, `plugin/go.sum`

- [ ] **Step 1: Write the failing test file**

Create `plugin/internal/exhaustionproofbuilder/exhaustionproofbuilder_test.go`:

```go
package exhaustionproofbuilder

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/shared/exhaustionproof"
)

// (errors imported for errors.Is in TestBuilder_Build_RejectsInvalidInput etc.;
//  io for io.ErrUnexpectedEOF in errRand; proto for the determinism test.)

// fixedNow returns a fixed time.Time for deterministic captured_at.
var fixedNow = func() time.Time {
	return time.Unix(1714000010, 0).UTC()
}

// zeroRand fills the buffer with 0x00 bytes — a deterministic stand-in for
// crypto/rand.Read in tests.
func zeroRand(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// errRand always returns an io error.
func errRand(_ []byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}

// validInput returns a fully-populated ProofInput that should pass all rules.
func validInput() ProofInput {
	return ProofInput{
		StopFailureMatcher:    "rate_limit",
		StopFailureAt:         time.Unix(1714000000, 0).UTC(),
		StopFailureErrorShape: []byte(`{"type":"rate_limit_error"}`),
		UsageProbeAt:          time.Unix(1714000005, 0).UTC(),
		UsageProbeOutput:      []byte(`Current session: 99% used`),
	}
}

func newTestBuilder() *Builder {
	b := NewBuilder()
	b.Now = fixedNow
	b.RandRead = zeroRand
	return b
}

func TestBuilder_Build_HappyPath(t *testing.T) {
	b := newTestBuilder()

	proof, err := b.Build(validInput())
	require.NoError(t, err)
	require.NotNil(t, proof)

	assert.Equal(t, "rate_limit", proof.StopFailure.Matcher)
	assert.Equal(t, uint64(1714000000), proof.StopFailure.At)
	assert.Equal(t, []byte(`{"type":"rate_limit_error"}`), proof.StopFailure.ErrorShape)
	assert.Equal(t, uint64(1714000005), proof.UsageProbe.At)
	assert.Equal(t, []byte(`Current session: 99% used`), proof.UsageProbe.Output)
	assert.Equal(t, uint64(1714000010), proof.CapturedAt)
	assert.Len(t, proof.Nonce, 16)
	assert.Equal(t, make([]byte, 16), proof.Nonce, "zeroRand should fill 16 zero bytes")

	// Independent re-validation: builder claims valid → ValidateProofV1 agrees.
	require.NoError(t, exhaustionproof.ValidateProofV1(proof))
}

func TestBuilder_Build_RejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(*ProofInput)
		wantField string
	}{
		{"empty matcher", func(in *ProofInput) { in.StopFailureMatcher = "" }, "StopFailureMatcher"},
		{"zero stop_failure.at", func(in *ProofInput) { in.StopFailureAt = time.Time{} }, "StopFailureAt"},
		{"zero usage_probe.at", func(in *ProofInput) { in.UsageProbeAt = time.Time{} }, "UsageProbeAt"},
		{"empty error_shape", func(in *ProofInput) { in.StopFailureErrorShape = nil }, "StopFailureErrorShape"},
		{"empty usage_probe.output", func(in *ProofInput) { in.UsageProbeOutput = nil }, "UsageProbeOutput"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validInput()
			tc.mutate(&in)
			b := newTestBuilder()
			proof, err := b.Build(in)
			assert.Nil(t, proof)
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrInvalidInput),
				"expected ErrInvalidInput, got: %v", err)
			assert.Contains(t, err.Error(), tc.wantField,
				"error should name the offending field: %v", err)
		})
	}
}

func TestBuilder_Build_FreshnessWindowExceeded(t *testing.T) {
	in := validInput()
	// 61s gap — outside the 60s window enforced by ValidateProofV1.
	in.UsageProbeAt = in.StopFailureAt.Add(61 * time.Second)
	b := newTestBuilder()
	proof, err := b.Build(in)
	assert.Nil(t, proof)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrValidation),
		"expected ErrValidation, got: %v", err)
	assert.Contains(t, err.Error(), "freshness")
}

func TestBuilder_Build_RandReadFailure(t *testing.T) {
	b := NewBuilder()
	b.Now = fixedNow
	b.RandRead = errRand
	proof, err := b.Build(validInput())
	assert.Nil(t, proof)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrRandFailed),
		"expected ErrRandFailed, got: %v", err)
}

func TestBuilder_Build_Determinism(t *testing.T) {
	b := newTestBuilder()
	a, err := b.Build(validInput())
	require.NoError(t, err)
	c, err := b.Build(validInput())
	require.NoError(t, err)

	first, err := proto.MarshalOptions{Deterministic: true}.Marshal(a)
	require.NoError(t, err)
	second, err := proto.MarshalOptions{Deterministic: true}.Marshal(c)
	require.NoError(t, err)
	assert.Equal(t, first, second,
		"same inputs + same fixed Now/RandRead must produce byte-identical marshal")
}

func TestNewBuilder_Defaults(t *testing.T) {
	b := NewBuilder()
	require.NotNil(t, b)
	assert.NotNil(t, b.Now)
	assert.NotNil(t, b.RandRead)
}
```

- [ ] **Step 2: Run the test, confirm it fails with "package not found" or undefined identifiers**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/receptive-arch/plugin
PATH="$HOME/.local/share/mise/shims:$PATH" go test -count=1 ./internal/exhaustionproofbuilder/...
```

Expected: compilation failure. Errors like *"no Go files"* or `Builder undefined`, `ProofInput undefined`, `ErrInvalidInput undefined`, etc. The package itself doesn't exist yet.

- [ ] **Step 3: Create the package doc file**

Create `plugin/internal/exhaustionproofbuilder/doc.go`:

```go
// Package exhaustionproofbuilder assembles a *exhaustionproof.ExhaustionProofV1
// from a typed ProofInput. The caller (ccproxy-network) adapts its own state
// (cached fallback ticket + /usage probe timestamp) into ProofInput.
//
// The builder validates the assembled proof via shared/exhaustionproof.ValidateProofV1
// before returning. Time and randomness are injected as function-typed fields
// to keep tests deterministic; production uses time.Now and crypto/rand.Read.
//
// This package imports only shared/exhaustionproof + Go stdlib. It is a leaf
// of the plugin-internal dependency graph.
package exhaustionproofbuilder
```

- [ ] **Step 4: Create the production file**

Create `plugin/internal/exhaustionproofbuilder/exhaustionproofbuilder.go`:

```go
package exhaustionproofbuilder

import (
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/token-bay/token-bay/shared/exhaustionproof"
)

// Sentinel errors. Callers discriminate via errors.Is.
var (
	ErrInvalidInput = errors.New("exhaustionproofbuilder: invalid input")
	ErrRandFailed   = errors.New("exhaustionproofbuilder: random read failed")
	ErrValidation   = errors.New("exhaustionproofbuilder: validation failed")
)

// nonceLen matches shared/exhaustionproof's required Nonce length.
const nonceLen = 16

// ProofInput carries the raw materials the proof needs. Caller (ccproxy-network)
// adapts its EntryMetadata + the /usage probe timestamp into this struct.
type ProofInput struct {
	StopFailureMatcher    string    // expected: "rate_limit"
	StopFailureAt         time.Time // when the StopFailure hook fired
	StopFailureErrorShape []byte    // raw JSON from StopFailureHookInput
	UsageProbeAt          time.Time // when `claude /usage` was executed
	UsageProbeOutput      []byte    // raw PTY-captured /usage bytes
}

// Builder is stateless beyond its long-lived deps. Safe for concurrent use.
type Builder struct {
	// Now returns the current time. Defaults to time.Now. Override in tests.
	Now func() time.Time
	// RandRead fills p with random bytes. Defaults to crypto/rand.Read.
	// Override in tests for deterministic nonces.
	RandRead func(p []byte) (n int, err error)
}

// NewBuilder returns a Builder with production defaults.
func NewBuilder() *Builder {
	return &Builder{
		Now:      time.Now,
		RandRead: rand.Read,
	}
}

// Build assembles, validates, and returns a *ExhaustionProofV1.
func (b *Builder) Build(in ProofInput) (*exhaustionproof.ExhaustionProofV1, error) {
	if err := validateInput(in); err != nil {
		return nil, err
	}

	nonce := make([]byte, nonceLen)
	if _, err := b.RandRead(nonce); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRandFailed, err)
	}

	proof := &exhaustionproof.ExhaustionProofV1{
		StopFailure: &exhaustionproof.StopFailure{
			Matcher:    in.StopFailureMatcher,
			At:         uint64(in.StopFailureAt.Unix()),
			ErrorShape: in.StopFailureErrorShape,
		},
		UsageProbe: &exhaustionproof.UsageProbe{
			At:     uint64(in.UsageProbeAt.Unix()),
			Output: in.UsageProbeOutput,
		},
		CapturedAt: uint64(b.Now().Unix()),
		Nonce:      nonce,
	}

	if err := exhaustionproof.ValidateProofV1(proof); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}

	return proof, nil
}

// validateInput rejects empty/zero ProofInput fields up front. ValidateProofV1
// also checks most of these, but a dedicated input-shape error names the
// caller-side field so call-site bugs are easier to diagnose. Empty
// StopFailureErrorShape and UsageProbeOutput are rejected here defensively;
// ValidateProofV1 does not check the byte length of those fields.
func validateInput(in ProofInput) error {
	if in.StopFailureMatcher == "" {
		return fmt.Errorf("%w: StopFailureMatcher is empty", ErrInvalidInput)
	}
	if in.StopFailureAt.IsZero() {
		return fmt.Errorf("%w: StopFailureAt is zero", ErrInvalidInput)
	}
	if in.UsageProbeAt.IsZero() {
		return fmt.Errorf("%w: UsageProbeAt is zero", ErrInvalidInput)
	}
	if len(in.StopFailureErrorShape) == 0 {
		return fmt.Errorf("%w: StopFailureErrorShape is empty", ErrInvalidInput)
	}
	if len(in.UsageProbeOutput) == 0 {
		return fmt.Errorf("%w: UsageProbeOutput is empty", ErrInvalidInput)
	}
	return nil
}
```

- [ ] **Step 5: Run `go mod tidy` to wire `shared/` into `plugin/go.mod`**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/receptive-arch/plugin
PATH="$HOME/.local/share/mise/shims:$PATH" go mod tidy
```

Expected: a new line `require github.com/token-bay/token-bay/shared v0.0.0-...` (or similar pseudo-version) appears in `plugin/go.mod`. `plugin/go.sum` may pick up new sums for `google.golang.org/protobuf` (transitively via `shared/`). The existing `replace` directive resolves the require to `../shared`.

Verify with `git diff plugin/go.mod`. The `// Note: no require ...` comment in `go.mod` may now be stale — that's fine, leave it as-is or trim later in a follow-up; not in scope for this plan.

- [ ] **Step 6: Run `go work sync` from repo root**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/receptive-arch
PATH="$HOME/.local/share/mise/shims:$PATH" go work sync
```

Expected: no output, exit 0.

- [ ] **Step 7: Run the test, confirm PASS**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/receptive-arch/plugin
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 -v ./internal/exhaustionproofbuilder/...
```

Expected: all subtests pass:
- `TestBuilder_Build_HappyPath`
- `TestBuilder_Build_RejectsInvalidInput/empty_matcher` … (5 subtests)
- `TestBuilder_Build_FreshnessWindowExceeded`
- `TestBuilder_Build_RandReadFailure`
- `TestBuilder_Build_Determinism`
- `TestNewBuilder_Defaults`

- [ ] **Step 8: Run `make -C plugin lint`**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/receptive-arch
PATH="$HOME/.local/share/mise/shims:$PATH" make -C plugin lint
```

Expected: clean. If `golangci-lint` flags anything, fix before committing.

- [ ] **Step 9: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/receptive-arch
git add plugin/internal/exhaustionproofbuilder plugin/go.mod plugin/go.sum
git commit -m "$(cat <<'EOF'
feat(plugin/exhaustionproofbuilder): assemble + validate ExhaustionProofV1

Builds a *exhaustionproof.ExhaustionProofV1 from a typed ProofInput.
Validates via shared/exhaustionproof.ValidateProofV1 before returning;
rejects empty input fields with ErrInvalidInput; surfaces RandRead
failures as ErrRandFailed; surfaces validator rejections as
ErrValidation. Time and randomness are injected as function-typed
fields for deterministic tests.

First plugin-side import of shared/ — populates the dangling require
in plugin/go.mod via `go mod tidy`.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: `envelopebuilder` — types, Signer interface, happy path

**Files:**
- Create: `plugin/internal/envelopebuilder/doc.go`
- Create: `plugin/internal/envelopebuilder/envelopebuilder.go`
- Create: `plugin/internal/envelopebuilder/envelopebuilder_test.go`

- [ ] **Step 1: Write the failing test file**

Create `plugin/internal/envelopebuilder/envelopebuilder_test.go`:

```go
package envelopebuilder

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/exhaustionproof"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// fakeSigner is a configurable test stand-in for the production Signer.
// It is concurrency-safe via mu — TestBuild_Concurrent exercises that.
type fakeSigner struct {
	id        ids.IdentityID
	signBytes []byte // returned from Sign on success
	signErr   error  // if non-nil, Sign returns this
	mu        sync.Mutex
	calls     int
}

func (f *fakeSigner) Sign(body *tbproto.EnvelopeBody) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.signErr != nil {
		return nil, f.signErr
	}
	return f.signBytes, nil
}

func (f *fakeSigner) IdentityID() ids.IdentityID { return f.id }

func (f *fakeSigner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newFakeSigner() *fakeSigner {
	var id ids.IdentityID
	for i := range id {
		id[i] = byte(i + 1) // 0x01..0x20
	}
	sig := make([]byte, 64)
	for i := range sig {
		sig[i] = 0xAA
	}
	return &fakeSigner{id: id, signBytes: sig}
}

func fixedNow() time.Time { return time.Unix(1714000020, 0).UTC() }

func zeroRand(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// validProof returns a hand-constructed ExhaustionProofV1 that passes
// ValidateProofV1. Tests that need a valid proof start from this.
func validProof() *exhaustionproof.ExhaustionProofV1 {
	return &exhaustionproof.ExhaustionProofV1{
		StopFailure: &exhaustionproof.StopFailure{
			Matcher:    "rate_limit",
			At:         1714000000,
			ErrorShape: []byte(`{"type":"rate_limit_error"}`),
		},
		UsageProbe: &exhaustionproof.UsageProbe{
			At:     1714000005,
			Output: []byte(`Current session: 99% used`),
		},
		CapturedAt: 1714000010,
		Nonce:      bytesOfLen(16, 0xCC),
	}
}

// validBalance returns a hand-constructed *SignedBalanceSnapshot. The
// envelope builder only asserts non-nil — internal balance fields aren't
// validated by ValidateEnvelopeBody.
func validBalance() *tbproto.SignedBalanceSnapshot {
	return &tbproto.SignedBalanceSnapshot{
		Body: &tbproto.BalanceSnapshotBody{
			IdentityId:   bytesOfLen(32, 0x11),
			Credits:      100,
			ChainTipHash: bytesOfLen(32, 0x22),
			ChainTipSeq:  42,
			IssuedAt:     1714000000,
			ExpiresAt:    1714000600,
		},
		TrackerSig: bytesOfLen(64, 0x33),
	}
}

func validSpec() RequestSpec {
	return RequestSpec{
		Model:           "claude-sonnet-4-6",
		MaxInputTokens:  100_000,
		MaxOutputTokens: 8_192,
		Tier:            tbproto.PrivacyTier_PRIVACY_TIER_STANDARD,
		BodyHash:        bytesOfLen(32, 0x44),
	}
}

func bytesOfLen(n int, fill byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = fill
	}
	return out
}

func newTestBuilder(s Signer) *Builder {
	b := NewBuilder(s)
	b.Now = fixedNow
	b.RandRead = zeroRand
	return b
}

func TestBuild_HappyPath(t *testing.T) {
	signer := newFakeSigner()
	b := newTestBuilder(signer)

	env, err := b.Build(validSpec(), validProof(), validBalance())
	require.NoError(t, err)
	require.NotNil(t, env)
	require.NotNil(t, env.Body)

	// Body fields
	assert.Equal(t, uint32(tbproto.ProtocolVersion), env.Body.ProtocolVersion)
	assert.Equal(t, signer.id[:], env.Body.ConsumerId)
	assert.Equal(t, "claude-sonnet-4-6", env.Body.Model)
	assert.Equal(t, uint64(100_000), env.Body.MaxInputTokens)
	assert.Equal(t, uint64(8_192), env.Body.MaxOutputTokens)
	assert.Equal(t, tbproto.PrivacyTier_PRIVACY_TIER_STANDARD, env.Body.Tier)
	assert.Equal(t, bytesOfLen(32, 0x44), env.Body.BodyHash)
	assert.NotNil(t, env.Body.ExhaustionProof)
	assert.NotNil(t, env.Body.BalanceProof)
	assert.Equal(t, uint64(1714000020), env.Body.CapturedAt)
	assert.Len(t, env.Body.Nonce, 16)

	// Signature fed by the fake signer.
	assert.Equal(t, signer.signBytes, env.ConsumerSig)
	assert.Equal(t, 1, signer.callCount(), "Signer.Sign called exactly once")

	// Independent re-validation: builder claims valid → ValidateEnvelopeBody agrees.
	require.NoError(t, tbproto.ValidateEnvelopeBody(env.Body))
}

func TestNewBuilder_PanicsOnNilSigner(t *testing.T) {
	assert.Panics(t, func() { NewBuilder(nil) })
}

func TestNewBuilder_Defaults(t *testing.T) {
	b := NewBuilder(newFakeSigner())
	require.NotNil(t, b)
	assert.NotNil(t, b.Now)
	assert.NotNil(t, b.RandRead)
	assert.NotNil(t, b.Signer)
}

// Compile-time check: fakeSigner must satisfy Signer.
var _ Signer = (*fakeSigner)(nil)
```

- [ ] **Step 2: Run the test, confirm FAIL**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/receptive-arch/plugin
PATH="$HOME/.local/share/mise/shims:$PATH" go test -count=1 ./internal/envelopebuilder/...
```

Expected: compilation failure — `Signer`, `Builder`, `NewBuilder`, `RequestSpec`, sentinels all undefined; package itself doesn't exist.

- [ ] **Step 3: Create the package doc file**

Create `plugin/internal/envelopebuilder/doc.go`:

```go
// Package envelopebuilder assembles a *proto.EnvelopeSigned from a typed
// RequestSpec, a *exhaustionproof.ExhaustionProofV1, and a tracker-issued
// *proto.SignedBalanceSnapshot. The Signer interface owns Ed25519; the
// production implementation lives in plugin/internal/identity (separate
// plan), tests use a fake.
//
// The builder validates the assembled body via shared/proto.ValidateEnvelopeBody
// before signing. Time and randomness are injected as function-typed fields
// for deterministic tests.
//
// This package imports only shared/... + Go stdlib. It is a leaf of the
// plugin-internal dependency graph.
package envelopebuilder
```

- [ ] **Step 4: Create the production file**

Create `plugin/internal/envelopebuilder/envelopebuilder.go`:

```go
package envelopebuilder

import (
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/token-bay/token-bay/shared/exhaustionproof"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// Sentinel errors. Callers discriminate via errors.Is.
var (
	ErrNilProof     = errors.New("envelopebuilder: proof is nil")
	ErrNilBalance   = errors.New("envelopebuilder: balance is nil")
	ErrInvalidSpec  = errors.New("envelopebuilder: invalid RequestSpec")
	ErrRandFailed   = errors.New("envelopebuilder: random read failed")
	ErrValidation   = errors.New("envelopebuilder: validation failed")
	ErrSign         = errors.New("envelopebuilder: signer failed")
)

// nonceLen matches shared/proto's required EnvelopeBody.Nonce length.
const nonceLen = 16

// bodyHashLen matches shared/proto's required EnvelopeBody.BodyHash length.
const bodyHashLen = 32

// RequestSpec is the per-request slice of the envelope, supplied by the
// caller (ccproxy-network) after parsing /v1/messages and resolving the
// privacy tier from config.
type RequestSpec struct {
	Model           string
	MaxInputTokens  uint64
	MaxOutputTokens uint64
	Tier            tbproto.PrivacyTier
	BodyHash        []byte // SHA-256, len 32
}

// Signer abstracts the consumer's identity. Production impl lives in
// plugin/internal/identity; tests use a fake. Implementations must be
// safe for concurrent use — Build calls Sign once per request.
type Signer interface {
	// Sign returns an Ed25519 signature over DeterministicMarshal(body).
	Sign(body *tbproto.EnvelopeBody) ([]byte, error)
	// IdentityID returns this Signer's 32-byte identity (pubkey hash).
	IdentityID() ids.IdentityID
}

// Builder is stateless beyond its long-lived deps. Safe for concurrent use.
type Builder struct {
	// Signer is required. NewBuilder panics on nil.
	Signer Signer
	// Now returns the current time. Defaults to time.Now. Override in tests.
	Now func() time.Time
	// RandRead fills p with random bytes. Defaults to crypto/rand.Read.
	// Override in tests for deterministic nonces.
	RandRead func(p []byte) (n int, err error)
}

// NewBuilder returns a Builder with production defaults. Panics on nil
// Signer — a programmer error caught at startup, not a runtime condition.
// All Build()-time errors still fail closed via sentinels.
func NewBuilder(s Signer) *Builder {
	if s == nil {
		panic("envelopebuilder: NewBuilder called with nil Signer")
	}
	return &Builder{
		Signer:   s,
		Now:      time.Now,
		RandRead: rand.Read,
	}
}

// Build assembles, validates, signs, and returns *EnvelopeSigned.
func (b *Builder) Build(
	spec RequestSpec,
	proof *exhaustionproof.ExhaustionProofV1,
	balance *tbproto.SignedBalanceSnapshot,
) (*tbproto.EnvelopeSigned, error) {
	if proof == nil {
		return nil, ErrNilProof
	}
	if balance == nil {
		return nil, ErrNilBalance
	}
	if err := validateSpec(spec); err != nil {
		return nil, err
	}

	nonce := make([]byte, nonceLen)
	if _, err := b.RandRead(nonce); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRandFailed, err)
	}

	id := b.Signer.IdentityID()
	body := &tbproto.EnvelopeBody{
		ProtocolVersion: uint32(tbproto.ProtocolVersion),
		ConsumerId:      append([]byte(nil), id[:]...),
		Model:           spec.Model,
		MaxInputTokens:  spec.MaxInputTokens,
		MaxOutputTokens: spec.MaxOutputTokens,
		Tier:            spec.Tier,
		BodyHash:        spec.BodyHash,
		ExhaustionProof: proof,
		BalanceProof:    balance,
		CapturedAt:      uint64(b.Now().Unix()),
		Nonce:           nonce,
	}

	if err := tbproto.ValidateEnvelopeBody(body); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}

	sig, err := b.Signer.Sign(body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSign, err)
	}

	return &tbproto.EnvelopeSigned{
		Body:        body,
		ConsumerSig: sig,
	}, nil
}

// validateSpec rejects obviously-bad RequestSpec fields with ErrInvalidSpec.
// ValidateEnvelopeBody re-checks these post-assembly, but the dedicated
// pre-check error names the caller-side field for easier diagnosis.
func validateSpec(spec RequestSpec) error {
	if spec.Model == "" {
		return fmt.Errorf("%w: Model is empty", ErrInvalidSpec)
	}
	if len(spec.BodyHash) != bodyHashLen {
		return fmt.Errorf("%w: BodyHash length %d, want %d", ErrInvalidSpec, len(spec.BodyHash), bodyHashLen)
	}
	if spec.Tier == tbproto.PrivacyTier_PRIVACY_TIER_UNSPECIFIED {
		return fmt.Errorf("%w: Tier is UNSPECIFIED", ErrInvalidSpec)
	}
	return nil
}
```

- [ ] **Step 5: Run the test, confirm PASS**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/receptive-arch/plugin
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 -v ./internal/envelopebuilder/...
```

Expected: `TestBuild_HappyPath`, `TestNewBuilder_PanicsOnNilSigner`, `TestNewBuilder_Defaults` all PASS.

- [ ] **Step 6: Run lint**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/receptive-arch
PATH="$HOME/.local/share/mise/shims:$PATH" make -C plugin lint
```

Expected: clean.

- [ ] **Step 7: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/receptive-arch
git add plugin/internal/envelopebuilder
git commit -m "$(cat <<'EOF'
feat(plugin/envelopebuilder): assemble + validate + sign EnvelopeSigned

Builder takes a typed RequestSpec + *ExhaustionProofV1 + tracker-issued
*SignedBalanceSnapshot, assembles an EnvelopeBody, runs
shared/proto.ValidateEnvelopeBody, then signs via the injected Signer
interface. Production Signer impl lives in plugin/internal/identity
(future plan); tests use a fake. Time and randomness are injectable.

NewBuilder panics on nil Signer (constructor-time programmer error).
All Build()-time error paths fail closed via sentinels: ErrNilProof,
ErrNilBalance, ErrInvalidSpec, ErrRandFailed, ErrValidation, ErrSign.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: `envelopebuilder` — real Ed25519 round-trip + tamper detection

**Files:**
- Modify: `plugin/internal/envelopebuilder/envelopebuilder_test.go`

- [ ] **Step 1: Append the round-trip + tamper tests**

Append to `plugin/internal/envelopebuilder/envelopebuilder_test.go`:

```go
// realEd25519Signer wraps shared/signing.SignEnvelope with a fresh keypair.
// Used by tests that exercise actual cryptographic verification.
type realEd25519Signer struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
	id   ids.IdentityID
}

func newRealSigner(t *testing.T) *realEd25519Signer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	// Derive IdentityID = SHA-256(pubkey). The exact derivation is irrelevant
	// to these tests (the tracker validates only that consumer_id is 32 bytes);
	// what matters is that it's deterministic per keypair.
	h := sha256.Sum256(pub)
	var id ids.IdentityID
	copy(id[:], h[:])
	return &realEd25519Signer{priv: priv, pub: pub, id: id}
}

func (r *realEd25519Signer) Sign(body *tbproto.EnvelopeBody) ([]byte, error) {
	return signing.SignEnvelope(r.priv, body)
}

func (r *realEd25519Signer) IdentityID() ids.IdentityID { return r.id }

func TestBuild_RealEd25519_RoundTrip(t *testing.T) {
	signer := newRealSigner(t)
	b := newTestBuilder(signer)

	env, err := b.Build(validSpec(), validProof(), validBalance())
	require.NoError(t, err)

	// Marshal → unmarshal — survives the wire round-trip.
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(env)
	require.NoError(t, err)

	var parsed tbproto.EnvelopeSigned
	require.NoError(t, proto.Unmarshal(wire, &parsed))

	// Real Ed25519 verification against the parsed message.
	assert.True(t, signing.VerifyEnvelope(signer.pub, &parsed),
		"VerifyEnvelope must accept a freshly-built envelope")
}

func TestBuild_RealEd25519_TamperDetected(t *testing.T) {
	signer := newRealSigner(t)
	b := newTestBuilder(signer)

	env, err := b.Build(validSpec(), validProof(), validBalance())
	require.NoError(t, err)
	require.True(t, signing.VerifyEnvelope(signer.pub, env), "sanity: original verifies")

	// Mutate the body after signing.
	env.Body.Model = "claude-haiku-4-5-20251001"
	assert.False(t, signing.VerifyEnvelope(signer.pub, env),
		"VerifyEnvelope must reject a body that has been mutated post-sign")
}
```

Replace the entire import block at the top of the file with:

```go
import (
	"crypto/ed25519"
	"crypto/sha256"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/shared/exhaustionproof"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
)
```

- [ ] **Step 2: Run, confirm PASS**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/receptive-arch/plugin
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 -v -run 'TestBuild_RealEd25519' ./internal/envelopebuilder/...
```

Expected: `TestBuild_RealEd25519_RoundTrip` and `TestBuild_RealEd25519_TamperDetected` PASS.

- [ ] **Step 3: Run the whole suite + lint**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/receptive-arch/plugin
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/envelopebuilder/...
PATH="$HOME/.local/share/mise/shims:$PATH" golangci-lint run ./internal/envelopebuilder/...
```

Expected: full suite green; lint clean.

- [ ] **Step 4: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/receptive-arch
git add plugin/internal/envelopebuilder/envelopebuilder_test.go
git commit -m "$(cat <<'EOF'
test(plugin/envelopebuilder): real-Ed25519 round-trip + tamper detection

Replaces the fake-signer-only happy path with a second cycle that
generates a fresh Ed25519 keypair, builds the envelope, marshals →
unmarshals → calls shared/signing.VerifyEnvelope. Tamper test mutates
Body.Model after signing and asserts verification fails. Sanity check
that the production signing path is wired correctly through the
Signer interface.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: `envelopebuilder` — error path matrix

**Files:**
- Modify: `plugin/internal/envelopebuilder/envelopebuilder_test.go`

- [ ] **Step 1: Add `errors` to the test file's imports**

Add `"errors"` to the stdlib group of imports at the top of `plugin/internal/envelopebuilder/envelopebuilder_test.go` (the matrix tests use `errors.Is` and `errors.New`):

```go
import (
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/shared/exhaustionproof"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
)
```

- [ ] **Step 2: Append the error-path matrix tests**

Append to `plugin/internal/envelopebuilder/envelopebuilder_test.go`:

```go
func TestBuild_RejectsNilProof(t *testing.T) {
	b := newTestBuilder(newFakeSigner())
	env, err := b.Build(validSpec(), nil, validBalance())
	assert.Nil(t, env)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNilProof), "expected ErrNilProof, got: %v", err)
}

func TestBuild_RejectsNilBalance(t *testing.T) {
	b := newTestBuilder(newFakeSigner())
	env, err := b.Build(validSpec(), validProof(), nil)
	assert.Nil(t, env)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNilBalance), "expected ErrNilBalance, got: %v", err)
}

func TestBuild_RejectsInvalidSpec(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(*RequestSpec)
		wantField string
	}{
		{"empty model", func(s *RequestSpec) { s.Model = "" }, "Model"},
		{"short body_hash", func(s *RequestSpec) { s.BodyHash = bytesOfLen(31, 0x44) }, "BodyHash"},
		{"long body_hash", func(s *RequestSpec) { s.BodyHash = bytesOfLen(33, 0x44) }, "BodyHash"},
		{"unspecified tier", func(s *RequestSpec) { s.Tier = tbproto.PrivacyTier_PRIVACY_TIER_UNSPECIFIED }, "Tier"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := validSpec()
			tc.mutate(&spec)
			b := newTestBuilder(newFakeSigner())
			env, err := b.Build(spec, validProof(), validBalance())
			assert.Nil(t, env)
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrInvalidSpec),
				"expected ErrInvalidSpec, got: %v", err)
			assert.Contains(t, err.Error(), tc.wantField,
				"error should name the offending field: %v", err)
		})
	}
}

func TestBuild_SignerFailure(t *testing.T) {
	signer := newFakeSigner()
	signer.signErr = errors.New("keychain locked")
	b := newTestBuilder(signer)

	env, err := b.Build(validSpec(), validProof(), validBalance())
	assert.Nil(t, env)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSign), "expected ErrSign, got: %v", err)
	assert.Contains(t, err.Error(), "keychain locked")
}

func TestBuild_RandReadFailure(t *testing.T) {
	signer := newFakeSigner()
	b := NewBuilder(signer)
	b.Now = fixedNow
	b.RandRead = func(_ []byte) (int, error) { return 0, errors.New("entropy starved") }

	env, err := b.Build(validSpec(), validProof(), validBalance())
	assert.Nil(t, env)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrRandFailed), "expected ErrRandFailed, got: %v", err)
	assert.Equal(t, 0, signer.callCount(), "Signer.Sign must NOT be called on RandRead failure")
}

func TestBuild_ValidationFailure(t *testing.T) {
	// A proof that passes ProofInput-shape but fails ValidateProofV1
	// (matcher != "rate_limit") should surface ErrValidation. Construct
	// the proof manually since the proof builder rejects this earlier.
	badProof := validProof()
	badProof.StopFailure.Matcher = "server_error"

	b := newTestBuilder(newFakeSigner())
	env, err := b.Build(validSpec(), badProof, validBalance())
	assert.Nil(t, env)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrValidation), "expected ErrValidation, got: %v", err)
}
```

- [ ] **Step 3: Run, confirm PASS**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/receptive-arch/plugin
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 -v ./internal/envelopebuilder/...
```

Expected: all error-path tests PASS, plus the previous suite still green.

- [ ] **Step 4: Lint**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/receptive-arch
PATH="$HOME/.local/share/mise/shims:$PATH" make -C plugin lint
```

Expected: clean.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/receptive-arch
git add plugin/internal/envelopebuilder/envelopebuilder_test.go
git commit -m "$(cat <<'EOF'
test(plugin/envelopebuilder): error path matrix

Locks every documented error sentinel via errors.Is:
  - ErrNilProof     (proof argument nil)
  - ErrNilBalance   (balance argument nil)
  - ErrInvalidSpec  (Model/BodyHash/Tier — table-driven)
  - ErrSign         (Signer.Sign returns an error)
  - ErrRandFailed   (RandRead returns an error; asserts Signer never called)
  - ErrValidation   (post-assembly ValidateEnvelopeBody fails)

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: `envelopebuilder` — determinism + concurrency

**Files:**
- Modify: `plugin/internal/envelopebuilder/envelopebuilder_test.go`

- [ ] **Step 1: Append determinism + concurrency tests**

Append to `plugin/internal/envelopebuilder/envelopebuilder_test.go`:

```go
func TestBuild_DeterministicMarshal(t *testing.T) {
	// Same inputs (same spec, same proof, same balance, same fixed Now,
	// same zeroRand sequence, same fake-signer canned bytes) → byte-identical
	// DeterministicMarshal of the body across two Build() calls.
	signer := newFakeSigner()
	b := newTestBuilder(signer)

	a, err := b.Build(validSpec(), validProof(), validBalance())
	require.NoError(t, err)
	c, err := b.Build(validSpec(), validProof(), validBalance())
	require.NoError(t, err)

	first, err := proto.MarshalOptions{Deterministic: true}.Marshal(a.Body)
	require.NoError(t, err)
	second, err := proto.MarshalOptions{Deterministic: true}.Marshal(c.Body)
	require.NoError(t, err)
	assert.Equal(t, first, second,
		"identical inputs must produce identical DeterministicMarshal output")
}

func TestBuild_Concurrent(t *testing.T) {
	// 50 goroutines call Build() on the same Builder. The fake signer is
	// concurrency-safe via its mu; Builder itself holds no mutable per-request
	// state. -race must be clean.
	signer := newFakeSigner()
	b := newTestBuilder(signer)

	const N = 50
	var wg sync.WaitGroup
	wg.Add(N)
	results := make([]*tbproto.EnvelopeSigned, N)
	errs := make([]error, N)

	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			// Distinct BodyHash per goroutine — distinct envelopes.
			spec := validSpec()
			spec.BodyHash = bytesOfLen(32, byte(i+1))
			env, err := b.Build(spec, validProof(), validBalance())
			results[i] = env
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i := 0; i < N; i++ {
		require.NoErrorf(t, errs[i], "goroutine %d errored", i)
		require.NotNil(t, results[i])
		assert.Equal(t, byte(i+1), results[i].Body.BodyHash[0],
			"goroutine %d produced an envelope with the wrong BodyHash", i)
	}
	assert.Equal(t, N, signer.callCount(), "Signer.Sign called once per goroutine")
}
```

- [ ] **Step 2: Run with `-race`**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/receptive-arch/plugin
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 -v ./internal/envelopebuilder/...
```

Expected: full suite green, no race-detector warnings.

- [ ] **Step 3: Lint**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/receptive-arch
PATH="$HOME/.local/share/mise/shims:$PATH" make -C plugin lint
```

Expected: clean.

- [ ] **Step 4: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/receptive-arch
git add plugin/internal/envelopebuilder/envelopebuilder_test.go
git commit -m "$(cat <<'EOF'
test(plugin/envelopebuilder): determinism + concurrency

Two property locks:
  - DeterministicMarshal of the body is byte-identical across two
    Build() calls with the same inputs + same injected Now/RandRead
    sequence. Tripwire for accidental field-ordering changes.
  - 50 goroutines on the same Builder produce distinct envelopes with
    no -race warnings. Confirms the Builder holds no mutable per-request
    state.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: Coverage gate + final verification

**Files:**
- Modify (only if gaps found): `plugin/internal/exhaustionproofbuilder/exhaustionproofbuilder_test.go`, `plugin/internal/envelopebuilder/envelopebuilder_test.go`

- [ ] **Step 1: Run the full test suite with coverage**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/receptive-arch/plugin
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -coverprofile=coverage.out ./internal/exhaustionproofbuilder/... ./internal/envelopebuilder/...
PATH="$HOME/.local/share/mise/shims:$PATH" go tool cover -func=coverage.out | tail -30
```

Expected: each function in both packages reports ≥ 90% coverage. The summary `total:` line should be ≥ 90% per package.

- [ ] **Step 2: If any function is < 90%, identify the uncovered branches and add a targeted test**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/receptive-arch/plugin
PATH="$HOME/.local/share/mise/shims:$PATH" go tool cover -html=coverage.out -o coverage.html
# Open coverage.html in a browser to see uncovered lines highlighted in red.
```

For each uncovered branch, add a focused test in the corresponding `_test.go` file. The most likely gap is a specific `fmt.Errorf("%w: ...", ErrX, ...)` line for an input-validation case the existing matrix doesn't yet exercise — add a row to the matching table.

If no gaps, skip to Step 3.

- [ ] **Step 3: Run the plugin's full check (test + lint)**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/receptive-arch
PATH="$HOME/.local/share/mise/shims:$PATH" make -C plugin check
```

Expected: tests + lint green.

- [ ] **Step 4: Verify no plugin-internal cross-imports**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/receptive-arch
grep -R '"github.com/token-bay/token-bay/plugin/internal' plugin/internal/exhaustionproofbuilder plugin/internal/envelopebuilder
```

Expected: zero matches. Both packages must import only `shared/...` + Go stdlib.

- [ ] **Step 5: Verify `go work sync` is clean**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/receptive-arch
PATH="$HOME/.local/share/mise/shims:$PATH" go work sync
rm -f plugin/coverage.out plugin/coverage.html
git status -s
```

Expected: `go work sync` exits 0; `git status -s` shows only the staged-or-unstaged source-file changes from prior tasks (and any test additions from Step 2). No `coverage.out`/`coverage.html` artefacts; do not commit them.

- [ ] **Step 6: Commit (only if Step 2 added tests)**

If Step 2 added tests:

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/receptive-arch
git add plugin/internal/exhaustionproofbuilder/exhaustionproofbuilder_test.go plugin/internal/envelopebuilder/envelopebuilder_test.go
git commit -m "$(cat <<'EOF'
test(plugin/envelopebuilder): coverage backfill to ≥90% target

Adds targeted tests for branches missed by the property-driven suite.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

If Step 2 added nothing, no commit. The task is a pure verification gate.

---

## 3. Out of scope for this plan

These ship in subsequent feature plans:

- **`plugin/internal/identity`** — production `Signer` implementation (key file, optional OS keychain). Tests in this plan use a fake.
- **`plugin/internal/trackerclient`** — QUIC client + balance-snapshot cache. Owns the `*SignedBalanceSnapshot` that the envelope builder consumes per-request.
- **`plugin/internal/ccproxy-network`** — the real `NetworkRouter`. Parses `/v1/messages`, computes `body_hash`, resolves tier from config, looks up session mode in `ccproxy.SessionModeStore`, adapts `EntryMetadata → ProofInput`, calls both builders in order.
- **Audit-log integration** — the audit-log spec may want to record build outcomes; trivial post-hoc addition at the call site, no builder changes.

## 4. Open questions

- **Should `RequestSpec` carry the raw body for downstream debug logging?** Currently no — `BodyHash` is sufficient. Re-visit if the audit-log plan disagrees.
- **Keep the dangling-replace comment in `plugin/go.mod`?** After Task 1 the `replace` is no longer dangling; the comment is now mildly stale. Not in scope for this plan; leave for a future docs sweep.

## 5. Self-review

**Spec coverage:**
- §2 in-scope items (exhaustionproofbuilder package, envelopebuilder package, sentinel errors, adapter conventions documented, first plugin→shared import) — covered by Tasks 1, 2, 3, 4, 5; adapter conventions are documented in the spec (this plan's Task 3 step 1 imports + Task 2 step 1 imports of `shared/...` populate the require). ✓
- §3 constraints — leaf imports verified in Task 6 step 4 ✓; no I/O (production code uses only `time.Now` and `crypto/rand.Read` defaults — no file/socket access) ✓; validate-before-sign exercised in `TestBuild_HappyPath` and `TestBuild_ValidationFailure` ✓; deterministic for tests via `Now`/`RandRead` ✓; concurrency-safe verified by `TestBuild_Concurrent` ✓.
- §6.1 `exhaustionproofbuilder` API (ProofInput, Builder, NewBuilder, Build) — Task 1 step 4 ✓.
- §6.2 `envelopebuilder` API (RequestSpec, Signer, Builder, NewBuilder, Build) — Task 2 step 4 ✓.
- §6.3 sentinels (ErrInvalidInput/ErrRandFailed/ErrValidation in proof; ErrNilProof/ErrNilBalance/ErrInvalidSpec/ErrRandFailed/ErrValidation/ErrSign in envelope) — declared in Tasks 1 step 4 + 2 step 4; tested in Tasks 1 step 1 + 4 step 1 ✓.
- §8.1 proof builder test inventory (happy/validation matrix/RandRead failure/determinism) — Task 1 step 1 ✓.
- §8.2 envelope builder test inventory (happy/round-trip+verify/tamper/nil+invalid matrix/signer failure/RandRead failure/determinism/concurrency) — Tasks 2/3/4/5 ✓.
- §8.3 ≥90% coverage — Task 6 step 1 ✓.
- §9 file impact (go.mod require, two new packages, no other changes) — Task 1 step 5 + 6 ✓.
- §12 acceptance criteria — every item maps to a verification step in Task 6.

**Placeholder scan:** No "TBD"/"TODO"/"add appropriate"/"similar to". Every code step shows full code. Every command shows expected output.

**Type consistency:** `ProofInput`, `Builder`, `RequestSpec`, `Signer`, `NewBuilder` (both packages), `Build` (both packages), `ErrInvalidInput`, `ErrRandFailed`, `ErrValidation`, `ErrNilProof`, `ErrNilBalance`, `ErrInvalidSpec`, `ErrSign`, field names (`StopFailureMatcher`, `StopFailureAt`, `StopFailureErrorShape`, `UsageProbeAt`, `UsageProbeOutput`, `Model`, `MaxInputTokens`, `MaxOutputTokens`, `Tier`, `BodyHash`) — all consistent across tasks.

## 6. Next plans (after this lands)

The wire-format plan's "Next plans" cluster, with this plan complete:

1. ~~`plugin/internal/envelopebuilder`~~ ✓ (this plan).
2. **`plugin/internal/identity`** — small. Implements the `Signer` interface defined here.
3. **`plugin/internal/trackerclient`** — bigger. QUIC + RPC + balance-snapshot cache.
4. **`plugin/internal/ccproxy-network`** — the real `NetworkRouter`. Stitches everything end-to-end.
5. **`tracker/internal/broker`** — receiver-side envelope validation + selection algorithm.
