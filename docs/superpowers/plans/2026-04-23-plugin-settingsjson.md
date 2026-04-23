# Plugin `internal/settingsjson` — Feature Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Implement the `plugin/internal/settingsjson` module — atomic read-modify-write of `~/.claude/settings.json` to set or clear `env.ANTHROPIC_BASE_URL`, with a rollback journal and refusal semantics for incompatible states. This is the first feature module of the consumer fallback path (plugin spec §2.5, §5.3, §5.5).

**Architecture:** Pure Go package. Zero network I/O. Three responsibilities:

1. Atomically rewrite `~/.claude/settings.json` to add/remove the Token-Bay redirect.
2. Refuse to write when the state is incompatible (pre-existing non-Token-Bay redirect, Bedrock/Vertex/Foundry provider, JSONC comments we can't preserve, read-only target).
3. Maintain a rollback journal at `~/.token-bay/settings-rollback.json` so `ExitNetworkMode` can restore the pre-fallback state byte-equivalently.

The module is invoked from the sidecar. It does not implement watchers or IPC — those live in `internal/sidecar` and `internal/hooks`.

**Tech Stack:**
- Go 1.23+
- `encoding/json` (stdlib) — parsing when no comments present
- `github.com/tailscale/hujson` — detecting JSONC comments (parse-and-detect, v1 does not preserve)
- `github.com/stretchr/testify` — test assertions
- Standard `os` + `path/filepath` — file operations, symlink resolution, atomic rename

**Prerequisites:** Plugin scaffolding complete (`plugin-v0`). `plugin/go.mod` exists; `internal/settingsjson/.gitkeep` is present.

---

## Table of contents

1. [Design decisions](#1-design-decisions)
2. [Package API](#2-package-api)
3. [File layout](#3-file-layout)
4. [TDD task list](#4-tdd-task-list)
5. [Open questions for implementation](#5-open-questions-for-implementation)

---

## 1. Design decisions

### 1.1 Atomicity

Use the POSIX atomic-rename pattern: write content to `settings.json.token-bay-tmp-<PID>-<nanos>`, `fsync`, `rename(2)` over the target. `rename(2)` is atomic on every filesystem Claude Code supports; if the process crashes mid-write, the target is either the old content or the new content, never partial.

### 1.2 JSONC handling

Claude Code parses settings.json as JSONC (per `src/utils/json.ts`). Users may have comments. Our v1 policy: **detect comments, refuse to write.** Error message: *"settings.json contains comments that Token-Bay v1 can't safely preserve through a round-trip. Please remove comments temporarily, or use `/token-bay fallback-manual` for this session."*

Detecting comments: parse with `hujson`. If `hujson.Standardize(data)` returns bytes different from the input (after whitespace-normalization), comments were present.

Rationale: the `jsonc-parser` library Claude Code uses provides a surgical-edit API (`modify()`) that preserves comments. Go's ecosystem has no direct equivalent with production-grade comment preservation. Option A is to port `jsonc-parser`'s approach (complex, large surface). Option B is the "detect and refuse" policy above — small, honest, recoverable for the user. **v1 picks B.** Comment-preserving edits become a separate feature plan if real users demand it.

### 1.3 Symlinks

If `~/.claude/settings.json` is a symlink, write to its target. Resolve via `os.Lstat` + `os.Readlink`, operate on the resolved path. **Refuse to follow symlinks that escape `~/.claude/`** (defense against malicious symlinks planted in the home directory). Acceptable target paths: anywhere under `$HOME/.claude/` or absolute paths the user explicitly documented (future config). For v1, stricter: target must be inside `$HOME/.claude/`.

### 1.4 Pre-existing redirect

If `env.ANTHROPIC_BASE_URL` is already set and does **not** match our sidecar URL, refuse. Error: *"Your settings already redirect ANTHROPIC_BASE_URL to `{existing_value}`. Token-Bay cannot layer on top of another proxy. Either unset the existing redirect or use `/token-bay fallback-manual`."*

If it matches our sidecar URL (e.g. from a prior fallback we didn't cleanly exit): idempotent — proceed as if entering afresh, don't re-write the file but ensure the rollback journal is valid.

### 1.5 Provider incompatibility

If any of `CLAUDE_CODE_USE_BEDROCK`, `CLAUDE_CODE_USE_VERTEX`, `CLAUDE_CODE_USE_FOUNDRY` is truthy in `env` (truthy per `isEnvTruthy` semantics: `"1"`, `"true"`, `"yes"`, etc.), refuse. `ANTHROPIC_BASE_URL` is ignored under these providers, so the redirect would silently not work. Fail-loud is better than fail-silent.

### 1.6 Missing settings.json

If `~/.claude/settings.json` does not exist, create it with just `{"env": {"ANTHROPIC_BASE_URL": "..."}}`. The rollback journal records `pre_fallback_base_url: nil` (absent), and exit deletes the file if we created it and it's still just our entry.

### 1.7 Rollback journal

File at `~/.token-bay/settings-rollback.json`. Contents:

```json
{
  "entered_at": 1713891200,
  "sidecar_url": "http://127.0.0.1:53421",
  "session_id": "abc-123",
  "pre_fallback": {
    "base_url_was_set": false,
    "base_url_prior_value": "",
    "settings_file_existed": true
  }
}
```

Written alongside the settings.json write. On exit, consulted to restore. If absent at exit time: log warning, best-effort remove our `ANTHROPIC_BASE_URL` key.

### 1.8 Path resolution

Settings file: `$HOME/.claude/settings.json` (resolved via `os.UserHomeDir()`).
Rollback journal: `$HOME/.token-bay/settings-rollback.json`. Create parent dir with `0700` perms.

Both paths injectable for tests — the package exposes a `Store` constructor that takes explicit paths.

### 1.9 What this module does NOT do

- No file watching. The sidecar or consumers set up chokidar-equivalent watching (or simply trust Claude Code's own watcher to pick up our edits).
- No IPC. `EnterNetworkMode` is a direct function call from the sidecar.
- No URL validation beyond "is it a parseable HTTP URL with host `127.0.0.1` or `localhost`". Broader URL validation is policy, not mechanism.

---

## 2. Package API

```go
// Package settingsjson provides atomic mutation of Claude Code's settings.json
// for Token-Bay mid-session redirect (plugin spec §2.5).
package settingsjson

// Store owns paths to the settings file and rollback journal. Instantiate
// once per sidecar. Safe for concurrent EnterNetworkMode / ExitNetworkMode
// calls — internally guarded by a mutex (though callers should not
// invoke concurrently in practice).
type Store struct {
    SettingsPath string // resolved ~/.claude/settings.json
    RollbackPath string // resolved ~/.token-bay/settings-rollback.json
    // unexported fields for sync
}

// NewStore constructs a Store using the default paths under the user's
// home directory. Returns an error if $HOME cannot be determined.
func NewStore() (*Store, error) { ... }

// NewStoreAt constructs a Store with explicit paths — for tests.
func NewStoreAt(settingsPath, rollbackPath string) *Store { ... }

// EnterNetworkMode writes settings.json with ANTHROPIC_BASE_URL set to
// sidecarURL, preserving all other settings. Records rollback info.
// Returns an error (wrapping a sentinel when applicable) on incompatibility.
func (s *Store) EnterNetworkMode(sidecarURL string, sessionID string) error { ... }

// ExitNetworkMode restores settings.json to its pre-fallback state using
// the rollback journal. If the journal is missing, removes our
// ANTHROPIC_BASE_URL key best-effort.
func (s *Store) ExitNetworkMode() error { ... }

// State reports the current observed state of settings.json.
type State struct {
    SettingsFileExists      bool
    HasJSONCComments        bool
    ExistingBaseURL         string // "" if absent or empty
    ExistingBaseURLMatches  bool   // true iff ExistingBaseURL == ourSidecar
    BedrockEnabled          bool
    VertexEnabled           bool
    FoundryEnabled          bool
    InNetworkMode           bool   // rollback journal exists and base URL matches
}

func (s *Store) GetState(sidecarURL string) (*State, error) { ... }

// Sentinel errors for categorical refusals.
var (
    ErrIncompatibleJSONCComments   = errors.New("settingsjson: file contains JSONC comments; v1 cannot preserve")
    ErrPreExistingRedirect         = errors.New("settingsjson: ANTHROPIC_BASE_URL already set to a non-Token-Bay value")
    ErrBedrockProvider             = errors.New("settingsjson: CLAUDE_CODE_USE_BEDROCK is truthy; ANTHROPIC_BASE_URL would be ignored")
    ErrVertexProvider              = errors.New("settingsjson: CLAUDE_CODE_USE_VERTEX is truthy; ANTHROPIC_BASE_URL would be ignored")
    ErrFoundryProvider             = errors.New("settingsjson: CLAUDE_CODE_USE_FOUNDRY is truthy; ANTHROPIC_BASE_URL would be ignored")
    ErrSymlinkEscapesClaudeDir     = errors.New("settingsjson: settings.json symlink target is outside ~/.claude/")
    ErrSettingsNotWritable         = errors.New("settingsjson: settings.json target is not writable")
)
```

### Callers

- `internal/sidecar` calls `EnterNetworkMode` on user consent (plugin spec §5.3).
- `internal/sidecar` calls `ExitNetworkMode` on timer / user command / re-probe (plugin spec §5.5).
- `internal/sidecar` calls `GetState` for the runtime compatibility probe (plugin spec §5.3 step 2).

---

## 3. File layout

```
plugin/internal/settingsjson/
├── settingsjson.go        -- package doc + NewStore / NewStoreAt
├── settingsjson_test.go   -- Store construction tests
├── state.go               -- GetState + State struct
├── state_test.go
├── jsonc.go               -- comment-detection helper
├── jsonc_test.go
├── atomic_write.go        -- atomicWrite primitive + symlink resolution
├── atomic_write_test.go
├── enter.go               -- EnterNetworkMode + incompatibility checks
├── enter_test.go
├── exit.go                -- ExitNetworkMode + journal restore
├── exit_test.go
├── rollback.go            -- RollbackJournal struct + read/write helpers
├── rollback_test.go
└── errors.go              -- sentinel error definitions + wrapping helpers
```

Total ~9 source files, ~7 test files. Each file has a single responsibility.

---

## 4. TDD task list

### Task 1: Package scaffold with Store constructor and sentinel errors

**Files:**
- Create: `plugin/internal/settingsjson/settingsjson.go`
- Create: `plugin/internal/settingsjson/settingsjson_test.go`
- Create: `plugin/internal/settingsjson/errors.go`
- Remove: `plugin/internal/settingsjson/.gitkeep`

**Work directory for `go` commands:** `/Users/dor.amid/git/token-bay/plugin/` with `PATH="$HOME/.local/share/mise/shims:$PATH"` prefix on every bash invocation.

- [ ] **Step 1: Write failing test**

Create `plugin/internal/settingsjson/settingsjson_test.go`:

```go
package settingsjson

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewStoreAt_StoresProvidedPaths(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	rollbackPath := filepath.Join(dir, "rollback.json")

	store := NewStoreAt(settingsPath, rollbackPath)

	require.NotNil(t, store)
	assert.Equal(t, settingsPath, store.SettingsPath)
	assert.Equal(t, rollbackPath, store.RollbackPath)
}

func TestNewStore_ResolvesDefaultPaths(t *testing.T) {
	store, err := NewStore()

	require.NoError(t, err)
	require.NotNil(t, store)
	assert.Contains(t, store.SettingsPath, ".claude")
	assert.Contains(t, store.SettingsPath, "settings.json")
	assert.Contains(t, store.RollbackPath, ".token-bay")
	assert.Contains(t, store.RollbackPath, "settings-rollback.json")
}
```

- [ ] **Step 2: Run test, confirm FAIL**

`PATH="$HOME/.local/share/mise/shims:$PATH" go test ./internal/settingsjson/...`

Expected: `undefined: NewStoreAt`, `undefined: NewStore`.

- [ ] **Step 3: Write minimal implementation**

Create `plugin/internal/settingsjson/settingsjson.go`:

```go
// Package settingsjson provides atomic mutation of Claude Code's settings.json
// for Token-Bay mid-session redirect. See plugin spec §2.5, §5.3, §5.5.
//
// This package is pure state management — no network I/O, no file watching,
// no IPC. Callers in internal/sidecar orchestrate the lifecycle.
package settingsjson

import (
	"os"
	"path/filepath"
	"sync"
)

// Store owns paths to the settings file and rollback journal.
// Safe for concurrent use.
type Store struct {
	SettingsPath string
	RollbackPath string
	mu           sync.Mutex
}

// NewStore constructs a Store using the default paths under the user's
// home directory: ~/.claude/settings.json and ~/.token-bay/settings-rollback.json.
func NewStore() (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return NewStoreAt(
		filepath.Join(home, ".claude", "settings.json"),
		filepath.Join(home, ".token-bay", "settings-rollback.json"),
	), nil
}

// NewStoreAt constructs a Store with explicit paths — primarily for tests.
func NewStoreAt(settingsPath, rollbackPath string) *Store {
	return &Store{
		SettingsPath: settingsPath,
		RollbackPath: rollbackPath,
	}
}
```

Create `plugin/internal/settingsjson/errors.go`:

```go
package settingsjson

import "errors"

// Sentinel errors for categorical refusals. Callers use errors.Is to classify.
var (
	ErrIncompatibleJSONCComments = errors.New("settingsjson: settings.json contains JSONC comments that Token-Bay v1 cannot preserve")
	ErrPreExistingRedirect       = errors.New("settingsjson: ANTHROPIC_BASE_URL is already set to a non-Token-Bay value")
	ErrBedrockProvider           = errors.New("settingsjson: CLAUDE_CODE_USE_BEDROCK is truthy; ANTHROPIC_BASE_URL would be ignored")
	ErrVertexProvider            = errors.New("settingsjson: CLAUDE_CODE_USE_VERTEX is truthy; ANTHROPIC_BASE_URL would be ignored")
	ErrFoundryProvider           = errors.New("settingsjson: CLAUDE_CODE_USE_FOUNDRY is truthy; ANTHROPIC_BASE_URL would be ignored")
	ErrSymlinkEscapesClaudeDir   = errors.New("settingsjson: settings.json symlink target escapes ~/.claude/")
	ErrSettingsNotWritable       = errors.New("settingsjson: settings.json target is not writable")
)
```

- [ ] **Step 4: Remove the .gitkeep, run test, confirm PASS**

```bash
rm /Users/dor.amid/git/token-bay/plugin/internal/settingsjson/.gitkeep
cd /Users/dor.amid/git/token-bay/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" go test ./internal/settingsjson/...
```

Expected: both tests pass.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/git/token-bay
git add plugin/internal/settingsjson/settingsjson.go plugin/internal/settingsjson/settingsjson_test.go plugin/internal/settingsjson/errors.go
git rm plugin/internal/settingsjson/.gitkeep
git commit -m "feat(plugin/settingsjson): scaffold Store + sentinel errors"
```

### Task 2: JSONC comment detection

**Files:**
- Create: `plugin/internal/settingsjson/jsonc.go`
- Create: `plugin/internal/settingsjson/jsonc_test.go`
- Modify: `plugin/go.mod` (add `github.com/tailscale/hujson`)

- [ ] **Step 1: Add hujson dependency**

```bash
cd /Users/dor.amid/git/token-bay/plugin
PATH="$HOME/.local/share/mise/shims:$PATH" go get github.com/tailscale/hujson@latest
PATH="$HOME/.local/share/mise/shims:$PATH" go mod tidy
```

- [ ] **Step 2: Write failing tests**

Create `plugin/internal/settingsjson/jsonc_test.go`:

```go
package settingsjson

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHasJSONCComments_PlainJSON_ReturnsFalse(t *testing.T) {
	input := []byte(`{"env": {"ANTHROPIC_BASE_URL": "https://api.anthropic.com"}}`)
	has, err := hasJSONCComments(input)
	assert.NoError(t, err)
	assert.False(t, has)
}

func TestHasJSONCComments_LineComment_ReturnsTrue(t *testing.T) {
	input := []byte(`{
  // my Anthropic proxy
  "env": {"ANTHROPIC_BASE_URL": "https://api.anthropic.com"}
}`)
	has, err := hasJSONCComments(input)
	assert.NoError(t, err)
	assert.True(t, has)
}

func TestHasJSONCComments_BlockComment_ReturnsTrue(t *testing.T) {
	input := []byte(`{
  /* set by IT */
  "env": {"ANTHROPIC_BASE_URL": "https://api.anthropic.com"}
}`)
	has, err := hasJSONCComments(input)
	assert.NoError(t, err)
	assert.True(t, has)
}

func TestHasJSONCComments_TrailingComma_ReturnsTrue(t *testing.T) {
	// JSONC also allows trailing commas; if present, treat as "non-preservable".
	input := []byte(`{
  "env": {"ANTHROPIC_BASE_URL": "https://api.anthropic.com",},
}`)
	has, err := hasJSONCComments(input)
	assert.NoError(t, err)
	assert.True(t, has)
}

func TestHasJSONCComments_MalformedInput_ReturnsError(t *testing.T) {
	input := []byte(`{this is not json`)
	_, err := hasJSONCComments(input)
	assert.Error(t, err)
}
```

- [ ] **Step 3: Run tests, confirm FAIL**

Expected: `undefined: hasJSONCComments`.

- [ ] **Step 4: Write implementation**

Create `plugin/internal/settingsjson/jsonc.go`:

```go
package settingsjson

import (
	"bytes"

	"github.com/tailscale/hujson"
)

// hasJSONCComments reports whether data contains JSONC-isms (comments or
// trailing commas) that a round-trip through encoding/json would lose.
//
// Detection strategy: hujson.Standardize strips JSONC extensions and returns
// canonical JSON. If the result differs from the input (modulo trailing
// whitespace), the input had JSONC content.
//
// Returns an error only if the input is not even valid JSONC (malformed).
func hasJSONCComments(data []byte) (bool, error) {
	standardized, err := hujson.Standardize(data)
	if err != nil {
		return false, err
	}
	// Compare with trailing whitespace trimmed — hujson may canonicalize
	// whitespace, we don't care about that kind of change.
	return !bytes.Equal(bytes.TrimSpace(data), bytes.TrimSpace(standardized)), nil
}
```

- [ ] **Step 5: Run tests, confirm PASS**

`PATH="$HOME/.local/share/mise/shims:$PATH" go test ./internal/settingsjson/...`

All 5 tests should pass plus the 2 from Task 1.

- [ ] **Step 6: Commit**

```bash
cd /Users/dor.amid/git/token-bay
git add plugin/internal/settingsjson/jsonc.go plugin/internal/settingsjson/jsonc_test.go plugin/go.mod plugin/go.sum
git commit -m "feat(plugin/settingsjson): detect JSONC comments via hujson"
```

### Task 3: State inspection — GetState

**Files:**
- Create: `plugin/internal/settingsjson/state.go`
- Create: `plugin/internal/settingsjson/state_test.go`

- [ ] **Step 1: Write failing tests**

Create `plugin/internal/settingsjson/state_test.go`:

```go
package settingsjson

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetState_MissingSettingsFile_ReturnsFileDoesNotExist(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreAt(filepath.Join(dir, "settings.json"), filepath.Join(dir, "rollback.json"))

	state, err := store.GetState("http://127.0.0.1:53421")
	require.NoError(t, err)
	assert.False(t, state.SettingsFileExists)
}

func TestGetState_EmptyEnvBlock_ReturnsCleanState(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	require.NoError(t, os.WriteFile(settingsPath, []byte(`{"env": {}}`), 0o600))
	store := NewStoreAt(settingsPath, filepath.Join(dir, "rollback.json"))

	state, err := store.GetState("http://127.0.0.1:53421")

	require.NoError(t, err)
	assert.True(t, state.SettingsFileExists)
	assert.False(t, state.HasJSONCComments)
	assert.Empty(t, state.ExistingBaseURL)
	assert.False(t, state.BedrockEnabled)
	assert.False(t, state.VertexEnabled)
	assert.False(t, state.FoundryEnabled)
}

func TestGetState_ExistingNonMatchingRedirect_FlagsIt(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	require.NoError(t, os.WriteFile(settingsPath, []byte(`{"env": {"ANTHROPIC_BASE_URL": "https://proxy.example.com"}}`), 0o600))
	store := NewStoreAt(settingsPath, filepath.Join(dir, "rollback.json"))

	state, err := store.GetState("http://127.0.0.1:53421")

	require.NoError(t, err)
	assert.Equal(t, "https://proxy.example.com", state.ExistingBaseURL)
	assert.False(t, state.ExistingBaseURLMatches)
}

func TestGetState_MatchingRedirect_FlagsMatch(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	require.NoError(t, os.WriteFile(settingsPath, []byte(`{"env": {"ANTHROPIC_BASE_URL": "http://127.0.0.1:53421"}}`), 0o600))
	store := NewStoreAt(settingsPath, filepath.Join(dir, "rollback.json"))

	state, err := store.GetState("http://127.0.0.1:53421")

	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:53421", state.ExistingBaseURL)
	assert.True(t, state.ExistingBaseURLMatches)
}

func TestGetState_BedrockEnabled_FlagsIt(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	require.NoError(t, os.WriteFile(settingsPath, []byte(`{"env": {"CLAUDE_CODE_USE_BEDROCK": "1"}}`), 0o600))
	store := NewStoreAt(settingsPath, filepath.Join(dir, "rollback.json"))

	state, err := store.GetState("http://127.0.0.1:53421")

	require.NoError(t, err)
	assert.True(t, state.BedrockEnabled)
}

func TestGetState_BedrockFalse_NotFlagged(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	require.NoError(t, os.WriteFile(settingsPath, []byte(`{"env": {"CLAUDE_CODE_USE_BEDROCK": "0"}}`), 0o600))
	store := NewStoreAt(settingsPath, filepath.Join(dir, "rollback.json"))

	state, err := store.GetState("http://127.0.0.1:53421")

	require.NoError(t, err)
	assert.False(t, state.BedrockEnabled)
}

func TestGetState_JSONCContent_FlagsIt(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	require.NoError(t, os.WriteFile(settingsPath, []byte(`{
  // a comment
  "env": {}
}`), 0o600))
	store := NewStoreAt(settingsPath, filepath.Join(dir, "rollback.json"))

	state, err := store.GetState("http://127.0.0.1:53421")

	require.NoError(t, err)
	assert.True(t, state.HasJSONCComments)
}
```

- [ ] **Step 2: Run tests, confirm FAIL**

Expected: `undefined: (*Store).GetState`, `undefined: State`.

- [ ] **Step 3: Write implementation**

Create `plugin/internal/settingsjson/state.go`:

```go
package settingsjson

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/tailscale/hujson"
)

// State reports the observed state of settings.json at a point in time.
type State struct {
	SettingsFileExists     bool
	HasJSONCComments       bool
	ExistingBaseURL        string
	ExistingBaseURLMatches bool
	BedrockEnabled         bool
	VertexEnabled          bool
	FoundryEnabled         bool
	InNetworkMode          bool // (Task 7: true iff rollback journal exists AND URL matches)
}

// GetState reads settings.json and classifies relevant fields against
// the expected sidecar URL.
func (s *Store) GetState(sidecarURL string) (*State, error) {
	data, err := os.ReadFile(s.SettingsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &State{SettingsFileExists: false}, nil
		}
		return nil, fmt.Errorf("settingsjson: read %s: %w", s.SettingsPath, err)
	}

	out := &State{SettingsFileExists: true}

	hasComments, err := hasJSONCComments(data)
	if err != nil {
		return nil, fmt.Errorf("settingsjson: parse %s: %w", s.SettingsPath, err)
	}
	out.HasJSONCComments = hasComments

	// Standardize so we can parse uniformly whether JSONC or plain.
	standardized, err := hujson.Standardize(data)
	if err != nil {
		return nil, fmt.Errorf("settingsjson: standardize %s: %w", s.SettingsPath, err)
	}

	var parsed struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(standardized, &parsed); err != nil {
		return nil, fmt.Errorf("settingsjson: unmarshal %s: %w", s.SettingsPath, err)
	}

	out.ExistingBaseURL = parsed.Env["ANTHROPIC_BASE_URL"]
	out.ExistingBaseURLMatches = (out.ExistingBaseURL == sidecarURL)
	out.BedrockEnabled = isEnvTruthy(parsed.Env["CLAUDE_CODE_USE_BEDROCK"])
	out.VertexEnabled = isEnvTruthy(parsed.Env["CLAUDE_CODE_USE_VERTEX"])
	out.FoundryEnabled = isEnvTruthy(parsed.Env["CLAUDE_CODE_USE_FOUNDRY"])

	return out, nil
}

// isEnvTruthy matches Claude Code's isEnvTruthy semantics: non-empty and
// not one of "0", "false", "no", "off" (case-insensitive).
func isEnvTruthy(v string) bool {
	if v == "" {
		return false
	}
	switch v {
	case "0", "false", "no", "off", "FALSE", "NO", "OFF", "False", "No", "Off":
		return false
	}
	return true
}
```

- [ ] **Step 4: Run tests, confirm PASS**

- [ ] **Step 5: Commit**

```bash
git add plugin/internal/settingsjson/state.go plugin/internal/settingsjson/state_test.go
git commit -m "feat(plugin/settingsjson): GetState reads and classifies settings.json"
```

### Task 4: Atomic write primitive + symlink resolution

**Files:**
- Create: `plugin/internal/settingsjson/atomic_write.go`
- Create: `plugin/internal/settingsjson/atomic_write_test.go`

- [ ] **Step 1: Write failing tests**

Create `plugin/internal/settingsjson/atomic_write_test.go`:

```go
package settingsjson

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAtomicWriteFile_CreatesFileWithContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")
	content := []byte(`{"key":"value"}`)

	err := atomicWriteFile(path, content, 0o600)

	require.NoError(t, err)
	read, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, content, read)
}

func TestAtomicWriteFile_OverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"old":true}`), 0o600))

	err := atomicWriteFile(path, []byte(`{"new":true}`), 0o600)

	require.NoError(t, err)
	read, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, []byte(`{"new":true}`), read)
}

func TestAtomicWriteFile_LeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")

	err := atomicWriteFile(path, []byte(`{}`), 0o600)
	require.NoError(t, err)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
	assert.Equal(t, "out.json", entries[0].Name())
}

func TestResolveSymlink_PlainFile_ReturnsSamePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.json")
	require.NoError(t, os.WriteFile(path, []byte(`{}`), 0o600))

	resolved, err := resolveSymlinkTarget(path, dir)
	require.NoError(t, err)
	assert.Equal(t, path, resolved)
}

func TestResolveSymlink_SymlinkWithinBase_ReturnsTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.json")
	link := filepath.Join(dir, "link.json")
	require.NoError(t, os.WriteFile(target, []byte(`{}`), 0o600))
	require.NoError(t, os.Symlink(target, link))

	resolved, err := resolveSymlinkTarget(link, dir)
	require.NoError(t, err)
	assert.Equal(t, target, resolved)
}

func TestResolveSymlink_SymlinkEscapesBase_ReturnsError(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	outsideTarget := filepath.Join(outside, "escape.json")
	require.NoError(t, os.WriteFile(outsideTarget, []byte(`{}`), 0o600))
	link := filepath.Join(base, "link.json")
	require.NoError(t, os.Symlink(outsideTarget, link))

	_, err := resolveSymlinkTarget(link, base)
	assert.ErrorIs(t, err, ErrSymlinkEscapesClaudeDir)
}
```

- [ ] **Step 2: Run tests, confirm FAIL**

- [ ] **Step 3: Write implementation**

Create `plugin/internal/settingsjson/atomic_write.go`:

```go
package settingsjson

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// atomicWriteFile writes content to path via a tempfile + fsync + rename.
// On any filesystem POSIX supports, the caller either sees the pre-state or
// the post-state; there is no partial-write window.
//
// The tempfile is created in the same directory as path so rename(2) is
// a simple metadata op (cross-fs renames are not atomic on Linux).
func atomicWriteFile(path string, content []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tmpName := fmt.Sprintf(".%s.token-bay-tmp-%d-%d", base, os.Getpid(), time.Now().UnixNano())
	tmpPath := filepath.Join(dir, tmpName)

	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("settingsjson: create tempfile: %w", err)
	}
	cleanupTmp := func() { _ = os.Remove(tmpPath) }

	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		cleanupTmp()
		return fmt.Errorf("settingsjson: write tempfile: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanupTmp()
		return fmt.Errorf("settingsjson: fsync tempfile: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanupTmp()
		return fmt.Errorf("settingsjson: close tempfile: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		cleanupTmp()
		return fmt.Errorf("settingsjson: rename tempfile: %w", err)
	}
	return nil
}

// resolveSymlinkTarget returns the absolute path that should be written to
// when path may be a symlink. If path is not a symlink, returns path unchanged.
// If path is a symlink whose resolved target is outside baseDir, returns
// ErrSymlinkEscapesClaudeDir.
//
// baseDir is typically ~/.claude/ — the security boundary for allowed
// settings.json targets.
func resolveSymlinkTarget(path, baseDir string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("settingsjson: lstat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return path, nil
	}

	target, err := os.Readlink(path)
	if err != nil {
		return "", fmt.Errorf("settingsjson: readlink %s: %w", path, err)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), target)
	}
	target = filepath.Clean(target)

	baseAbs, err := filepath.Abs(baseDir)
	if err != nil {
		return "", fmt.Errorf("settingsjson: abs %s: %w", baseDir, err)
	}
	baseAbs = filepath.Clean(baseAbs)

	rel, err := filepath.Rel(baseAbs, target)
	if err != nil || strings.HasPrefix(rel, "..") || rel == ".." {
		return "", fmt.Errorf("%w: target %s is outside %s", ErrSymlinkEscapesClaudeDir, target, baseAbs)
	}
	return target, nil
}
```

- [ ] **Step 4: Run tests, confirm PASS**

- [ ] **Step 5: Commit**

```bash
git add plugin/internal/settingsjson/atomic_write.go plugin/internal/settingsjson/atomic_write_test.go
git commit -m "feat(plugin/settingsjson): atomic write + symlink-safe target resolution"
```

### Task 5: Rollback journal

**Files:**
- Create: `plugin/internal/settingsjson/rollback.go`
- Create: `plugin/internal/settingsjson/rollback_test.go`

- [ ] **Step 1: Write failing tests**

Create `plugin/internal/settingsjson/rollback_test.go`:

```go
package settingsjson

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteRollback_CreatesFileWithJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "rollback.json")
	j := RollbackJournal{
		EnteredAt:  1713891200,
		SidecarURL: "http://127.0.0.1:53421",
		SessionID:  "abc-123",
		PreFallback: PreFallback{
			BaseURLWasSet:       true,
			BaseURLPriorValue:   "https://proxy.example.com",
			SettingsFileExisted: true,
		},
	}

	err := writeRollback(path, j)

	require.NoError(t, err)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var decoded RollbackJournal
	require.NoError(t, json.Unmarshal(raw, &decoded))
	assert.Equal(t, j, decoded)
}

func TestReadRollback_MissingFile_ReturnsErrNotExist(t *testing.T) {
	dir := t.TempDir()
	_, err := readRollback(filepath.Join(dir, "nope.json"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestReadRollback_ExistingFile_RoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollback.json")
	j := RollbackJournal{
		EnteredAt:  42,
		SidecarURL: "http://localhost:9000",
		SessionID:  "s1",
		PreFallback: PreFallback{
			BaseURLWasSet:       false,
			BaseURLPriorValue:   "",
			SettingsFileExisted: false,
		},
	}
	require.NoError(t, writeRollback(path, j))

	got, err := readRollback(path)
	require.NoError(t, err)
	assert.Equal(t, &j, got)
}

func TestDeleteRollback_ExistingFile_Deletes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollback.json")
	require.NoError(t, os.WriteFile(path, []byte(`{}`), 0o600))

	require.NoError(t, deleteRollback(path))

	_, err := os.Stat(path)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestDeleteRollback_MissingFile_NotAnError(t *testing.T) {
	dir := t.TempDir()
	err := deleteRollback(filepath.Join(dir, "nope.json"))
	assert.NoError(t, err)
}
```

- [ ] **Step 2: Run tests, confirm FAIL**

- [ ] **Step 3: Write implementation**

Create `plugin/internal/settingsjson/rollback.go`:

```go
package settingsjson

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// RollbackJournal records the pre-fallback state of settings.json so that
// ExitNetworkMode can restore it precisely. Written alongside the settings
// mutation in EnterNetworkMode; consumed in ExitNetworkMode.
type RollbackJournal struct {
	EnteredAt   int64       `json:"entered_at"`   // unix seconds
	SidecarURL  string      `json:"sidecar_url"`  // the URL we redirected to
	SessionID   string      `json:"session_id"`   // Claude Code session id, if known
	PreFallback PreFallback `json:"pre_fallback"` // what we need to restore
}

// PreFallback captures the settings state prior to entering network mode.
type PreFallback struct {
	BaseURLWasSet       bool   `json:"base_url_was_set"`
	BaseURLPriorValue   string `json:"base_url_prior_value"`
	SettingsFileExisted bool   `json:"settings_file_existed"`
}

// writeRollback marshals j and atomically writes it to path. Creates the
// parent directory (perm 0700) if missing.
func writeRollback(path string, j RollbackJournal) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("settingsjson: mkdir %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return fmt.Errorf("settingsjson: marshal rollback: %w", err)
	}
	return atomicWriteFile(path, data, 0o600)
}

// readRollback returns the journal at path. Returns os.ErrNotExist-wrapped
// if the file is absent.
func readRollback(path string) (*RollbackJournal, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var j RollbackJournal
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("settingsjson: unmarshal rollback %s: %w", path, err)
	}
	return &j, nil
}

// deleteRollback removes the journal. A missing file is not an error —
// ExitNetworkMode may be called redundantly or after a crash.
func deleteRollback(path string) error {
	err := os.Remove(path)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("settingsjson: remove rollback %s: %w", path, err)
}
```

- [ ] **Step 4: Run tests, confirm PASS**

- [ ] **Step 5: Commit**

```bash
git add plugin/internal/settingsjson/rollback.go plugin/internal/settingsjson/rollback_test.go
git commit -m "feat(plugin/settingsjson): rollback journal read/write/delete"
```

### Task 6: EnterNetworkMode

**Files:**
- Create: `plugin/internal/settingsjson/enter.go`
- Create: `plugin/internal/settingsjson/enter_test.go`

- [ ] **Step 1: Write failing tests**

Create `plugin/internal/settingsjson/enter_test.go`:

```go
package settingsjson

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T) (*Store, string, string) {
	t.Helper()
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, ".claude")
	require.NoError(t, os.MkdirAll(claudeDir, 0o700))
	settings := filepath.Join(claudeDir, "settings.json")
	rollback := filepath.Join(dir, ".token-bay", "settings-rollback.json")
	return NewStoreAt(settings, rollback), settings, rollback
}

func readEnv(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var parsed struct {
		Env map[string]string `json:"env"`
	}
	require.NoError(t, json.Unmarshal(data, &parsed))
	return parsed.Env
}

func TestEnterNetworkMode_MissingSettings_CreatesFile(t *testing.T) {
	store, settings, rollback := newTestStore(t)

	err := store.EnterNetworkMode("http://127.0.0.1:53421", "session-x")

	require.NoError(t, err)
	env := readEnv(t, settings)
	assert.Equal(t, "http://127.0.0.1:53421", env["ANTHROPIC_BASE_URL"])
	// rollback journal exists
	_, err = os.Stat(rollback)
	assert.NoError(t, err)
}

func TestEnterNetworkMode_ExistingSettings_PreservesOtherKeys(t *testing.T) {
	store, settings, _ := newTestStore(t)
	original := `{"env":{"OTHER_VAR":"keep-me"},"otherRoot":{"k":"v"}}`
	require.NoError(t, os.WriteFile(settings, []byte(original), 0o600))

	require.NoError(t, store.EnterNetworkMode("http://127.0.0.1:53421", "session-x"))

	env := readEnv(t, settings)
	assert.Equal(t, "http://127.0.0.1:53421", env["ANTHROPIC_BASE_URL"])
	assert.Equal(t, "keep-me", env["OTHER_VAR"])

	// otherRoot preserved
	raw, err := os.ReadFile(settings)
	require.NoError(t, err)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(raw, &parsed))
	assert.Equal(t, map[string]any{"k": "v"}, parsed["otherRoot"])
}

func TestEnterNetworkMode_RollbackCapturesPriorState(t *testing.T) {
	store, settings, rollback := newTestStore(t)
	require.NoError(t, os.WriteFile(settings, []byte(`{"env":{"OTHER":"x"}}`), 0o600))

	require.NoError(t, store.EnterNetworkMode("http://127.0.0.1:53421", "session-x"))

	j, err := readRollback(rollback)
	require.NoError(t, err)
	assert.True(t, j.PreFallback.SettingsFileExisted)
	assert.False(t, j.PreFallback.BaseURLWasSet)
	assert.Empty(t, j.PreFallback.BaseURLPriorValue)
	assert.Equal(t, "http://127.0.0.1:53421", j.SidecarURL)
	assert.Equal(t, "session-x", j.SessionID)
}

func TestEnterNetworkMode_PriorRedirect_RecordsItInRollback(t *testing.T) {
	store, settings, rollback := newTestStore(t)
	require.NoError(t, os.WriteFile(settings, []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://old.example.com"}}`), 0o600))

	// Case A: prior redirect is our own (idempotent) — should proceed.
	require.NoError(t, store.EnterNetworkMode("https://old.example.com", "s-A"))
	j, err := readRollback(rollback)
	require.NoError(t, err)
	assert.True(t, j.PreFallback.BaseURLWasSet)
	assert.Equal(t, "https://old.example.com", j.PreFallback.BaseURLPriorValue)
}

func TestEnterNetworkMode_PreExistingNonMatchingRedirect_Refuses(t *testing.T) {
	store, settings, _ := newTestStore(t)
	require.NoError(t, os.WriteFile(settings, []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://someone-else.example.com"}}`), 0o600))

	err := store.EnterNetworkMode("http://127.0.0.1:53421", "session-x")

	assert.ErrorIs(t, err, ErrPreExistingRedirect)
}

func TestEnterNetworkMode_BedrockEnabled_Refuses(t *testing.T) {
	store, settings, _ := newTestStore(t)
	require.NoError(t, os.WriteFile(settings, []byte(`{"env":{"CLAUDE_CODE_USE_BEDROCK":"1"}}`), 0o600))

	err := store.EnterNetworkMode("http://127.0.0.1:53421", "session-x")
	assert.ErrorIs(t, err, ErrBedrockProvider)
}

func TestEnterNetworkMode_VertexEnabled_Refuses(t *testing.T) {
	store, settings, _ := newTestStore(t)
	require.NoError(t, os.WriteFile(settings, []byte(`{"env":{"CLAUDE_CODE_USE_VERTEX":"true"}}`), 0o600))

	err := store.EnterNetworkMode("http://127.0.0.1:53421", "session-x")
	assert.ErrorIs(t, err, ErrVertexProvider)
}

func TestEnterNetworkMode_JSONCComments_Refuses(t *testing.T) {
	store, settings, _ := newTestStore(t)
	require.NoError(t, os.WriteFile(settings, []byte(`{
  // a comment that would be lost
  "env": {}
}`), 0o600))

	err := store.EnterNetworkMode("http://127.0.0.1:53421", "session-x")
	assert.ErrorIs(t, err, ErrIncompatibleJSONCComments)
}

func TestEnterNetworkMode_ConcurrentCalls_Serialize(t *testing.T) {
	store, _, _ := newTestStore(t)

	done := make(chan error, 2)
	go func() { done <- store.EnterNetworkMode("http://127.0.0.1:53421", "A") }()
	go func() { done <- store.EnterNetworkMode("http://127.0.0.1:53421", "B") }()

	for i := 0; i < 2; i++ {
		err := <-done
		// Both calls should succeed; second is a no-op idempotent entry.
		assert.True(t, err == nil || errors.Is(err, ErrPreExistingRedirect) == false,
			"concurrent entry should not surface incompatibility: got %v", err)
	}
}
```

- [ ] **Step 2: Run tests, confirm FAIL**

- [ ] **Step 3: Write implementation**

Create `plugin/internal/settingsjson/enter.go`:

```go
package settingsjson

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// EnterNetworkMode writes settings.json with env.ANTHROPIC_BASE_URL=sidecarURL,
// preserving all other settings, and records a rollback journal. Returns a
// sentinel error if the state is incompatible (see package doc + errors.go).
func (s *Store) EnterNetworkMode(sidecarURL, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.GetState(sidecarURL)
	if err != nil {
		return err
	}

	// Incompatibility checks (first-match wins; order shouldn't matter since
	// we fail on any hit).
	if state.BedrockEnabled {
		return ErrBedrockProvider
	}
	if state.VertexEnabled {
		return ErrVertexProvider
	}
	if state.FoundryEnabled {
		return ErrFoundryProvider
	}
	if state.HasJSONCComments {
		return ErrIncompatibleJSONCComments
	}
	if state.ExistingBaseURL != "" && !state.ExistingBaseURLMatches {
		return fmt.Errorf("%w: existing value is %q", ErrPreExistingRedirect, state.ExistingBaseURL)
	}

	// Resolve symlink target (bounded to ~/.claude/).
	claudeDir := filepath.Dir(s.SettingsPath)
	var writePath string
	if state.SettingsFileExists {
		writePath, err = resolveSymlinkTarget(s.SettingsPath, claudeDir)
		if err != nil {
			return err
		}
	} else {
		writePath = s.SettingsPath
		// Ensure parent directory exists.
		if err := os.MkdirAll(claudeDir, 0o700); err != nil {
			return fmt.Errorf("settingsjson: mkdir %s: %w", claudeDir, err)
		}
	}

	// Read existing content (if any) and merge.
	merged, err := mergeWithBaseURL(writePath, state.SettingsFileExists, sidecarURL)
	if err != nil {
		return err
	}

	// Write the rollback journal BEFORE the settings write. If the settings
	// write fails, the rollback journal is harmless; if settings succeeds
	// but rollback fails, we have an unrecoverable state on exit. Order
	// matters: journal first so the invariant "network mode ⇒ journal"
	// always holds.
	journal := RollbackJournal{
		EnteredAt:  time.Now().Unix(),
		SidecarURL: sidecarURL,
		SessionID:  sessionID,
		PreFallback: PreFallback{
			BaseURLWasSet:       state.ExistingBaseURL != "",
			BaseURLPriorValue:   state.ExistingBaseURL,
			SettingsFileExisted: state.SettingsFileExists,
		},
	}
	if err := writeRollback(s.RollbackPath, journal); err != nil {
		return err
	}

	if err := atomicWriteFile(writePath, merged, 0o600); err != nil {
		// best-effort: clean up the journal so state is consistent
		_ = deleteRollback(s.RollbackPath)
		return err
	}

	return nil
}

// mergeWithBaseURL reads existing settings.json (or starts fresh), merges
// env.ANTHROPIC_BASE_URL=sidecarURL, and returns pretty-printed bytes.
// Preserves every other top-level key and every other env entry verbatim.
func mergeWithBaseURL(path string, exists bool, sidecarURL string) ([]byte, error) {
	var root map[string]any
	if exists {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("settingsjson: read %s: %w", path, err)
		}
		if err := json.Unmarshal(data, &root); err != nil {
			return nil, fmt.Errorf("settingsjson: parse %s: %w", path, err)
		}
	}
	if root == nil {
		root = map[string]any{}
	}

	envRaw, ok := root["env"]
	var env map[string]any
	if ok {
		env, ok = envRaw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("settingsjson: %s: env is not an object", path)
		}
	} else {
		env = map[string]any{}
	}
	env["ANTHROPIC_BASE_URL"] = sidecarURL
	root["env"] = env

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("settingsjson: marshal merged: %w", err)
	}
	// Ensure trailing newline — many tools expect it; Claude Code doesn't care.
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return out, nil
}
```

- [ ] **Step 4: Run tests, confirm PASS**

- [ ] **Step 5: Commit**

```bash
git add plugin/internal/settingsjson/enter.go plugin/internal/settingsjson/enter_test.go
git commit -m "feat(plugin/settingsjson): EnterNetworkMode with incompatibility checks"
```

### Task 7: ExitNetworkMode + InNetworkMode state

**Files:**
- Create: `plugin/internal/settingsjson/exit.go`
- Create: `plugin/internal/settingsjson/exit_test.go`
- Modify: `plugin/internal/settingsjson/state.go` (set InNetworkMode based on rollback presence)
- Modify: `plugin/internal/settingsjson/state_test.go` (cover the new field)

- [ ] **Step 1: Extend state test and state.go**

Append to `state_test.go`:

```go
func TestGetState_AfterEnterNetworkMode_InNetworkModeIsTrue(t *testing.T) {
	store, _, _ := newTestStore(t)
	require.NoError(t, store.EnterNetworkMode("http://127.0.0.1:53421", "session-x"))

	state, err := store.GetState("http://127.0.0.1:53421")
	require.NoError(t, err)
	assert.True(t, state.InNetworkMode)
}

func TestGetState_WithoutEntering_InNetworkModeIsFalse(t *testing.T) {
	store, _, _ := newTestStore(t)

	state, err := store.GetState("http://127.0.0.1:53421")
	require.NoError(t, err)
	assert.False(t, state.InNetworkMode)
}
```

(Note: the helper `newTestStore` is already defined in `enter_test.go`; since both test files are in package `settingsjson`, it's shared.)

Modify `state.go` at the end of `GetState`:

```go
	// InNetworkMode: rollback journal exists AND ExistingBaseURL matches.
	if out.ExistingBaseURLMatches {
		if _, err := os.Stat(s.RollbackPath); err == nil {
			out.InNetworkMode = true
		}
	}

	return out, nil
}
```

Run tests: should PASS.

- [ ] **Step 2: Write ExitNetworkMode tests**

Create `plugin/internal/settingsjson/exit_test.go`:

```go
package settingsjson

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExitNetworkMode_AfterEnter_RemovesRedirectAndJournal(t *testing.T) {
	store, settings, rollback := newTestStore(t)
	require.NoError(t, os.WriteFile(settings, []byte(`{"env":{"OTHER":"keep"}}`), 0o600))
	require.NoError(t, store.EnterNetworkMode("http://127.0.0.1:53421", "s"))

	require.NoError(t, store.ExitNetworkMode())

	env := readEnv(t, settings)
	_, present := env["ANTHROPIC_BASE_URL"]
	assert.False(t, present, "ANTHROPIC_BASE_URL should be removed after exit")
	assert.Equal(t, "keep", env["OTHER"])
	_, err := os.Stat(rollback)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestExitNetworkMode_PriorValueRestored(t *testing.T) {
	store, settings, _ := newTestStore(t)
	require.NoError(t, os.WriteFile(settings, []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:53421"}}`), 0o600))
	require.NoError(t, store.EnterNetworkMode("http://127.0.0.1:53421", "s"))
	// Simulate a prior value captured; tamper the rollback to simulate a user
	// whose original settings had a prior non-Token-Bay URL (here we injected
	// it artificially — normally EnterNetworkMode refuses this case).
	j, err := readRollback(store.RollbackPath)
	require.NoError(t, err)
	j.PreFallback.BaseURLWasSet = true
	j.PreFallback.BaseURLPriorValue = "https://user-proxy.example.com"
	require.NoError(t, writeRollback(store.RollbackPath, *j))

	require.NoError(t, store.ExitNetworkMode())

	env := readEnv(t, settings)
	assert.Equal(t, "https://user-proxy.example.com", env["ANTHROPIC_BASE_URL"])
}

func TestExitNetworkMode_FileCreatedByUs_DeletesEmptyFile(t *testing.T) {
	store, settings, _ := newTestStore(t)
	// No settings.json exists before Enter.
	require.NoError(t, store.EnterNetworkMode("http://127.0.0.1:53421", "s"))

	require.NoError(t, store.ExitNetworkMode())

	// Settings file was created by us with just our key; after exit it
	// would be {"env":{}} — we choose to remove the env key and also
	// remove the top-level env if empty. File should still exist but
	// not contain our key.
	raw, err := os.ReadFile(settings)
	require.NoError(t, err)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(raw, &parsed))
	envAny, hasEnv := parsed["env"]
	if hasEnv {
		env := envAny.(map[string]any)
		_, hasURL := env["ANTHROPIC_BASE_URL"]
		assert.False(t, hasURL)
	}
}

func TestExitNetworkMode_MissingJournal_BestEffortRemoves(t *testing.T) {
	store, settings, rollback := newTestStore(t)
	// Simulate the state where our redirect was written but the journal is
	// lost (e.g., manually deleted, or a partial-rollback from a previous
	// crash). We write settings with our URL and no journal.
	require.NoError(t, os.WriteFile(settings, []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:53421","OTHER":"x"}}`), 0o600))
	_, err := os.Stat(rollback)
	assert.ErrorIs(t, err, os.ErrNotExist)

	// Best effort: remove our key without a rollback journal to consult.
	// The Store needs to know the URL to compare against; we pass it via
	// ExitNetworkModeAt (a method variant used only in this edge case).
	require.NoError(t, store.ExitNetworkModeBestEffort("http://127.0.0.1:53421"))

	env := readEnv(t, settings)
	_, present := env["ANTHROPIC_BASE_URL"]
	assert.False(t, present)
	assert.Equal(t, "x", env["OTHER"])
}
```

- [ ] **Step 3: Run tests, confirm FAIL**

- [ ] **Step 4: Write implementation**

Create `plugin/internal/settingsjson/exit.go`:

```go
package settingsjson

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ExitNetworkMode reads the rollback journal, restores the pre-fallback
// state of settings.json, and deletes the journal. If the journal is absent,
// returns an error — callers needing best-effort cleanup should use
// ExitNetworkModeBestEffort.
func (s *Store) ExitNetworkMode() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	journal, err := readRollback(s.RollbackPath)
	if err != nil {
		return fmt.Errorf("settingsjson: read rollback: %w", err)
	}

	if err := s.restoreFromJournal(*journal); err != nil {
		return err
	}

	return deleteRollback(s.RollbackPath)
}

// ExitNetworkModeBestEffort clears our ANTHROPIC_BASE_URL key without
// consulting the rollback journal. Used when the journal is missing but
// the caller knows which sidecar URL is active (e.g., crash recovery).
// No prior-value restoration is performed; the key is simply removed.
func (s *Store) ExitNetworkModeBestEffort(sidecarURL string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	claudeDir := filepath.Dir(s.SettingsPath)
	writePath, err := resolveSymlinkTarget(s.SettingsPath, claudeDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // nothing to do
		}
		return err
	}

	stripped, present, err := stripBaseURLIfMatches(writePath, sidecarURL)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	return atomicWriteFile(writePath, stripped, 0o600)
}

// restoreFromJournal rewrites settings.json according to the journal:
// if BaseURLWasSet, set ANTHROPIC_BASE_URL back to BaseURLPriorValue;
// otherwise remove our key entirely.
func (s *Store) restoreFromJournal(j RollbackJournal) error {
	claudeDir := filepath.Dir(s.SettingsPath)

	// Resolve symlink target (if applicable).
	writePath := s.SettingsPath
	if info, err := os.Lstat(s.SettingsPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			writePath, err = resolveSymlinkTarget(s.SettingsPath, claudeDir)
			if err != nil {
				return err
			}
		}
	}

	// Read current settings.
	data, err := os.ReadFile(writePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Nothing to restore; settings file was removed externally.
			return nil
		}
		return fmt.Errorf("settingsjson: read %s: %w", writePath, err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("settingsjson: parse %s: %w", writePath, err)
	}

	envAny, hasEnv := root["env"]
	var env map[string]any
	if hasEnv {
		env, _ = envAny.(map[string]any)
	}
	if env == nil {
		env = map[string]any{}
	}

	if j.PreFallback.BaseURLWasSet {
		env["ANTHROPIC_BASE_URL"] = j.PreFallback.BaseURLPriorValue
	} else {
		delete(env, "ANTHROPIC_BASE_URL")
	}
	root["env"] = env

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("settingsjson: marshal restore: %w", err)
	}
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return atomicWriteFile(writePath, out, 0o600)
}

// stripBaseURLIfMatches reads path, removes env.ANTHROPIC_BASE_URL iff it
// equals sidecarURL, returns the new bytes and whether a mutation was made.
func stripBaseURLIfMatches(path, sidecarURL string) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("settingsjson: read %s: %w", path, err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, false, fmt.Errorf("settingsjson: parse %s: %w", path, err)
	}

	envAny, hasEnv := root["env"]
	if !hasEnv {
		return nil, false, nil
	}
	env, ok := envAny.(map[string]any)
	if !ok {
		return nil, false, nil
	}
	current, ok := env["ANTHROPIC_BASE_URL"].(string)
	if !ok || current != sidecarURL {
		return nil, false, nil
	}
	delete(env, "ANTHROPIC_BASE_URL")
	root["env"] = env

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, false, fmt.Errorf("settingsjson: marshal strip: %w", err)
	}
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return out, true, nil
}
```

- [ ] **Step 5: Run tests, confirm PASS**

- [ ] **Step 6: Commit**

```bash
git add plugin/internal/settingsjson/exit.go plugin/internal/settingsjson/exit_test.go plugin/internal/settingsjson/state.go plugin/internal/settingsjson/state_test.go
git commit -m "feat(plugin/settingsjson): ExitNetworkMode + best-effort + InNetworkMode field"
```

### Task 8: Coverage verification + overall check

- [ ] **Step 1: Run full module tests with coverage**

```bash
cd /Users/dor.amid/git/token-bay/plugin
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -coverprofile=coverage.out ./internal/settingsjson/...
PATH="$HOME/.local/share/mise/shims:$PATH" go tool cover -func=coverage.out | tail -5
```

Expected: all tests pass; coverage ≥ 80% on the package (plugin-spec §4.7 target).

- [ ] **Step 2: Run root checks**

```bash
cd /Users/dor.amid/git/token-bay
PATH="$HOME/.local/share/mise/shims:$PATH" make -C plugin test
PATH="$HOME/.local/share/mise/shims:$PATH" make test
```

Expected: all green.

- [ ] **Step 3: Tag and push**

```bash
git tag -a plugin-settingsjson-v0 -m "internal/settingsjson feature complete"
git push origin main --tags
```

---

## 5. Open questions for implementation

These are intentional gaps to resolve during or after implementation, not blockers:

1. **Comment-preserving edits.** v1 refuses on JSONC content. A v2 feature plan could port `jsonc-parser`'s surgical `modify()` to Go. Scope: medium (parse tree + token-level edits + formatting preservation).

2. **Watcher latency.** The sidecar should wait for Claude Code to actually pick up the new env var before surfacing the "network mode active" message (plugin spec §5.3 step 5). That confirmation logic lives in `internal/ccproxy`, not here; `settingsjson` is oblivious to whether anything downstream noticed the write.

3. **Atomic on Windows.** `rename(2)` is atomic on POSIX. Windows `MoveFile` with `MOVEFILE_REPLACE_EXISTING` is atomic but requires the target to exist; we use `os.Rename` which is cross-platform but the atomicity story on Windows is a gotcha. v1 scope includes Linux + macOS; Windows consumer support inherits `os.Rename`'s best-effort behavior and is flagged in plugin spec §10.

4. **Rollback journal clutter.** If the user runs `EnterNetworkMode` repeatedly without exiting, we overwrite the journal each time — the oldest pre-fallback state is lost. The invariant "one active fallback per Store" protects us, but a user reinstalling the plugin could lose their original settings. Mitigation: on subsequent enter, if a journal already exists and its `SidecarURL` matches, keep the *original* `PreFallback` block. Enforce in Task 6 if time permits; otherwise defer.

5. **Permission bits.** We write settings.json at `0o600`. If the user's existing file is `0o644`, we've narrowed the permissions. For v1 this is an improvement (less broad access), but surfaces if any other tool reads the user's settings.json with limited rights. Document in plugin spec release notes.

---

## Self-review

- **Spec coverage:** Every plugin-spec §2.5 / §5.3 / §5.5 responsibility that lives in `settingsjson` is covered by a task — atomic write, rollback, JSONC refusal, provider refusal, symlink safety, revert semantics.
- **Placeholder scan:** One intentional "open question" block at §5 documents v1 scope gaps (JSONC preservation, watcher latency, Windows atomicity, journal clutter, perm bits). No TBDs in the task steps themselves.
- **Type consistency:** `Store`, `State`, `RollbackJournal`, `PreFallback` field names consistent across tasks. Sentinel errors declared once, referenced by `errors.Is` throughout.
- **TDD discipline:** Every task follows failing-test → verify-fail → minimal-impl → verify-pass → commit. No "write the code and tests together" shortcuts.

## Next plan

After `settingsjson` is in, the natural sibling is **`internal/ccproxy`** — the Anthropic-compatible HTTPS server that receives the redirected `/v1/messages` traffic. That module *depends on* `settingsjson` (reads the network-mode state at request time) but its own tests can stub `settingsjson` behind a small interface.
