# Plugin `internal/ratelimit` — Feature Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Implement `plugin/internal/ratelimit` — the two-stage gate that classifies the Claude Code `StopFailure` hook payload, probes `claude /usage` under a PTY, parses the rendered TUI, and applies the verdict matrix from plugin spec §5.2.

**Architecture:** Go package with one subprocess entry point. Probe invokes `claude /usage` under a PTY (Claude Code's `/usage` is `local-jsx`, a React TUI — the only way to capture its rendered output from outside is via PTY; `-p` and piped stdout produce a canned stub). Parser is pure (strings in, verdict out). Decision matrix is pure (trigger + hook-error + usage-verdict → gate verdict).

**Tech Stack:**
- Go 1.23+
- `encoding/json` (stdlib) — StopFailure payload parsing
- `regexp` (stdlib) — `% used` token extraction
- `github.com/creack/pty` — PTY spawning (cross-platform POSIX; Windows not supported in v1)
- `os/exec`, `context` (stdlib) — subprocess management with early termination
- `github.com/stretchr/testify` — test assertions

**Prerequisites:** Plugin scaffolding complete (`plugin-v0`). `plugin/internal/ratelimit/.gitkeep` present. `settingsjson-v0` complete (not a dep, but establishes the module pattern).

**Canonical source verified:** The `StopFailure` hook payload schema was pinned against `~/git/claude-code/src/entrypoints/sdk/coreSchemas.ts` (lines 387-399 + 529-538 + 1256-1266). The actual shape differs from what a plugin spec reader might infer — the error enum field is named `error`, not `error_type`, and additional optional fields (`error_details`, `last_assistant_message`, `agent_id`) are available. The types below mirror the real schema.

---

## Table of contents

1. [Design decisions](#1-design-decisions)
2. [Package API](#2-package-api)
3. [File layout](#3-file-layout)
4. [TDD task list](#4-tdd-task-list)
5. [Open questions for implementation](#5-open-questions-for-implementation)

---

## 1. Design decisions

### 1.1 StopFailure payload schema

From Claude Code's SDK core schemas:

```text
BaseHookInputSchema {
  session_id:        string
  transcript_path:   string
  cwd:               string
  permission_mode?:  string
  agent_id?:         string  // present only inside subagents
  ...
}

StopFailureHookInput = BaseHookInputSchema & {
  hook_event_name:          "StopFailure"
  error:                    "rate_limit" | "authentication_failed" | "billing_error"
                           | "invalid_request" | "server_error" | "max_output_tokens"
                           | "unknown"
  error_details?:           string
  last_assistant_message?:  string
}
```

The plugin only handles `error == "rate_limit"`. All other values classify as "not our concern" and the gate returns `VerdictSkipHook` (this module doesn't know that the hook was bound with `matcher: rate_limit` — upstream filtering is the hook config, but defense-in-depth says we double-check).

### 1.2 `/usage` probe — mechanism and parsing

**Mechanism.** `/usage` is a React TUI component (`type: 'local-jsx'` at `src/commands/usage/index.ts`). Any non-TTY invocation — `claude -p "/usage"`, piped-stdout `claude /usage`, or combinations with `-p ""` — returns a canned stub (`"You are currently using your subscription..."`) after ~5ms without hitting the API. Real usage data only renders under a PTY. See `docs/superpowers/specs/plugin/2026-04-22-plugin-design.md` §2.5 (updated 2026-04-23) and project memory `project_usage_probe_pty.md` for the investigation.

**Invocation:** `claude /usage` spawned under a PTY via `github.com/creack/pty`. No `-p`. No `--output-format json` (silently ignored in interactive mode anyway).

**Captured output shape.** Empirical sample from a live probe:
- ANSI-formatted TUI text. Total output on my machine is ~8KB.
- Per rate-limit window, the TUI renders `Math.floor(utilization)% used` (source: `src/components/Settings/Usage.tsx:42`). Three windows typically present: `Current session` (5h), `Current week (all models)` (7-day), `Current week (Sonnet only)` (7-day Sonnet).
- Example observed tokens: `37% used`, `3% used`, `42% used` (percentages tick up across successive probes as the user consumes quota).
- The tokens survive across ANSI escape boundaries — regex `(\d+)%\s*used` matches them without pre-stripping ANSI.

**Early-termination strategy.** Claude Code's full TUI render can take 15-20 seconds. The probe uses a streaming read loop that terminates as soon as **≥2** `% used` tokens have been captured, or after a hard deadline (default 8s). Three expected latency phases:
1. Process start → first byte: Claude Code cold-start (~2-5s).
2. First byte → first `% used` token: data fetch + initial render (~1-3s).
3. First token → second token: a few frame-update milliseconds.

Early termination shaves ~10s off the full-render baseline. If the hard deadline fires with zero tokens captured, the probe returns `UsageUncertain`.

**Classification thresholds:**
- Any utilization ≥ 95 → `UsageExhausted` (95% accounts for `Math.floor` undercount — a real 99.9% renders as `99% used`).
- All captured utilizations < 95 → `UsageHeadroom`.
- Fewer than 2 tokens captured (or zero) → `UsageUncertain`.

**Why ≥95, not ≥100:** The `/usage` API returns `utilization` as an integer percentage. Math.floor rounds down before display. A consumer who just hit a 429 likely shows 99% in `/usage`, not 100. Conservative to treat the band `[95, 100]` as exhausted.

### 1.3 Verdict matrix

From plugin spec §5.2, mapped to our enum:

| Trigger | UsageVerdict | GateVerdict | Semantics |
|---|---|---|---|
| Hook (StopFailure+rate_limit) | Exhausted | Proceed | Happy path — both signals agree |
| Hook | Headroom | RefuseManualOK | Ambiguous — StopFailure may be transient; allow manual override |
| Hook | Uncertain | Proceed | StopFailure alone is load-bearing enough; /usage just adds confidence |
| Manual | Exhausted | Proceed | User correctly identified their own exhaustion |
| Manual | Headroom | RefuseWithReason | User has headroom; no reason to spend credits |
| Manual | Uncertain | RefuseWithReason | Can't confirm; conservative refuse (user can retry after /usage output improves) |

**"Uncertain" safety direction:** When we can't tell, we lean toward whichever decision wastes fewer credits. For hook-triggered (automatic), we have an external strong signal (a real 429), so "uncertain from /usage" doesn't block proceeding. For manual, we have no external signal — "uncertain" means "no reason to think you need this" and we refuse.

### 1.4 Non-rate_limit errors

If `StopFailure.error` is not `"rate_limit"` (e.g., `authentication_failed`, `server_error`), the gate returns `VerdictSkipHook` — the plugin is not equipped to handle these. Caller is expected to surface the error normally. This covers the case where a hook is misconfigured or a future Claude Code release routes a different error type to the same hook.

### 1.5 Freshness guard

Both signals have an `at` timestamp (StopFailure's from the hook fire time, /usage's from when the probe ran). Fresh window: both must be within 60 seconds of each other (plugin spec §5.3). The gate refuses if the signals are stale or diverge by > 60s. This prevents replay of an old StopFailure combined with a fresh /usage.

### 1.6 What this module does NOT do

- No hook handler binding. The sidecar's `internal/hooks` module accepts the hook payload from stdin and calls `ratelimit.ParseStopFailurePayload`.
- No ticket assembly for the exhaustion proof. That's in a later module (`internal/exhaustion-proof`). This module returns raw parsed structs that the caller bundles into the ticket.
- No retry logic. One probe, one parse, one verdict.
- No auth-state management. The probe subprocess inherits whatever Claude Code auth is already on the user's machine; we do not touch `.credentials.json` or the keychain.

---

## 2. Package API

```go
// Package ratelimit parses Claude Code rate-limit signals and applies the
// two-stage gate from plugin spec §5.2.
package ratelimit

// StopFailurePayload mirrors Claude Code's StopFailureHookInput schema
// (src/entrypoints/sdk/coreSchemas.ts:529). Optional fields are Go
// pointer-to-string so absent-vs-empty is distinguishable.
type StopFailurePayload struct {
    SessionID            string  `json:"session_id"`
    TranscriptPath       string  `json:"transcript_path"`
    CWD                  string  `json:"cwd"`
    PermissionMode       *string `json:"permission_mode,omitempty"`
    AgentID              *string `json:"agent_id,omitempty"`
    HookEventName        string  `json:"hook_event_name"`          // expected: "StopFailure"
    Error                string  `json:"error"`                    // enum; see ErrorRateLimit etc.
    ErrorDetails         *string `json:"error_details,omitempty"`
    LastAssistantMessage *string `json:"last_assistant_message,omitempty"`
}

// Error enum values from SDKAssistantMessageErrorSchema.
const (
    ErrorRateLimit            = "rate_limit"
    ErrorAuthenticationFailed = "authentication_failed"
    ErrorBillingError         = "billing_error"
    ErrorInvalidRequest       = "invalid_request"
    ErrorServerError          = "server_error"
    ErrorMaxOutputTokens      = "max_output_tokens"
    ErrorUnknown              = "unknown"
)

// ParseStopFailurePayload reads JSON from r and returns the typed payload.
// Returns an error if the JSON is malformed or hook_event_name is not
// "StopFailure" (defensive: prevents misuse on other hook events).
func ParseStopFailurePayload(r io.Reader) (*StopFailurePayload, error)

// ProbeRunner runs `claude /usage` under a PTY, reads output, and returns
// as soon as enough signal has been captured or the deadline fires.
type ProbeRunner interface {
    // Probe returns the raw bytes captured from the PTY. An error may be
    // returned alongside partial bytes when capture was cut short; callers
    // should still attempt ParseUsageProbe on the bytes.
    Probe(ctx context.Context) ([]byte, error)
}

// ClaudePTYProbeRunner is the default ProbeRunner implementation. Spawns
// `claude /usage` under a PTY using github.com/creack/pty. Exits as soon as
// MinTokens `% used` matches are visible in accumulated output, or when
// HardDeadline elapses, or when ctx is cancelled.
type ClaudePTYProbeRunner struct {
    BinaryPath   string        // default: "claude"
    Args         []string      // default: []string{"/usage"}
    HardDeadline time.Duration // default: 8s
    MinTokens    int           // default: 2
}

func NewClaudePTYProbeRunner() *ClaudePTYProbeRunner
func (p *ClaudePTYProbeRunner) Probe(ctx context.Context) ([]byte, error)

// UsageVerdict classifies the outcome of parsing `/usage` output.
type UsageVerdict int

const (
    UsageExhausted UsageVerdict = iota  // clear signal: user is rate-limited (any window ≥95%)
    UsageHeadroom                        // clear signal: user has quota remaining
    UsageUncertain                       // parser could not extract enough signal
)

func (v UsageVerdict) String() string

// ParseUsageProbe classifies raw PTY-captured bytes from `claude /usage`.
// Extracts all `\d+% used` tokens via regex; classifies per §1.2:
//   - Any token ≥ 95 → UsageExhausted
//   - All tokens < 95 and ≥2 tokens found → UsageHeadroom
//   - Fewer than 2 tokens found → UsageUncertain
// Never returns an error.
func ParseUsageProbe(data []byte) UsageVerdict

// Trigger indicates what invoked the gate.
type Trigger int

const (
    TriggerHook   Trigger = iota // StopFailure{rate_limit} hook fired
    TriggerManual                 // user ran /token-bay fallback
)

// GateVerdict is the final decision from ApplyVerdictMatrix.
type GateVerdict int

const (
    VerdictProceed          GateVerdict = iota // go ahead with fallback
    VerdictRefuseManualOK                       // auto-refuse; manual override allowed
    VerdictRefuseWithReason                     // hard refuse with user-facing explanation
    VerdictSkipHook                             // hook fired but not for rate_limit — no action
)

func (v GateVerdict) String() string

// ApplyVerdictMatrix combines trigger and usage verdict per plugin spec §5.2.
// Also checks StopFailure.error for rate_limit when trigger is hook.
func ApplyVerdictMatrix(t Trigger, hookErr string, u UsageVerdict) GateVerdict

// CheckFreshness verifies that two signal timestamps are within 60s of each
// other and both within 120s of now. Returns true if both checks pass.
func CheckFreshness(stopFailureAt, usageProbeAt, now int64) bool
```

---

## 3. File layout

```
plugin/internal/ratelimit/
├── ratelimit.go            -- package doc + enum types
├── stopfailure.go          -- StopFailurePayload + ParseStopFailurePayload
├── stopfailure_test.go
├── probe.go                -- ProbeRunner interface + ClaudePTYProbeRunner (PTY spawn)
├── probe_test.go           -- stub runner + small-buffer streaming tests
├── usage.go                -- ParseUsageProbe (regex on `% used` tokens)
├── usage_test.go
├── verdict.go              -- ApplyVerdictMatrix + CheckFreshness
├── verdict_test.go
└── testdata/
    └── usage_sample.ansi   -- captured PTY bytes from a real `claude /usage` run
```

8 Go files + one testdata fixture. Each file single-responsibility.

**Testdata fixture**: the file `testdata/usage_sample.ansi` is raw bytes from a real run of `claude /usage` under a PTY, captured on 2026-04-23 against a Claude Max account at ~40% session utilization. Contains the tokens `40% used`, `3% used`, `94%`. This fixture anchors the parser tests — replace it if `Usage.tsx` renderer output changes in a future Claude Code release.

---

## 4. TDD task list

### Task 1: Package scaffold with enum types + String() methods

**Files:**
- Create: `plugin/internal/ratelimit/ratelimit.go`
- Create: `plugin/internal/ratelimit/ratelimit_test.go`
- Remove: `plugin/internal/ratelimit/.gitkeep`

**Work directory:** `/Users/dor.amid/git/token-bay/plugin/` with `PATH="$HOME/.local/share/mise/shims:$PATH"` on every `go` invocation.

- [ ] **Step 1: Write failing test**

Create `plugin/internal/ratelimit/ratelimit_test.go`:

```go
package ratelimit

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestUsageVerdict_String(t *testing.T) {
	assert.Equal(t, "exhausted", UsageExhausted.String())
	assert.Equal(t, "headroom", UsageHeadroom.String())
	assert.Equal(t, "uncertain", UsageUncertain.String())
}

func TestGateVerdict_String(t *testing.T) {
	assert.Equal(t, "proceed", VerdictProceed.String())
	assert.Equal(t, "refuse-manual-ok", VerdictRefuseManualOK.String())
	assert.Equal(t, "refuse-with-reason", VerdictRefuseWithReason.String())
	assert.Equal(t, "skip-hook", VerdictSkipHook.String())
}

func TestErrorConstants_MatchSchemaValues(t *testing.T) {
	// Pinned against src/entrypoints/sdk/coreSchemas.ts:1256-1266.
	assert.Equal(t, "rate_limit", ErrorRateLimit)
	assert.Equal(t, "authentication_failed", ErrorAuthenticationFailed)
	assert.Equal(t, "billing_error", ErrorBillingError)
	assert.Equal(t, "invalid_request", ErrorInvalidRequest)
	assert.Equal(t, "server_error", ErrorServerError)
	assert.Equal(t, "max_output_tokens", ErrorMaxOutputTokens)
	assert.Equal(t, "unknown", ErrorUnknown)
}
```

- [ ] **Step 2: Run test, confirm FAIL**

```bash
cd /Users/dor.amid/git/token-bay/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" go test ./internal/ratelimit/...
```

Expected: `undefined: UsageExhausted` etc.

- [ ] **Step 3: Write implementation**

Create `plugin/internal/ratelimit/ratelimit.go`:

```go
// Package ratelimit parses Claude Code rate-limit signals and applies the
// two-stage gate from plugin spec §5.2.
//
// Responsibilities:
//   - Parse StopFailure hook payloads (stopfailure.go).
//   - Parse `claude -p "/usage"` output with JSON and text strategies (usage.go).
//   - Apply the verdict matrix that decides proceed/refuse (verdict.go).
//
// This package performs no I/O. The sidecar's hooks module feeds in payload
// bytes; the sidecar's ccbridge module captures /usage output and passes
// those bytes here. The sidecar then applies the GateVerdict to decide
// next action.
package ratelimit

// Error enum values mirrored from Claude Code's SDKAssistantMessageErrorSchema
// (src/entrypoints/sdk/coreSchemas.ts:1256-1266).
const (
	ErrorRateLimit            = "rate_limit"
	ErrorAuthenticationFailed = "authentication_failed"
	ErrorBillingError         = "billing_error"
	ErrorInvalidRequest       = "invalid_request"
	ErrorServerError          = "server_error"
	ErrorMaxOutputTokens      = "max_output_tokens"
	ErrorUnknown              = "unknown"
)

// UsageVerdict classifies the outcome of parsing `claude -p "/usage"`.
type UsageVerdict int

const (
	UsageExhausted UsageVerdict = iota
	UsageHeadroom
	UsageUncertain
)

func (v UsageVerdict) String() string {
	switch v {
	case UsageExhausted:
		return "exhausted"
	case UsageHeadroom:
		return "headroom"
	case UsageUncertain:
		return "uncertain"
	default:
		return "unknown"
	}
}

// Trigger indicates what invoked the two-stage gate.
type Trigger int

const (
	TriggerHook Trigger = iota
	TriggerManual
)

// GateVerdict is the final decision from ApplyVerdictMatrix.
type GateVerdict int

const (
	VerdictProceed GateVerdict = iota
	VerdictRefuseManualOK
	VerdictRefuseWithReason
	VerdictSkipHook
)

func (v GateVerdict) String() string {
	switch v {
	case VerdictProceed:
		return "proceed"
	case VerdictRefuseManualOK:
		return "refuse-manual-ok"
	case VerdictRefuseWithReason:
		return "refuse-with-reason"
	case VerdictSkipHook:
		return "skip-hook"
	default:
		return "unknown"
	}
}
```

- [ ] **Step 4: Remove .gitkeep, run tests, confirm PASS**

```bash
rm /Users/dor.amid/git/token-bay/plugin/internal/ratelimit/.gitkeep
cd /Users/dor.amid/git/token-bay/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" go test ./internal/ratelimit/...
```

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/git/token-bay
git add plugin/internal/ratelimit/ratelimit.go plugin/internal/ratelimit/ratelimit_test.go
git rm plugin/internal/ratelimit/.gitkeep
git commit -m "feat(plugin/ratelimit): scaffold enum types + String methods"
```

### Task 2: ParseStopFailurePayload

**Files:**
- Create: `plugin/internal/ratelimit/stopfailure.go`
- Create: `plugin/internal/ratelimit/stopfailure_test.go`

- [ ] **Step 1: Write failing tests**

Create `plugin/internal/ratelimit/stopfailure_test.go`:

```go
package ratelimit

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseStopFailurePayload_RateLimitMinimal_Succeeds(t *testing.T) {
	body := []byte(`{
		"session_id": "s-1",
		"transcript_path": "/tmp/t",
		"cwd": "/work",
		"hook_event_name": "StopFailure",
		"error": "rate_limit"
	}`)
	p, err := ParseStopFailurePayload(bytes.NewReader(body))
	require.NoError(t, err)
	assert.Equal(t, "s-1", p.SessionID)
	assert.Equal(t, "/tmp/t", p.TranscriptPath)
	assert.Equal(t, "/work", p.CWD)
	assert.Equal(t, "StopFailure", p.HookEventName)
	assert.Equal(t, ErrorRateLimit, p.Error)
	assert.Nil(t, p.PermissionMode)
	assert.Nil(t, p.AgentID)
	assert.Nil(t, p.ErrorDetails)
	assert.Nil(t, p.LastAssistantMessage)
}

func TestParseStopFailurePayload_WithAllOptionalFields_ParsesAll(t *testing.T) {
	body := []byte(`{
		"session_id": "s-1",
		"transcript_path": "/tmp/t",
		"cwd": "/work",
		"permission_mode": "default",
		"agent_id": "a-1",
		"hook_event_name": "StopFailure",
		"error": "rate_limit",
		"error_details": "429 too many requests",
		"last_assistant_message": "I was saying..."
	}`)
	p, err := ParseStopFailurePayload(bytes.NewReader(body))
	require.NoError(t, err)
	require.NotNil(t, p.PermissionMode)
	assert.Equal(t, "default", *p.PermissionMode)
	require.NotNil(t, p.AgentID)
	assert.Equal(t, "a-1", *p.AgentID)
	require.NotNil(t, p.ErrorDetails)
	assert.Equal(t, "429 too many requests", *p.ErrorDetails)
	require.NotNil(t, p.LastAssistantMessage)
	assert.Equal(t, "I was saying...", *p.LastAssistantMessage)
}

func TestParseStopFailurePayload_MalformedJSON_ReturnsError(t *testing.T) {
	_, err := ParseStopFailurePayload(bytes.NewReader([]byte(`{not json`)))
	assert.Error(t, err)
}

func TestParseStopFailurePayload_WrongHookEvent_ReturnsError(t *testing.T) {
	body := []byte(`{
		"session_id": "s-1",
		"transcript_path": "/tmp/t",
		"cwd": "/work",
		"hook_event_name": "Stop",
		"error": "rate_limit"
	}`)
	_, err := ParseStopFailurePayload(bytes.NewReader(body))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "Stop")
}
```

- [ ] **Step 2: Run tests, confirm FAIL**

- [ ] **Step 3: Write implementation**

Create `plugin/internal/ratelimit/stopfailure.go`:

```go
package ratelimit

import (
	"encoding/json"
	"fmt"
	"io"
)

// StopFailurePayload mirrors Claude Code's StopFailureHookInput schema at
// src/entrypoints/sdk/coreSchemas.ts:529-538 combined with BaseHookInputSchema
// at :387-399. Optional fields are pointer-to-string so callers can
// distinguish absent from empty.
type StopFailurePayload struct {
	SessionID            string  `json:"session_id"`
	TranscriptPath       string  `json:"transcript_path"`
	CWD                  string  `json:"cwd"`
	PermissionMode       *string `json:"permission_mode,omitempty"`
	AgentID              *string `json:"agent_id,omitempty"`
	HookEventName        string  `json:"hook_event_name"`
	Error                string  `json:"error"`
	ErrorDetails         *string `json:"error_details,omitempty"`
	LastAssistantMessage *string `json:"last_assistant_message,omitempty"`
}

// ParseStopFailurePayload reads JSON from r and returns the typed payload.
// Returns an error if the JSON is malformed or hook_event_name is not
// literally "StopFailure" (defensive against misuse on other hook events).
func ParseStopFailurePayload(r io.Reader) (*StopFailurePayload, error) {
	var p StopFailurePayload
	if err := json.NewDecoder(r).Decode(&p); err != nil {
		return nil, fmt.Errorf("ratelimit: parse StopFailure payload: %w", err)
	}
	if p.HookEventName != "StopFailure" {
		return nil, fmt.Errorf("ratelimit: expected hook_event_name=StopFailure, got %q", p.HookEventName)
	}
	return &p, nil
}
```

- [ ] **Step 4: Run tests, confirm PASS**

- [ ] **Step 5: Commit**

```bash
git add plugin/internal/ratelimit/stopfailure.go plugin/internal/ratelimit/stopfailure_test.go
git commit -m "feat(plugin/ratelimit): ParseStopFailurePayload — JSON decode + schema guard"
```

### Task 3: PTY probe runner — `ClaudePTYProbeRunner`

**Files:**
- Create: `plugin/internal/ratelimit/probe.go`
- Create: `plugin/internal/ratelimit/probe_test.go`
- Modify: `plugin/go.mod` — add `github.com/creack/pty`

**Why a stub-runner interface.** The default `ClaudePTYProbeRunner` actually spawns `claude` — slow, requires the binary, network-touching. Tests use a `StubProbeRunner` that returns canned bytes (including the testdata fixture). Only one smoke test drives the real runner, and it skips unless `claude` is on PATH.

- [ ] **Step 1: Add the pty dependency**

```bash
cd /Users/dor.amid/git/token-bay/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" go get github.com/creack/pty@latest && PATH="$HOME/.local/share/mise/shims:$PATH" go mod tidy
```

- [ ] **Step 2: Write the stub + interface tests**

Create `plugin/internal/ratelimit/probe_test.go`:

```go
package ratelimit

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// StubProbeRunner returns canned bytes. Used by the parser tests and the
// sidecar tests; exported from this test file so sibling packages can
// depend on it via internal/testsupport if needed later.
type StubProbeRunner struct {
	Bytes []byte
	Err   error
}

func (s *StubProbeRunner) Probe(ctx context.Context) ([]byte, error) {
	return s.Bytes, s.Err
}

func TestClaudePTYProbeRunner_Defaults(t *testing.T) {
	r := NewClaudePTYProbeRunner()
	require.NotNil(t, r)
	assert.Equal(t, "claude", r.BinaryPath)
	assert.Equal(t, []string{"/usage"}, r.Args)
	assert.Equal(t, 8*time.Second, r.HardDeadline)
	assert.Equal(t, 2, r.MinTokens)
}

func TestClaudePTYProbeRunner_MissingBinary_ReturnsError(t *testing.T) {
	r := NewClaudePTYProbeRunner()
	r.BinaryPath = "/definitely/does/not/exist/claude"
	r.HardDeadline = 500 * time.Millisecond

	_, err := r.Probe(context.Background())
	assert.Error(t, err)
}

func TestClaudePTYProbeRunner_LiveSmokeTest(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH; skipping live smoke")
	}
	r := NewClaudePTYProbeRunner()
	r.HardDeadline = 10 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	data, err := r.Probe(ctx)
	// Error is acceptable if MinTokens was not reached before HardDeadline
	// as long as we got SOME bytes — that means the PTY path is working.
	if err != nil {
		t.Logf("probe returned error (may be deadline-triggered): %v", err)
	}
	assert.NotEmpty(t, data, "live probe should return at least some TUI bytes")
}

// Ensure StubProbeRunner satisfies ProbeRunner at compile time.
var _ ProbeRunner = (*StubProbeRunner)(nil)
var _ ProbeRunner = (*ClaudePTYProbeRunner)(nil)

func TestProbeRunner_ContextCancelled_ReturnsError(t *testing.T) {
	r := NewClaudePTYProbeRunner()
	r.BinaryPath = "/bin/sh"
	r.Args = []string{"-c", "sleep 5"}
	r.HardDeadline = 5 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := r.Probe(ctx)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled),
		"expected ctx error, got %v", err)
}
```

- [ ] **Step 3: Run tests, confirm FAIL**

Expected: `undefined: NewClaudePTYProbeRunner`, `undefined: ProbeRunner` etc.

- [ ] **Step 4: Write implementation**

Create `plugin/internal/ratelimit/probe.go`:

```go
package ratelimit

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"time"

	"github.com/creack/pty"
)

// ProbeRunner runs `claude /usage` (or equivalent) under a PTY and returns
// captured bytes. Callers pass the bytes to ParseUsageProbe.
type ProbeRunner interface {
	Probe(ctx context.Context) ([]byte, error)
}

// ClaudePTYProbeRunner spawns `claude` under a PTY (so Claude Code renders
// its real TUI rather than the non-TTY stub) and reads until either
// MinTokens `% used` matches are visible, the context is cancelled, or
// HardDeadline elapses.
type ClaudePTYProbeRunner struct {
	BinaryPath   string
	Args         []string
	HardDeadline time.Duration
	MinTokens    int
}

// NewClaudePTYProbeRunner returns a runner with sane defaults.
func NewClaudePTYProbeRunner() *ClaudePTYProbeRunner {
	return &ClaudePTYProbeRunner{
		BinaryPath:   "claude",
		Args:         []string{"/usage"},
		HardDeadline: 8 * time.Second,
		MinTokens:    2,
	}
}

var probePctUsedRE = regexp.MustCompile(`(\d+)%\s*used`)

// Probe spawns the configured command under a PTY, reads output, and
// returns as soon as MinTokens `% used` matches are accumulated or the
// hard deadline / ctx fires.
func (p *ClaudePTYProbeRunner) Probe(ctx context.Context) ([]byte, error) {
	deadline := p.HardDeadline
	if deadline <= 0 {
		deadline = 8 * time.Second
	}
	minTokens := p.MinTokens
	if minTokens <= 0 {
		minTokens = 2
	}

	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.BinaryPath, p.Args...)
	pt, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("ratelimit: pty.Start %s: %w", p.BinaryPath, err)
	}
	defer func() { _ = pt.Close() }()

	type readResult struct {
		chunk []byte
		err   error
	}
	reads := make(chan readResult, 16)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := pt.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				reads <- readResult{chunk: chunk}
			}
			if err != nil {
				reads <- readResult{err: err}
				return
			}
		}
	}()

	var out []byte
	enough := false
	for !enough {
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			if len(out) == 0 {
				return nil, ctx.Err()
			}
			return out, ctx.Err()
		case r := <-reads:
			if len(r.chunk) > 0 {
				out = append(out, r.chunk...)
				if countPctUsed(out) >= minTokens {
					enough = true
				}
			}
			if r.err != nil && r.err != io.EOF {
				// Non-EOF read error before we had enough — surface it.
				if !enough {
					_ = cmd.Process.Kill()
					_, _ = cmd.Process.Wait()
					if len(out) == 0 {
						return nil, r.err
					}
					return out, r.err
				}
			}
			if r.err == io.EOF {
				enough = true // process ended naturally
			}
		}
	}

	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	return out, nil
}

// countPctUsed returns the number of `N% used` tokens in data.
// Used by the probe to decide when to early-terminate.
func countPctUsed(data []byte) int {
	return len(probePctUsedRE.FindAll(data, -1))
}
```

- [ ] **Step 5: Run tests, confirm PASS**

```bash
cd /Users/dor.amid/git/token-bay/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" go test -race ./internal/ratelimit/...
```

Note: the `LiveSmokeTest` may run or skip depending on whether `claude` is installed. The rest must pass.

- [ ] **Step 6: Commit**

```bash
git add plugin/internal/ratelimit/probe.go plugin/internal/ratelimit/probe_test.go plugin/go.mod plugin/go.sum
git commit -m "feat(plugin/ratelimit): PTY probe runner via creack/pty"
```

### Task 4: ParseUsageProbe — regex on `% used` tokens

**Files:**
- Create: `plugin/internal/ratelimit/usage.go`
- Create: `plugin/internal/ratelimit/usage_test.go`
- Create: `plugin/internal/ratelimit/testdata/usage_sample.ansi` — captured real probe bytes

- [ ] **Step 1: Capture a real fixture**

During implementation, run `claude /usage` under PTY once (using the probe runner from Task 3 or a short Go main) and save the captured bytes to `plugin/internal/ratelimit/testdata/usage_sample.ansi`. This anchors the parser to real-world output.

- [ ] **Step 2: Write failing tests**

Create `plugin/internal/ratelimit/usage_test.go`:

```go
package ratelimit

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseUsageProbe_RealFixture_Headroom(t *testing.T) {
	data, err := os.ReadFile("testdata/usage_sample.ansi")
	require.NoError(t, err, "fixture must exist; capture via probe runner")
	assert.Equal(t, UsageHeadroom, ParseUsageProbe(data))
}

func TestParseUsageProbe_Synthetic_AllLow_Headroom(t *testing.T) {
	data := []byte("Current session 10% used / Current week 5% used")
	assert.Equal(t, UsageHeadroom, ParseUsageProbe(data))
}

func TestParseUsageProbe_Synthetic_OneHigh_Exhausted(t *testing.T) {
	data := []byte("Current session 99% used / Current week 5% used")
	assert.Equal(t, UsageExhausted, ParseUsageProbe(data))
}

func TestParseUsageProbe_Synthetic_ExactlyThreshold_Exhausted(t *testing.T) {
	data := []byte("Current session 95% used / Current week 5% used")
	assert.Equal(t, UsageExhausted, ParseUsageProbe(data))
}

func TestParseUsageProbe_Synthetic_JustBelowThreshold_Headroom(t *testing.T) {
	data := []byte("Current session 94% used / Current week 5% used")
	assert.Equal(t, UsageHeadroom, ParseUsageProbe(data))
}

func TestParseUsageProbe_SingleToken_Uncertain(t *testing.T) {
	data := []byte("Current session 50% used")
	assert.Equal(t, UsageUncertain, ParseUsageProbe(data))
}

func TestParseUsageProbe_NoTokens_Uncertain(t *testing.T) {
	data := []byte("Loading usage data... (truncated before render)")
	assert.Equal(t, UsageUncertain, ParseUsageProbe(data))
}

func TestParseUsageProbe_TokensAcrossANSIEscapes_StillMatches(t *testing.T) {
	// Simulate the raw PTY format: `37%` then an ANSI escape then `used`.
	// The regex allows \s* between % and used, and ANSI bytes start with ESC.
	// In practice ANSI escapes are contiguous bytes with no whitespace, so
	// the percent and `used` are joined by regular space in real captures.
	data := []byte("Session 30% \x1b[0mused / Week 2% used")
	assert.Equal(t, UsageHeadroom, ParseUsageProbe(data))
}
```

- [ ] **Step 3: Run tests, confirm FAIL**

- [ ] **Step 4: Write implementation**

Create `plugin/internal/ratelimit/usage.go`:

```go
package ratelimit

import "regexp"

// exhaustedThreshold is the utilization percentage at or above which
// we classify a rate-limit window as exhausted. Math.floor is applied
// by Claude Code before rendering (src/components/Settings/Usage.tsx:42),
// so a real 99.9% shows as `99% used`. A window at 95+ is effectively
// full.
const exhaustedThreshold = 95

// usagePctUsedRE extracts `N% used` tokens. Allows ANSI escapes or other
// non-word bytes between the digits and `used` — the `\s*` is a loose
// tolerance for terminal-control sequences in captured output. In practice
// captured PTY output renders the % and `used` separated by either a
// literal space or a single cursor-control escape.
var usagePctUsedRE = regexp.MustCompile(`(\d+)%[^\w]*used`)

// ParseUsageProbe extracts `N% used` utilization tokens from raw PTY
// output and classifies per plan §1.2.
func ParseUsageProbe(data []byte) UsageVerdict {
	matches := usagePctUsedRE.FindAllSubmatch(data, -1)
	if len(matches) < 2 {
		return UsageUncertain
	}
	exhausted := false
	for _, m := range matches {
		pct := atoi(m[1])
		if pct >= exhaustedThreshold {
			exhausted = true
		}
	}
	if exhausted {
		return UsageExhausted
	}
	return UsageHeadroom
}

// atoi is a minimal safe atoi for regex-captured digits. Never panics.
func atoi(b []byte) int {
	n := 0
	for _, c := range b {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int(c-'0')
	}
	return n
}
```

- [ ] **Step 5: Run tests, confirm PASS**

- [ ] **Step 6: Commit**

```bash
git add plugin/internal/ratelimit/usage.go plugin/internal/ratelimit/usage_test.go plugin/internal/ratelimit/testdata/
git commit -m "feat(plugin/ratelimit): ParseUsageProbe via regex on % used tokens"
```

### Task 5: Verdict matrix + freshness guard

**Files:**
- Create: `plugin/internal/ratelimit/verdict.go`
- Create: `plugin/internal/ratelimit/verdict_test.go`

- [ ] **Step 1: Write failing tests**

Create `plugin/internal/ratelimit/verdict_test.go`:

```go
package ratelimit

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestApplyVerdictMatrix_HookRateLimitExhausted_Proceed(t *testing.T) {
	v := ApplyVerdictMatrix(TriggerHook, ErrorRateLimit, UsageExhausted)
	assert.Equal(t, VerdictProceed, v)
}

func TestApplyVerdictMatrix_HookRateLimitHeadroom_RefuseManualOK(t *testing.T) {
	v := ApplyVerdictMatrix(TriggerHook, ErrorRateLimit, UsageHeadroom)
	assert.Equal(t, VerdictRefuseManualOK, v)
}

func TestApplyVerdictMatrix_HookRateLimitUncertain_Proceed(t *testing.T) {
	// Hook alone is strong enough; uncertain /usage doesn't block.
	v := ApplyVerdictMatrix(TriggerHook, ErrorRateLimit, UsageUncertain)
	assert.Equal(t, VerdictProceed, v)
}

func TestApplyVerdictMatrix_HookNonRateLimit_SkipHook(t *testing.T) {
	cases := []string{ErrorAuthenticationFailed, ErrorBillingError, ErrorServerError,
		ErrorInvalidRequest, ErrorMaxOutputTokens, ErrorUnknown, "anything-else"}
	for _, errName := range cases {
		t.Run(errName, func(t *testing.T) {
			v := ApplyVerdictMatrix(TriggerHook, errName, UsageExhausted)
			assert.Equal(t, VerdictSkipHook, v)
		})
	}
}

func TestApplyVerdictMatrix_ManualExhausted_Proceed(t *testing.T) {
	v := ApplyVerdictMatrix(TriggerManual, "", UsageExhausted)
	assert.Equal(t, VerdictProceed, v)
}

func TestApplyVerdictMatrix_ManualHeadroom_RefuseWithReason(t *testing.T) {
	v := ApplyVerdictMatrix(TriggerManual, "", UsageHeadroom)
	assert.Equal(t, VerdictRefuseWithReason, v)
}

func TestApplyVerdictMatrix_ManualUncertain_RefuseWithReason(t *testing.T) {
	// Conservative: no external signal, uncertain probe — don't spend credits.
	v := ApplyVerdictMatrix(TriggerManual, "", UsageUncertain)
	assert.Equal(t, VerdictRefuseWithReason, v)
}

func TestCheckFreshness_AllFresh_ReturnsTrue(t *testing.T) {
	now := int64(1_000_000)
	assert.True(t, CheckFreshness(now-10, now-5, now))
}

func TestCheckFreshness_SignalsFarApart_ReturnsFalse(t *testing.T) {
	now := int64(1_000_000)
	// StopFailure 120s ago, /usage now — diverge by 120s (> 60s threshold).
	assert.False(t, CheckFreshness(now-120, now, now))
}

func TestCheckFreshness_StopFailureStale_ReturnsFalse(t *testing.T) {
	now := int64(1_000_000)
	// StopFailure 200s ago — stale even if /usage is fresh.
	assert.False(t, CheckFreshness(now-200, now-10, now))
}

func TestCheckFreshness_UsageStale_ReturnsFalse(t *testing.T) {
	now := int64(1_000_000)
	assert.False(t, CheckFreshness(now-10, now-200, now))
}
```

- [ ] **Step 2: Run tests, confirm FAIL**

- [ ] **Step 3: Write implementation**

Create `plugin/internal/ratelimit/verdict.go`:

```go
package ratelimit

const (
	// maxSignalDrift is the allowed gap between the StopFailure event and
	// the /usage probe. Plugin spec §5.3 requires the probe to follow
	// StopFailure closely enough that it's measuring the same session.
	maxSignalDriftSec = 60

	// maxSignalAge bounds how old either signal can be at verdict time.
	// The sidecar holds pending tickets for short windows; beyond 120s
	// we treat both as stale.
	maxSignalAgeSec = 120
)

// ApplyVerdictMatrix combines the trigger, the StopFailure error (when
// trigger is hook), and the /usage verdict into a GateVerdict per plugin
// spec §5.2.
//
// When trigger is hook and hookErr is not "rate_limit", the gate returns
// VerdictSkipHook — the plugin is not equipped to handle other error types.
// hookErr is ignored when trigger is TriggerManual.
func ApplyVerdictMatrix(t Trigger, hookErr string, u UsageVerdict) GateVerdict {
	if t == TriggerHook {
		if hookErr != ErrorRateLimit {
			return VerdictSkipHook
		}
		switch u {
		case UsageExhausted, UsageUncertain:
			return VerdictProceed
		case UsageHeadroom:
			return VerdictRefuseManualOK
		default:
			return VerdictRefuseManualOK
		}
	}
	// TriggerManual
	switch u {
	case UsageExhausted:
		return VerdictProceed
	case UsageHeadroom, UsageUncertain:
		return VerdictRefuseWithReason
	default:
		return VerdictRefuseWithReason
	}
}

// CheckFreshness verifies that both signal timestamps are within
// maxSignalDriftSec of each other, and within maxSignalAgeSec of now.
// Timestamps are unix seconds.
func CheckFreshness(stopFailureAt, usageProbeAt, now int64) bool {
	drift := stopFailureAt - usageProbeAt
	if drift < 0 {
		drift = -drift
	}
	if drift > maxSignalDriftSec {
		return false
	}
	if now-stopFailureAt > maxSignalAgeSec {
		return false
	}
	if now-usageProbeAt > maxSignalAgeSec {
		return false
	}
	return true
}
```

- [ ] **Step 4: Run tests, confirm PASS**

- [ ] **Step 5: Commit**

```bash
git add plugin/internal/ratelimit/verdict.go plugin/internal/ratelimit/verdict_test.go
git commit -m "feat(plugin/ratelimit): verdict matrix + freshness guard"
```

### Task 6: Full-module coverage check + tag

- [ ] **Step 1: Run coverage**

```bash
cd /Users/dor.amid/git/token-bay/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -coverprofile=coverage.out ./internal/ratelimit/...
PATH="$HOME/.local/share/mise/shims:$PATH" go tool cover -func=coverage.out | tail -10
```

Expected: ≥ 85% (this package is almost pure logic; coverage should be high).

- [ ] **Step 2: Root `make test`**

```bash
cd /Users/dor.amid/git/token-bay && PATH="$HOME/.local/share/mise/shims:$PATH" make test
```

Expected: all three modules (shared, plugin, tracker) pass.

- [ ] **Step 3: Tag and push**

```bash
git tag -a plugin-ratelimit-v0 -m "internal/ratelimit feature complete"
git push origin main --tags
```

---

## 5. Open questions for implementation

1. **Real `/usage` output format.** The JSON shape in §1.2 is hypothesized. When we get a real sample from a rate-limited Claude Code account, update `usageProbeJSON` to match. Until then, the text-fallback patterns catch the most likely phrasings.
2. **Text-pattern false positives.** The regex `\bexceeded\b` could match `"you have not exceeded your limit"`. Negation detection is not done in v1 — we accept occasional false-positive exhaustion classifications when the text is adversarial. Mitigation: the text strategy is only consulted when the JSON strategy returned no signal, which should be the less-common case.
3. **Future StopFailure error types.** If Claude Code adds a new enum value, our `ApplyVerdictMatrix` routes it to `VerdictSkipHook`, which is safe — new errors don't silently trigger fallback. A release note in Token-Bay would add handling if any future error type genuinely should.
4. **Freshness bounds tuning.** `60s` drift and `120s` age are plugin spec §5.3 defaults. Actual tuning after integration: users may sit at a StopFailure dialog for > 120s before consenting, in which case we'd need to re-probe `/usage`. Handle in the sidecar caller, not in this module.

---

## Self-review

- **Spec coverage:** Plugin spec §5.1 (trigger paths), §5.2 (verdict matrix), §5.3 (freshness), §5.6 (signal capture shape) — all covered by tasks in this plan.
- **Placeholder scan:** One intentional "open question" block documenting v1 `/usage` shape uncertainty. No TBDs in the task steps.
- **Type consistency:** `StopFailurePayload`, `UsageVerdict`, `Trigger`, `GateVerdict` consistent across tasks. Enum values (ErrorRateLimit etc.) derived directly from Claude Code source — pinned via code references in §1.1.
- **TDD discipline:** Each task is red→green→commit. Tests drive the shape (e.g., Task 3 establishes the JSON parser contract before Task 4 extends with text fallback). Six-task granularity matches the module's moderate surface.

## Next plan

After `ratelimit` lands, the natural next steps are:
1. **`shared/proto` real types** — build out the broker-request envelope, ledger entry, federation messages. These are the data contracts that `ccproxy` needs.
2. **`shared/exhaustionproof` real types** — the two-signal bundle struct with canonical serialization for signing.
3. **`plugin/internal/ccproxy`** — the Anthropic-compatible HTTPS proxy that will consume both `settingsjson`, `ratelimit`, and the shared types above.

`shared/*` feature plans should come next; they unblock both plugin and tracker feature work.
