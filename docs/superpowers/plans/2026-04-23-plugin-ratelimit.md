# Plugin `internal/ratelimit` — Feature Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Implement `plugin/internal/ratelimit` — the two-stage gate that classifies the Claude Code `StopFailure` hook payload, parses `claude -p "/usage"` output, and applies the verdict matrix from plugin spec §5.2.

**Architecture:** Pure Go, no I/O (the sidecar is responsible for invoking `claude -p "/usage"` and piping stdin into the hook handler). This module is a parser + decision matrix. Leaf: imports nothing from `shared/` or `plugin/internal/*`.

**Tech Stack:**
- Go 1.23+
- `encoding/json` (stdlib)
- `regexp` (stdlib) — for text-format `/usage` fallback parser
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

### 1.2 `/usage` output — strategy

Format unknown at spec time (plugin spec §10 open question). v1 uses a two-strategy parser:

**Strategy 1 — JSON.** Attempt `json.Unmarshal` into a hypothesized shape:
```text
{
  "status":            "limited" | "ok"
  "remaining_percent": number
  "messages":          { "used": number, "limit": number }
  ...
}
```

If parsing succeeds and ANY of the hypothesized fields is populated, extract a verdict:
- `status == "limited"` OR `remaining_percent == 0` OR `messages.used >= messages.limit` → `UsageExhausted`
- `remaining_percent > 0` with a numeric value → `UsageHeadroom`
- None of the fields populated → `UsageUncertain`

**Strategy 2 — text.** If JSON unmarshal fails or returns `UsageUncertain`:
- Exhaustion indicators (regex, case-insensitive): `rate[- ]limit`, `exhausted`, `exceeded`, `no (requests?|messages?) (remaining|left)`, `0% remaining`
- Headroom indicators: `\d+% remaining`, `\d+ (requests?|messages?) (remaining|left)`
- If exhaustion indicator matches and headroom doesn't → `UsageExhausted`
- If headroom matches and exhaustion doesn't → `UsageHeadroom`
- Otherwise → `UsageUncertain`

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

- No process invocation. The sidecar runs `claude -p "/usage"`; this module parses the output.
- No hook handler binding. The sidecar's `internal/hooks` module accepts the hook payload from stdin and calls `ratelimit.ParseStopFailurePayload`.
- No ticket assembly for the exhaustion proof. That's in a later module (`internal/exhaustion-proof`). This module returns raw parsed structs that the caller bundles into the ticket.
- No retry logic. One parse, one verdict.

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

// UsageVerdict classifies the outcome of parsing `claude -p "/usage"`.
type UsageVerdict int

const (
    UsageExhausted UsageVerdict = iota  // clear signal: user is rate-limited
    UsageHeadroom                        // clear signal: user has quota remaining
    UsageUncertain                       // parser ran but signal is ambiguous
)

func (v UsageVerdict) String() string

// ParseUsageProbe parses `claude -p "/usage"` output. Tries JSON first
// (UsageProbeJSON shape); falls back to regex-based text heuristics.
// Never returns an error — worst case is UsageUncertain.
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
├── usage.go                -- ParseUsageProbe (JSON + text strategies)
├── usage_test.go
├── verdict.go              -- ApplyVerdictMatrix + CheckFreshness
├── verdict_test.go
```

7 files. Each file single-responsibility.

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

### Task 3: ParseUsageProbe — JSON strategy

**Files:**
- Create: `plugin/internal/ratelimit/usage.go`
- Create: `plugin/internal/ratelimit/usage_test.go`

- [ ] **Step 1: Write failing tests — JSON cases only**

Create `plugin/internal/ratelimit/usage_test.go`:

```go
package ratelimit

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseUsageProbe_JSONStatusLimited_ReturnsExhausted(t *testing.T) {
	data := []byte(`{"status":"limited"}`)
	assert.Equal(t, UsageExhausted, ParseUsageProbe(data))
}

func TestParseUsageProbe_JSONStatusOK_ReturnsHeadroom(t *testing.T) {
	data := []byte(`{"status":"ok","remaining_percent":42}`)
	assert.Equal(t, UsageHeadroom, ParseUsageProbe(data))
}

func TestParseUsageProbe_JSONRemainingZero_ReturnsExhausted(t *testing.T) {
	data := []byte(`{"status":"ok","remaining_percent":0}`)
	assert.Equal(t, UsageExhausted, ParseUsageProbe(data))
}

func TestParseUsageProbe_JSONMessagesFull_ReturnsExhausted(t *testing.T) {
	data := []byte(`{"messages":{"used":45,"limit":45}}`)
	assert.Equal(t, UsageExhausted, ParseUsageProbe(data))
}

func TestParseUsageProbe_JSONMessagesRoom_ReturnsHeadroom(t *testing.T) {
	data := []byte(`{"messages":{"used":10,"limit":45}}`)
	assert.Equal(t, UsageHeadroom, ParseUsageProbe(data))
}

func TestParseUsageProbe_EmptyJSON_ReturnsUncertain(t *testing.T) {
	data := []byte(`{}`)
	assert.Equal(t, UsageUncertain, ParseUsageProbe(data))
}
```

- [ ] **Step 2: Run tests, confirm FAIL**

- [ ] **Step 3: Write JSON-strategy implementation**

Create `plugin/internal/ratelimit/usage.go`:

```go
package ratelimit

import "encoding/json"

// usageProbeJSON is the hypothesized JSON shape of `claude -p "/usage"`.
// Actual fields that Claude Code emits are uncertain as of the plugin spec
// §10 open question — we parse defensively and treat any one populated
// field as a signal.
type usageProbeJSON struct {
	Status           string       `json:"status,omitempty"`            // "limited" | "ok" | ""
	RemainingPercent *float64     `json:"remaining_percent,omitempty"` // 0..100
	Messages         *usageCounts `json:"messages,omitempty"`
}

type usageCounts struct {
	Used  int `json:"used"`
	Limit int `json:"limit"`
}

// ParseUsageProbe classifies `claude -p "/usage"` output. Tries JSON first;
// falls back to text heuristics (added in Task 4). Never errors — worst case
// is UsageUncertain, which the verdict matrix handles.
func ParseUsageProbe(data []byte) UsageVerdict {
	if v, ok := parseUsageJSON(data); ok {
		return v
	}
	return UsageUncertain
}

func parseUsageJSON(data []byte) (UsageVerdict, bool) {
	var p usageProbeJSON
	if err := json.Unmarshal(data, &p); err != nil {
		return UsageUncertain, false
	}
	// Status field has priority when present.
	switch p.Status {
	case "limited":
		return UsageExhausted, true
	case "ok":
		// fall through; use numeric signals
	}
	// remaining_percent takes precedence over messages when present.
	if p.RemainingPercent != nil {
		if *p.RemainingPercent <= 0 {
			return UsageExhausted, true
		}
		return UsageHeadroom, true
	}
	if p.Messages != nil && p.Messages.Limit > 0 {
		if p.Messages.Used >= p.Messages.Limit {
			return UsageExhausted, true
		}
		return UsageHeadroom, true
	}
	// Parsed but no signal.
	return UsageUncertain, false
}
```

- [ ] **Step 4: Run tests, confirm PASS**

- [ ] **Step 5: Commit**

```bash
git add plugin/internal/ratelimit/usage.go plugin/internal/ratelimit/usage_test.go
git commit -m "feat(plugin/ratelimit): ParseUsageProbe JSON strategy"
```

### Task 4: ParseUsageProbe — text fallback strategy

**Files:**
- Modify: `plugin/internal/ratelimit/usage.go`
- Modify: `plugin/internal/ratelimit/usage_test.go`

- [ ] **Step 1: Append failing tests for text strategy**

Append to `plugin/internal/ratelimit/usage_test.go`:

```go
func TestParseUsageProbe_TextExhaustion_ReturnsExhausted(t *testing.T) {
	cases := []string{
		"You have been rate limited. Wait 2h 14m.",
		"Rate-limit exceeded",
		"Quota exhausted for this window",
		"No requests remaining",
		"0% remaining",
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			assert.Equal(t, UsageExhausted, ParseUsageProbe([]byte(c)))
		})
	}
}

func TestParseUsageProbe_TextHeadroom_ReturnsHeadroom(t *testing.T) {
	cases := []string{
		"You have 42% remaining in this window.",
		"3 messages left",
		"20 requests remaining",
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			assert.Equal(t, UsageHeadroom, ParseUsageProbe([]byte(c)))
		})
	}
}

func TestParseUsageProbe_TextAmbiguous_ReturnsUncertain(t *testing.T) {
	cases := []string{
		"",
		"Hello world",
		"You are currently on the Pro plan.",
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			assert.Equal(t, UsageUncertain, ParseUsageProbe([]byte(c)))
		})
	}
}

func TestParseUsageProbe_TextConflicting_ReturnsUncertain(t *testing.T) {
	// Contains both an exhaustion phrase and a headroom phrase — can't decide.
	data := []byte("Rate limit exceeded for daily quota. 5 messages left in 5-hour window.")
	assert.Equal(t, UsageUncertain, ParseUsageProbe(data))
}
```

- [ ] **Step 2: Run tests, confirm FAIL on text cases**

- [ ] **Step 3: Extend `usage.go` with text strategy**

Add to `plugin/internal/ratelimit/usage.go`:

```go
import (
	"encoding/json"
	"regexp"
)

var (
	exhaustionPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)rate[- ]limit(ed| exceeded| hit)?`),
		regexp.MustCompile(`(?i)\bexhausted\b`),
		regexp.MustCompile(`(?i)\bexceeded\b`),
		regexp.MustCompile(`(?i)no (requests?|messages?|quota) (remaining|left)`),
		regexp.MustCompile(`(?i)\b0 *% *remaining\b`),
	}
	headroomPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b([1-9][0-9]?|100) *% *remaining\b`),
		regexp.MustCompile(`(?i)\b([1-9][0-9]*) (requests?|messages?) (remaining|left)\b`),
	}
)

// parseUsageText returns a verdict based on regex matches against text
// output. Returns UsageUncertain if neither or both categories match.
func parseUsageText(data []byte) UsageVerdict {
	hasExhaustion := anyMatch(exhaustionPatterns, data)
	hasHeadroom := anyMatch(headroomPatterns, data)
	switch {
	case hasExhaustion && !hasHeadroom:
		return UsageExhausted
	case hasHeadroom && !hasExhaustion:
		return UsageHeadroom
	default:
		return UsageUncertain
	}
}

func anyMatch(patterns []*regexp.Regexp, data []byte) bool {
	for _, p := range patterns {
		if p.Match(data) {
			return true
		}
	}
	return false
}
```

Modify the top-level `ParseUsageProbe` to fall through to text:

```go
func ParseUsageProbe(data []byte) UsageVerdict {
	if v, ok := parseUsageJSON(data); ok {
		return v
	}
	return parseUsageText(data)
}
```

- [ ] **Step 4: Run tests, confirm PASS across all usage tests**

- [ ] **Step 5: Commit**

```bash
git add plugin/internal/ratelimit/usage.go plugin/internal/ratelimit/usage_test.go
git commit -m "feat(plugin/ratelimit): ParseUsageProbe text fallback via regex"
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
