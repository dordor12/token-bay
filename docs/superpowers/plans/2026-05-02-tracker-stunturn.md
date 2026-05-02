# tracker/internal/stunturn Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement `tracker/internal/stunturn` — a pure-logic module providing (a) thin wrappers over `github.com/pion/stun/v2` for STUN binding-request decode and binding-response encode (`Reflect` for the listener path), and (b) an in-process TURN session `Allocator` with per-seeder kbps token-bucket rate limiting, idle-session expiry, and dedupe-by-`RequestID`.

**Architecture:** No sockets, no goroutines, no metrics. The `internal/server` package will own the UDP listeners on `:3478` and `:3479` and call into this module per packet. State is in-memory: three indexes (`byToken`, `bySID`, `byReq`) over `*sessionEntry` and a per-seeder `tokenBucket` map, all guarded by a single `sync.Mutex` (small critical sections, ≤ 10² concurrent sessions per spec — see §7 of the design). Time and randomness are caller-injected; the module never calls `time.Now()` or imports `crypto/rand` directly. Returned `Session` values are deep copies — callers cannot mutate store state through them.

**Tech Stack:** Go 1.25 stdlib (`sync`, `time`, `net/netip`, `io`, `errors`, `fmt`, `encoding/hex`); `github.com/pion/stun/v2` (codec); `github.com/stretchr/testify` (tests); `github.com/token-bay/token-bay/shared/ids` for `IdentityID`.

**Spec:** `docs/superpowers/specs/tracker/2026-05-02-tracker-stunturn-design.md`. Parent: `docs/superpowers/specs/tracker/2026-04-22-tracker-design.md` §3, §5.4, §6.

**Repo path note:** This worktree lives at `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn`. (Yes — the worktree path coincidentally contains the package path; that's the branch name, not nesting. The repo root is the worktree root.) All absolute paths in this plan use that prefix.

---

## File map

```
tracker/
├── go.mod                                          ← MODIFY (Task 1: add pion/stun/v2)
├── internal/
│   ├── config/
│   │   ├── config.go                               ← MODIFY (Task 2: add SessionTTLSeconds)
│   │   ├── apply_defaults.go                       ← MODIFY (Task 2)
│   │   ├── validate.go                             ← MODIFY (Task 2)
│   │   ├── validate_test.go                        ← MODIFY (Task 2)
│   │   └── testdata/full.yaml                      ← MODIFY (Task 2)
│   └── stunturn/
│       ├── .gitkeep                                ← REMOVE (Task 3)
│       ├── doc.go                                  ← CREATE (Task 3)
│       ├── errors.go                               ← CREATE (Task 4)
│       ├── errors_test.go                          ← CREATE (Task 4)
│       ├── token.go                                ← CREATE (Task 5)
│       ├── token_test.go                           ← CREATE (Task 5)
│       ├── codec.go                                ← CREATE (Task 6)
│       ├── codec_test.go                           ← CREATE (Task 6)
│       ├── reflect.go                              ← CREATE (Task 7)
│       ├── reflect_test.go                         ← CREATE (Task 7)
│       ├── allocator.go                            ← CREATE (Task 8, expanded by Tasks 9–14)
│       └── allocator_test.go                       ← CREATE (Task 8, expanded by Tasks 9–15)
```

## Conventions used in this plan

- All `go test` / `go build` / `go mod tidy` commands run from `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker` unless a step says otherwise.
- One commit per task. Conventional-commit prefixes: `feat(tracker/stunturn):`, `test(tracker/stunturn):`, `chore(tracker):`, `docs(tracker/stunturn):`, `feat(tracker/config):`.
- Co-Authored-By footer on every commit:
  ```
  Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
  ```
- TDD per repo CLAUDE.md: failing test first; run it; confirm it fails for the *expected* reason; minimal implementation; run again to confirm green; commit.
- Lint policy: respect `.golangci.yml` (errcheck, gofumpt, gosec, ineffassign, misspell, revive[exported], staticcheck, unused). Every exported symbol needs a doc comment.
- Coverage ≥ 90% per file. Use `go test -race -cover ./internal/stunturn/...` after every task.
- Time handling: pass `time.Time` in from callers; the module never calls `time.Now()`.
- Randomness: callers pass `io.Reader`; the module never imports `crypto/rand`.
- Network addresses: use `net/netip.AddrPort` (immutable, comparable).
- Lefthook pre-commit runs `gofumpt`, `go vet`, and `golangci-lint run --new-from-rev=HEAD` per module on staged Go files. Run `gofumpt -l -w <files>` before `git add` to avoid the formatter rewriting your stage.

---

## Task 1: Add `github.com/pion/stun/v2` to `tracker/go.mod`

**Why this is its own task:** importing pion in any subsequent Go file before the require is materialized would fail the build. We get the require in place up-front.

**Files:**
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/go.mod`
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/go.sum`

- [ ] **Step 1: Add a temporary scratch file that imports pion**

Create `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/_scratch_pion.go` (file does not begin with underscore — Go would skip it; we want `tidy` to see the import):

Filename: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/scratch_pion.go`

```go
// Package stunturn — TEMPORARY scratch file used only to materialize the
// `require github.com/pion/stun/v2` line in tracker/go.mod. Removed in
// the same task.
package stunturn

import _ "github.com/pion/stun/v2"
```

- [ ] **Step 2: Run `go mod tidy`**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker
go mod tidy
```

Expected: `tracker/go.mod` gains a `require github.com/pion/stun/v2 vX.Y.Z` line; `tracker/go.sum` gains the corresponding checksum lines.

- [ ] **Step 3: Verify build**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker
go build ./...
```

Expected: no errors.

- [ ] **Step 4: Remove the scratch file and re-tidy (require should remain because the next task imports it; if not, we re-add via Task 6)**

```bash
rm /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/scratch_pion.go
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker
go mod tidy
```

If `go mod tidy` removes the pion `require` (no `.go` file imports it after deletion), that is expected. The require returns when Task 6 lands `codec.go`. Don't fight it; just verify the `replace` directive for `shared` is still present:

```bash
grep -E "^(require|replace)" /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/go.mod | head
```

Expected output includes:
```
replace github.com/token-bay/token-bay/shared => ../shared
```

- [ ] **Step 5: Commit (only if `go.mod` / `go.sum` changed)**

If neither file changed (because `tidy` reverted), skip the commit and proceed to Task 2. Otherwise:

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn
git add tracker/go.mod tracker/go.sum
git commit -m "$(cat <<'EOF'
chore(tracker): add pion/stun/v2 dep for stunturn module

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: Add `SessionTTLSeconds` to `STUNTURNConfig`

**Why:** the design (§9.2) makes the TURN session idle TTL operator-tunable. Default 30s.

**Files:**
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/config/config.go`
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/config/apply_defaults.go`
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/config/validate.go`
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/config/validate_test.go`
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/config/testdata/full.yaml`

- [ ] **Step 1: Write failing validate test for the new rule**

Open `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/config/validate_test.go` and find the existing `TestValidate_STUNTurn_TURNRelayMaxKbpsZero` test (it asserts `stun_turn.turn_relay_max_kbps`). Append a new test next to it:

```go
func TestValidate_STUNTurn_SessionTTLSecondsZero(t *testing.T) {
	c := validConfig()
	c.STUNTURN.SessionTTLSeconds = 0

	err := Validate(c)

	assertOneFieldError(t, err, "stun_turn.session_ttl_seconds")
}
```

- [ ] **Step 2: Run the test — it must fail to compile**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker
go test ./internal/config/ -run TestValidate_STUNTurn_SessionTTLSecondsZero
```

Expected: build failure mentioning `c.STUNTURN.SessionTTLSeconds undefined`.

- [ ] **Step 3: Add the field to `STUNTURNConfig`**

Edit `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/config/config.go`. Replace the `STUNTURNConfig` struct:

```go
type STUNTURNConfig struct {
	STUNListenAddr    string `yaml:"stun_listen_addr"`
	TURNListenAddr    string `yaml:"turn_listen_addr"`
	TURNRelayMaxKbps  int    `yaml:"turn_relay_max_kbps"`
	SessionTTLSeconds int    `yaml:"session_ttl_seconds"`
}
```

In the same file, in `DefaultConfig()`, replace the `STUNTURN: STUNTURNConfig{...}` block with:

```go
		STUNTURN: STUNTURNConfig{
			STUNListenAddr:    ":3478",
			TURNListenAddr:    ":3479",
			TURNRelayMaxKbps:  1024,
			SessionTTLSeconds: 30,
		},
```

- [ ] **Step 4: Run the test — should still fail (now in Validate logic, not at compile time)**

```bash
go test ./internal/config/ -run TestValidate_STUNTurn_SessionTTLSecondsZero
```

Expected: test failure asserting `stun_turn.session_ttl_seconds` field error is missing.

- [ ] **Step 5: Add the validate rule**

Edit `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/config/validate.go`. Find `func (v *validator) checkSTUNTURN` and replace with:

```go
func (v *validator) checkSTUNTURN(c *Config) {
	if c.STUNTURN.TURNRelayMaxKbps <= 0 {
		v.add("stun_turn.turn_relay_max_kbps", "must be > 0")
	}
	if c.STUNTURN.SessionTTLSeconds <= 0 {
		v.add("stun_turn.session_ttl_seconds", "must be > 0")
	}
}
```

- [ ] **Step 6: Add a default-fill stanza in `apply_defaults.go`**

Edit `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/config/apply_defaults.go`. Find the `// STUN/TURN` section and append:

```go
	if c.STUNTURN.SessionTTLSeconds == 0 {
		c.STUNTURN.SessionTTLSeconds = d.STUNTURN.SessionTTLSeconds
	}
```

It must sit after the other three STUNTURN default-fills.

- [ ] **Step 7: Update `testdata/full.yaml`**

Edit `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/config/testdata/full.yaml`. Find:

```yaml
stun_turn:
  stun_listen_addr: ":3478"
  turn_listen_addr: ":3479"
  turn_relay_max_kbps: 1024
```

Append `  session_ttl_seconds: 30` so the block reads:

```yaml
stun_turn:
  stun_listen_addr: ":3478"
  turn_listen_addr: ":3479"
  turn_relay_max_kbps: 1024
  session_ttl_seconds: 30
```

- [ ] **Step 8: Run the full config test suite**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker
go test -race -cover ./internal/config/...
```

Expected: PASS for every config test, including the new one and all existing round-trip and default-fill tests. If `TestApplyDefaults_*` or `TestLoad_FullYAML_*` fails, double-check that `testdata/full.yaml` was updated and that `DefaultConfig()` populates the new field.

- [ ] **Step 9: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn
gofumpt -l -w tracker/internal/config/config.go tracker/internal/config/apply_defaults.go tracker/internal/config/validate.go tracker/internal/config/validate_test.go
git add tracker/internal/config/config.go tracker/internal/config/apply_defaults.go tracker/internal/config/validate.go tracker/internal/config/validate_test.go tracker/internal/config/testdata/full.yaml
git commit -m "$(cat <<'EOF'
feat(tracker/config): add stun_turn.session_ttl_seconds (default 30)

Operator-tunable idle TTL for the upcoming TURN session allocator.
Default chosen to expire allocator state before the broker's
stream_idle_s (60s) so the listener releases sessions before the
request times out (see stunturn design §9.2).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: Package doc + remove `.gitkeep`

**Files:**
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/doc.go`
- Remove: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/.gitkeep`

- [ ] **Step 1: Write `doc.go`**

Create `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/doc.go`:

```go
// Package stunturn provides STUN binding-response reflection and TURN
// session-state allocation for the tracker.
//
// This package is pure logic. It does not own any UDP socket; the caller
// (internal/server) opens the listeners on STUNListenAddr (:3478) and
// TURNListenAddr (:3479) and dispatches each datagram into the functions
// here.
//
// STUN side: DecodeBindingRequest validates an inbound RFC 5389 binding
// request and returns its 12-byte transaction ID; EncodeBindingResponse
// produces the wire-ready binding-response bytes carrying one
// XOR-MAPPED-ADDRESS attribute. Both wrap github.com/pion/stun/v2 — wire
// format compliance is delegated to that library. Reflect is a one-line
// helper that combines them for the listener's hot path.
//
// TURN side: Allocator manages session state — 16-byte opaque tokens,
// per-seeder kbps token-bucket rate limiting (driven by
// STUNTURNConfig.TURNRelayMaxKbps), idle-session expiry driven by
// STUNTURNConfig.SessionTTLSeconds, and dedupe-by-RequestID. The hot path
// is ResolveAndCharge, called once per inbound TURN datagram by the
// listener. Sweep is the periodic GC that callers run from a single
// goroutine on a timer.
//
// Concurrency model: every public method on *Allocator holds a single
// sync.Mutex. Critical sections are O(1) map lookups and small
// arithmetic; expected scale is ≤ 10^2 concurrent sessions per tracker
// per spec §11. STUN binding requests do not touch the allocator —
// Reflect is stateless — so STUN traffic does not contend on this lock.
//
// Future scaling: if the allocator ever shows up under contention, shard
// by IdentityID (the seeder), embed the shard index in the high byte of
// Token, and walk shards in Sweep. See design spec §7.2.
//
// Time and randomness are caller-injected: AllocatorConfig.Now and Rand.
// The package never calls time.Now() and never imports crypto/rand
// directly. Tests pass a fixed clock and a deterministic byte stream.
//
// Returned Session values are deep copies. Callers cannot mutate the
// allocator's internal state through them.
//
// The package intentionally does not know about: the broker selection
// algorithm, reputation freezes, federation, the registry's external
// address bookkeeping, or persistence. The tracker's own session and
// server modules wire stunturn into those subsystems.
//
// Spec: docs/superpowers/specs/tracker/2026-05-02-tracker-stunturn-design.md.
package stunturn
```

- [ ] **Step 2: Remove `.gitkeep`**

```bash
rm /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/.gitkeep
```

- [ ] **Step 3: Verify build**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker
go build ./internal/stunturn/...
```

Expected: no errors.

- [ ] **Step 4: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn
gofumpt -l -w tracker/internal/stunturn/doc.go
git add tracker/internal/stunturn/doc.go
git rm tracker/internal/stunturn/.gitkeep
git commit -m "$(cat <<'EOF'
docs(tracker/stunturn): package doc — pure-logic STUN/TURN module

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: Sentinel errors

**Files:**
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/errors.go`
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/errors_test.go`

- [ ] **Step 1: Write failing test for distinct sentinels**

Create `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/errors_test.go`:

```go
package stunturn

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSentinelsAreDistinct asserts every pair of sentinel errors is
// distinguishable via errors.Is.
func TestSentinelsAreDistinct(t *testing.T) {
	all := []error{
		ErrInvalidConfig,
		ErrInvalidPacket,
		ErrUnknownToken,
		ErrSessionExpired,
		ErrThrottled,
		ErrDuplicateRequest,
		ErrRandFailed,
	}
	for i, a := range all {
		for j, b := range all {
			if i == j {
				continue
			}
			assert.False(t, errors.Is(a, b),
				"errors.Is(%v, %v) must be false (sentinels must be distinct)", a, b)
		}
	}
}

// TestErrInvalidConfig_WrapsReason asserts that wrapping ErrInvalidConfig
// with %w preserves both the sentinel identity (errors.Is) and the underlying
// reason text.
func TestErrInvalidConfig_WrapsReason(t *testing.T) {
	wrapped := fmt.Errorf("%w: MaxKbpsPerSeeder must be > 0", ErrInvalidConfig)

	require.ErrorIs(t, wrapped, ErrInvalidConfig)
	assert.Contains(t, wrapped.Error(), "MaxKbpsPerSeeder must be > 0")
}
```

- [ ] **Step 2: Run the test — must fail to compile**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker
go test ./internal/stunturn/...
```

Expected: build error mentioning `ErrInvalidConfig undefined` (and the others).

- [ ] **Step 3: Implement `errors.go`**

Create `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/errors.go`:

```go
package stunturn

import "errors"

// ErrInvalidConfig is returned by NewAllocator when AllocatorConfig is
// malformed. Wrap-friendly: callers wrap a specific reason with %w.
var ErrInvalidConfig = errors.New("stunturn: invalid allocator config")

// ErrInvalidPacket is returned by DecodeBindingRequest when the bytes
// are not a valid RFC 5389 binding request (malformed bytes, wrong
// message type, wrong magic cookie, or unknown comprehension-required
// attribute).
var ErrInvalidPacket = errors.New("stunturn: invalid stun packet")

// ErrUnknownToken is returned by ResolveAndCharge when the token is not
// in the allocator (never allocated, or already Released / Swept).
var ErrUnknownToken = errors.New("stunturn: unknown session token")

// ErrSessionExpired is returned by ResolveAndCharge when the session's
// LastActive is older than SessionTTL. The entry is deleted on the same
// call; subsequent calls return ErrUnknownToken.
var ErrSessionExpired = errors.New("stunturn: session expired")

// ErrThrottled is returned by ResolveAndCharge / Charge when the
// seeder's per-second bandwidth bucket has insufficient credit for the
// requested byte count. The bucket is NOT debited on a throttled call.
var ErrThrottled = errors.New("stunturn: seeder throttled")

// ErrDuplicateRequest is returned by Allocate when a session for the
// same RequestID is already live. Caller should treat the prior
// allocation as still good and not retry.
var ErrDuplicateRequest = errors.New("stunturn: duplicate request id")

// ErrRandFailed wraps any failure from the injected io.Reader during
// token generation. The error chain preserves the underlying error via
// errors.Is / errors.As.
var ErrRandFailed = errors.New("stunturn: token randomness failed")
```

- [ ] **Step 4: Run the test — must pass**

```bash
go test -race -cover ./internal/stunturn/...
```

Expected: PASS for both tests.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn
gofumpt -l -w tracker/internal/stunturn/errors.go tracker/internal/stunturn/errors_test.go
git add tracker/internal/stunturn/errors.go tracker/internal/stunturn/errors_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/stunturn): sentinel errors

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: `Token` type

**Files:**
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/token.go`
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/token_test.go`

- [ ] **Step 1: Write failing tests**

Create `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/token_test.go`:

```go
package stunturn

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestToken_String_Hex(t *testing.T) {
	tok := Token{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
	}

	assert.Equal(t, "0102030405060708090a0b0c0d0e0f10", tok.String())
}

func TestToken_Zero_String(t *testing.T) {
	var tok Token

	assert.Equal(t, "00000000000000000000000000000000", tok.String())
}

// TestToken_UsableAsMapKey ensures Token can key a Go map (it is a value
// array; this is only a smoke test to catch accidental future changes
// like switching to a slice).
func TestToken_UsableAsMapKey(t *testing.T) {
	m := map[Token]int{}
	m[Token{1}] = 1
	m[Token{2}] = 2

	assert.Equal(t, 2, len(m))
	assert.Equal(t, 1, m[Token{1}])
}
```

- [ ] **Step 2: Run — must fail to compile**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker
go test ./internal/stunturn/...
```

Expected: `Token undefined`.

- [ ] **Step 3: Implement `token.go`**

Create `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/token.go`:

```go
package stunturn

import "encoding/hex"

// Token is the 16-byte opaque handle a TURN session is identified by.
// It is generated from AllocatorConfig.Rand at Allocate time and used
// as a map key inside the allocator. The value-array shape (rather than
// a slice) makes Token comparable and usable as a map key.
type Token [16]byte

// String returns the lowercase hex encoding of the token. Never
// special-cases the zero value.
func (t Token) String() string {
	return hex.EncodeToString(t[:])
}
```

- [ ] **Step 4: Run — must pass**

```bash
go test -race -cover ./internal/stunturn/...
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn
gofumpt -l -w tracker/internal/stunturn/token.go tracker/internal/stunturn/token_test.go
git add tracker/internal/stunturn/token.go tracker/internal/stunturn/token_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/stunturn): Token — 16-byte opaque session handle

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: STUN codec wrappers — `DecodeBindingRequest`, `EncodeBindingResponse`

**Files:**
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/codec.go`
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/codec_test.go`

- [ ] **Step 1: Write failing tests**

Create `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/codec_test.go`:

```go
package stunturn

import (
	"errors"
	"net/netip"
	"testing"

	pionstun "github.com/pion/stun/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var refTxID = [12]byte{
	0x01, 0x02, 0x03, 0x04, 0x05, 0x06,
	0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c,
}

// TestEncodeBindingResponse_IPv4_RoundTrip encodes a binding response
// and asserts pion can decode it back to the same address.
func TestEncodeBindingResponse_IPv4_RoundTrip(t *testing.T) {
	observed := netip.MustParseAddrPort("1.2.3.4:50001")

	raw := EncodeBindingResponse(refTxID, observed)
	require.Len(t, raw, 32, "IPv4 binding response is 32 bytes")

	var m pionstun.Message
	require.NoError(t, m.UnmarshalBinary(raw))
	assert.Equal(t, refTxID[:], m.TransactionID[:])

	var xma pionstun.XORMappedAddress
	require.NoError(t, xma.GetFrom(&m))
	assert.True(t, xma.IP.To4() != nil, "address must be IPv4")
	assert.Equal(t, "1.2.3.4", xma.IP.String())
	assert.Equal(t, 50001, xma.Port)
}

// TestEncodeBindingResponse_IPv6_RoundTrip is the same for IPv6.
func TestEncodeBindingResponse_IPv6_RoundTrip(t *testing.T) {
	observed := netip.MustParseAddrPort("[2001:db8::1]:50001")

	raw := EncodeBindingResponse(refTxID, observed)
	require.Len(t, raw, 44, "IPv6 binding response is 44 bytes")

	var m pionstun.Message
	require.NoError(t, m.UnmarshalBinary(raw))
	assert.Equal(t, refTxID[:], m.TransactionID[:])

	var xma pionstun.XORMappedAddress
	require.NoError(t, xma.GetFrom(&m))
	assert.Nil(t, xma.IP.To4(), "address must be IPv6")
	assert.Equal(t, "2001:db8::1", xma.IP.String())
	assert.Equal(t, 50001, xma.Port)
}

// TestEncodeBindingResponse_PanicsOnInvalid asserts the documented
// "caller responsibility" contract.
func TestEncodeBindingResponse_PanicsOnInvalid(t *testing.T) {
	assert.Panics(t, func() {
		_ = EncodeBindingResponse(refTxID, netip.AddrPort{})
	})
}

// TestDecodeBindingRequest_AcceptsPionRequest uses pion to build a
// well-formed request and asserts our wrapper extracts the txID.
func TestDecodeBindingRequest_AcceptsPionRequest(t *testing.T) {
	m, err := pionstun.Build(
		pionstun.NewTransactionIDSetter(refTxID[:]),
		pionstun.BindingRequest,
	)
	require.NoError(t, err)

	got, err := DecodeBindingRequest(m.Raw)

	require.NoError(t, err)
	assert.Equal(t, refTxID, got)
}

func TestDecodeBindingRequest_ShortHeader(t *testing.T) {
	got, err := DecodeBindingRequest(make([]byte, 19))

	assert.Equal(t, [12]byte{}, got)
	assert.True(t, errors.Is(err, ErrInvalidPacket), "want ErrInvalidPacket, got %v", err)
}

func TestDecodeBindingRequest_BindingResponse_Rejected(t *testing.T) {
	// A binding success response, not a request. Our wrapper must reject.
	m, err := pionstun.Build(
		pionstun.NewTransactionIDSetter(refTxID[:]),
		pionstun.BindingSuccess,
	)
	require.NoError(t, err)

	got, decErr := DecodeBindingRequest(m.Raw)

	assert.Equal(t, [12]byte{}, got)
	assert.True(t, errors.Is(decErr, ErrInvalidPacket), "want ErrInvalidPacket, got %v", decErr)
}

func TestDecodeBindingRequest_GarbageBytes(t *testing.T) {
	// Bytes that look like a STUN message length-wise but pion's
	// UnmarshalBinary fails on (e.g., wrong magic cookie).
	junk := make([]byte, 20)
	for i := range junk {
		junk[i] = 0xff
	}

	got, err := DecodeBindingRequest(junk)

	assert.Equal(t, [12]byte{}, got)
	assert.True(t, errors.Is(err, ErrInvalidPacket), "want ErrInvalidPacket, got %v", err)
}

func TestDecodeBindingRequest_UnknownComprehensionRequiredAttr(t *testing.T) {
	// Build a binding request, then add a raw attribute with type 0x0001
	// (USERNAME-class, comprehension-required, but not part of our
	// expected attr set) so our wrapper rejects it.
	m := new(pionstun.Message)
	m.TransactionID = refTxID
	m.Type = pionstun.MessageType{
		Method: pionstun.MethodBinding,
		Class:  pionstun.ClassRequest,
	}
	m.Add(pionstun.AttrType(0x0001), []byte{0xaa, 0xbb, 0xcc, 0xdd})
	m.Encode()

	got, err := DecodeBindingRequest(m.Raw)

	assert.Equal(t, [12]byte{}, got)
	assert.True(t, errors.Is(err, ErrInvalidPacket), "want ErrInvalidPacket, got %v", err)
}
```

- [ ] **Step 2: Run — must fail (functions undefined)**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker
go test ./internal/stunturn/...
```

Expected: build error mentioning `EncodeBindingResponse undefined` / `DecodeBindingRequest undefined`. (If `pion` is also missing, that means Task 1 reverted — re-add it; `go mod tidy` will pick the require back up after this task lands the import.)

- [ ] **Step 3: Implement `codec.go`**

Create `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/codec.go`:

```go
package stunturn

import (
	"fmt"
	"net"
	"net/netip"

	pionstun "github.com/pion/stun/v2"
)

// expectedRequestAttrs is the set of attribute types the binding-request
// validator accepts in the comprehension-required range. v1 expects an
// empty body — any other comprehension-required attr causes rejection.
//
// Comprehension-required attrs are 0x0000–0x7FFF; comprehension-optional
// attrs (0x8000–0xFFFF) are skipped per RFC 5389 §7.3.1 regardless.
var expectedRequestAttrs = map[pionstun.AttrType]struct{}{}

// DecodeBindingRequest unmarshals p with pion/stun, validates it is a
// binding request with the expected attribute set, and returns the
// 12-byte transaction ID. Returns ErrInvalidPacket (wrapping the
// underlying reason) on any failure.
func DecodeBindingRequest(p []byte) ([12]byte, error) {
	var m pionstun.Message
	if err := m.UnmarshalBinary(p); err != nil {
		return [12]byte{}, fmt.Errorf("%w: %v", ErrInvalidPacket, err)
	}
	if m.Type.Method != pionstun.MethodBinding || m.Type.Class != pionstun.ClassRequest {
		return [12]byte{}, fmt.Errorf("%w: not a binding request", ErrInvalidPacket)
	}
	for _, attr := range m.Attributes {
		if attr.Type >= 0x8000 {
			continue // comprehension-optional, skip
		}
		if _, ok := expectedRequestAttrs[attr.Type]; !ok {
			return [12]byte{}, fmt.Errorf(
				"%w: unknown comprehension-required attribute 0x%04x",
				ErrInvalidPacket, uint16(attr.Type),
			)
		}
	}
	var out [12]byte
	copy(out[:], m.TransactionID[:])
	return out, nil
}

// EncodeBindingResponse builds a STUN binding success response with one
// XOR-MAPPED-ADDRESS attribute and returns the wire bytes.
//
// Result length: 32 bytes for IPv4, 44 bytes for IPv6.
//
// Panics if observed.IsValid() == false. The listener owns input
// validation; passing a zero-value AddrPort is a programmer error.
func EncodeBindingResponse(txID [12]byte, observed netip.AddrPort) []byte {
	if !observed.IsValid() {
		panic("stunturn: EncodeBindingResponse: observed address invalid")
	}

	m := new(pionstun.Message)
	m.TransactionID = txID
	m.Type = pionstun.MessageType{
		Method: pionstun.MethodBinding,
		Class:  pionstun.ClassSuccessResponse,
	}

	addr := observed.Addr()
	var ip net.IP
	if addr.Is4() {
		v4 := addr.As4()
		ip = net.IP(v4[:])
	} else {
		v16 := addr.As16()
		ip = net.IP(v16[:])
	}

	xma := &pionstun.XORMappedAddress{IP: ip, Port: int(observed.Port())}
	if err := xma.AddTo(m); err != nil {
		// pion's AddTo only fails on programmer error (nil receiver,
		// invalid IP family). Both are caller-responsibility above.
		panic(fmt.Sprintf("stunturn: AddTo: %v", err))
	}
	m.Encode()

	out := make([]byte, len(m.Raw))
	copy(out, m.Raw)
	return out
}
```

- [ ] **Step 4: Re-tidy to pick up the pion require if Task 1 reverted**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker
go mod tidy
```

Expected: `tracker/go.mod` now contains `require github.com/pion/stun/v2 vX.Y.Z` (will stay, because `codec.go` imports it).

- [ ] **Step 5: Run — must pass**

```bash
go test -race -cover ./internal/stunturn/...
```

Expected: PASS for all codec tests.

- [ ] **Step 6: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn
gofumpt -l -w tracker/internal/stunturn/codec.go tracker/internal/stunturn/codec_test.go
git add tracker/internal/stunturn/codec.go tracker/internal/stunturn/codec_test.go tracker/go.mod tracker/go.sum
git commit -m "$(cat <<'EOF'
feat(tracker/stunturn): STUN codec wrappers via pion/stun/v2

DecodeBindingRequest validates inbound binding requests and rejects
non-binding-request types and unknown comprehension-required attrs.
EncodeBindingResponse builds a binding success response with one
XOR-MAPPED-ADDRESS attribute (IPv4 32B / IPv6 44B).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 7: Reflector

**Files:**
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/reflect.go`
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/reflect_test.go`

- [ ] **Step 1: Write failing tests**

Create `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/reflect_test.go`:

```go
package stunturn

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestReflect_IPv4_DelegatesToEncode(t *testing.T) {
	observed := netip.MustParseAddrPort("1.2.3.4:50001")

	got := Reflect(refTxID, observed)

	assert.Equal(t, observed, got.Observed)
	assert.Equal(t, EncodeBindingResponse(refTxID, observed), got.Response)
}

func TestReflect_IPv6_DelegatesToEncode(t *testing.T) {
	observed := netip.MustParseAddrPort("[2001:db8::1]:50001")

	got := Reflect(refTxID, observed)

	assert.Equal(t, observed, got.Observed)
	assert.Equal(t, EncodeBindingResponse(refTxID, observed), got.Response)
}

func TestReflect_InvalidAddr_EmptyResult(t *testing.T) {
	got := Reflect(refTxID, netip.AddrPort{})

	assert.False(t, got.Observed.IsValid())
	assert.Nil(t, got.Response)
}
```

- [ ] **Step 2: Run — must fail (Reflect / ReflectResult undefined)**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker
go test ./internal/stunturn/...
```

Expected: build error.

- [ ] **Step 3: Implement `reflect.go`**

Create `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/reflect.go`:

```go
package stunturn

import "net/netip"

// ReflectResult is the value Reflect returns. The listener writes
// Response to the remote endpoint at Observed.
type ReflectResult struct {
	Observed netip.AddrPort
	Response []byte
}

// Reflect builds a binding-response payload for the observed remote
// address. Returns a zero-value ReflectResult (empty Response, invalid
// Observed) when observed.IsValid() == false; the listener should drop
// the packet in that case.
func Reflect(txID [12]byte, observed netip.AddrPort) ReflectResult {
	if !observed.IsValid() {
		return ReflectResult{}
	}
	return ReflectResult{
		Observed: observed,
		Response: EncodeBindingResponse(txID, observed),
	}
}
```

- [ ] **Step 4: Run — must pass**

```bash
go test -race -cover ./internal/stunturn/...
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn
gofumpt -l -w tracker/internal/stunturn/reflect.go tracker/internal/stunturn/reflect_test.go
git add tracker/internal/stunturn/reflect.go tracker/internal/stunturn/reflect_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/stunturn): Reflect — listener-path STUN response helper

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 8: `AllocatorConfig` + `NewAllocator`

This task lays down the allocator scaffold (types, constructor, validation). The state-mutating methods land in subsequent tasks.

**Files:**
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/allocator.go`
- Create: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/allocator_test.go`

- [ ] **Step 1: Write failing construction tests**

Create `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/allocator_test.go`:

```go
package stunturn

import (
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validCfg returns an AllocatorConfig that NewAllocator accepts. Subtests
// mutate one field and assert the precise validation error.
func validCfg() AllocatorConfig {
	now := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	return AllocatorConfig{
		MaxKbpsPerSeeder: 1024,
		SessionTTL:       30 * time.Second,
		Now:              func() time.Time { return now },
		Rand:             rand.Reader,
	}
}

func TestNewAllocator_Valid(t *testing.T) {
	a, err := NewAllocator(validCfg())

	require.NoError(t, err)
	require.NotNil(t, a)
}

func TestNewAllocator_RejectsZeroKbps(t *testing.T) {
	cfg := validCfg()
	cfg.MaxKbpsPerSeeder = 0

	a, err := NewAllocator(cfg)

	assert.Nil(t, a)
	require.True(t, errors.Is(err, ErrInvalidConfig), "want ErrInvalidConfig, got %v", err)
	assert.True(t, strings.Contains(err.Error(), "MaxKbpsPerSeeder"), "error should name the field, got %q", err.Error())
}

func TestNewAllocator_RejectsNegativeKbps(t *testing.T) {
	cfg := validCfg()
	cfg.MaxKbpsPerSeeder = -1

	a, err := NewAllocator(cfg)

	assert.Nil(t, a)
	require.True(t, errors.Is(err, ErrInvalidConfig))
	assert.True(t, strings.Contains(err.Error(), "MaxKbpsPerSeeder"))
}

func TestNewAllocator_RejectsZeroTTL(t *testing.T) {
	cfg := validCfg()
	cfg.SessionTTL = 0

	a, err := NewAllocator(cfg)

	assert.Nil(t, a)
	require.True(t, errors.Is(err, ErrInvalidConfig))
	assert.True(t, strings.Contains(err.Error(), "SessionTTL"))
}

func TestNewAllocator_RejectsNilNow(t *testing.T) {
	cfg := validCfg()
	cfg.Now = nil

	a, err := NewAllocator(cfg)

	assert.Nil(t, a)
	require.True(t, errors.Is(err, ErrInvalidConfig))
	assert.True(t, strings.Contains(err.Error(), "Now"))
}

func TestNewAllocator_RejectsNilRand(t *testing.T) {
	cfg := validCfg()
	cfg.Rand = nil

	a, err := NewAllocator(cfg)

	assert.Nil(t, a)
	require.True(t, errors.Is(err, ErrInvalidConfig))
	assert.True(t, strings.Contains(err.Error(), "Rand"))
}
```

- [ ] **Step 2: Run — must fail to compile**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker
go test ./internal/stunturn/...
```

Expected: build error mentioning `AllocatorConfig undefined`, `NewAllocator undefined`.

- [ ] **Step 3: Implement scaffolding in `allocator.go`**

Create `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/allocator.go`:

```go
package stunturn

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
)

// AllocatorConfig parameterizes a *Allocator. Now and Rand are
// dependency-injected so tests run with deterministic time and bytes.
//
// Production wiring: cfg.MaxKbpsPerSeeder = STUNTURN.TURNRelayMaxKbps,
// cfg.SessionTTL = time.Duration(STUNTURN.SessionTTLSeconds) * time.Second,
// cfg.Now = time.Now, cfg.Rand = crypto/rand.Reader.
type AllocatorConfig struct {
	// MaxKbpsPerSeeder is the per-seeder bandwidth cap, in kilobits per
	// second. Must be > 0. Internally converted to bytes-per-second.
	MaxKbpsPerSeeder int

	// SessionTTL is the idle-expiry threshold. A session whose
	// LastActive is older than SessionTTL is treated as expired by
	// ResolveAndCharge and removed by Sweep. Must be > 0.
	SessionTTL time.Duration

	// Now returns the current time. Must be non-nil.
	Now func() time.Time

	// Rand is the source of randomness for token generation. Must be
	// non-nil. Production callers pass crypto/rand.Reader.
	Rand io.Reader
}

// Session is the public projection of a TURN session. Returned by
// Allocate / Resolve / ResolveAndCharge as a value; mutations to a
// returned Session do not affect allocator state.
type Session struct {
	Token       Token
	SessionID   uint64
	ConsumerID  ids.IdentityID
	SeederID    ids.IdentityID
	RequestID   [16]byte
	AllocatedAt time.Time
	LastActive  time.Time
}

// Allocator manages TURN session state. Safe for concurrent use; every
// public method holds an internal sync.Mutex. See package doc for the
// scaling notes and the documented forward path.
type Allocator struct {
	cfg AllocatorConfig

	mu      sync.Mutex
	nextID  uint64
	byToken map[Token]*sessionEntry
	bySID   map[uint64]*sessionEntry
	byReq   map[[16]byte]*sessionEntry
	buckets map[ids.IdentityID]*tokenBucket
}

// sessionEntry is the internal heap record. The same pointer is shared
// across byToken / bySID / byReq.
type sessionEntry struct {
	session Session
}

// tokenBucket models per-seeder kbps rate limiting. capacityBytes is one
// second of burst at MaxKbpsPerSeeder; refillPerSec equals capacityBytes
// (steady-state matches the cap).
type tokenBucket struct {
	capacityBytes float64
	refillPerSec  float64
	available     float64
	lastRefill    time.Time
}

// NewAllocator validates cfg and returns an empty Allocator.
func NewAllocator(cfg AllocatorConfig) (*Allocator, error) {
	if cfg.MaxKbpsPerSeeder <= 0 {
		return nil, fmt.Errorf("%w: MaxKbpsPerSeeder must be > 0, got %d",
			ErrInvalidConfig, cfg.MaxKbpsPerSeeder)
	}
	if cfg.SessionTTL <= 0 {
		return nil, fmt.Errorf("%w: SessionTTL must be > 0, got %s",
			ErrInvalidConfig, cfg.SessionTTL)
	}
	if cfg.Now == nil {
		return nil, fmt.Errorf("%w: Now must not be nil", ErrInvalidConfig)
	}
	if cfg.Rand == nil {
		return nil, fmt.Errorf("%w: Rand must not be nil", ErrInvalidConfig)
	}
	return &Allocator{
		cfg:     cfg,
		byToken: make(map[Token]*sessionEntry),
		bySID:   make(map[uint64]*sessionEntry),
		byReq:   make(map[[16]byte]*sessionEntry),
		buckets: make(map[ids.IdentityID]*tokenBucket),
	}, nil
}
```

- [ ] **Step 4: Run — all six construction tests pass**

```bash
go test -race -cover ./internal/stunturn/...
```

Expected: PASS for `TestNewAllocator_*`.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn
gofumpt -l -w tracker/internal/stunturn/allocator.go tracker/internal/stunturn/allocator_test.go
git add tracker/internal/stunturn/allocator.go tracker/internal/stunturn/allocator_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/stunturn): NewAllocator + AllocatorConfig

Lays down the type scaffolding (AllocatorConfig, Session, Allocator,
internal sessionEntry / tokenBucket) and the validating constructor.
State-mutating methods land in subsequent commits.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 9: `Allocate`

**Files:**
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/allocator.go`
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/allocator_test.go`

- [ ] **Step 1: Write failing tests for `Allocate`**

Append to `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/allocator_test.go`:

```go
// fixedClockCfg returns a cfg whose Now and Rand are deterministic.
// rand is bytes.NewReader(...) wrapped to refuse re-reads after exhaustion.
func fixedClockCfg(now time.Time, randBytes []byte) AllocatorConfig {
	return AllocatorConfig{
		MaxKbpsPerSeeder: 1024,
		SessionTTL:       30 * time.Second,
		Now:              func() time.Time { return now },
		Rand:             bytes.NewReader(randBytes),
	}
}

func mustAlloc(t *testing.T, a *Allocator, consumer, seeder ids.IdentityID, reqID [16]byte, now time.Time) Session {
	t.Helper()
	s, err := a.Allocate(consumer, seeder, reqID, now)
	require.NoError(t, err)
	return s
}

// id8 returns an IdentityID whose first byte is b (rest zero) — convenient
// for distinguishable test identities.
func id8(b byte) ids.IdentityID {
	var raw [32]byte
	raw[0] = b
	return ids.IdentityID(raw)
}

// req8 returns a [16]byte RequestID with first byte b.
func req8(b byte) [16]byte {
	var r [16]byte
	r[0] = b
	return r
}

func TestAllocate_HappyPath(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 16)
	for i := range tokBytes {
		tokBytes[i] = byte(i + 1) // 0x01..0x10
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)

	s, err := a.Allocate(id8(1), id8(2), req8(7), t0)

	require.NoError(t, err)
	assert.Equal(t, uint64(1), s.SessionID)
	assert.Equal(t, id8(1), s.ConsumerID)
	assert.Equal(t, id8(2), s.SeederID)
	assert.Equal(t, req8(7), s.RequestID)
	assert.Equal(t, t0, s.AllocatedAt)
	assert.Equal(t, t0, s.LastActive)
	wantTok := Token{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	assert.Equal(t, wantTok, s.Token)
}

func TestAllocate_AssignsDistinctSessionIDs(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	// 48 bytes = 3 distinct tokens
	tokBytes := make([]byte, 48)
	for i := range tokBytes {
		tokBytes[i] = byte(i)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)

	s1 := mustAlloc(t, a, id8(1), id8(2), req8(1), t0)
	s2 := mustAlloc(t, a, id8(1), id8(2), req8(2), t0)
	s3 := mustAlloc(t, a, id8(1), id8(2), req8(3), t0)

	assert.Equal(t, uint64(1), s1.SessionID)
	assert.Equal(t, uint64(2), s2.SessionID)
	assert.Equal(t, uint64(3), s3.SessionID)
	assert.NotEqual(t, s1.Token, s2.Token)
	assert.NotEqual(t, s2.Token, s3.Token)
}

func TestAllocate_DuplicateRequestID(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 32) // enough for two attempts
	for i := range tokBytes {
		tokBytes[i] = byte(i)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)
	s1 := mustAlloc(t, a, id8(1), id8(2), req8(7), t0)

	s2, err2 := a.Allocate(id8(1), id8(2), req8(7), t0)

	require.True(t, errors.Is(err2, ErrDuplicateRequest), "want ErrDuplicateRequest, got %v", err2)
	assert.Equal(t, Session{}, s2)
	// Original is unchanged.
	assert.Equal(t, uint64(1), s1.SessionID)
}

// errReader always errors. Used to simulate Rand failure.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("simulated rand failure") }

func TestAllocate_RandFailure(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	cfg := AllocatorConfig{
		MaxKbpsPerSeeder: 1024,
		SessionTTL:       30 * time.Second,
		Now:              func() time.Time { return t0 },
		Rand:             errReader{},
	}
	a, err := NewAllocator(cfg)
	require.NoError(t, err)

	s, err := a.Allocate(id8(1), id8(2), req8(1), t0)

	assert.Equal(t, Session{}, s)
	require.True(t, errors.Is(err, ErrRandFailed), "want ErrRandFailed, got %v", err)
	assert.Contains(t, err.Error(), "simulated rand failure")
}
```

Add the imports the new tests need at the top of `allocator_test.go`:

```go
import (
	"bytes"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/token-bay/token-bay/shared/ids"
)
```

- [ ] **Step 2: Run — must fail (Allocate undefined)**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker
go test ./internal/stunturn/...
```

Expected: build error.

- [ ] **Step 3: Implement `Allocate` and the small bucket helper**

Append to `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/allocator.go`:

```go
// Allocate creates a new TURN session for the given consumer, seeder,
// and requestID. Returns ErrDuplicateRequest if a live session already
// exists for requestID; ErrRandFailed if the injected Rand fails.
//
// The returned Session is a value copy; mutating it does not affect
// allocator state.
func (a *Allocator) Allocate(consumer, seeder ids.IdentityID, requestID [16]byte, now time.Time) (Session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, dup := a.byReq[requestID]; dup {
		return Session{}, ErrDuplicateRequest
	}
	var tokBuf [16]byte
	if _, err := io.ReadFull(a.cfg.Rand, tokBuf[:]); err != nil {
		return Session{}, fmt.Errorf("%w: %v", ErrRandFailed, err)
	}
	tok := Token(tokBuf)
	if _, collide := a.byToken[tok]; collide {
		// 2^-128 with crypto/rand; treat as a hard error rather than
		// retry-loop. Tests inject deterministic streams, so a real
		// collision would be a test bug we want to surface.
		return Session{}, fmt.Errorf("%w: token collision", ErrRandFailed)
	}

	a.nextID++
	entry := &sessionEntry{session: Session{
		Token:       tok,
		SessionID:   a.nextID,
		ConsumerID:  consumer,
		SeederID:    seeder,
		RequestID:   requestID,
		AllocatedAt: now,
		LastActive:  now,
	}}
	a.byToken[tok] = entry
	a.bySID[entry.session.SessionID] = entry
	a.byReq[requestID] = entry
	a.ensureBucket(seeder, now)
	return entry.session, nil
}

// ensureBucket initializes the per-seeder token bucket if absent.
// Must be called with a.mu held.
func (a *Allocator) ensureBucket(seederID ids.IdentityID, now time.Time) *tokenBucket {
	if b, ok := a.buckets[seederID]; ok {
		return b
	}
	cap := float64(a.cfg.MaxKbpsPerSeeder) * 1024.0 / 8.0
	b := &tokenBucket{
		capacityBytes: cap,
		refillPerSec:  cap, // 1s of burst, refilled at the cap rate
		available:     cap, // start full
		lastRefill:    now,
	}
	a.buckets[seederID] = b
	return b
}

// deleteIndexes removes entry from byToken / bySID / byReq. The
// caller must hold a.mu. Buckets are not touched.
func (a *Allocator) deleteIndexes(entry *sessionEntry) {
	delete(a.byToken, entry.session.Token)
	delete(a.bySID, entry.session.SessionID)
	delete(a.byReq, entry.session.RequestID)
}
```

- [ ] **Step 4: Run — must pass**

```bash
go test -race -cover ./internal/stunturn/...
```

Expected: PASS for `TestAllocate_*` and all earlier tests.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn
gofumpt -l -w tracker/internal/stunturn/allocator.go tracker/internal/stunturn/allocator_test.go
git add tracker/internal/stunturn/allocator.go tracker/internal/stunturn/allocator_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/stunturn): Allocator.Allocate + ensureBucket / deleteIndexes

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 10: `Resolve` (read-only peek)

**Files:**
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/allocator.go`
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/allocator_test.go`

- [ ] **Step 1: Write failing tests**

Append to `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/allocator_test.go`:

```go
func TestResolve_Hit(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 16)
	for i := range tokBytes {
		tokBytes[i] = byte(i + 1)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)
	want := mustAlloc(t, a, id8(1), id8(2), req8(1), t0)

	got, ok := a.Resolve(want.Token, t0)

	require.True(t, ok)
	assert.Equal(t, want, got)
}

func TestResolve_UnknownToken(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	a, err := NewAllocator(fixedClockCfg(t0, make([]byte, 16)))
	require.NoError(t, err)

	got, ok := a.Resolve(Token{0xff}, t0)

	assert.False(t, ok)
	assert.Equal(t, Session{}, got)
}

// TestResolve_DoesNotUpdateLastActive: Resolve is a peek only; it
// must not extend the session's life. Session expires at LastActive +
// SessionTTL regardless of Resolve calls.
func TestResolve_DoesNotUpdateLastActive(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 16)
	for i := range tokBytes {
		tokBytes[i] = byte(i + 1)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)
	want := mustAlloc(t, a, id8(1), id8(2), req8(1), t0)

	// Resolve at a later time.
	later := t0.Add(10 * time.Second)
	got, ok := a.Resolve(want.Token, later)

	require.True(t, ok)
	assert.Equal(t, t0, got.LastActive,
		"Resolve must not advance LastActive (peek-only contract)")
}

// TestResolve_ReturnsCopy: mutating the returned Session must not
// change subsequent Resolve results.
func TestResolve_ReturnsCopy(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 16)
	for i := range tokBytes {
		tokBytes[i] = byte(i + 1)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)
	want := mustAlloc(t, a, id8(1), id8(2), req8(1), t0)

	first, _ := a.Resolve(want.Token, t0)
	first.LastActive = first.LastActive.Add(1000 * time.Hour) // mutate caller copy

	second, ok := a.Resolve(want.Token, t0)
	require.True(t, ok)
	assert.Equal(t, t0, second.LastActive,
		"allocator state must not be mutable through a returned Session")
	_ = strings.TrimSpace // keep strings import live if other tests don't yet use it
}
```

- [ ] **Step 2: Run — must fail (Resolve undefined)**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker
go test ./internal/stunturn/...
```

Expected: build error.

- [ ] **Step 3: Implement `Resolve`**

Append to `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/allocator.go`:

```go
// Resolve looks up a session by token without updating LastActive and
// without expiring an idle entry. Returns (Session{}, false) when the
// token is unknown. The returned Session is a value copy.
//
// Resolve is intended for diagnostics / admin paths. The hot
// per-datagram path is ResolveAndCharge.
func (a *Allocator) Resolve(tok Token, _ time.Time) (Session, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.byToken[tok]
	if !ok {
		return Session{}, false
	}
	return entry.session, true
}
```

(The `time.Time` parameter is unused on the read-only path but kept for API symmetry with `ResolveAndCharge`. Underscore-naming the parameter avoids `unused-parameter` style warnings; revive accepts it.)

- [ ] **Step 4: Run — must pass**

```bash
go test -race -cover ./internal/stunturn/...
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn
gofumpt -l -w tracker/internal/stunturn/allocator.go tracker/internal/stunturn/allocator_test.go
git add tracker/internal/stunturn/allocator.go tracker/internal/stunturn/allocator_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/stunturn): Allocator.Resolve — read-only session peek

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 11: `ResolveAndCharge` (hot path) + token bucket refill

**Files:**
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/allocator.go`
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/allocator_test.go`

- [ ] **Step 1: Write failing tests**

Append to `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/allocator_test.go`:

```go
func TestResolveAndCharge_Happy(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 16)
	for i := range tokBytes {
		tokBytes[i] = byte(i + 1)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)
	want := mustAlloc(t, a, id8(1), id8(2), req8(1), t0)

	got, err := a.ResolveAndCharge(want.Token, 1500, t0)

	require.NoError(t, err)
	assert.Equal(t, want.SessionID, got.SessionID)
	assert.Equal(t, t0, got.LastActive)
}

func TestResolveAndCharge_UnknownToken(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	a, err := NewAllocator(fixedClockCfg(t0, make([]byte, 16)))
	require.NoError(t, err)

	_, err = a.ResolveAndCharge(Token{0xff}, 1500, t0)

	require.True(t, errors.Is(err, ErrUnknownToken), "want ErrUnknownToken, got %v", err)
}

// TestResolveAndCharge_Throttled: at MaxKbpsPerSeeder=1024, capacity
// is 1024*1024/8 = 131072 bytes. Charge 200_000 fails; bucket is NOT
// debited so a follow-up small charge succeeds.
func TestResolveAndCharge_Throttled(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 16)
	for i := range tokBytes {
		tokBytes[i] = byte(i + 1)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)
	want := mustAlloc(t, a, id8(1), id8(2), req8(1), t0)

	_, err = a.ResolveAndCharge(want.Token, 200_000, t0)
	require.True(t, errors.Is(err, ErrThrottled))

	// Bucket was not debited — a small charge still succeeds.
	_, err = a.ResolveAndCharge(want.Token, 100, t0)
	require.NoError(t, err)
}

// TestResolveAndCharge_RefillsOverTime: exhaust the bucket, advance
// 0.5s, half-capacity should be available.
func TestResolveAndCharge_RefillsOverTime(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 16)
	for i := range tokBytes {
		tokBytes[i] = byte(i + 1)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)
	want := mustAlloc(t, a, id8(1), id8(2), req8(1), t0)

	// 131072 = capacity; drain it.
	_, err = a.ResolveAndCharge(want.Token, 131072, t0)
	require.NoError(t, err)
	// Same instant — fully drained.
	_, err = a.ResolveAndCharge(want.Token, 1, t0)
	require.True(t, errors.Is(err, ErrThrottled))

	// Half a second later — half capacity (65536) refilled.
	half := t0.Add(500 * time.Millisecond)
	_, err = a.ResolveAndCharge(want.Token, 65000, half)
	require.NoError(t, err)
	_, err = a.ResolveAndCharge(want.Token, 1000, half)
	require.True(t, errors.Is(err, ErrThrottled), "remaining bucket should be ~536 bytes; 1000 must throttle")
}

// TestResolveAndCharge_ExpiresOnIdle: SessionTTL=30s; advance 31s
// without activity; first call returns ErrSessionExpired (and deletes
// the entry); follow-up returns ErrUnknownToken.
func TestResolveAndCharge_ExpiresOnIdle(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 16)
	for i := range tokBytes {
		tokBytes[i] = byte(i + 1)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)
	want := mustAlloc(t, a, id8(1), id8(2), req8(1), t0)

	expired := t0.Add(31 * time.Second)
	_, err = a.ResolveAndCharge(want.Token, 100, expired)
	require.True(t, errors.Is(err, ErrSessionExpired))

	_, err = a.ResolveAndCharge(want.Token, 100, expired)
	require.True(t, errors.Is(err, ErrUnknownToken),
		"after ErrSessionExpired the entry must be deleted")
}

// TestResolveAndCharge_LastActiveAdvances: a session that gets touched
// every 10s does not expire at the 30s mark.
func TestResolveAndCharge_LastActiveAdvances(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 16)
	for i := range tokBytes {
		tokBytes[i] = byte(i + 1)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)
	want := mustAlloc(t, a, id8(1), id8(2), req8(1), t0)

	for i := 1; i <= 4; i++ {
		_, err := a.ResolveAndCharge(want.Token, 100, t0.Add(time.Duration(i*10)*time.Second))
		require.NoError(t, err, "call %d at +%ds must succeed", i, i*10)
	}
}

// TestResolveAndCharge_ClockBackwards: a clock that goes backward must
// not produce negative refill (no underflow).
func TestResolveAndCharge_ClockBackwards(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 16)
	for i := range tokBytes {
		tokBytes[i] = byte(i + 1)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)
	want := mustAlloc(t, a, id8(1), id8(2), req8(1), t0)

	// Drain the bucket at t0.
	_, err = a.ResolveAndCharge(want.Token, 131072, t0)
	require.NoError(t, err)

	// Time goes backwards; available must remain 0 (no negative refill,
	// no overflow). The 1-byte ask still throttles.
	_, err = a.ResolveAndCharge(want.Token, 1, t0.Add(-1*time.Second))
	require.True(t, errors.Is(err, ErrThrottled))
}

// TestResolveAndCharge_NoBytes_KeepsAlive: charging 0 bytes (or
// negative) does not throttle and DOES update LastActive — the listener
// observed a packet, the session is alive.
func TestResolveAndCharge_NoBytes_KeepsAlive(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 16)
	for i := range tokBytes {
		tokBytes[i] = byte(i + 1)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)
	want := mustAlloc(t, a, id8(1), id8(2), req8(1), t0)

	// Drain the bucket so any debit would throttle.
	_, err = a.ResolveAndCharge(want.Token, 131072, t0)
	require.NoError(t, err)

	got, err := a.ResolveAndCharge(want.Token, 0, t0.Add(20*time.Second))
	require.NoError(t, err, "n=0 should never throttle")
	assert.Equal(t, t0.Add(20*time.Second), got.LastActive)
}
```

- [ ] **Step 2: Run — must fail (ResolveAndCharge undefined)**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker
go test ./internal/stunturn/...
```

Expected: build error.

- [ ] **Step 3: Implement `ResolveAndCharge` and `refill`**

Append to `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/allocator.go`:

```go
// ResolveAndCharge atomically (under one mutex acquisition) resolves
// tok, refreshes the seeder's bucket, debits n bytes if n > 0,
// updates LastActive, and returns the session.
//
// Errors:
//   ErrUnknownToken   — tok not in allocator (or expired and deleted)
//   ErrSessionExpired — LastActive older than SessionTTL; entry deleted
//                       on this call (subsequent calls return
//                       ErrUnknownToken)
//   ErrThrottled      — bucket has < n bytes available; bucket NOT
//                       debited
//
// n <= 0 skips the bucket check and debit but DOES update LastActive
// (the listener observed a packet — the session is alive even if the
// payload is a zero-byte keepalive).
func (a *Allocator) ResolveAndCharge(tok Token, n int, now time.Time) (Session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	entry, ok := a.byToken[tok]
	if !ok {
		return Session{}, ErrUnknownToken
	}
	if now.Sub(entry.session.LastActive) > a.cfg.SessionTTL {
		a.deleteIndexes(entry)
		return Session{}, ErrSessionExpired
	}
	if n > 0 {
		b := a.buckets[entry.session.SeederID]
		if b == nil {
			// Allocate created the bucket; this is an internal
			// invariant. Surface it loudly.
			panic("stunturn: bucket missing for known seeder; allocator invariants violated")
		}
		refill(b, now)
		if b.available < float64(n) {
			return Session{}, ErrThrottled
		}
		b.available -= float64(n)
	}
	entry.session.LastActive = now
	return entry.session, nil
}

// refill brings b.available up to date based on elapsed time since
// b.lastRefill. Caller must hold the allocator's mutex. Capped at
// b.capacityBytes; clock-backwards is a no-op.
func refill(b *tokenBucket, now time.Time) {
	elapsed := now.Sub(b.lastRefill).Seconds()
	if elapsed <= 0 {
		return
	}
	b.available += elapsed * b.refillPerSec
	if b.available > b.capacityBytes {
		b.available = b.capacityBytes
	}
	b.lastRefill = now
}
```

- [ ] **Step 4: Run — must pass**

```bash
go test -race -cover ./internal/stunturn/...
```

Expected: PASS for all `TestResolveAndCharge_*` tests.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn
gofumpt -l -w tracker/internal/stunturn/allocator.go tracker/internal/stunturn/allocator_test.go
git add tracker/internal/stunturn/allocator.go tracker/internal/stunturn/allocator_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/stunturn): Allocator.ResolveAndCharge + token-bucket refill

Hot-path single-mutex call: lookup, expiry check, refill, debit,
LastActive update. Refill is event-driven (computed on each call from
elapsed time) and clock-backwards safe.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 12: `Charge` (bucket-only)

**Files:**
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/allocator.go`
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/allocator_test.go`

- [ ] **Step 1: Write failing tests**

Append to `allocator_test.go`:

```go
func TestCharge_Happy(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	a, err := NewAllocator(fixedClockCfg(t0, make([]byte, 16)))
	require.NoError(t, err)

	require.NoError(t, a.Charge(id8(2), 1000, t0))
}

func TestCharge_ZeroAndNegativeAreNoOps(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	a, err := NewAllocator(fixedClockCfg(t0, make([]byte, 16)))
	require.NoError(t, err)

	// Drain bucket via a real Charge.
	require.NoError(t, a.Charge(id8(2), 131072, t0))

	// 0 and negative must not return ErrThrottled even though bucket is empty.
	require.NoError(t, a.Charge(id8(2), 0, t0))
	require.NoError(t, a.Charge(id8(2), -1, t0))
}

func TestCharge_LazyBucketInitForUnknownSeeder(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	a, err := NewAllocator(fixedClockCfg(t0, make([]byte, 16)))
	require.NoError(t, err)

	// Seeder never had Allocate called; Charge still works.
	require.NoError(t, a.Charge(id8(99), 1000, t0))
}

func TestCharge_Throttled(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	a, err := NewAllocator(fixedClockCfg(t0, make([]byte, 16)))
	require.NoError(t, err)

	require.NoError(t, a.Charge(id8(2), 131072, t0)) // drain
	err = a.Charge(id8(2), 1, t0)
	require.True(t, errors.Is(err, ErrThrottled), "want ErrThrottled, got %v", err)
}
```

- [ ] **Step 2: Run — must fail (Charge undefined)**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker
go test ./internal/stunturn/...
```

Expected: build error.

- [ ] **Step 3: Implement `Charge`**

Append to `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/allocator.go`:

```go
// Charge debits n bytes from the seeder's bucket without touching
// session state. Use this only when the listener has already resolved
// the session and just needs to bill more bytes; otherwise use
// ResolveAndCharge.
//
// n <= 0 returns nil without modifying the bucket. The seeder's bucket
// is lazy-initialized on first use, so Charge succeeds for seeders that
// never had Allocate called.
func (a *Allocator) Charge(seederID ids.IdentityID, n int, now time.Time) error {
	if n <= 0 {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	b := a.ensureBucket(seederID, now)
	refill(b, now)
	if b.available < float64(n) {
		return ErrThrottled
	}
	b.available -= float64(n)
	return nil
}
```

- [ ] **Step 4: Run — must pass**

```bash
go test -race -cover ./internal/stunturn/...
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn
gofumpt -l -w tracker/internal/stunturn/allocator.go tracker/internal/stunturn/allocator_test.go
git add tracker/internal/stunturn/allocator.go tracker/internal/stunturn/allocator_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/stunturn): Allocator.Charge — bucket-only debit, lazy init

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 13: `Release`

**Files:**
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/allocator.go`
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/allocator_test.go`

- [ ] **Step 1: Write failing tests**

Append to `allocator_test.go`:

```go
func TestRelease_RemovesAllIndexes(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 16)
	for i := range tokBytes {
		tokBytes[i] = byte(i + 1)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)
	s := mustAlloc(t, a, id8(1), id8(2), req8(7), t0)

	a.Release(s.SessionID)

	// All three indexes report unknown.
	_, ok := a.Resolve(s.Token, t0)
	assert.False(t, ok, "byToken should be empty")
	_, err = a.ResolveAndCharge(s.Token, 100, t0)
	assert.True(t, errors.Is(err, ErrUnknownToken))

	// byReq is freed too — we can re-Allocate the same RequestID.
	tokBytes2 := []byte{
		0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00, 0x11,
		0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99,
	}
	cfg := fixedClockCfg(t0, tokBytes2)
	a2, err := NewAllocator(cfg)
	require.NoError(t, err)
	s1 := mustAlloc(t, a2, id8(1), id8(2), req8(7), t0)
	a2.Release(s1.SessionID)
	// Allocate again with the same RequestID; must not return
	// ErrDuplicateRequest.
	cfg.Rand = bytes.NewReader([]byte{
		0x10, 0x20, 0x30, 0x40, 0x50, 0x60, 0x70, 0x80,
		0x90, 0xa0, 0xb0, 0xc0, 0xd0, 0xe0, 0xf0, 0x01,
	})
	a3, err := NewAllocator(cfg)
	require.NoError(t, err)
	_, err = a3.Allocate(id8(1), id8(2), req8(7), t0)
	require.NoError(t, err)
}

func TestRelease_UnknownSID_NoPanic(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	a, err := NewAllocator(fixedClockCfg(t0, make([]byte, 16)))
	require.NoError(t, err)

	assert.NotPanics(t, func() { a.Release(0) })
	assert.NotPanics(t, func() { a.Release(99999) })
}

func TestRelease_Idempotent(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 16)
	for i := range tokBytes {
		tokBytes[i] = byte(i + 1)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)
	s := mustAlloc(t, a, id8(1), id8(2), req8(7), t0)

	a.Release(s.SessionID)
	assert.NotPanics(t, func() { a.Release(s.SessionID) })
}
```

- [ ] **Step 2: Run — must fail (Release undefined)**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker
go test ./internal/stunturn/...
```

Expected: build error.

- [ ] **Step 3: Implement `Release`**

Append to `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/allocator.go`:

```go
// Release deletes the session by SessionID. Idempotent — a no-op for
// unknown SessionIDs. The seeder's bucket is left in place (it is
// shared across that seeder's sessions and self-decays via refill
// arithmetic).
func (a *Allocator) Release(sessionID uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.bySID[sessionID]
	if !ok {
		return
	}
	a.deleteIndexes(entry)
}
```

- [ ] **Step 4: Run — must pass**

```bash
go test -race -cover ./internal/stunturn/...
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn
gofumpt -l -w tracker/internal/stunturn/allocator.go tracker/internal/stunturn/allocator_test.go
git add tracker/internal/stunturn/allocator.go tracker/internal/stunturn/allocator_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/stunturn): Allocator.Release — idempotent index cleanup

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 14: `Sweep`

**Files:**
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/allocator.go`
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/allocator_test.go`

- [ ] **Step 1: Write failing tests**

Append to `allocator_test.go`:

```go
func TestSweep_Empty(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	a, err := NewAllocator(fixedClockCfg(t0, make([]byte, 16)))
	require.NoError(t, err)

	assert.Equal(t, 0, a.Sweep(t0))
}

func TestSweep_RemovesIdleSessions(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 32) // two tokens
	for i := range tokBytes {
		tokBytes[i] = byte(i + 1)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)
	s1 := mustAlloc(t, a, id8(1), id8(2), req8(1), t0)
	s2 := mustAlloc(t, a, id8(1), id8(2), req8(2), t0)

	swept := a.Sweep(t0.Add(31 * time.Second))

	assert.Equal(t, 2, swept)
	_, ok := a.Resolve(s1.Token, t0)
	assert.False(t, ok)
	_, ok = a.Resolve(s2.Token, t0)
	assert.False(t, ok)
}

func TestSweep_KeepsActiveSessions(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 16)
	for i := range tokBytes {
		tokBytes[i] = byte(i + 1)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)
	s := mustAlloc(t, a, id8(1), id8(2), req8(1), t0)

	// Touch the session at +15s.
	_, err = a.ResolveAndCharge(s.Token, 100, t0.Add(15*time.Second))
	require.NoError(t, err)

	// Sweep at +31s — LastActive is +15s, so age is 16s < 30s TTL: keep.
	swept := a.Sweep(t0.Add(31 * time.Second))
	assert.Equal(t, 0, swept)
	_, ok := a.Resolve(s.Token, t0)
	assert.True(t, ok)
}

func TestSweep_DoesNotTouchBuckets(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 16)
	for i := range tokBytes {
		tokBytes[i] = byte(i + 1)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)
	s := mustAlloc(t, a, id8(1), id8(2), req8(1), t0)

	// Drain the bucket so we can detect bucket re-init.
	_, err = a.ResolveAndCharge(s.Token, 131072, t0)
	require.NoError(t, err)

	// Sweep removes the session.
	require.Equal(t, 1, a.Sweep(t0.Add(31*time.Second)))

	// The bucket survived. A subsequent same-second Charge throttles
	// (would succeed if Sweep had wiped buckets and re-init filled it).
	err = a.Charge(id8(2), 1, t0.Add(31*time.Second))
	require.True(t, errors.Is(err, ErrThrottled),
		"bucket should persist post-Sweep; got %v", err)
}
```

- [ ] **Step 2: Run — must fail (Sweep undefined)**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker
go test ./internal/stunturn/...
```

Expected: build error.

- [ ] **Step 3: Implement `Sweep`**

Append to `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/allocator.go`:

```go
// Sweep removes every session whose LastActive is older than
// SessionTTL. Returns the count removed. Buckets are not touched.
//
// Intended to run from a single goroutine on a periodic timer (e.g.,
// every 1s). Calling from multiple goroutines is correct but wasteful.
func (a *Allocator) Sweep(now time.Time) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, entry := range a.byToken {
		if now.Sub(entry.session.LastActive) > a.cfg.SessionTTL {
			a.deleteIndexes(entry)
			n++
		}
	}
	return n
}
```

Note: deleting from a map during a `for ... range map` is safe in Go (the spec explicitly allows it). The reference run is over `a.byToken`; `deleteIndexes` removes from all three maps.

- [ ] **Step 4: Run — must pass**

```bash
go test -race -cover ./internal/stunturn/...
```

Expected: PASS for all `TestSweep_*` tests.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn
gofumpt -l -w tracker/internal/stunturn/allocator.go tracker/internal/stunturn/allocator_test.go
git add tracker/internal/stunturn/allocator.go tracker/internal/stunturn/allocator_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/stunturn): Allocator.Sweep — TTL-based session GC

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 15: Concurrency stress tests

**Files:**
- Modify: `/Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker/internal/stunturn/allocator_test.go`

- [ ] **Step 1: Write the stress tests**

Append to `allocator_test.go`:

```go
// TestConcurrent_AllocateResolveCharge fans out N goroutines that each
// allocate, hit the session with K ResolveAndCharge calls, then release.
// Race detector must stay clean and the final state must be empty.
func TestConcurrent_AllocateResolveCharge(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	const N, K = 16, 32
	cfg := AllocatorConfig{
		MaxKbpsPerSeeder: 1024,
		SessionTTL:       30 * time.Second,
		Now:              func() time.Time { return t0 },
		Rand:             rand.Reader, // real randomness for unique tokens
	}
	a, err := NewAllocator(cfg)
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			var reqID [16]byte
			binary.BigEndian.PutUint64(reqID[:], uint64(i))
			s, err := a.Allocate(id8(byte(i+1)), id8(byte(i+1)), reqID, t0)
			if err != nil {
				t.Errorf("Allocate: %v", err)
				return
			}
			for j := 0; j < K; j++ {
				if _, err := a.ResolveAndCharge(s.Token, 100, t0); err != nil {
					t.Errorf("RAC: %v", err)
					return
				}
			}
			a.Release(s.SessionID)
		}()
	}
	wg.Wait()

	// Final state: zero sessions left.
	assert.Equal(t, 0, a.Sweep(t0.Add(time.Hour)),
		"all sessions should already be Released; Sweep finds nothing")
}

// TestConcurrent_DuplicateRequestRace: N goroutines race to Allocate
// the same RequestID. Exactly one wins; the rest see ErrDuplicateRequest.
func TestConcurrent_DuplicateRequestRace(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	const N = 32
	cfg := AllocatorConfig{
		MaxKbpsPerSeeder: 1024,
		SessionTTL:       30 * time.Second,
		Now:              func() time.Time { return t0 },
		Rand:             rand.Reader,
	}
	a, err := NewAllocator(cfg)
	require.NoError(t, err)
	reqID := req8(42)

	var wg sync.WaitGroup
	wg.Add(N)
	results := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, err := a.Allocate(id8(1), id8(2), reqID, t0)
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	wins, dupes := 0, 0
	for err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrDuplicateRequest):
			dupes++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	assert.Equal(t, 1, wins, "exactly one Allocate should win")
	assert.Equal(t, N-1, dupes, "the rest should see ErrDuplicateRequest")
}

// TestConcurrent_SweepWithChurn runs Sweep on a tight ticker against
// goroutines that allocate/release. Race detector clean; no panics.
func TestConcurrent_SweepWithChurn(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	cfg := AllocatorConfig{
		MaxKbpsPerSeeder: 1024,
		SessionTTL:       30 * time.Second,
		Now:              func() time.Time { return t0 },
		Rand:             rand.Reader,
	}
	a, err := NewAllocator(cfg)
	require.NoError(t, err)

	stop := make(chan struct{})

	// Sweeper.
	var sweepWG sync.WaitGroup
	sweepWG.Add(1)
	go func() {
		defer sweepWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = a.Sweep(t0.Add(time.Hour)) // ages everything; deletes whatever's there
			}
		}
	}()

	// Workers.
	var wg sync.WaitGroup
	const N, K = 8, 64
	for i := 0; i < N; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < K; j++ {
				var reqID [16]byte
				binary.BigEndian.PutUint64(reqID[:], uint64(i*K+j))
				s, err := a.Allocate(id8(byte(i+1)), id8(byte(i+1)), reqID, t0)
				if err != nil {
					// Sweep may have raced and deleted; tolerate
					// nothing other than ErrDuplicateRequest is
					// expected here, and we never reuse reqID, so
					// the only acceptable outcome is success.
					t.Errorf("Allocate: %v", err)
					return
				}
				_, _ = a.ResolveAndCharge(s.Token, 1, t0) // may race-with-Sweep; ok
				a.Release(s.SessionID)
			}
		}()
	}
	wg.Wait()
	close(stop)
	sweepWG.Wait()
}
```

Add the new imports at the top of `allocator_test.go`:

```go
	"encoding/binary"
	"sync"
```

(Keep all existing imports — these go alongside.)

- [ ] **Step 2: Run with race detector**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker
go test -race ./internal/stunturn/...
```

Expected: PASS, no race-detector warnings.

- [ ] **Step 3: Coverage check**

```bash
go test -race -cover ./internal/stunturn/...
```

Expected: each file ≥ 90% coverage on the rollup. If coverage drops on any file below 90%, find the un-covered lines via:

```bash
go test -race -coverprofile=cov.out ./internal/stunturn/...
go tool cover -func=cov.out | sort -k3 -n
```

and add a small focused test for the gap.

- [ ] **Step 4: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn
gofumpt -l -w tracker/internal/stunturn/allocator_test.go
git add tracker/internal/stunturn/allocator_test.go
git commit -m "$(cat <<'EOF'
test(tracker/stunturn): concurrency stress — Allocate/RAC/Release/Sweep

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 16: Lint pass + final verification

**Files:**
- All under `tracker/internal/stunturn/`

- [ ] **Step 1: Run gofumpt and golangci-lint per-module**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker
gofumpt -l ./internal/stunturn
```

Expected: no output (every file already formatted). If any file is listed, run:

```bash
gofumpt -l -w ./internal/stunturn
```

and amend the most recent commit (`git add -u && git commit --amend --no-edit`).

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker
golangci-lint run ./internal/stunturn/...
```

Expected: no findings. If revive flags an exported symbol without a doc comment, add the comment and amend the relevant commit.

- [ ] **Step 2: Run `go vet`**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker
go vet ./internal/stunturn/...
```

Expected: no findings.

- [ ] **Step 3: Final race + coverage roll-up**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker
go test -race -cover ./internal/stunturn/...
```

Expected: all tests pass; per-package coverage ≥ 90%.

- [ ] **Step 4: Build the whole tracker module**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker
go build ./...
```

Expected: no errors.

- [ ] **Step 5: Run the full tracker test suite**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker/internal/stunturn/tracker
go test -race ./...
```

Expected: PASS — including config tests that depend on the new `SessionTTLSeconds` field.

- [ ] **Step 6: No commit needed** unless any prior step required a fix.

---

## Self-review checklist

Run this after writing the plan, before handing off.

**Spec coverage:**
- §2.1 STUN codec via pion → Task 6.
- §2.1 Reflect → Task 7.
- §2.1 Allocator + lifecycle methods → Tasks 8–14.
- §2.1 Per-seeder kbps token bucket → Task 11 + Task 12.
- §2.1 Idle expiry + Sweep → Tasks 11, 14.
- §2.2 Out-of-scope items: not in any task (correct).
- §4 Public surface: every exported symbol lands in Tasks 4–14.
- §4.1 Sentinel errors → Task 4.
- §6 Algorithms: Allocate (9), Resolve (10), ResolveAndCharge (11), Charge (12), Release (13), Sweep (14), refill math (11).
- §7 Concurrency: single mutex, established in Task 8 (`sync.Mutex` field) and exercised in Task 15.
- §9 Configuration → Task 2.
- §10 Testing strategy: codec (6), reflector (7), allocator unit tests (8–14), concurrency (15).
- §12 Acceptance criteria: race-clean, ≥90% coverage, lint-clean, doc.go forward reference, exported godocs — all covered by Task 16's verification gates and Task 3's doc.go.

**Placeholder scan:** zero `TBD` / `TODO` / `placeholder` in plan body. Every step contains complete code or an exact command.

**Type consistency:**
- `Token` introduced Task 5; used in Tasks 6, 8–15.
- `Session` introduced Task 8; used in Tasks 9–14.
- `AllocatorConfig` field names match Task 8 (`MaxKbpsPerSeeder`, `SessionTTL`, `Now`, `Rand`) throughout.
- `ReflectResult` introduced Task 7; not referenced again (terminal type).
- Method signatures: `Allocate(consumer, seeder ids.IdentityID, requestID [16]byte, now time.Time)` consistent across §4 of spec, Task 8 (test setup), and Task 9 (impl).
- `STUNTURNConfig.SessionTTLSeconds`: defined Task 2; spec §9 references it; allocator consumes it via `AllocatorConfig.SessionTTL` (caller computes `time.Duration(SessionTTLSeconds) * time.Second` in `internal/server` wiring — out of scope here, noted in Task 8 godoc).

No gaps; ready to execute.
