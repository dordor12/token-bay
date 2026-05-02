# Plugin integration tests — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land a `plugin/test/integration/` test package that exercises the cross-module composition of the four plugin subsystems already implemented today — `ccproxy`, `settingsjson`, `ratelimit`, `envelopebuilder`/`exhaustionproofbuilder` — against real loopback HTTP servers and a real temp-dir filesystem. Each test asserts a contract that no single module's unit test can: that the published interfaces line up when wired together.

**Architecture:** Single new external test package (`package integration_test`) under `plugin/test/integration/`. Tests are fully hermetic — they spin up an in-process `ccproxy.Server` on `127.0.0.1:0`, write to per-test `t.TempDir()`-rooted settings/rollback paths, and use `httptest.NewServer` for the "Anthropic upstream." No `claude` binary, no real tracker, no QUIC. Build tag is unset so they run as part of `make test`; they should add < 1s wall time to the suite. End-to-end tests that need a stubbed tracker + stubbed `claude` (per `plugin/CLAUDE.md`) remain reserved for the still-empty `plugin/test/e2e/`.

**Tech Stack:**
- Go 1.25 stdlib `net/http`, `net/http/httptest`, `os`, `crypto/ed25519`, `crypto/sha256`, `encoding/json`
- `github.com/stretchr/testify/assert` + `require`
- `google.golang.org/protobuf/proto` (transitively via `shared/`)
- Imports under test: `plugin/internal/ccproxy`, `plugin/internal/settingsjson`, `plugin/internal/ratelimit`, `plugin/internal/envelopebuilder`, `plugin/internal/exhaustionproofbuilder`
- Imports from `shared/`: `proto`, `exhaustionproof`, `ids`, `signing`

**Spec references:**
- Plugin design: `docs/superpowers/specs/plugin/2026-04-22-plugin-design.md` §2.5, §5.3, §5.4, §5.5, §5.6, §12 (acceptance criteria)
- Envelope/proof builders design: `docs/superpowers/specs/plugin/2026-04-25-envelopebuilder-design.md` §4, §7, §8
- Pinned source references already validated by the unit-test plans for each module — nothing new to pin here.

**Prerequisites:** all four implemented modules at HEAD on this branch — verify by running `make -C plugin test` and getting a green tree before starting.

**Out of scope (defer to later plans):**
- Real `claude` binary calls (covered by `make conformance`).
- Stubbed-tracker e2e (`plugin/test/e2e/`; needs the still-empty `internal/trackerclient` and `internal/sidecar`).
- The runtime compatibility probe end-to-end (needs `internal/sidecar` orchestrator).
- Auth-prober integration (the `claude auth status --json` shell-out is unit-tested with a stubbed `AuthProber`; an integration test would need a real `claude` binary, which is conformance territory).

---

## Table of contents

1. [File map](#1-file-map)
2. [Conventions](#2-conventions)
3. [Task 1: Package scaffold + shared helpers](#task-1-package-scaffold--shared-helpers)
4. [Task 2: Pass-through round-trip via settings.json](#task-2-pass-through-round-trip-via-settingsjson)
5. [Task 3: Network-mode session returns 501 stub](#task-3-network-mode-session-returns-501-stub)
6. [Task 4: Enter→Exit settings.json rollback preserves prior keys](#task-4-enterexit-settingsjson-rollback-preserves-prior-keys)
7. [Task 5: Two-stage gate composition (ratelimit modules)](#task-5-two-stage-gate-composition-ratelimit-modules)
8. [Task 6: Builders chain + Ed25519 verify (happy path)](#task-6-builders-chain--ed25519-verify-happy-path)
9. [Task 7: Tamper detection on the assembled envelope](#task-7-tamper-detection-on-the-assembled-envelope)

---

## 1. File map

Created in this plan:

| Path | Purpose |
|---|---|
| `plugin/test/integration/doc.go` | Package documentation comment |
| `plugin/test/integration/helpers_test.go` | Shared test helpers (temp store, ccproxy spinup, fake upstream, fake Signer) |
| `plugin/test/integration/ccproxy_settingsjson_test.go` | Tasks 2, 3, 4 — ccproxy + settingsjson cross-wiring |
| `plugin/test/integration/builders_chain_test.go` | Tasks 5, 6, 7 — ratelimit + builders + signing |

Modified: none. The `plugin/test/integration/` directory does not exist yet; it is added by Task 1.

The plan does not change any production code, any existing test, the Makefile, or `go.mod`. All imports are already in `plugin/go.sum` from the prior feature plans (`stretchr/testify`, `google.golang.org/protobuf`, `shared/`).

---

## 2. Conventions

- All Go commands run from `/Users/dor.amid/.superset/worktrees/token-bay/plan-plugin-integ-test/plugin` unless stated. Use absolute paths in commands so the engineer can copy-paste.
- `PATH="$HOME/.local/share/mise/shims:$PATH"` may be required if Go is managed by mise. Each Bash step that runs `go` includes it for safety.
- One commit per task. Conventional-commit prefix: `test(plugin/integration): <summary>`.
- Co-Authored-By footer (every commit): `Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>`.
- Test files use external test package `package integration_test` so they consume only exported APIs from the modules under test — same boundary a future caller would have.
- All tests use `t.TempDir()` for filesystem state. No global state; tests must be safe under `-race` and `-parallel`.
- No build tag. These tests run on every `make test`.

---

## Task 1: Package scaffold + shared helpers

**Files:**
- Create: `plugin/test/integration/doc.go`
- Create: `plugin/test/integration/helpers_test.go`

The helpers file holds the shared scaffolding consumed by the test files in subsequent tasks. By landing it in its own commit we get a clean reusable foundation that the per-test commits build on top of.

- [ ] **Step 1: Create the package doc**

Create `plugin/test/integration/doc.go`:

```go
// Package integration contains cross-module integration tests for the
// Token-Bay plugin. Each test composes two or more plugin/internal/*
// subsystems against a real loopback HTTP server and a real temp-dir
// filesystem — proving that the published interfaces line up when wired
// together, beyond what any single module's unit tests can show.
//
// Tests are hermetic: no real `claude` binary, no real tracker, no QUIC.
// Stubbed-tracker/stubbed-claude end-to-end tests live separately under
// plugin/test/e2e/ (see plugin/CLAUDE.md).
//
// Run with: make -C plugin test (no build tag required).
package integration
```

- [ ] **Step 2: Create the helpers test file**

Create `plugin/test/integration/helpers_test.go`:

```go
package integration_test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/ccproxy"
	"github.com/token-bay/token-bay/plugin/internal/settingsjson"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
)

// newTempStore returns a settingsjson.Store rooted in t.TempDir(). Both
// the settings file and the rollback journal live under the test's temp
// dir, so each test gets a fully isolated filesystem.
func newTempStore(t *testing.T) *settingsjson.Store {
	t.Helper()
	dir := t.TempDir()
	return settingsjson.NewStoreAt(
		filepath.Join(dir, "settings.json"),
		filepath.Join(dir, "settings-rollback.json"),
	)
}

// fakeUpstream wraps an httptest.Server with a parsed *url.URL, ready to
// hand to a ccproxy.PassThroughRouter as its UpstreamURL.
type fakeUpstream struct {
	server *httptest.Server
	url    *url.URL
}

// newFakeUpstream starts an httptest.Server with the given handler and
// registers Close() with t.Cleanup. The returned URL is the upstream the
// PassThroughRouter forwards to.
func newFakeUpstream(t *testing.T, handler http.HandlerFunc) *fakeUpstream {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	return &fakeUpstream{server: srv, url: u}
}

// startCCProxy boots a ccproxy.Server on 127.0.0.1:0 with the given
// pass-through upstream. The server's Close is registered via t.Cleanup.
// Returns the server itself (for SessionModeStore access in network-mode
// tests) and its base URL (e.g. "http://127.0.0.1:54321/").
func startCCProxy(t *testing.T, upstream *url.URL) (*ccproxy.Server, string) {
	t.Helper()
	router := &ccproxy.PassThroughRouter{UpstreamURL: upstream}
	srv := ccproxy.New(ccproxy.WithPassThroughRouter(router))
	// context.Background is fine: ccproxy.Server.Close is called via
	// t.Cleanup below, which is what actually shuts the listener down.
	// The Start-context goroutine in ccproxy is a backup path that we
	// don't need to drive here.
	require.NoError(t, srv.Start(context.Background()))
	t.Cleanup(func() { _ = srv.Close() })
	return srv, srv.URL()
}

// readBaseURLFromSettings parses the settings.json at path and returns
// the value of env.ANTHROPIC_BASE_URL, or "" if the key is absent.
// Mimics what Claude Code's settings watcher hands to the Anthropic SDK
// client constructor.
func readBaseURLFromSettings(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var root struct {
		Env map[string]string `json:"env"`
	}
	require.NoError(t, json.Unmarshal(data, &root))
	return root.Env["ANTHROPIC_BASE_URL"]
}

// fakeSigner is a real-Ed25519 envelopebuilder.Signer for integration
// tests. It signs envelope bodies via shared/signing.SignEnvelope using
// a fixed in-memory keypair and exposes the public key for verification.
type fakeSigner struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
	id   ids.IdentityID
}

// newFakeSigner generates a fresh keypair and derives IdentityID =
// SHA-256(pub). Plugin spec §4.2 hashes orgId for production identity;
// hashing the pubkey here is functionally equivalent for tests — the
// builder only cares that IdentityID() returns 32 stable bytes.
func newFakeSigner(t *testing.T) *fakeSigner {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	return &fakeSigner{priv: priv, pub: pub, id: ids.IdentityID(sha256.Sum256(pub))}
}

func (s *fakeSigner) Sign(body *tbproto.EnvelopeBody) ([]byte, error) {
	return signing.SignEnvelope(s.priv, body)
}

func (s *fakeSigner) IdentityID() ids.IdentityID { return s.id }

// signTestBalance returns a tracker-signed *SignedBalanceSnapshot with
// fields chosen to satisfy ValidateEnvelopeBody (which delegates to the
// nil-check on BalanceProof but does not validate its internals). The
// keypair is throwaway — the consumer-side envelope-build flow does not
// re-verify the tracker's signature.
func signTestBalance(t *testing.T, identityID ids.IdentityID) *tbproto.SignedBalanceSnapshot {
	t.Helper()
	_, trackerPriv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	body := &tbproto.BalanceSnapshotBody{
		IdentityId:    identityID[:],
		Credits:       100,
		ChainTipHash:  make([]byte, 32),
		ChainTipSeq:   1,
		IssuedAt:      1714000000,
		ExpiresAt:     1714003600,
	}
	sig, err := signing.SignBalanceSnapshot(trackerPriv, body)
	require.NoError(t, err)
	return &tbproto.SignedBalanceSnapshot{Body: body, TrackerSig: sig}
}
```

- [ ] **Step 3: Verify the package builds**

Run:

```bash
PATH="$HOME/.local/share/mise/shims:$PATH" go vet ./test/integration/...
```

Expected: no output (success). If a helper imports something that doesn't exist (e.g. `signing.SignBalanceSnapshot` arity mismatch, or a renamed `tbproto` field), `go vet` surfaces the fix before any test code is written.

If `tbproto.SignedBalanceSnapshot` does not have a `TrackerSig` field on this branch, open `shared/proto/balance.pb.go` and copy the actual signature-field name — the helper here is the only place that mentions it. Fix and re-run vet.

- [ ] **Step 4: Run the (empty) test set to confirm wiring**

Run:

```bash
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race ./test/integration/...
```

Expected: `ok ... [no test files]` for the integration directory (no `Test*` funcs yet — `helpers_test.go` only defines helpers). Exit 0.

- [ ] **Step 5: Commit**

```bash
git add plugin/test/integration/doc.go plugin/test/integration/helpers_test.go
git commit -m "$(cat <<'EOF'
test(plugin/integration): scaffold + shared helpers

Adds the plugin/test/integration package and shared test helpers (temp
settingsjson.Store, fake-upstream httptest server, ccproxy spinup, real-
Ed25519 fakeSigner, tracker-signed balance fixture). Subsequent commits
add the cross-module test cases that consume these helpers.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: Pass-through round-trip via settings.json

**Files:**
- Create: `plugin/test/integration/ccproxy_settingsjson_test.go`

**What it proves:** the spec §2.5 redirect path is end-to-end consistent — `settingsjson.EnterNetworkMode` writes a URL into a real `settings.json` that, when read by a hypothetical "Claude Code" HTTP client, points at a live `ccproxy.Server`, which forwards a request without a registered session through the `PassThroughRouter` to the (faked) Anthropic upstream byte-for-byte.

This is the smallest piece of plugin spec §12 acceptance criterion 3 ("user's next prompt routes through Token-Bay within 2 seconds, without Claude Code restart") that does not require the real Claude Code binary.

- [ ] **Step 1: Write the failing test**

Create `plugin/test/integration/ccproxy_settingsjson_test.go`:

```go
package integration_test

import (
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPassThroughRoundTripViaSettings: after EnterNetworkMode writes the
// sidecar URL into settings.json, a request issued against that URL
// (without a registered session) flows through ccproxy's PassThroughRouter
// to the fake upstream and back to the client byte-for-byte.
func TestPassThroughRoundTripViaSettings(t *testing.T) {
	upstream := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		// The PassThroughRouter rewrites Host so the upstream sees its own
		// hostname; the path arrives unmodified.
		assert.Equal(t, "/v1/messages", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_fake","type":"message"}`))
	})

	_, sidecarURL := startCCProxy(t, upstream.url)

	store := newTempStore(t)
	require.NoError(t, store.EnterNetworkMode(sidecarURL, "session-passthrough"))

	// Mimic Claude Code reading env.ANTHROPIC_BASE_URL on the next request.
	gotBase := readBaseURLFromSettings(t, store.SettingsPath)
	require.Equal(t, sidecarURL, gotBase)

	// Issue a request through the sidecar URL with no X-Claude-Code-Session-Id
	// header — it falls into pass-through.
	resp, err := http.Get(gotBase + "v1/messages")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.JSONEq(t, `{"id":"msg_fake","type":"message"}`, string(body))
}
```

- [ ] **Step 2: Run the test to confirm the wiring**

Run:

```bash
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -run TestPassThroughRoundTripViaSettings ./test/integration/...
```

Expected: PASS. (The "failing test" step is degenerate here — every module under test already exists, and Task 1's helpers were vetted. The point of this commit is the new assertion that the *boundary* between settings.json mutation and ccproxy serving works.)

If the test fails, do NOT modify production code from this task. Read the failure, identify whether it's a helper bug or a real wiring regression, and stop to investigate. A regression in any of the four modules is out of scope here — file an issue and cherry-pick a fix into a separate commit.

- [ ] **Step 3: Commit**

```bash
git add plugin/test/integration/ccproxy_settingsjson_test.go
git commit -m "$(cat <<'EOF'
test(plugin/integration): pass-through round-trip via settings.json

End-to-end check that EnterNetworkMode writes the sidecar URL into
settings.json, that a request issued against that URL (no session header)
flows through ccproxy's PassThroughRouter to a fake upstream, and the
upstream response returns to the client unmodified. Exercises the
plugin spec §2.5 / §5.4 redirect surface for non-network-mode sessions.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: Network-mode session returns 501 stub

**Files:**
- Modify: `plugin/test/integration/ccproxy_settingsjson_test.go`

**What it proves:** when `SessionModeStore.EnterNetworkMode` registers a sessionID, a request carrying `X-Claude-Code-Session-Id: <sessionID>` is dispatched to `NetworkRouter` (not `PassThroughRouter`) and returns the v1 501 stub. This is the contract that the future `ccproxy-network` module will replace — proving today that the router-selection branch fires correctly means the swap-in is purely local.

- [ ] **Step 1: Append the failing test**

Append to `plugin/test/integration/ccproxy_settingsjson_test.go`:

```go
import "time"   // add to the existing import block, alphabetised

// (helpers from helpers_test.go: ccproxy, ratelimit type lives there too)
import (
	"github.com/token-bay/token-bay/plugin/internal/ccproxy"
	"github.com/token-bay/token-bay/plugin/internal/ratelimit"
)

// TestNetworkModeReturnsStub: a session registered with the
// SessionModeStore is dispatched to NetworkRouter, which v1-stubs with a
// 501 + Anthropic-shaped error JSON.
func TestNetworkModeReturnsStub(t *testing.T) {
	upstream := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream should not be called for a network-mode session, got %s", r.URL.Path)
	})
	srv, sidecarURL := startCCProxy(t, upstream.url)

	const sessionID = "session-network-mode"
	srv.Store.EnterNetworkMode(sessionID, ccproxy.EntryMetadata{
		EnteredAt:    time.Now(),
		ExpiresAt:    time.Now().Add(15 * time.Minute),
		UsageVerdict: ratelimit.UsageExhausted,
	})

	req, err := http.NewRequest(http.MethodPost, sidecarURL+"v1/messages", http.NoBody)
	require.NoError(t, err)
	req.Header.Set("X-Claude-Code-Session-Id", sessionID)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotImplemented, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), `"type":"not_implemented"`)
	assert.Contains(t, string(body), "Token-Bay network routing not yet implemented")
}
```

If the existing import block at the top of the file already imports `"net/http"`, `"io"`, `"testing"`, etc., consolidate the new `time`, `ccproxy`, and `ratelimit` imports into that single block — Go forbids duplicate import statements. The example above uses two `import` blocks for clarity; collapse them in the actual file.

- [ ] **Step 2: Run both tests in this file**

Run:

```bash
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -run "TestPassThroughRoundTripViaSettings|TestNetworkModeReturnsStub" ./test/integration/...
```

Expected: PASS for both.

- [ ] **Step 3: Commit**

```bash
git add plugin/test/integration/ccproxy_settingsjson_test.go
git commit -m "$(cat <<'EOF'
test(plugin/integration): network-mode session returns v1 stub

After SessionModeStore.EnterNetworkMode registers a session, a request
carrying that session's X-Claude-Code-Session-Id is dispatched to
NetworkRouter and returns the 501 + Anthropic-shaped error JSON. Pins
the router-selection branch so the future ccproxy-network swap-in is a
local change.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: Enter→Exit settings.json rollback preserves prior keys

**Files:**
- Modify: `plugin/test/integration/ccproxy_settingsjson_test.go`

**What it proves:** plugin spec §5.5 acceptance — after `ExitNetworkMode`, `settings.json` is restored to its pre-fallback state. This wires the rollback journal end-to-end: pre-existing unrelated env keys must survive both Enter and Exit untouched.

- [ ] **Step 1: Append the failing test**

Append to `plugin/test/integration/ccproxy_settingsjson_test.go`:

```go
import "encoding/json" // add to the existing import block

// TestEnterExitPreservesUnrelatedSettings: writing settings.json with
// some unrelated env keys, calling EnterNetworkMode then ExitNetworkMode,
// must leave the unrelated env keys untouched and remove our redirect.
func TestEnterExitPreservesUnrelatedSettings(t *testing.T) {
	upstream := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {})
	_, sidecarURL := startCCProxy(t, upstream.url)

	store := newTempStore(t)

	// Write a pre-existing settings.json with unrelated env entries.
	preExisting := `{
  "env": {
    "EDITOR": "vim",
    "MY_FLAG": "1"
  },
  "permissions": ["Bash"]
}
`
	require.NoError(t, os.MkdirAll(filepath.Dir(store.SettingsPath), 0o700))
	require.NoError(t, os.WriteFile(store.SettingsPath, []byte(preExisting), 0o600))

	require.NoError(t, store.EnterNetworkMode(sidecarURL, "session-rollback"))

	// During network mode: our key is set; theirs are still there.
	mid, err := os.ReadFile(store.SettingsPath)
	require.NoError(t, err)
	var midRoot struct {
		Env         map[string]string `json:"env"`
		Permissions []string          `json:"permissions"`
	}
	require.NoError(t, json.Unmarshal(mid, &midRoot))
	assert.Equal(t, sidecarURL, midRoot.Env["ANTHROPIC_BASE_URL"])
	assert.Equal(t, "vim", midRoot.Env["EDITOR"])
	assert.Equal(t, "1", midRoot.Env["MY_FLAG"])
	assert.Equal(t, []string{"Bash"}, midRoot.Permissions)

	require.NoError(t, store.ExitNetworkMode())

	// After exit: our key is gone; theirs survive.
	post, err := os.ReadFile(store.SettingsPath)
	require.NoError(t, err)
	var postRoot struct {
		Env         map[string]string `json:"env"`
		Permissions []string          `json:"permissions"`
	}
	require.NoError(t, json.Unmarshal(post, &postRoot))
	_, hadKey := postRoot.Env["ANTHROPIC_BASE_URL"]
	assert.False(t, hadKey, "ANTHROPIC_BASE_URL should be removed after ExitNetworkMode")
	assert.Equal(t, "vim", postRoot.Env["EDITOR"])
	assert.Equal(t, "1", postRoot.Env["MY_FLAG"])
	assert.Equal(t, []string{"Bash"}, postRoot.Permissions)

	// Rollback journal cleaned up.
	_, err = os.Stat(store.RollbackPath)
	assert.True(t, os.IsNotExist(err), "rollback journal should not exist post-exit")
}
```

Reuses `os` and `filepath` imports added in Task 1's helpers — they're already present at the top of the file via the helpers package. If the editor flags them as unused in this test file, add them to the imports block here.

- [ ] **Step 2: Run all three tests in this file**

Run:

```bash
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race ./test/integration/...
```

Expected: PASS for all three (`TestPassThroughRoundTripViaSettings`, `TestNetworkModeReturnsStub`, `TestEnterExitPreservesUnrelatedSettings`).

- [ ] **Step 3: Commit**

```bash
git add plugin/test/integration/ccproxy_settingsjson_test.go
git commit -m "$(cat <<'EOF'
test(plugin/integration): rollback preserves unrelated settings keys

EnterNetworkMode injects env.ANTHROPIC_BASE_URL while preserving every
other settings.json key; ExitNetworkMode removes it and leaves unrelated
env keys plus top-level keys (permissions, etc.) byte-identical. Verifies
plugin spec §5.5 rollback contract end-to-end across the journal.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: Two-stage gate composition (ratelimit modules)

**Files:**
- Create: `plugin/test/integration/builders_chain_test.go`

**What it proves:** plugin spec §5.2 — feeding raw StopFailure JSON and a synthetic `/usage` PTY byte stream through `ratelimit.ParseStopFailurePayload`, `ratelimit.ParseUsageProbe`, and `ratelimit.ApplyVerdictMatrix` together produces the documented verdict for both the proceed path and the refuse-manual-OK (headroom) path. None of these are tested as a *chain* in their unit tests today.

- [ ] **Step 1: Write the failing test**

Create `plugin/test/integration/builders_chain_test.go`:

```go
package integration_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/ratelimit"
)

// TestRateLimitGateComposition_Proceed: rate-limited StopFailure +
// exhausted /usage probe → ApplyVerdictMatrix returns VerdictProceed.
func TestRateLimitGateComposition_Proceed(t *testing.T) {
	stopFailureJSON := `{
  "session_id": "sess-1",
  "transcript_path": "/tmp/t",
  "cwd": "/tmp",
  "hook_event_name": "StopFailure",
  "error": "rate_limit"
}`
	payload, err := ratelimit.ParseStopFailurePayload(strings.NewReader(stopFailureJSON))
	require.NoError(t, err)
	assert.Equal(t, ratelimit.ErrorRateLimit, payload.Error)

	usageBytes := []byte("Current session: 99% used\nCurrent week (all models): 88% used\n")
	verdict := ratelimit.ParseUsageProbe(usageBytes)
	assert.Equal(t, ratelimit.UsageExhausted, verdict)

	gate := ratelimit.ApplyVerdictMatrix(ratelimit.TriggerHook, payload.Error, verdict)
	assert.Equal(t, ratelimit.VerdictProceed, gate)
}

// TestRateLimitGateComposition_RefuseManualOK: rate-limited StopFailure
// + headroom /usage probe → ApplyVerdictMatrix returns
// VerdictRefuseManualOK. Establishes that the chain correctly refuses
// to spend credits when the second signal contradicts the first.
func TestRateLimitGateComposition_RefuseManualOK(t *testing.T) {
	stopFailureJSON := `{
  "session_id": "sess-1",
  "transcript_path": "/tmp/t",
  "cwd": "/tmp",
  "hook_event_name": "StopFailure",
  "error": "rate_limit"
}`
	payload, err := ratelimit.ParseStopFailurePayload(strings.NewReader(stopFailureJSON))
	require.NoError(t, err)

	headroomBytes := []byte("Current session: 12% used\nCurrent week (all models): 30% used\n")
	verdict := ratelimit.ParseUsageProbe(headroomBytes)
	assert.Equal(t, ratelimit.UsageHeadroom, verdict)

	gate := ratelimit.ApplyVerdictMatrix(ratelimit.TriggerHook, payload.Error, verdict)
	assert.Equal(t, ratelimit.VerdictRefuseManualOK, gate)
}
```

- [ ] **Step 2: Run the test**

Run:

```bash
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -run "TestRateLimitGateComposition" ./test/integration/...
```

Expected: PASS for both.

- [ ] **Step 3: Commit**

```bash
git add plugin/test/integration/builders_chain_test.go
git commit -m "$(cat <<'EOF'
test(plugin/integration): two-stage rate-limit gate composition

Drives the plugin spec §5.2 chain — ParseStopFailurePayload +
ParseUsageProbe + ApplyVerdictMatrix — through both the proceed path
(rate-limit hook + exhausted probe) and the refuse-manual-OK path
(rate-limit hook + headroom probe). Pins the boundary between the
ratelimit module's three exported helpers.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: Builders chain + Ed25519 verify (happy path)

**Files:**
- Modify: `plugin/test/integration/builders_chain_test.go`

**What it proves:** the envelope-builder spec §4.1 / §7 contract — given the raw materials a future `ccproxy-network` request handler will assemble (a `ProofInput` derived from the cached fallback ticket and a `RequestSpec` derived from the parsed `/v1/messages`), `exhaustionproofbuilder.Build` and `envelopebuilder.Build` together produce a `*EnvelopeSigned` that:

1. Passes `shared/proto.ValidateEnvelopeBody` (which transitively re-runs `exhaustionproof.ValidateProofV1`).
2. Verifies under `shared/signing.VerifyEnvelope` with the Signer's public key.

This is the largest cross-module composition the implemented plugin code can express today — every step except the wire-format ship is real.

- [ ] **Step 1: Append the failing test**

Append to `plugin/test/integration/builders_chain_test.go`:

```go
import (
	"crypto/sha256"
	"time"

	"github.com/token-bay/token-bay/plugin/internal/envelopebuilder"
	"github.com/token-bay/token-bay/plugin/internal/exhaustionproofbuilder"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
)

// TestBuildersChain_HappyPath: ProofInput → ExhaustionProofV1 →
// EnvelopeSigned, then ValidateEnvelopeBody + VerifyEnvelope both
// succeed. Uses real Ed25519 throughout.
func TestBuildersChain_HappyPath(t *testing.T) {
	signer := newFakeSigner(t)

	proofIn := exhaustionproofbuilder.ProofInput{
		StopFailureMatcher:    "rate_limit",
		StopFailureAt:         time.Unix(1714000000, 0).UTC(),
		StopFailureErrorShape: []byte(`{"type":"rate_limit_error"}`),
		UsageProbeAt:          time.Unix(1714000005, 0).UTC(),
		UsageProbeOutput:      []byte("Current session: 99% used\nCurrent week (all models): 88% used\n"),
	}
	proof, err := exhaustionproofbuilder.NewBuilder().Build(proofIn)
	require.NoError(t, err)
	require.NotNil(t, proof)

	bodyHash := sha256.Sum256([]byte(`{"model":"claude-opus-4-7","messages":[]}`))
	spec := envelopebuilder.RequestSpec{
		Model:           "claude-opus-4-7",
		MaxInputTokens:  4_000,
		MaxOutputTokens: 1_000,
		Tier:            tbproto.PrivacyTier_PRIVACY_TIER_STANDARD,
		BodyHash:        bodyHash[:],
	}
	balance := signTestBalance(t, signer.IdentityID())

	signed, err := envelopebuilder.NewBuilder(signer).Build(spec, proof, balance)
	require.NoError(t, err)
	require.NotNil(t, signed)

	// Independent re-validation: builder said valid → ValidateEnvelopeBody agrees.
	require.NoError(t, tbproto.ValidateEnvelopeBody(signed.Body))

	// Real Ed25519 round-trip via the same public key.
	assert.True(t, signing.VerifyEnvelope(signer.pub, signed),
		"VerifyEnvelope should accept the assembled signature")
}
```

Collapse the new imports into the file's existing import block — Go rejects two `import (...)` blocks back-to-back without intervening declarations.

- [ ] **Step 2: Run the test**

Run:

```bash
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -run "TestBuildersChain_HappyPath" ./test/integration/...
```

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add plugin/test/integration/builders_chain_test.go
git commit -m "$(cat <<'EOF'
test(plugin/integration): builders chain produces verifiable envelope

Wires exhaustionproofbuilder + envelopebuilder + shared/signing into the
end-to-end chain that a future ccproxy-network handler will execute on
each /v1/messages call. Asserts the assembled EnvelopeSigned passes
ValidateEnvelopeBody and verifies under the signer's public key with
real Ed25519. Pins the envelope-builder spec §4.1 peer-composition
contract.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 7: Tamper detection on the assembled envelope

**Files:**
- Modify: `plugin/test/integration/builders_chain_test.go`

**What it proves:** the integration chain's signature really is binding — flipping a single byte in `Body.Model` of an envelope produced by this codebase causes `VerifyEnvelope` to reject it. This is the sanity-check for Task 6: without it, a silently-broken `Sign`/`Verify` pair would pass Task 6 and ship.

The test mirrors `envelopebuilder` unit-test inventory item §8.2 (3) "tamper detection," but at the integration boundary — proving the same property holds when the envelope is assembled from the full builder chain rather than a hand-rolled body.

- [ ] **Step 1: Append the failing test**

Append to `plugin/test/integration/builders_chain_test.go`:

```go
// TestBuildersChain_TamperDetected: the Ed25519 signature produced by
// the chain is binding — mutating Body.Model after Build invalidates
// VerifyEnvelope. Sanity-check that Task 6's PASS isn't an artifact of
// a degenerate fake.
func TestBuildersChain_TamperDetected(t *testing.T) {
	signer := newFakeSigner(t)

	proof, err := exhaustionproofbuilder.NewBuilder().Build(exhaustionproofbuilder.ProofInput{
		StopFailureMatcher:    "rate_limit",
		StopFailureAt:         time.Unix(1714000000, 0).UTC(),
		StopFailureErrorShape: []byte(`{"type":"rate_limit_error"}`),
		UsageProbeAt:          time.Unix(1714000005, 0).UTC(),
		UsageProbeOutput:      []byte("Current session: 99% used\nCurrent week (all models): 88% used\n"),
	})
	require.NoError(t, err)

	bodyHash := sha256.Sum256([]byte(`{"model":"claude-opus-4-7","messages":[]}`))
	signed, err := envelopebuilder.NewBuilder(signer).Build(
		envelopebuilder.RequestSpec{
			Model:           "claude-opus-4-7",
			MaxInputTokens:  4_000,
			MaxOutputTokens: 1_000,
			Tier:            tbproto.PrivacyTier_PRIVACY_TIER_STANDARD,
			BodyHash:        bodyHash[:],
		},
		proof,
		signTestBalance(t, signer.IdentityID()),
	)
	require.NoError(t, err)
	require.True(t, signing.VerifyEnvelope(signer.pub, signed), "pre-tamper should verify")

	signed.Body.Model = "claude-haiku-4-5"
	assert.False(t, signing.VerifyEnvelope(signer.pub, signed),
		"VerifyEnvelope must reject a body modified after signing")
}
```

- [ ] **Step 2: Run the full integration suite**

Run:

```bash
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race ./test/integration/...
```

Expected: PASS for all seven tests:
- `TestPassThroughRoundTripViaSettings` (Task 2)
- `TestNetworkModeReturnsStub` (Task 3)
- `TestEnterExitPreservesUnrelatedSettings` (Task 4)
- `TestRateLimitGateComposition_Proceed` (Task 5)
- `TestRateLimitGateComposition_RefuseManualOK` (Task 5)
- `TestBuildersChain_HappyPath` (Task 6)
- `TestBuildersChain_TamperDetected` (Task 7)

- [ ] **Step 3: Run the full plugin test suite under -race**

Run:

```bash
PATH="$HOME/.local/share/mise/shims:$PATH" make -C /Users/dor.amid/.superset/worktrees/token-bay/plan-plugin-integ-test/plugin test
```

Expected: every unit test and every integration test PASS, no race-detector warnings, exit 0. The integration package adds < 1s.

- [ ] **Step 4: Run the linter**

Run:

```bash
PATH="$HOME/.local/share/mise/shims:$PATH" make -C /Users/dor.amid/.superset/worktrees/token-bay/plan-plugin-integ-test/plugin lint
```

Expected: no findings. If golangci-lint complains about the new test files (e.g., unused parameter `r` in the upstream handler that ignores its request), apply the suggested fix inline before committing.

- [ ] **Step 5: Commit**

```bash
git add plugin/test/integration/builders_chain_test.go
git commit -m "$(cat <<'EOF'
test(plugin/integration): tamper detected on assembled envelope

Mutating Body.Model after envelopebuilder.Build invalidates
VerifyEnvelope — sanity check that the chain's Ed25519 signature is
really binding. Closes the integration suite for the four implemented
plugin subsystems (ccproxy, settingsjson, ratelimit, builders).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Self-review notes (for the implementing engineer)

These notes record the cross-spec consistency checks already done while writing the plan. If a divergence appears at implementation time, prefer matching the actual code over what's written here.

- Helpers' `signTestBalance` populates `BalanceSnapshotBody` fields that exist in `shared/proto/balance.pb.go` at this branch (`IdentityId`, `Credits`, `ChainTipHash`, `ChainTipSeq`, `IssuedAt`, `ExpiresAt`). If the branch you're on has a renamed field, adjust the helper — `ValidateEnvelopeBody` only nil-checks `BalanceProof`, so the inner shape isn't load-bearing for the integration tests, but the helper must still compile.
- `ccproxy.PassThroughRouter` exposes `UpstreamURL` directly (verified in `plugin/internal/ccproxy/router.go`); no setter needed.
- `settingsjson.Store.SettingsPath` and `RollbackPath` are exported (verified in `plugin/internal/ccproxy/...` — actually `plugin/internal/settingsjson/settingsjson.go`), so `readBaseURLFromSettings` can read them.
- `ccproxy.Server.URL()` returns `"http://<addr>/"` with a trailing slash — that's why the test code uses `gotBase + "v1/messages"` (no leading slash).
- The helper uses `context.Background()` for `srv.Start` to keep the suite portable; ccproxy shutdown comes from the `t.Cleanup(srv.Close)` registered immediately after — the start-context goroutine in `ccproxy.Server.Start` is a redundant secondary path.
- The integration package uses `package integration_test` (external test package) so it can only depend on exported APIs of the modules under test. This is the right boundary — a future caller would be in the same position.
- No build tag means `make test` runs these. CLAUDE.md describes `test/e2e/` as needing a stubbed tracker + stubbed `claude`; this plan deliberately stays out of `test/e2e/` until those stubs exist.
