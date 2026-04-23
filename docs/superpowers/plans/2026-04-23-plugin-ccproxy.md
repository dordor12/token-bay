# Plugin `internal/ccproxy` — Feature Implementation Plan (v1)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Implement `plugin/internal/ccproxy` v1 — the Anthropic-compatible HTTPS server on `127.0.0.1` that receives Claude Code's `/v1/messages` traffic when the sidecar has activated mid-session redirect (plugin spec §2.5, §5.4). v1 scope focuses on the HTTP surface, session-mode state, pre-flight auth checks, and pass-through routing. **Network-mode routing is stubbed** — the actual tracker-client + tunnel integration lives in separate feature plans.

**Architecture:** In-process HTTP server. One listener per sidecar instance, dynamic port. Session ID is carried in Claude Code's `X-Claude-Code-Session-Id` request header (seen in `src/services/api/client.ts:108`). Per-session routing state is in memory. Handler picks a `RequestRouter` based on session mode and delegates. v1 ships two routers: `PassThroughRouter` (forwards to Anthropic with original Authorization header byte-for-byte) and `NetworkRouter` (v1 stub returning 501).

**Tech Stack:**
- Go 1.23+
- `net/http` stdlib — HTTP server + reverse-proxy
- `net/http/httputil` — `ReverseProxy` for the pass-through case
- `encoding/json` + `os/exec` — parsing `claude auth status --json`
- `context`, `sync` — concurrency primitives
- `github.com/stretchr/testify`
- Consumes `plugin/internal/ratelimit` (StopFailurePayload + UsageVerdict types for EntryMetadata)

**Prerequisites:** `plugin-v0` scaffolding, `plugin-ratelimit-v0` (for type imports), `plugin-settingsjson-v0` (conceptually — ccproxy's runtime probe will use settingsjson in a later plan, but v1 has no dependency on it).

**Pinned source references:**
- `claude auth status --json` output shape: `src/cli/handlers/auth.ts:232-319` in the upstream Claude Code repo (verified 2026-04-23).
- `X-Claude-Code-Session-Id` header: `src/services/api/client.ts:108`.
- `Usage.tsx`/`fetchUtilization` OAuth data source: `src/services/api/usage.ts` — not directly used here but explains why `authMethod == "claude.ai"` is a precondition.

---

## Table of contents

1. [Design decisions](#1-design-decisions)
2. [Package API](#2-package-api)
3. [File layout](#3-file-layout)
4. [TDD task list](#4-tdd-task-list)
5. [Out-of-scope for v1](#5-out-of-scope-for-v1)
6. [Open questions](#6-open-questions)

---

## 1. Design decisions

### 1.1 Pre-flight: `claude auth status --json`

The sidecar calls `claude auth status --json` once at startup AND before each `EnterNetworkMode` to validate that the user's Claude Code setup is compatible with Token-Bay consumer fallback. Three required conditions:

| Field | Required value | Why |
|---|---|---|
| `loggedIn` | `true` | `/usage` probe and fallback both require auth |
| `apiProvider` | `"firstParty"` | `ANTHROPIC_BASE_URL` redirect (plugin spec §2.5) is ignored under Bedrock/Vertex/Foundry. Redirect would silently fail. |
| `authMethod` | `"claude.ai"` | `/usage` probe requires OAuth auth; the `/api/oauth/usage` endpoint (`src/services/api/usage.ts:34-36`) guards on `isClaudeAISubscriber() && hasProfileScope()`. |

If any of these is not met, the sidecar refuses consumer-role activation with a clear diagnostic. Seeder role is independent; it can still work (seeder's `claude -p` uses whatever auth Claude Code is configured with, even Bedrock).

**`orgId` as identity anchor.** When logged in with `claude.ai`, `orgId` is a stable UUID. Token-Bay hashes it into the identity fingerprint (resolves the plugin spec §4.2 "account binding" open question to option `α`). The `email` is user-visible in the enrollment UX; `subscriptionType` is informational only.

### 1.2 Session ID source

Claude Code's Anthropic SDK client sets `X-Claude-Code-Session-Id: <sessionID>` on every `/v1/messages` call (`src/services/api/client.ts:108`). The ccproxy reads this header as the primary session key. If the header is absent (very old Claude Code versions, or an SDK consumer that didn't set it), the request is treated as an unknown session → routed pass-through.

### 1.3 Session mode state machine

```
  (missing from store)                        ← EnterNetworkMode
         │                                            │
         │  ── session mode = PassThrough ─────► NetworkMode (with EntryMetadata)
         ▲                                            │
         │  ← ExpireAt hit / ExitNetworkMode ─────────┘
```

- `EnterNetworkMode(sessionID, meta)` — sidecar calls this on user-consent after ratelimit verdict.
- `ExitNetworkMode(sessionID)` — sidecar calls this on timer / explicit command.
- `GetMode(sessionID)` — ccproxy handler calls this per-request. Returns `NetworkMode` + meta, or `PassThrough`.
- Entries have an `ExpiresAt` timestamp; `GetMode` treats expired entries as if they were removed (but doesn't remove them — a background reaper handles cleanup).

State is in-memory. On sidecar restart, all network-mode entries are lost and the next user request goes pass-through. That's acceptable — the redirect itself is gone too (settings.json has been restored if the sidecar shut down cleanly; if it crashed, the user's next 429 re-triggers the full flow).

### 1.4 RequestRouter interface + two v1 impls

**Why there's a `PassThroughRouter` at all.** The plugin spec §2.5 redirect mechanism sets `ANTHROPIC_BASE_URL` in `settings.json` — a **process-global** env var that applies to every Anthropic-bound request from every Claude Code process sharing this user's settings file. Three realities force the sidecar to be a cooperating proxy, not just an intercept-and-route:

1. **Non-turn endpoints.** Claude Code hits `/v1/messages/count_tokens`, `/v1/models`, OAuth refreshes, etc. Network-routing them is meaningless (metadata, not turn execution). Without a pass-through, Claude Code's token counter, model picker, and auth refresh break while network mode is active.
2. **Concurrent Claude Code sessions.** A second Claude Code window (session B) in the same user's settings share sees the redirected `ANTHROPIC_BASE_URL` too. Without pass-through, B silently fails the moment session A enters network mode.
3. **Race window at network-mode entry.** Between the `settings.json` write and the sidecar's `SessionModeStore.EnterNetworkMode` call, a request can arrive for a session we haven't registered yet. Pass-through keeps it working; the user's next retry naturally lands in `NetworkRouter`.

Rejected alternatives:
- **Refuse non-registered traffic.** Better credential isolation, terrible UX: user must close other Claude Code windows before fallback works, quota-counting breaks mid-session.
- **Per-session redirect.** Doesn't exist in Claude Code — `ANTHROPIC_BASE_URL` is the only programmatic redirect knob, and it's process-global.

The tradeoff is a real credential-handling concession: `PassThroughRouter` sees `Authorization` header bytes on the wire (we never parse/store/log them, but they transit our process). Plugin spec §2.5 already accepted this implicitly when it chose `ANTHROPIC_BASE_URL` redirect over "relaunch Claude Code" — once we said the sidecar is on the HTTPS path, pass-through was the cost of that architectural choice.

**`PassThroughRouter`**:
- Uses `httputil.ReverseProxy` with a Director that rewrites the target to `https://api.anthropic.com`.
- Copies the original `Authorization` header byte-for-byte. Never parses, logs, or persists it.
- Copies the response (including streaming SSE) verbatim back to Claude Code.
- Invoked for: non-`/v1/messages` endpoints on any session, **and** `/v1/messages` on sessions not in network mode.

**`NetworkRouter`** (v1 stub):
- Returns `501 Not Implemented` with an Anthropic-style error JSON:
  ```json
  {"type": "error", "error": {"type": "not_implemented",
   "message": "Token-Bay network routing not yet implemented in this build (v1 stub)."}}
  ```
- Claude Code surfaces this as a failed turn. The user sees a clear message.
- The real `NetworkRouter` lands in the `internal/ccproxy-network` feature plan (separate), once `shared/proto` envelope types and `internal/tunnel` exist.

### 1.5 HTTP server lifecycle

- Binds to `127.0.0.1:0` (OS-assigned port) unless config specifies otherwise. Port is available via `Server.URL()` so the sidecar can write it into `settings.json`'s `ANTHROPIC_BASE_URL`.
- `Start(ctx)` returns when the listener is ready; the server runs in a goroutine until `ctx` is cancelled or `Close()` is called.
- `/v1/messages` — main handler. Other Anthropic paths (e.g., `/v1/messages/count_tokens`, `/v1/models`) are all routed through `PassThroughRouter` by default for forward-compatibility — the sidecar has no reason to intercept them.
- `/token-bay/health` — JSON `{"status": "ok", "addr": "...", "uptime_sec": N}`. Non-Anthropic path; Claude Code never calls it. Used by the runtime compatibility probe (future feature plan) and for operator debugging.

### 1.6 TLS

The sidecar listens over **plain HTTP** on `127.0.0.1`. No TLS. Rationale: the traffic is loopback-only, the Anthropic SDK client doesn't care about HTTP vs HTTPS as long as it can reach the endpoint, and generating a self-signed cert adds a trust-configuration burden for the user (their Claude Code would reject a self-signed cert unless the plugin injects a trust anchor, which is a much bigger credential-handling intrusion than we want).

`ANTHROPIC_BASE_URL` will therefore be `http://127.0.0.1:<port>`. The Anthropic SDK accepts arbitrary base URLs including `http://`.

### 1.7 What this module does NOT do (v1)

- No actual network-mode routing. `NetworkRouter` is a stub.
- No settings.json mutation. That's the sidecar's job, and it uses `internal/settingsjson`.
- No ratelimit decision. That's `internal/ratelimit`. ccproxy stores a `ratelimit.StopFailurePayload` in its `EntryMetadata` but doesn't make decisions from it.
- No tracker RPC, no exhaustion proof building, no QUIC tunnel. These are later feature plans.
- No TLS. Plain HTTP on loopback.

---

## 2. Package API

```go
// Package ccproxy provides the Anthropic-compatible HTTPS proxy endpoint
// that receives Claude Code's /v1/messages traffic when the Token-Bay
// sidecar has activated mid-session redirect (plugin spec §2.5).
package ccproxy

// ---- Auth precondition ----

// AuthState mirrors `claude auth status --json` output. See
// src/cli/handlers/auth.ts:293-316 in the upstream Claude Code repo.
type AuthState struct {
    LoggedIn         bool   `json:"loggedIn"`
    AuthMethod       string `json:"authMethod"`
    APIProvider      string `json:"apiProvider"`
    APIKeySource     string `json:"apiKeySource,omitempty"`
    Email            string `json:"email,omitempty"`
    OrgID            string `json:"orgId,omitempty"`
    OrgName          string `json:"orgName,omitempty"`
    SubscriptionType string `json:"subscriptionType,omitempty"`
}

// AuthProber runs `claude auth status --json` and returns the parsed
// state. Injectable so tests can stub it.
type AuthProber interface {
    Probe(ctx context.Context) (*AuthState, error)
}

// ClaudeAuthProber is the default AuthProber — shells out to `claude`.
type ClaudeAuthProber struct {
    BinaryPath string        // default: "claude"
    Timeout    time.Duration // default: 5s
}

func NewClaudeAuthProber() *ClaudeAuthProber
func (p *ClaudeAuthProber) Probe(ctx context.Context) (*AuthState, error)

// Compatibility reasons for failing IsCompatible.
const (
    IncompatNotLoggedIn  = "user is not logged into Claude Code (run: claude auth login)"
    IncompatWrongProvider = "apiProvider is not 'firstParty' — Token-Bay cannot redirect under Bedrock/Vertex/Foundry"
    IncompatWrongMethod  = "authMethod is not 'claude.ai' — /usage probe requires OAuth-based Claude AI subscription auth"
)

// IsCompatible returns (true, "") when the auth state supports Token-Bay
// consumer fallback. On incompatibility returns (false, human-readable reason).
func (a *AuthState) IsCompatible() (bool, string)

// ---- Session-mode state ----

// SessionMode classifies a session's current routing.
type SessionMode int

const (
    ModePassThrough SessionMode = iota
    ModeNetwork
)

// EntryMetadata is the per-session state we carry for ModeNetwork sessions.
// The ratelimit fields come from a confirmed rate-limit event; callers
// read them later when assembling an exhaustion proof.
type EntryMetadata struct {
    EnteredAt          time.Time
    ExpiresAt          time.Time
    StopFailurePayload *ratelimit.StopFailurePayload
    UsageProbeBytes    []byte           // raw PTY bytes from `claude /usage`
    UsageVerdict       ratelimit.UsageVerdict
}

// SessionModeStore is a thread-safe in-memory map of session ID → entry.
type SessionModeStore struct {
    // unexported
}

func NewSessionModeStore() *SessionModeStore
func (s *SessionModeStore) EnterNetworkMode(sessionID string, meta EntryMetadata)
func (s *SessionModeStore) ExitNetworkMode(sessionID string) (existed bool)
func (s *SessionModeStore) GetMode(sessionID string) (SessionMode, *EntryMetadata)

// ---- Routing ----

// RequestRouter handles a Claude Code /v1/messages request once the HTTP
// server has classified the session mode.
type RequestRouter interface {
    Route(w http.ResponseWriter, r *http.Request, meta *EntryMetadata)
}

// PassThroughRouter forwards to Anthropic using httputil.ReverseProxy.
type PassThroughRouter struct {
    UpstreamURL *url.URL // defaults to https://api.anthropic.com
}

func NewPassThroughRouter() *PassThroughRouter

// NetworkRouter is a v1 stub. Real implementation lands in a later plan.
type NetworkRouter struct{}

func (n *NetworkRouter) Route(w http.ResponseWriter, r *http.Request, meta *EntryMetadata)

// ---- HTTP server ----

// Server is the ccproxy listener.
type Server struct {
    Addr         string // defaults to "127.0.0.1:0"
    Store        *SessionModeStore
    PassThrough  RequestRouter
    Network      RequestRouter
    Started      time.Time

    // unexported
}

func New(opts ...Option) *Server

// Start begins serving. Returns when the listener is ready; the server
// runs in a goroutine until ctx is cancelled or Close() is called.
func (s *Server) Start(ctx context.Context) error

// URL returns "http://127.0.0.1:<port>/" once the listener is bound.
// Empty string before Start() returns.
func (s *Server) URL() string

// Close initiates graceful shutdown.
func (s *Server) Close() error

// Options for functional configuration at New().
type Option func(*Server)

func WithAddr(addr string) Option
func WithPassThroughRouter(r RequestRouter) Option
func WithNetworkRouter(r RequestRouter) Option
func WithSessionStore(store *SessionModeStore) Option

// sessionIDHeader is what Claude Code's SDK client sets on every call.
// Pinned against src/services/api/client.ts:108 (upstream 2026-04-23).
const sessionIDHeader = "X-Claude-Code-Session-Id"
```

---

## 3. File layout

```
plugin/internal/ccproxy/
├── ccproxy.go             -- package doc + exported constants
├── auth.go                -- AuthState + AuthProber + ClaudeAuthProber
├── auth_test.go
├── compat.go              -- IsCompatible + Incompat* reason constants
├── compat_test.go
├── sessionmode.go         -- SessionMode enum + EntryMetadata + SessionModeStore
├── sessionmode_test.go
├── router.go              -- RequestRouter interface + PassThroughRouter + NetworkRouter stub
├── router_test.go
├── server.go              -- Server struct + Start + URL + Close + options
├── server_test.go
└── testdata/
    └── auth_status_max.json  -- captured real `claude auth status --json` output
```

11 files plus one test fixture. Each file single-responsibility; `compat.go` split from `auth.go` so the pure-logic precondition check is testable in isolation.

---

## 4. TDD task list

### Task 1: Package scaffold + AuthState type + JSON round-trip

**Files:**
- Create: `plugin/internal/ccproxy/ccproxy.go`
- Create: `plugin/internal/ccproxy/auth.go`
- Create: `plugin/internal/ccproxy/auth_test.go`
- Create: `plugin/internal/ccproxy/testdata/auth_status_max.json` (capture a real sample in Step 1)

**Work directory:** `/Users/dor.amid/git/token-bay/plugin/` with `PATH="$HOME/.local/share/mise/shims:$PATH"` prefix on every `go` invocation.

- [ ] **Step 1: Capture a real `claude auth status --json` fixture**

```bash
mkdir -p plugin/internal/ccproxy/testdata
claude auth status --json > plugin/internal/ccproxy/testdata/auth_status_max.json
cat plugin/internal/ccproxy/testdata/auth_status_max.json
```

Expected: structured JSON with `loggedIn: true`, `authMethod`, `apiProvider`, etc. Redact the `email` field before committing if privacy-sensitive.

- [ ] **Step 2: Write failing tests**

Create `plugin/internal/ccproxy/auth_test.go`:

```go
package ccproxy

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuthState_UnmarshalFullFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/auth_status_max.json")
	require.NoError(t, err, "regenerate fixture: claude auth status --json > testdata/auth_status_max.json")

	var state AuthState
	require.NoError(t, json.Unmarshal(data, &state))

	assert.True(t, state.LoggedIn)
	assert.NotEmpty(t, state.AuthMethod)
	assert.NotEmpty(t, state.APIProvider)
}

func TestAuthState_UnmarshalMinimalNotLoggedIn(t *testing.T) {
	data := []byte(`{"loggedIn": false, "authMethod": "none", "apiProvider": "firstParty"}`)

	var state AuthState
	require.NoError(t, json.Unmarshal(data, &state))

	assert.False(t, state.LoggedIn)
	assert.Equal(t, "none", state.AuthMethod)
	assert.Empty(t, state.Email)
	assert.Empty(t, state.OrgID)
}
```

- [ ] **Step 3: Run tests, confirm FAIL**

Expected: `undefined: AuthState`.

- [ ] **Step 4: Implement `AuthState`**

Create `plugin/internal/ccproxy/ccproxy.go`:

```go
// Package ccproxy provides the Anthropic-compatible HTTPS proxy endpoint
// that receives Claude Code's /v1/messages traffic when the Token-Bay
// sidecar has activated mid-session redirect (plugin spec §2.5).
//
// v1 scope:
//   - HTTP server on 127.0.0.1 with /v1/messages and /token-bay/health
//   - Session-mode state store (in-memory)
//   - Pre-flight auth check via `claude auth status --json`
//   - PassThroughRouter (forwards to Anthropic byte-for-byte)
//   - NetworkRouter stub (returns 501)
//
// Real network-mode routing lands in a subsequent feature plan that
// integrates shared/proto (envelope types) and internal/tunnel.
package ccproxy

// sessionIDHeader is the header Claude Code's Anthropic SDK client sets
// on every /v1/messages call. Pinned against
// src/services/api/client.ts:108 in the upstream Claude Code repo.
const sessionIDHeader = "X-Claude-Code-Session-Id"
```

Create `plugin/internal/ccproxy/auth.go`:

```go
package ccproxy

// AuthState mirrors the JSON output of `claude auth status --json`.
// See src/cli/handlers/auth.ts:293-316 upstream for the authoritative
// field list.
type AuthState struct {
	LoggedIn         bool   `json:"loggedIn"`
	AuthMethod       string `json:"authMethod"`
	APIProvider      string `json:"apiProvider"`
	APIKeySource     string `json:"apiKeySource,omitempty"`
	Email            string `json:"email,omitempty"`
	OrgID            string `json:"orgId,omitempty"`
	OrgName          string `json:"orgName,omitempty"`
	SubscriptionType string `json:"subscriptionType,omitempty"`
}
```

- [ ] **Step 5: Run tests, confirm PASS**

- [ ] **Step 6: Commit**

```bash
cd /Users/dor.amid/git/token-bay
git add plugin/internal/ccproxy/ccproxy.go plugin/internal/ccproxy/auth.go plugin/internal/ccproxy/auth_test.go plugin/internal/ccproxy/testdata/
git rm plugin/internal/ccproxy/.gitkeep
git commit -m "feat(plugin/ccproxy): scaffold package + AuthState JSON round-trip"
```

### Task 2: `ClaudeAuthProber` — shells out to `claude auth status --json`

**Files:**
- Modify: `plugin/internal/ccproxy/auth.go`
- Modify: `plugin/internal/ccproxy/auth_test.go`

- [ ] **Step 1: Add tests for `ClaudeAuthProber`**

Append to `auth_test.go`:

```go
import (
	"context"
	"os/exec"
	"time"
)

func TestClaudeAuthProber_Defaults(t *testing.T) {
	p := NewClaudeAuthProber()
	assert.Equal(t, "claude", p.BinaryPath)
	assert.Equal(t, 5*time.Second, p.Timeout)
}

func TestClaudeAuthProber_MissingBinary_ReturnsError(t *testing.T) {
	p := NewClaudeAuthProber()
	p.BinaryPath = "/definitely/does/not/exist/claude"

	_, err := p.Probe(context.Background())
	assert.Error(t, err)
}

// AuthProber interface compliance at compile time.
var _ AuthProber = (*ClaudeAuthProber)(nil)

func TestClaudeAuthProber_LiveSmokeTest(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude not on PATH; skipping live smoke")
	}
	p := NewClaudeAuthProber()

	state, err := p.Probe(context.Background())
	// Exit code 1 from `claude auth status` (not logged in) is reflected
	// in state.LoggedIn, not as a Probe error.
	require.NoError(t, err, "probe should parse output regardless of login state")
	require.NotNil(t, state)
	assert.NotEmpty(t, state.AuthMethod, "authMethod should be populated")
}
```

- [ ] **Step 2: Run, confirm FAIL** on `undefined: NewClaudeAuthProber`, `undefined: AuthProber`.

- [ ] **Step 3: Implement**

Append to `auth.go`:

```go
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// AuthProber runs `claude auth status --json` and returns the parsed state.
// Injectable so tests can substitute a stub.
type AuthProber interface {
	Probe(ctx context.Context) (*AuthState, error)
}

// ClaudeAuthProber shells out to the real claude binary.
type ClaudeAuthProber struct {
	BinaryPath string
	Timeout    time.Duration
}

// NewClaudeAuthProber returns a prober with defaults.
func NewClaudeAuthProber() *ClaudeAuthProber {
	return &ClaudeAuthProber{
		BinaryPath: "claude",
		Timeout:    5 * time.Second,
	}
}

// Probe runs `claude auth status --json` and parses stdout. Non-zero
// exit codes (user not logged in) are expected — they reflect in
// AuthState.LoggedIn, not as a Probe error. A Probe error is returned
// only when the command can't run or its stdout isn't valid JSON.
func (p *ClaudeAuthProber) Probe(ctx context.Context) (*AuthState, error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.BinaryPath, "auth", "status", "--json")
	out, err := cmd.Output()
	// `claude auth status --json` exits 1 when not logged in; its stdout
	// is still valid JSON with loggedIn:false. We tolerate that exit code.
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return nil, fmt.Errorf("ccproxy: run %s auth status: %w", p.BinaryPath, err)
	}

	var state AuthState
	if jerr := json.Unmarshal(out, &state); jerr != nil {
		return nil, fmt.Errorf("ccproxy: parse auth status JSON: %w", jerr)
	}
	return &state, nil
}
```

- [ ] **Step 4: Run, confirm PASS** (live smoke passes if `claude` is installed; skipped otherwise).

- [ ] **Step 5: Commit**

```bash
git add plugin/internal/ccproxy/auth.go plugin/internal/ccproxy/auth_test.go
git commit -m "feat(plugin/ccproxy): ClaudeAuthProber — parses claude auth status --json"
```

### Task 3: `IsCompatible` precondition check

**Files:**
- Create: `plugin/internal/ccproxy/compat.go`
- Create: `plugin/internal/ccproxy/compat_test.go`

- [ ] **Step 1: Write failing tests**

`compat_test.go`:

```go
package ccproxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsCompatible_HappyPath(t *testing.T) {
	s := &AuthState{LoggedIn: true, AuthMethod: "claude.ai", APIProvider: "firstParty"}
	ok, reason := s.IsCompatible()
	assert.True(t, ok)
	assert.Empty(t, reason)
}

func TestIsCompatible_NotLoggedIn(t *testing.T) {
	s := &AuthState{LoggedIn: false, AuthMethod: "none", APIProvider: "firstParty"}
	ok, reason := s.IsCompatible()
	assert.False(t, ok)
	assert.Equal(t, IncompatNotLoggedIn, reason)
}

func TestIsCompatible_Bedrock(t *testing.T) {
	s := &AuthState{LoggedIn: true, AuthMethod: "claude.ai", APIProvider: "bedrock"}
	ok, reason := s.IsCompatible()
	assert.False(t, ok)
	assert.Equal(t, IncompatWrongProvider, reason)
}

func TestIsCompatible_APIKey(t *testing.T) {
	// Logged in, firstParty, but auth method is raw api_key — no /usage access.
	s := &AuthState{LoggedIn: true, AuthMethod: "api_key", APIProvider: "firstParty"}
	ok, reason := s.IsCompatible()
	assert.False(t, ok)
	assert.Equal(t, IncompatWrongMethod, reason)
}
```

- [ ] **Step 2: Run, confirm FAIL**.

- [ ] **Step 3: Implement**

Create `compat.go`:

```go
package ccproxy

// Incompatibility reasons returned by IsCompatible. Exported so callers
// can switch on them and format matching UX messages.
const (
	IncompatNotLoggedIn   = "user is not logged into Claude Code (run: claude auth login)"
	IncompatWrongProvider = "apiProvider is not 'firstParty' — Token-Bay cannot redirect under Bedrock/Vertex/Foundry"
	IncompatWrongMethod   = "authMethod is not 'claude.ai' — /usage probe requires OAuth-based Claude AI subscription auth"
)

// IsCompatible checks whether the user's auth state permits Token-Bay
// consumer fallback. See design §1.1 for why each condition matters.
func (a *AuthState) IsCompatible() (bool, string) {
	if !a.LoggedIn {
		return false, IncompatNotLoggedIn
	}
	if a.APIProvider != "firstParty" {
		return false, IncompatWrongProvider
	}
	if a.AuthMethod != "claude.ai" {
		return false, IncompatWrongMethod
	}
	return true, ""
}
```

- [ ] **Step 4: Run, confirm PASS**.

- [ ] **Step 5: Commit**

```bash
git add plugin/internal/ccproxy/compat.go plugin/internal/ccproxy/compat_test.go
git commit -m "feat(plugin/ccproxy): IsCompatible precondition check"
```

### Task 4: `SessionModeStore`

**Files:**
- Create: `plugin/internal/ccproxy/sessionmode.go`
- Create: `plugin/internal/ccproxy/sessionmode_test.go`

- [ ] **Step 1: Write failing tests**

Coverage targets: Get on empty store → PassThrough, Enter+Get returns Network+meta, Enter twice overwrites, Exit returns existed, expiry treated as PassThrough, concurrent Enter/Get.

```go
package ccproxy

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionModeStore_EmptyGet_ReturnsPassThrough(t *testing.T) {
	s := NewSessionModeStore()
	mode, meta := s.GetMode("session-x")
	assert.Equal(t, ModePassThrough, mode)
	assert.Nil(t, meta)
}

func TestSessionModeStore_EnterThenGet_ReturnsNetwork(t *testing.T) {
	s := NewSessionModeStore()
	meta := EntryMetadata{
		EnteredAt: time.Now(),
		ExpiresAt: time.Now().Add(15 * time.Minute),
	}
	s.EnterNetworkMode("session-x", meta)

	mode, got := s.GetMode("session-x")
	require.Equal(t, ModeNetwork, mode)
	require.NotNil(t, got)
	assert.Equal(t, meta.ExpiresAt, got.ExpiresAt)
}

func TestSessionModeStore_Expired_ReturnsPassThrough(t *testing.T) {
	s := NewSessionModeStore()
	s.EnterNetworkMode("session-x", EntryMetadata{
		EnteredAt: time.Now().Add(-20 * time.Minute),
		ExpiresAt: time.Now().Add(-5 * time.Minute),
	})
	mode, _ := s.GetMode("session-x")
	assert.Equal(t, ModePassThrough, mode)
}

func TestSessionModeStore_Exit_Existing_ReturnsTrue(t *testing.T) {
	s := NewSessionModeStore()
	s.EnterNetworkMode("s", EntryMetadata{ExpiresAt: time.Now().Add(time.Minute)})
	assert.True(t, s.ExitNetworkMode("s"))
	assert.False(t, s.ExitNetworkMode("s")) // idempotent
}

func TestSessionModeStore_Exit_Missing_ReturnsFalse(t *testing.T) {
	s := NewSessionModeStore()
	assert.False(t, s.ExitNetworkMode("never-entered"))
}

func TestSessionModeStore_Concurrent_Safe(t *testing.T) {
	s := NewSessionModeStore()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sid := "s" + string(rune('a'+i%26))
			s.EnterNetworkMode(sid, EntryMetadata{ExpiresAt: time.Now().Add(time.Minute)})
			_, _ = s.GetMode(sid)
			s.ExitNetworkMode(sid)
		}(i)
	}
	wg.Wait()
	// No panic / data race — test passes under -race if we got here.
}
```

- [ ] **Step 2: Run, confirm FAIL**.

- [ ] **Step 3: Implement**

```go
package ccproxy

import (
	"sync"
	"time"

	"github.com/token-bay/token-bay/plugin/internal/ratelimit"
)

// SessionMode classifies the routing path for a given session.
type SessionMode int

const (
	ModePassThrough SessionMode = iota
	ModeNetwork
)

// EntryMetadata is the per-session state carried for ModeNetwork sessions.
type EntryMetadata struct {
	EnteredAt          time.Time
	ExpiresAt          time.Time
	StopFailurePayload *ratelimit.StopFailurePayload
	UsageProbeBytes    []byte
	UsageVerdict       ratelimit.UsageVerdict
}

// SessionModeStore is a thread-safe in-memory map of session ID → entry.
type SessionModeStore struct {
	mu      sync.RWMutex
	entries map[string]*EntryMetadata
}

// NewSessionModeStore returns an empty store.
func NewSessionModeStore() *SessionModeStore {
	return &SessionModeStore{entries: make(map[string]*EntryMetadata)}
}

// EnterNetworkMode records meta for sessionID. Replaces any prior entry.
func (s *SessionModeStore) EnterNetworkMode(sessionID string, meta EntryMetadata) {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := meta
	s.entries[sessionID] = &copy
}

// ExitNetworkMode removes the entry for sessionID. Returns true if an
// entry was present.
func (s *SessionModeStore) ExitNetworkMode(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.entries[sessionID]
	if ok {
		delete(s.entries, sessionID)
	}
	return ok
}

// GetMode returns the current routing mode for sessionID. An expired
// entry (ExpiresAt < now) is reported as PassThrough; the stale entry
// remains in the map until a background reaper removes it (not in v1;
// sidecar orchestrator calls ExitNetworkMode on timer).
func (s *SessionModeStore) GetMode(sessionID string) (SessionMode, *EntryMetadata) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.entries[sessionID]
	if !ok {
		return ModePassThrough, nil
	}
	if !entry.ExpiresAt.IsZero() && time.Now().After(entry.ExpiresAt) {
		return ModePassThrough, nil
	}
	// Return a copy so callers can't mutate internal state.
	copy := *entry
	return ModeNetwork, &copy
}
```

- [ ] **Step 4: Run, confirm PASS** under `-race`.

- [ ] **Step 5: Commit**

```bash
git add plugin/internal/ccproxy/sessionmode.go plugin/internal/ccproxy/sessionmode_test.go
git commit -m "feat(plugin/ccproxy): SessionModeStore — thread-safe per-session state"
```

### Task 5: `PassThroughRouter` + `NetworkRouter` stub

**Files:**
- Create: `plugin/internal/ccproxy/router.go`
- Create: `plugin/internal/ccproxy/router_test.go`

- [ ] **Step 1: Write failing tests**

Coverage targets: pass-through forwards request body + preserves Authorization header + streams response body back; NetworkRouter returns 501 with Anthropic-style JSON error.

Test uses `httptest.NewServer` as a mock Anthropic upstream. Pass-through is given that upstream's URL; a GET to the test server's `/v1/messages` should reach the mock and the response should appear in the recorder.

```go
package ccproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPassThroughRouter_ForwardsRequestAndResponse(t *testing.T) {
	// Mock upstream: echoes method+path and the Authorization header.
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"method":"`+r.Method+`","path":"`+r.URL.Path+`"}`)
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	router := &PassThroughRouter{UpstreamURL: u}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"x"}`))
	req.Header.Set("Authorization", "Bearer secret-token-xyz")
	rec := httptest.NewRecorder()

	router.Route(rec, req, nil)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"path":"/v1/messages"`)
	assert.Equal(t, "Bearer secret-token-xyz", gotAuth)
}

func TestNetworkRouter_Returns501_WithAnthropicErrorJSON(t *testing.T) {
	r := &NetworkRouter{}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()

	r.Route(rec, req, &EntryMetadata{})

	assert.Equal(t, http.StatusNotImplemented, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `"type":"error"`)
	assert.Contains(t, body, `"not_implemented"`)
}

// Compile-time interface checks.
var (
	_ RequestRouter = (*PassThroughRouter)(nil)
	_ RequestRouter = (*NetworkRouter)(nil)
)

func TestNewPassThroughRouter_DefaultUpstreamIsAnthropic(t *testing.T) {
	r := NewPassThroughRouter()
	require.NotNil(t, r.UpstreamURL)
	assert.Equal(t, "https", r.UpstreamURL.Scheme)
	assert.Equal(t, "api.anthropic.com", r.UpstreamURL.Host)
}
```

- [ ] **Step 2: Run, confirm FAIL**.

- [ ] **Step 3: Implement**

```go
package ccproxy

import (
	"net/http"
	"net/http/httputil"
	"net/url"
)

// RequestRouter handles a classified request. Server resolves session
// mode → router → Route.
type RequestRouter interface {
	Route(w http.ResponseWriter, r *http.Request, meta *EntryMetadata)
}

// PassThroughRouter forwards to Anthropic via httputil.ReverseProxy.
// Original headers (including Authorization) are copied byte-for-byte.
type PassThroughRouter struct {
	UpstreamURL *url.URL
	proxy       *httputil.ReverseProxy
}

// NewPassThroughRouter returns a router targeting api.anthropic.com.
func NewPassThroughRouter() *PassThroughRouter {
	u, _ := url.Parse("https://api.anthropic.com")
	return &PassThroughRouter{UpstreamURL: u}
}

// Route forwards r to the upstream and writes the response to w.
func (p *PassThroughRouter) Route(w http.ResponseWriter, r *http.Request, _ *EntryMetadata) {
	if p.proxy == nil {
		p.proxy = httputil.NewSingleHostReverseProxy(p.UpstreamURL)
	}
	// Rewrite Host so Anthropic's TLS cert matches.
	r.Host = p.UpstreamURL.Host
	p.proxy.ServeHTTP(w, r)
}

// NetworkRouter is the v1 stub. Real implementation lands in a later
// feature plan (ccproxy-network) once shared/proto envelopes and
// internal/tunnel exist.
type NetworkRouter struct{}

// Route returns a 501 with an Anthropic-style error payload so Claude
// Code surfaces the failure as a normal API error.
func (n *NetworkRouter) Route(w http.ResponseWriter, _ *http.Request, _ *EntryMetadata) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotImplemented)
	_, _ = w.Write([]byte(`{"type":"error","error":{"type":"not_implemented",` +
		`"message":"Token-Bay network routing not yet implemented in this build (v1 stub)."}}`))
}
```

- [ ] **Step 4: Run, confirm PASS**.

- [ ] **Step 5: Commit**

```bash
git add plugin/internal/ccproxy/router.go plugin/internal/ccproxy/router_test.go
git commit -m "feat(plugin/ccproxy): PassThroughRouter + NetworkRouter v1 stub"
```

### Task 6: HTTP server + `/v1/messages` + `/token-bay/health`

**Files:**
- Create: `plugin/internal/ccproxy/server.go`
- Create: `plugin/internal/ccproxy/server_test.go`

- [ ] **Step 1: Write failing tests**

Coverage targets:
- Start binds to 127.0.0.1:0 and URL() returns the resolved port.
- Unknown session → PassThrough (mocked router records it).
- Known session-in-network-mode → NetworkRouter (mocked router records it).
- `/token-bay/health` returns `{"status":"ok", ...}`.
- Close() shuts down gracefully.
- Start then Close without ever serving a request succeeds cleanly.

Use a tiny stub `recordingRouter` to observe which router was invoked.

```go
package ccproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingRouter struct {
	mu     sync.Mutex
	called int
	status int
	body   string
}

func (r *recordingRouter) Route(w http.ResponseWriter, req *http.Request, _ *EntryMetadata) {
	r.mu.Lock()
	r.called++
	r.mu.Unlock()
	w.WriteHeader(r.status)
	_, _ = w.Write([]byte(r.body))
}

func newServerForTest(t *testing.T) (*Server, *recordingRouter, *recordingRouter) {
	t.Helper()
	pass := &recordingRouter{status: http.StatusOK, body: "from-pass"}
	net := &recordingRouter{status: http.StatusOK, body: "from-net"}
	s := New(
		WithAddr("127.0.0.1:0"),
		WithPassThroughRouter(pass),
		WithNetworkRouter(net),
	)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = s.Close()
	})
	require.NoError(t, s.Start(ctx))
	return s, pass, net
}

func TestServer_StartBindsDynamicPort(t *testing.T) {
	s, _, _ := newServerForTest(t)
	assert.NotEmpty(t, s.URL())
	assert.Contains(t, s.URL(), "127.0.0.1")
}

func TestServer_UnknownSession_RoutesPassThrough(t *testing.T) {
	s, pass, net := newServerForTest(t)
	req, _ := http.NewRequest(http.MethodPost, s.URL()+"v1/messages", strings.NewReader(`{"model":"x"}`))
	req.Header.Set("X-Claude-Code-Session-Id", "unknown-session")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "from-pass", string(body))
	assert.Equal(t, 1, pass.called)
	assert.Equal(t, 0, net.called)
}

func TestServer_NetworkModeSession_RoutesNetwork(t *testing.T) {
	s, pass, net := newServerForTest(t)
	s.Store.EnterNetworkMode("net-session", EntryMetadata{
		EnteredAt: time.Now(),
		ExpiresAt: time.Now().Add(10 * time.Minute),
	})

	req, _ := http.NewRequest(http.MethodPost, s.URL()+"v1/messages", strings.NewReader(`{}`))
	req.Header.Set("X-Claude-Code-Session-Id", "net-session")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	assert.Equal(t, "from-net", string(body))
	assert.Equal(t, 1, net.called)
	assert.Equal(t, 0, pass.called)
}

func TestServer_MissingSessionHeader_RoutesPassThrough(t *testing.T) {
	s, pass, _ := newServerForTest(t)
	resp, err := http.Post(s.URL()+"v1/messages", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 1, pass.called)
}

func TestServer_HealthEndpoint_ReturnsOK(t *testing.T) {
	s, _, _ := newServerForTest(t)
	resp, err := http.Get(s.URL() + "token-bay/health")
	require.NoError(t, err)
	defer resp.Body.Close()

	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, "ok", body["status"])
	assert.Contains(t, body["addr"], "127.0.0.1")
}

func TestServer_CleanClose_NoError(t *testing.T) {
	s := New(WithAddr("127.0.0.1:0"))
	require.NoError(t, s.Start(context.Background()))
	assert.NoError(t, s.Close())
}
```

- [ ] **Step 2: Run, confirm FAIL**.

- [ ] **Step 3: Implement**

```go
package ccproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// Server is the ccproxy HTTP listener.
type Server struct {
	Addr        string
	Store       *SessionModeStore
	PassThrough RequestRouter
	Network     RequestRouter
	Started     time.Time

	mu       sync.Mutex
	listener net.Listener
	srv      *http.Server
	resolvedAddr string
}

// Option is a functional configuration option.
type Option func(*Server)

// WithAddr overrides the default bind address.
func WithAddr(addr string) Option {
	return func(s *Server) { s.Addr = addr }
}

// WithPassThroughRouter injects a router (primarily for tests).
func WithPassThroughRouter(r RequestRouter) Option {
	return func(s *Server) { s.PassThrough = r }
}

// WithNetworkRouter injects a router (primarily for tests).
func WithNetworkRouter(r RequestRouter) Option {
	return func(s *Server) { s.Network = r }
}

// WithSessionStore overrides the default SessionModeStore.
func WithSessionStore(store *SessionModeStore) Option {
	return func(s *Server) { s.Store = store }
}

// New constructs a Server with defaults and applies the given options.
func New(opts ...Option) *Server {
	s := &Server{
		Addr:        "127.0.0.1:0",
		Store:       NewSessionModeStore(),
		PassThrough: NewPassThroughRouter(),
		Network:     &NetworkRouter{},
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Start binds the listener and begins serving. Returns once ready.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("ccproxy: listen %s: %w", s.Addr, err)
	}
	s.listener = ln
	s.resolvedAddr = ln.Addr().String()

	mux := http.NewServeMux()
	mux.HandleFunc("/token-bay/health", s.handleHealth)
	mux.HandleFunc("/", s.handleAnthropic)

	s.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	s.Started = time.Now()

	go func() { _ = s.srv.Serve(ln) }()

	go func() {
		<-ctx.Done()
		_ = s.Close()
	}()

	return nil
}

// URL returns the base URL including the resolved port.
func (s *Server) URL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resolvedAddr == "" {
		return ""
	}
	return "http://" + s.resolvedAddr + "/"
}

// Close shuts the server down.
func (s *Server) Close() error {
	s.mu.Lock()
	srv := s.srv
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
}

// handleAnthropic routes Anthropic-API-shaped requests (anything not
// /token-bay/*). Resolves the session mode and delegates to the router.
func (s *Server) handleAnthropic(w http.ResponseWriter, r *http.Request) {
	sessionID := r.Header.Get(sessionIDHeader)
	mode, meta := s.Store.GetMode(sessionID)
	switch mode {
	case ModeNetwork:
		s.Network.Route(w, r, meta)
	default:
		s.PassThrough.Route(w, r, nil)
	}
}

// handleHealth returns a small JSON status blob. Used by the runtime
// compatibility probe (future feature) and for operator debugging.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	uptime := time.Since(s.Started).Seconds()
	payload := map[string]any{
		"status":     "ok",
		"addr":       s.resolvedAddr,
		"uptime_sec": uptime,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}
```

- [ ] **Step 4: Run, confirm PASS** under `-race`.

- [ ] **Step 5: Commit**

```bash
git add plugin/internal/ccproxy/server.go plugin/internal/ccproxy/server_test.go
git commit -m "feat(plugin/ccproxy): HTTP server with session-mode routing + health"
```

### Task 7: Coverage + tag + push

- [ ] **Step 1: Full module test + coverage**

```bash
cd /Users/dor.amid/git/token-bay/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -coverprofile=coverage.out ./internal/ccproxy/...
PATH="$HOME/.local/share/mise/shims:$PATH" go tool cover -func=coverage.out | tail -12
```

Expected ≥ 80%. `ClaudeAuthProber.Probe` may be lower (hard to fully cover error branches without mocking `os/exec`); acceptable.

- [ ] **Step 2: Root `make check`**

```bash
cd /Users/dor.amid/git/token-bay && PATH="$HOME/.local/share/mise/shims:$PATH" make test
```

- [ ] **Step 3: Tag + push**

```bash
git tag -a plugin-ccproxy-v0 -m "internal/ccproxy v1 (auth check + session store + pass-through + stub network router)"
git push origin main --tags
```

---

## 5. Out-of-scope for v1

These will ship in subsequent feature plans:

- **Real `NetworkRouter`** — builds exhaustion-proof from EntryMetadata, constructs broker envelope (requires `shared/proto`), calls `TrackerClient`, opens QUIC tunnel (requires `internal/tunnel`), streams response back. This is the heaviest module in the plugin; it deserves its own plan once dependencies land.
- **Runtime compatibility probe** — writes a canary key to settings.json, waits for Claude Code to reflect it via a request hitting `/token-bay/health`, cleans up. Lives in a later plan (likely sibling to ccproxy).
- **Envelope building from EntryMetadata** — parks in `internal/exhaustionproofbuilder` + `internal/envelopebuilder` (per plugin spec §3 module layout).
- **Auth state caching** — for now the sidecar calls `Probe` each time it needs auth info. A small in-memory cache with 60s TTL will come with the sidecar orchestrator plan.

## 6. Open questions

- **Multi-Claude-Code users.** If two Claude Code processes are running, both point at the same ccproxy (same `ANTHROPIC_BASE_URL` in settings.json). Both send `X-Claude-Code-Session-Id`; the session IDs distinguish them, so the routing just works. But the redirect *activation* is tricky — entering network mode for session A also redirects session B's traffic. Acceptable for v1; document in plugin spec §10.
- **Upstream URL override** for `PassThroughRouter` — e.g., for testing or if the user has a legitimate Anthropic proxy of their own. v1 hardcodes `https://api.anthropic.com`; caller-injectable via `WithPassThroughRouter(&PassThroughRouter{UpstreamURL: ...})`. Worth exposing as a config field in a future iteration.
- **`claude auth status` subcommand stability.** We depend on the command existing and its JSON schema. If Claude Code renames it or changes fields, our `ClaudeAuthProber` breaks. Pin the source reference (already in §1 above) so divergence is easy to spot.
- **Pass-through streaming.** The default `httputil.ReverseProxy` handles SSE streaming correctly (it doesn't buffer). Verified empirically for typical Anthropic responses but worth a dedicated streaming test in a future iteration.

---

## Self-review

- **Spec coverage:** plugin spec §5.1 (trigger path — where ccproxy receives the redirected request), §5.4 (request lifecycle in network mode — ccproxy's role), §2.5 (the redirect mechanism — ccproxy is the endpoint the redirect points to), §10 (resolves the response-injection question — no injection, ccproxy is the native endpoint).
- **Placeholder scan:** one intentional `501 Not Implemented` stub in `NetworkRouter`. Stub is honest — it explicitly tells the user real routing is not in this build. No hidden TBDs.
- **Type consistency:** `SessionMode`, `EntryMetadata`, `RequestRouter`, `AuthState` all use the same names across tasks. EntryMetadata imports `ratelimit.StopFailurePayload` and `ratelimit.UsageVerdict` — compile-time dependency verified.
- **TDD discipline:** each task is red → green → commit. No task skips the red phase. Task 2 and Task 6 have the most failure-branch coverage to balance the integration surface.

## Next plan

After `ccproxy-v1` lands, the natural next steps are a tight cluster:

1. **`shared/proto` envelope types** — the broker-request `Envelope` struct with canonical serialization. Small.
2. **`shared/exhaustionproof.ProofV1`** — the two-signal bundle from ratelimit + StopFailure. Small.
3. **`plugin/internal/envelopebuilder`** — assembles `Envelope` from `ccproxy.EntryMetadata` + `shared/proto.Envelope`. Small.
4. **`plugin/internal/trackerclient` (skeleton)** — QUIC client + RPC codec. Bigger.
5. **`plugin/internal/ccproxy-network`** — real `NetworkRouter` using all of the above. Stitches everything together.

The ordering is reversed from what you'd normally imagine because the types (1, 2) unblock the builder (3), which unblocks the tracker client (4), which unblocks the real router (5). Each is a separate feature plan.
