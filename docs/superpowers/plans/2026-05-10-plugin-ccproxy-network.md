# Plugin ccproxy NetworkRouter Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the v1 stub `NetworkRouter` (HTTP 501) in `plugin/internal/ccproxy/router.go` with a real implementation that dials a peer seeder over `internal/tunnel`, ships a length-prefixed `/v1/messages` body, reads the status byte, and `io.Copy`'s SSE bytes from the tunnel into Claude Code's open HTTP response. Add the `SeederAddr`/`SeederPubkey`/`EphemeralPriv` fields to `EntryMetadata` so the future StopFailure-hook orchestrator (separate plan) can stage assignments.

**Architecture:** A `PeerDialer` interface keeps `NetworkRouter` decoupled from `internal/tunnel` so unit tests can drive every error branch with a fake. The production `tunnelDialer` (in its own file) wraps `tunnel.Dial`, reading `EphemeralPriv`+`SeederPubkey`+`SeederAddr` from `EntryMetadata`. A small cross-cutting change exports `tunnel.StatusOK`/`tunnel.StatusError` (renamed from the existing private `status` type) so the dialer adapter can discriminate without magic bytes. Failures map to Anthropic-shaped JSON error bodies; success sets `Content-Type: text/event-stream` and streams bytes verbatim.

**Tech Stack:** Go 1.25 stdlib (`context`, `crypto/ed25519`, `encoding/json`, `errors`, `fmt`, `io`, `net/http`, `net/netip`, `time`); `github.com/quic-go/quic-go` (transitively, via `internal/tunnel`); `github.com/stretchr/testify` (tests); `github.com/rs/zerolog` (already a plugin runtime dep). No new third-party deps.

**Spec:** [`docs/superpowers/specs/plugin/2026-05-10-ccproxy-network-design.md`](../specs/plugin/2026-05-10-ccproxy-network-design.md). Parents: plugin design §5.4, architecture §10.3, tunnel plan `2026-05-03-plugin-tunnel.md`.

---

## 1. File map

Created in this plan:

| Path | Purpose |
|---|---|
| `plugin/internal/ccproxy/network_dialer.go` | Production `tunnelDialer` + `tunnelPeerConn` wrapping `tunnel.Dial` |
| `plugin/internal/ccproxy/network_dialer_test.go` | E2E test against a real `tunnel.Listener` with canned SSE bytes |

Modified:

| Path | Change |
|---|---|
| `plugin/internal/tunnel/frame.go` | Export `Status`, `StatusOK`, `StatusError` (rename of private `status`/`statusOK`/`statusError`) |
| `plugin/internal/tunnel/frame_test.go` | Update internal references to the renamed identifiers |
| `plugin/internal/tunnel/tunnel.go` | Update internal references to the renamed identifiers |
| `plugin/internal/tunnel/listen_test.go` | Update internal references (if it touches `statusOK`) |
| `plugin/internal/tunnel/e2e_test.go` | Update internal reference to `statusOK` |
| `plugin/internal/ccproxy/router.go` | Replace `NetworkRouter` stub with real `Route`; add `PeerDialer`/`PeerConn` interfaces |
| `plugin/internal/ccproxy/router_test.go` | Replace 501-stub test with table-driven branch coverage using a fake `PeerDialer` |
| `plugin/internal/ccproxy/sessionmode.go` | Add `SeederAddr`/`SeederPubkey`/`EphemeralPriv` fields to `EntryMetadata` |
| `plugin/internal/ccproxy/sessionmode_test.go` | Round-trip the new fields through `EnterNetworkMode`/`GetMode` |
| `plugin/internal/ccproxy/server.go` | Update default `New` to construct a real `NetworkRouter` instead of `&NetworkRouter{}` |

No `make localintegtest` dependency.

---

## 2. Conventions used in this plan

- Worktree root: `/Users/dor.amid/.superset/worktrees/token-bay/plugin-sse`. Go module at `plugin/`. All `go test`/`go build` commands run from `/Users/dor.amid/.superset/worktrees/token-bay/plugin-sse/plugin`.
- `PATH="$HOME/.local/share/mise/shims:$PATH"` is required when Go is managed by mise. All shell commands include it.
- One commit per task. Conventional-commit prefixes: `refactor(plugin/tunnel):` for the rename, `feat(plugin/ccproxy):` for the router, `test(plugin/ccproxy):` for tests. Co-Authored-By footer on every commit.
- Module path: `github.com/token-bay/token-bay/plugin`.
- TDD discipline: for the router work, failing test → minimal impl → passing test → commit. Task 1 (tunnel rename) is a refactor with semantically-preserving rewrite; tests must stay green throughout.
- The router never imports `internal/tunnel` directly — only `network_dialer.go` does. Tests that need the tunnel for E2E import it under the `_test.go` build constraint.

---

## Task 1: Export `tunnel.Status`/`tunnel.StatusOK`/`tunnel.StatusError`

A semantically-preserving rename of the private `status` type and its two constants. No wire change; no test breakage beyond rename propagation.

**Files:**
- Modify: `plugin/internal/tunnel/frame.go`
- Modify: `plugin/internal/tunnel/frame_test.go`
- Modify: `plugin/internal/tunnel/tunnel.go`
- Modify: `plugin/internal/tunnel/e2e_test.go`
- Possibly modify: `plugin/internal/tunnel/listen_test.go`, `plugin/internal/tunnel/dial_test.go`, `plugin/internal/tunnel/tunnel_test.go` (anywhere `statusOK`/`statusError`/`status` appears)

- [ ] **Step 1: Inventory all references**

Run: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse && grep -rn "\bstatus\(OK\|Error\)\?\b" plugin/internal/tunnel/ | head -30`
Expected: a small, finite list. Note every file/line.

- [ ] **Step 2: Rename in `plugin/internal/tunnel/frame.go`**

Edit `plugin/internal/tunnel/frame.go`:

Replace the private type/const block:

```go
// status enum on the seeder→consumer half of the bidi stream.
type status byte

const (
	statusOK    status = 0x00
	statusError status = 0x01
)
```

with the exported version:

```go
// Status is the one-byte response framing on the seeder→consumer half
// of the bidi stream. Callers outside this package (notably the
// consumer-side ccproxy NetworkRouter) discriminate on these values
// without re-encoding the wire bytes.
type Status byte

// Status enum values.
const (
	StatusOK    Status = 0x00
	StatusError Status = 0x01
)
```

In the same file, replace every other `status` (the type) with `Status` and every `statusOK`/`statusError` (the consts) with `StatusOK`/`StatusError`. Specifically:

- `func writeResponseStatus(w io.Writer, s status) error` → `s Status`
- `func writeResponseError(...)` body: `[]byte{byte(statusError)}` → `[]byte{byte(StatusError)}`
- `func readResponseStatus(r io.Reader) (status, error)` → `(Status, error)`
- inside `readResponseStatus`: `switch status(b[0]) { case statusOK, statusError: return status(b[0]), nil`
  → `switch Status(b[0]) { case StatusOK, StatusError: return Status(b[0]), nil`

- [ ] **Step 3: Rename references in `plugin/internal/tunnel/tunnel.go`**

Edit `plugin/internal/tunnel/tunnel.go`:

Find this signature:

```go
func (t *Tunnel) Receive(ctx context.Context) (status, io.Reader, error) {
```

Change to:

```go
func (t *Tunnel) Receive(ctx context.Context) (Status, io.Reader, error) {
```

Find inside `SendOK`:

```go
return writeResponseStatus(t.stream, statusOK)
```

Change to:

```go
return writeResponseStatus(t.stream, StatusOK)
```

Update any remaining references to `status` / `statusOK` / `statusError` to `Status` / `StatusOK` / `StatusError`.

- [ ] **Step 4: Rename references in test files**

Edit `plugin/internal/tunnel/e2e_test.go`:

Find: `assert.Equal(t, statusOK, st)`
Change to: `assert.Equal(t, StatusOK, st)`

Run grep to find any remaining references:

Run: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse && grep -rn "\bstatusOK\b\|\bstatusError\b\|\bstatus\b" plugin/internal/tunnel/ | grep -v "Status"`
Expected: no remaining lower-case references (apart from those describing them in comments). For any line still hitting `status`/`statusOK`/`statusError` in code, replace with `Status`/`StatusOK`/`StatusError`.

Note: the comment in `frame.go` line 10 says "status enum on the seeder→consumer half" — this comment now sits above `type Status byte`. Update the comment word "status" → "Status" if natural, or leave the lowercased form if it reads as the noun "status byte".

- [ ] **Step 5: Run tunnel tests under race**

Run: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" go test -race ./internal/tunnel/...`
Expected: `ok`, no skips.

- [ ] **Step 6: Lint**

Run: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" golangci-lint run ./internal/tunnel/...`
Expected: exit 0.

- [ ] **Step 7: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse
git add plugin/internal/tunnel/
git commit -m "$(cat <<'EOF'
refactor(plugin/tunnel): export Status/StatusOK/StatusError

Rename the private status enum to its exported form so callers
outside the package (notably the upcoming ccproxy NetworkRouter)
can discriminate on the response status byte without magic-number
literals or wire re-encoding. No wire-format change.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: Add `SeederAddr` / `SeederPubkey` / `EphemeralPriv` to `EntryMetadata`

**Files:**
- Modify: `plugin/internal/ccproxy/sessionmode.go`
- Modify: `plugin/internal/ccproxy/sessionmode_test.go`

- [ ] **Step 1: Add a failing test that round-trips the new fields**

Append to `plugin/internal/ccproxy/sessionmode_test.go`:

```go
import (
	"crypto/ed25519"
	"crypto/rand"
	"net/netip"
	// (plus existing imports — keep them)
)

func TestSessionModeStore_PreservesSeederAssignmentFields(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	addr := netip.MustParseAddrPort("127.0.0.1:5555")

	s := NewSessionModeStore()
	s.EnterNetworkMode("session-y", EntryMetadata{
		EnteredAt:     time.Now(),
		ExpiresAt:     time.Now().Add(15 * time.Minute),
		SeederAddr:    addr,
		SeederPubkey:  pub,
		EphemeralPriv: priv,
	})

	mode, got := s.GetMode("session-y")
	require.Equal(t, ModeNetwork, mode)
	require.NotNil(t, got)
	assert.Equal(t, addr, got.SeederAddr)
	assert.Equal(t, ed25519.PublicKey(pub), got.SeederPubkey)
	assert.Equal(t, ed25519.PrivateKey(priv), got.EphemeralPriv)
}
```

Note: if the test file already imports `crypto/ed25519`, `crypto/rand`, `net/netip` — leave those imports alone; add only the missing ones. If the existing import block is sorted, sort the additions in.

- [ ] **Step 2: Run, confirm FAIL**

Run: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" go test ./internal/ccproxy/... -run TestSessionModeStore_PreservesSeederAssignmentFields`
Expected: build error like `unknown field SeederAddr in struct literal of type EntryMetadata`.

- [ ] **Step 3: Add fields to `EntryMetadata`**

Edit `plugin/internal/ccproxy/sessionmode.go`. The current struct:

```go
// EntryMetadata is the per-session state carried for ModeNetwork sessions.
type EntryMetadata struct {
	EnteredAt          time.Time
	ExpiresAt          time.Time
	StopFailurePayload *ratelimit.StopFailurePayload
	UsageProbeBytes    []byte
	UsageVerdict       ratelimit.UsageVerdict
}
```

becomes:

```go
// EntryMetadata is the per-session state carried for ModeNetwork sessions.
type EntryMetadata struct {
	EnteredAt          time.Time
	ExpiresAt          time.Time
	StopFailurePayload *ratelimit.StopFailurePayload
	UsageProbeBytes    []byte
	UsageVerdict       ratelimit.UsageVerdict

	// Seeder routing — populated by the StopFailure-hook orchestrator
	// (separate plan) when activating ModeNetwork. NetworkRouter reads
	// these via PeerDialer to dial the seeder.
	SeederAddr    netip.AddrPort
	SeederPubkey  ed25519.PublicKey
	EphemeralPriv ed25519.PrivateKey
}
```

Add to the `import` block:

```go
import (
	"crypto/ed25519"
	"net/netip"
	"sync"
	"time"

	"github.com/token-bay/token-bay/plugin/internal/ratelimit"
)
```

- [ ] **Step 4: Run, confirm PASS**

Run: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" go test -race ./internal/ccproxy/...`
Expected: `ok`. The pre-existing `Returns501_WithAnthropicErrorJSON` test should still pass — Task 3 replaces it.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse
git add plugin/internal/ccproxy/sessionmode.go plugin/internal/ccproxy/sessionmode_test.go
git commit -m "$(cat <<'EOF'
feat(plugin/ccproxy): add seeder assignment fields to EntryMetadata

SeederAddr / SeederPubkey / EphemeralPriv carry the per-session tunnel
dial inputs that the upcoming NetworkRouter reads via PeerDialer. The
StopFailure-hook orchestrator (separate, future plan) populates them
when activating ModeNetwork; this commit only adds the fields and a
round-trip test, so the orchestrator has a stable target.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: Define `PeerDialer` / `PeerConn` interfaces + `NetworkRouter` skeleton

This task replaces the 501 stub with a router that delegates to a `PeerDialer` and writes a 502 with an Anthropic-style error body when no dialer or no metadata is present. Tests use a fake.

**Files:**
- Modify: `plugin/internal/ccproxy/router.go`
- Modify: `plugin/internal/ccproxy/router_test.go`

- [ ] **Step 1: Write the failing happy-path test**

Replace `TestNetworkRouter_Returns501_WithAnthropicErrorJSON` in `plugin/internal/ccproxy/router_test.go` with the following new tests (delete the old one):

```go
package ccproxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakePeerConn is a controllable stand-in for PeerConn.
type fakePeerConn struct {
	sendErr     error
	sentBody    []byte
	receiveOK   bool
	receiveMsg  string
	receiveBody string
	receiveErr  error
	closed      bool
}

func (f *fakePeerConn) Send(body []byte) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sentBody = append([]byte(nil), body...)
	return nil
}

func (f *fakePeerConn) Receive(ctx context.Context) (bool, string, io.Reader, error) {
	if f.receiveErr != nil {
		return false, "", nil, f.receiveErr
	}
	if !f.receiveOK {
		return false, f.receiveMsg, nil, nil
	}
	return true, "", strings.NewReader(f.receiveBody), nil
}

func (f *fakePeerConn) Close() error { f.closed = true; return nil }

// fakeDialer always returns the configured PeerConn or err.
type fakeDialer struct {
	conn *fakePeerConn
	err  error
}

func (d *fakeDialer) Dial(ctx context.Context, meta *EntryMetadata) (PeerConn, error) {
	if d.err != nil {
		return nil, d.err
	}
	return d.conn, nil
}

func TestNetworkRouter_HappyPath_StreamsSSEBytes(t *testing.T) {
	canned := "event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	conn := &fakePeerConn{receiveOK: true, receiveBody: canned}
	router := &NetworkRouter{Dialer: &fakeDialer{conn: conn}}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6"}`))
	rec := httptest.NewRecorder()

	router.Route(rec, req, &EntryMetadata{})

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	assert.Equal(t, canned, rec.Body.String())
	assert.Equal(t, []byte(`{"model":"claude-sonnet-4-6"}`), conn.sentBody)
	assert.True(t, conn.closed)
}

func TestNetworkRouter_NilMeta_Returns502(t *testing.T) {
	called := false
	dialer := &fakeDialer{}
	router := &NetworkRouter{Dialer: &dialerSpy{inner: dialer, called: &called}}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	router.Route(rec, req, nil)

	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Contains(t, rec.Body.String(), `"network_unavailable"`)
	assert.False(t, called, "dialer must not be called when meta is nil")
}

type dialerSpy struct {
	inner  PeerDialer
	called *bool
}

func (s *dialerSpy) Dial(ctx context.Context, meta *EntryMetadata) (PeerConn, error) {
	*s.called = true
	return s.inner.Dial(ctx, meta)
}

func TestNetworkRouter_DialError_Returns502(t *testing.T) {
	router := &NetworkRouter{Dialer: &fakeDialer{err: errors.New("dial boom")}}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	router.Route(rec, req, &EntryMetadata{})

	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Contains(t, rec.Body.String(), `"network_unavailable"`)
}

func TestNetworkRouter_SendError_Returns502_AndCloses(t *testing.T) {
	conn := &fakePeerConn{sendErr: errors.New("send boom")}
	router := &NetworkRouter{Dialer: &fakeDialer{conn: conn}}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	router.Route(rec, req, &EntryMetadata{})

	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.True(t, conn.closed)
}

func TestNetworkRouter_StatusError_Returns502_WithSeederMessage(t *testing.T) {
	conn := &fakePeerConn{receiveOK: false, receiveMsg: "seeder rate limited"}
	router := &NetworkRouter{Dialer: &fakeDialer{conn: conn}}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	router.Route(rec, req, &EntryMetadata{})

	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Contains(t, rec.Body.String(), "seeder rate limited")
	assert.True(t, conn.closed)
}

func TestNetworkRouter_ReceiveError_Returns502(t *testing.T) {
	conn := &fakePeerConn{receiveErr: errors.New("recv boom")}
	router := &NetworkRouter{Dialer: &fakeDialer{conn: conn}}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	router.Route(rec, req, &EntryMetadata{})

	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.True(t, conn.closed)
}

// errBody is an io.ReadCloser whose Read always errors. Verifies the
// router rejects unreadable request bodies before dialing.
type errBody struct{}

func (errBody) Read([]byte) (int, error) { return 0, errors.New("read boom") }
func (errBody) Close() error              { return nil }

func TestNetworkRouter_BodyReadError_Returns400(t *testing.T) {
	called := false
	dialer := &dialerSpy{inner: &fakeDialer{}, called: &called}
	router := &NetworkRouter{Dialer: dialer}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", errBody{})
	rec := httptest.NewRecorder()
	router.Route(rec, req, &EntryMetadata{})

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.False(t, called)
}

// Existing PassThroughRouter test stays.
func TestPassThroughRouter_ForwardsRequestAndResponse(t *testing.T) {
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

func TestNewPassThroughRouter_DefaultUpstreamIsAnthropic(t *testing.T) {
	r := NewPassThroughRouter()
	require.NotNil(t, r.UpstreamURL)
	assert.Equal(t, "https", r.UpstreamURL.Scheme)
	assert.Equal(t, "api.anthropic.com", r.UpstreamURL.Host)
}

// Compile-time interface checks.
var (
	_ RequestRouter = (*PassThroughRouter)(nil)
	_ RequestRouter = (*NetworkRouter)(nil)
)
```

- [ ] **Step 2: Run, confirm FAIL with the right error**

Run: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" go test ./internal/ccproxy/... -run TestNetworkRouter`
Expected: build errors like `undefined: PeerConn`, `undefined: PeerDialer`, `NetworkRouter has no field Dialer`.

- [ ] **Step 3: Implement `NetworkRouter`, `PeerDialer`, `PeerConn` in `router.go`**

Replace the `NetworkRouter` and `Route` definitions in `plugin/internal/ccproxy/router.go`. The new file content:

```go
package ccproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	r.Host = p.UpstreamURL.Host
	p.proxy.ServeHTTP(w, r)
}

// PeerDialer opens a tunnel to the seeder identified by meta. Production
// wiring (network_dialer.go) injects a tunnelDialer; tests inject fakes.
type PeerDialer interface {
	Dial(ctx context.Context, meta *EntryMetadata) (PeerConn, error)
}

// PeerConn is the half-duplex consumer-side tunnel surface.
type PeerConn interface {
	// Send writes the consumer's request body once.
	Send(body []byte) error
	// Receive returns (statusOK, errMsg, body, err). On statusOK==true,
	// body streams the seeder's SSE bytes until EOF or peer-close. On
	// statusOK==false, errMsg holds the seeder's UTF-8 error text and
	// body is nil. err is non-nil only on transport failures.
	Receive(ctx context.Context) (statusOK bool, errMsg string, body io.Reader, err error)
	// Close tears down the underlying tunnel. Idempotent.
	Close() error
}

// ErrNoAssignment is returned by a PeerDialer when EntryMetadata lacks
// the seeder routing fields.
var ErrNoAssignment = errors.New("ccproxy: no seeder assignment in entry metadata")

// maxRequestBytes mirrors tunnel.Config.MaxRequestBytes to prevent a
// caller from accidentally building a body larger than the wire allows.
const maxRequestBytes = 1 << 20

// NetworkRouter forwards ModeNetwork sessions over a tunnel to a seeder.
// Dialer is required at first call to Route; tests use fakes, production
// wires a tunnelDialer (see network_dialer.go).
type NetworkRouter struct {
	Dialer PeerDialer
}

// Route implements the consumer-side network forward. Failure modes map
// to Anthropic-shaped JSON error bodies; success sets Content-Type:
// text/event-stream and io.Copies the seeder's bytes verbatim.
func (n *NetworkRouter) Route(w http.ResponseWriter, r *http.Request, meta *EntryMetadata) {
	if meta == nil {
		writeAnthropicError(w, http.StatusBadGateway, "network_unavailable", "Token-Bay session not in network mode")
		return
	}
	if n.Dialer == nil {
		writeAnthropicError(w, http.StatusBadGateway, "network_unavailable", "Token-Bay network dialer not configured")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request", "failed to read request body")
		return
	}
	if len(body) > maxRequestBytes {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request", "request body exceeds 1 MiB")
		return
	}

	conn, err := n.Dialer.Dial(r.Context(), meta)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "network_unavailable", sanitizeDialErr(err))
		return
	}
	defer conn.Close()

	if err := conn.Send(body); err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "network_unavailable", "tunnel send failed")
		return
	}

	ok, errMsg, rdr, err := conn.Receive(r.Context())
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "network_unavailable", "tunnel receive failed")
		return
	}
	if !ok {
		writeAnthropicError(w, http.StatusBadGateway, "network_unavailable", errMsg)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		nb, rerr := rdr.Read(buf)
		if nb > 0 {
			if _, werr := w.Write(buf[:nb]); werr != nil {
				return // client gone; nothing more to do
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			// io.EOF is the normal termination signal from the seeder.
			return
		}
	}
}

// writeAnthropicError sets a JSON error body in the shape Claude Code
// surfaces naturally. The handler MUST not have written any bytes yet
// (Content-Type / status are still mutable).
func writeAnthropicError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	payload := map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": msg,
		},
	}
	_ = json.NewEncoder(w).Encode(payload)
}

// sanitizeDialErr maps tunnel/quic error chains to short, PII-free
// strings safe to surface to the consumer's claude.
func sanitizeDialErr(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, ErrNoAssignment) {
		return "no seeder assigned"
	}
	// Sentinel matching for tunnel.Err* lives in network_dialer.go's
	// adapter so the router stays import-free.
	return fmt.Sprintf("dial failed: %s", short(err.Error()))
}

func short(s string) string {
	const cap = 200
	if len(s) > cap {
		return s[:cap] + "…"
	}
	return s
}
```

- [ ] **Step 4: Run, confirm PASS**

Run: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" go test -race ./internal/ccproxy/...`
Expected: `ok`. Every named test from Step 1 should pass.

- [ ] **Step 5: Lint**

Run: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" golangci-lint run ./internal/ccproxy/...`
Expected: exit 0.

- [ ] **Step 6: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse
git add plugin/internal/ccproxy/router.go plugin/internal/ccproxy/router_test.go
git commit -m "$(cat <<'EOF'
feat(plugin/ccproxy): real NetworkRouter with PeerDialer seam

Replaces the v1 501 stub with a fully-routed flow: read body bounded,
dial the seeder via injectable PeerDialer, ship body, read status,
io.Copy SSE bytes onto the consumer's HTTP response with periodic
flushing. All failure branches surface Anthropic-shaped JSON error
bodies.

PeerDialer/PeerConn are the only types the router speaks; the
production tunnelDialer arrives in the next commit so the router
itself never imports internal/tunnel.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: Production `tunnelDialer` (network_dialer.go)

**Files:**
- Create: `plugin/internal/ccproxy/network_dialer.go`

- [ ] **Step 1: Create the production dialer**

Write `plugin/internal/ccproxy/network_dialer.go`:

```go
package ccproxy

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/token-bay/token-bay/plugin/internal/tunnel"
)

// tunnelDialer is the production PeerDialer wrapping internal/tunnel.Dial.
// Field-only construction keeps tests honest: a zero-value tunnelDialer
// applies sensible defaults via resolveTimeouts.
type tunnelDialer struct {
	DialTimeout time.Duration // default 5s
	Now         func() time.Time
}

// NewTunnelDialer returns a PeerDialer that opens a real tunnel.Tunnel
// per call. Defaults: 5s dial timeout, time.Now clock.
func NewTunnelDialer() PeerDialer {
	return &tunnelDialer{}
}

func (d *tunnelDialer) resolveTimeout() time.Duration {
	if d.DialTimeout > 0 {
		return d.DialTimeout
	}
	return 5 * time.Second
}

func (d *tunnelDialer) resolveNow() func() time.Time {
	if d.Now != nil {
		return d.Now
	}
	return time.Now
}

// Dial validates meta, then opens a tunnel.Tunnel.
func (d *tunnelDialer) Dial(ctx context.Context, meta *EntryMetadata) (PeerConn, error) {
	if meta == nil {
		return nil, ErrNoAssignment
	}
	if !meta.SeederAddr.IsValid() {
		return nil, fmt.Errorf("%w: invalid SeederAddr", ErrNoAssignment)
	}
	if len(meta.SeederPubkey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: SeederPubkey wrong size", ErrNoAssignment)
	}
	if len(meta.EphemeralPriv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%w: EphemeralPriv wrong size", ErrNoAssignment)
	}

	dialCtx, cancel := context.WithTimeout(ctx, d.resolveTimeout())
	defer cancel()

	cfg := tunnel.Config{
		EphemeralPriv: meta.EphemeralPriv,
		PeerPin:       meta.SeederPubkey,
		Now:           d.resolveNow(),
	}
	tun, err := tunnel.Dial(dialCtx, meta.SeederAddr, cfg)
	if err != nil {
		// Map sentinels to short, stable strings for the router's
		// sanitizeDialErr to consume. errors.Is preserves chains.
		switch {
		case errors.Is(err, tunnel.ErrPeerPinMismatch):
			return nil, fmt.Errorf("peer pin mismatch: %w", err)
		case errors.Is(err, tunnel.ErrALPNMismatch):
			return nil, fmt.Errorf("alpn mismatch: %w", err)
		case errors.Is(err, tunnel.ErrHandshakeFailed):
			return nil, fmt.Errorf("handshake failed: %w", err)
		case errors.Is(err, tunnel.ErrInvalidConfig):
			return nil, fmt.Errorf("invalid tunnel config: %w", err)
		default:
			return nil, fmt.Errorf("tunnel dial: %w", err)
		}
	}
	return &tunnelPeerConn{tun: tun}, nil
}

// tunnelPeerConn adapts a *tunnel.Tunnel to PeerConn.
type tunnelPeerConn struct {
	tun *tunnel.Tunnel
}

func (p *tunnelPeerConn) Send(body []byte) error {
	return p.tun.Send(body)
}

func (p *tunnelPeerConn) Receive(ctx context.Context) (bool, string, io.Reader, error) {
	st, r, err := p.tun.Receive(ctx)
	if err != nil {
		return false, "", nil, err
	}
	switch st {
	case tunnel.StatusOK:
		return true, "", r, nil
	case tunnel.StatusError:
		// readAll up to 4 KiB — the wire-format bound on error messages.
		const errCap = 4 << 10
		buf, _ := io.ReadAll(io.LimitReader(r, errCap))
		return false, string(buf), nil, nil
	default:
		return false, "", nil, fmt.Errorf("unknown tunnel status: 0x%02x", byte(st))
	}
}

func (p *tunnelPeerConn) Close() error { return p.tun.Close() }
```

- [ ] **Step 2: Wire the default dialer into `Server.New`**

Edit `plugin/internal/ccproxy/server.go`. Find the `New` function:

```go
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
```

Replace `Network: &NetworkRouter{},` with:

```go
		Network:     &NetworkRouter{Dialer: NewTunnelDialer()},
```

- [ ] **Step 3: Run the entire ccproxy package under race**

Run: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" go test -race ./internal/ccproxy/...`
Expected: `ok`. The Task 3 fake-dialer tests still pass (they construct `NetworkRouter{Dialer: ...}` explicitly, bypassing `New`).

- [ ] **Step 4: Lint**

Run: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" golangci-lint run ./internal/ccproxy/...`
Expected: exit 0.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse
git add plugin/internal/ccproxy/network_dialer.go plugin/internal/ccproxy/server.go
git commit -m "$(cat <<'EOF'
feat(plugin/ccproxy): production tunnelDialer wraps internal/tunnel.Dial

NewTunnelDialer constructs a PeerDialer that:
  - validates EntryMetadata (SeederAddr/SeederPubkey/EphemeralPriv)
  - applies a 5s default dial timeout
  - maps tunnel sentinels (ErrPeerPinMismatch / ErrALPNMismatch /
    ErrHandshakeFailed / ErrInvalidConfig) to short stable prefixes
    that NetworkRouter's sanitizeDialErr can surface to claude
  - adapts *tunnel.Tunnel → PeerConn including the Status enum
    discrimination

Server.New now wires this as the default Network router's Dialer.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: E2E test against a real `tunnel.Listener` (no real claude)

**Files:**
- Create: `plugin/internal/ccproxy/network_dialer_test.go`

- [ ] **Step 1: Write the e2e test**

Write `plugin/internal/ccproxy/network_dialer_test.go`:

```go
package ccproxy

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/tunnel"
)

// TestNetworkDialer_E2E_RealTunnelLoopback runs a full Route through a
// real tunnel.Listener with a hand-rolled fake seeder goroutine. No
// real claude binary, no SSE translation — just byte-level wire.
func TestNetworkDialer_E2E_RealTunnelLoopback(t *testing.T) {
	clk := func() time.Time { return time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC) }

	seederPub, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerPub, consumerPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	ln, err := tunnel.Listen(netip.MustParseAddrPort("127.0.0.1:0"), tunnel.Config{
		EphemeralPriv: seederPriv,
		PeerPin:       consumerPub,
		Now:           clk,
	})
	require.NoError(t, err)
	defer ln.Close()

	canned := "event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	requestBody := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`)

	var seederErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		seederTun, err := ln.Accept(ctx)
		if err != nil {
			seederErr = err
			return
		}
		// Do not Close — see tunnel/e2e_test.go: CloseWrite is the half-
		// duplex EOF signal. Test cleanup tears down the listener.

		body, err := seederTun.ReadRequest()
		if err != nil {
			seederErr = err
			return
		}
		if !bytes.Equal(body, requestBody) {
			seederErr = io.ErrUnexpectedEOF
			return
		}
		if err := seederTun.SendOK(); err != nil {
			seederErr = err
			return
		}
		w := seederTun.ResponseWriter()
		if _, err := io.WriteString(w, canned); err != nil {
			seederErr = err
			return
		}
		seederErr = seederTun.CloseWrite()
	}()

	dialer := &tunnelDialer{Now: clk}
	router := &NetworkRouter{Dialer: dialer}

	meta := &EntryMetadata{
		SeederAddr:    ln.LocalAddr(),
		SeederPubkey:  seederPub,
		EphemeralPriv: consumerPriv,
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(requestBody)))
	rec := httptest.NewRecorder()

	router.Route(rec, req, meta)

	wg.Wait()
	require.NoError(t, seederErr)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	assert.Equal(t, canned, rec.Body.String())
}

// TestNetworkDialer_RejectsZeroEntryMetadata asserts the validation
// gate before tunnel.Dial is reached.
func TestNetworkDialer_RejectsZeroEntryMetadata(t *testing.T) {
	d := &tunnelDialer{}
	_, err := d.Dial(context.Background(), &EntryMetadata{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoAssignment)
}
```

- [ ] **Step 2: Run, confirm PASS**

Run: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" go test -race ./internal/ccproxy/... -run E2E`
Expected: `ok`. If the listener bind fails (port already in use), the test will skip or fail visibly — re-run.

Run the whole package one more time: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" go test -race ./internal/ccproxy/...`
Expected: `ok`.

- [ ] **Step 3: Lint**

Run: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" golangci-lint run ./internal/ccproxy/...`
Expected: exit 0.

- [ ] **Step 4: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse
git add plugin/internal/ccproxy/network_dialer_test.go
git commit -m "$(cat <<'EOF'
test(plugin/ccproxy): e2e NetworkRouter against real tunnel.Listener

A loopback QUIC connection between a hand-rolled fake seeder
goroutine and the production tunnelDialer routes a /v1/messages POST
through ccproxy.NetworkRouter and asserts byte-for-byte SSE delivery
on the response. No real claude binary; no localintegtest dependency.
This is the closest test we can run before the seeder-coordinator
plan adds the real claude pipe.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: Server-level smoke test — full path through `Server.handleAnthropic`

**Files:**
- Modify: `plugin/internal/ccproxy/server_test.go`

- [ ] **Step 1: Inspect existing server tests**

Run: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse && grep -n "handleAnthropic\|ModeNetwork\|EnterNetworkMode" plugin/internal/ccproxy/server_test.go | head -20`
Expected: existing tests for handleAnthropic dispatch. Read the file to confirm style and pick a place to add the new test.

- [ ] **Step 2: Add a server-level smoke test**

Append to `plugin/internal/ccproxy/server_test.go` (do not duplicate imports — add only what's missing):

```go
func TestServer_HandleAnthropic_NetworkMode_RoutesThroughNetworkRouter(t *testing.T) {
	canned := "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	conn := &fakePeerConn{receiveOK: true, receiveBody: canned}
	dialer := &fakeDialer{conn: conn}

	store := NewSessionModeStore()
	store.EnterNetworkMode("session-z", EntryMetadata{
		EnteredAt: time.Now(),
		ExpiresAt: time.Now().Add(time.Minute),
	})

	srv := New(WithSessionStore(store), WithNetworkRouter(&NetworkRouter{Dialer: dialer}))
	require.NoError(t, srv.Start(context.Background()))
	defer srv.Close()

	httpReq, _ := http.NewRequest(http.MethodPost, srv.URL()+"v1/messages", strings.NewReader(`{"model":"x"}`))
	httpReq.Header.Set("X-Claude-Code-Session-Id", "session-z")

	resp, err := http.DefaultClient.Do(httpReq)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	assert.Equal(t, canned, string(body))
}
```

If `time`, `strings`, `io`, `context` are not yet imported in `server_test.go`, add them. The `fakePeerConn` and `fakeDialer` types are defined in `router_test.go` and visible in this same package.

- [ ] **Step 3: Run, confirm PASS**

Run: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" go test -race ./internal/ccproxy/...`
Expected: `ok`.

- [ ] **Step 4: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse
git add plugin/internal/ccproxy/server_test.go
git commit -m "$(cat <<'EOF'
test(plugin/ccproxy): server-level smoke through Server.handleAnthropic

A real ccproxy.Server with a staged ModeNetwork entry routes a POST
through the in-process HTTP listener, the session-mode store, the
NetworkRouter, the fake dialer/conn, and back to the test's HTTP
client. Verifies the wiring in Server.New plus session-id header
plumbing.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 7: Final verification — `make check`, race, lint, no localintegtest

**Files:** none (verification only)

- [ ] **Step 1: Full ccproxy + tunnel test sweep under race**

Run: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/ccproxy/... ./internal/tunnel/...`
Expected: `ok`, no skips.

- [ ] **Step 2: Lint the touched packages**

Run: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" golangci-lint run ./internal/ccproxy/... ./internal/tunnel/...`
Expected: exit 0.

- [ ] **Step 3: Repo-wide `make check`**

Run: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse && PATH="$HOME/.local/share/mise/shims:$PATH" make check`
Expected: tests + lint pass across all modules. If `make check` invokes `make localintegtest` indirectly, opt out with the appropriate target — but `make check` per CLAUDE.md is `test` + `lint`, no localintegtest.

- [ ] **Step 4: Confirm no goroutine leaks**

Run: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=3 ./internal/ccproxy/...`
Expected: `ok`. Three sequential runs catch flakiness from listener bind races.

- [ ] **Step 5: Push and open PR**

Confirm clean working tree:

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse && git status
```

Push:

```bash
git push -u origin HEAD
```

Create PR:

```bash
gh pr create --base main --title "feat(plugin): ccproxy NetworkRouter — real consumer-side tunnel forward" --body "$(cat <<'EOF'
## Summary

- Replaces the v1 501 stub `NetworkRouter` in `plugin/internal/ccproxy/router.go` with a real implementation: dial peer via injectable `PeerDialer`, ship `/v1/messages` body, read status byte, `io.Copy` SSE bytes back to claude.
- Adds `SeederAddr`/`SeederPubkey`/`EphemeralPriv` to `EntryMetadata` so the future StopFailure-hook orchestrator can stage seeder assignments. This PR does not populate them; it adds the seam.
- Production `tunnelDialer` (in `network_dialer.go`) wraps `tunnel.Dial`; the router itself never imports `internal/tunnel`.
- Cross-cutting: exports `tunnel.Status`/`tunnel.StatusOK`/`tunnel.StatusError` (renamed from private `status`/`statusOK`/`statusError`) so the dialer adapter can discriminate without magic bytes. No wire-format change.
- E2E test against a real `tunnel.Listener` with a hand-rolled fake seeder; server-level smoke test through `Server.handleAnthropic`. No real claude binary; no localintegtest dependency.

Spec: `docs/superpowers/specs/plugin/2026-05-10-ccproxy-network-design.md`.

## Test plan

- [x] `go test -race ./plugin/internal/ccproxy/... ./plugin/internal/tunnel/...` green.
- [x] `golangci-lint run` clean on touched packages.
- [x] `make check` green at repo root.
- [x] Three sequential races to catch listener-bind flakiness.
- [ ] Reviewer confirms `EntryMetadata` field additions look right for the upcoming StopFailure-hook orchestrator plan.

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

- [ ] **Step 6: Watch CI**

Run: `gh pr checks --watch`
Wait until all checks pass before merging.

- [ ] **Step 7: Merge**

Run: `gh pr merge --squash --delete-branch`

---

## 3. Self-review checklist

After all tasks are complete, verify:

- [ ] Spec coverage: every section in `2026-05-10-ccproxy-network-design.md` (§3.2 API, §3.3 production dialer, §3.4 state diagram, §3.5 HTTP responses, §3.6 D1–D8, §5.1 unit tests, §5.2 e2e, §6 cross-cutting tunnel rename) maps to at least one task above.
- [ ] No "TBD"/"TODO"/"figure out later" left in code.
- [ ] Type and method names match across tasks: `PeerDialer`, `PeerConn`, `NetworkRouter`, `tunnelDialer`, `tunnelPeerConn`, `ErrNoAssignment`, `tunnel.Status`, `tunnel.StatusOK`, `tunnel.StatusError` are consistent throughout.
- [ ] `EntryMetadata.SeederAddr` is `netip.AddrPort` (not `string`); `SeederPubkey` is `ed25519.PublicKey`; `EphemeralPriv` is `ed25519.PrivateKey`.
- [ ] `Server.New` wires `NewTunnelDialer()` as the default `Network` dialer.
- [ ] `router.go` has no `internal/tunnel` import; `network_dialer.go` does.
- [ ] No `make localintegtest` dependency. No `crypto/rand` in production code (test code only).
