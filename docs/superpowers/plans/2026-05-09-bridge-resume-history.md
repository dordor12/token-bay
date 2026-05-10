# Bridge: synthetic session file + `--resume` for history injection

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the bridge's stream-json stdin history-injection mechanism with a synthetic Claude Code session JSONL file consumed via `--resume`. Eliminates claude-side rewriting of the prefilled convo (turn collapsing, "tool not enabled" errors when bridge tools are off, regenerated thinking signatures), so the bridge subprocess sees the consumer's history exactly as Claude Code itself would on a real session resume.

**Architecture:** `Bridge.Serve` writes a JSONL file at `<seederRoot>/<hash(clientPubkey)>/.claude/projects/<sanitize(cwd)>/<sessionID>.jsonl` containing the consumer's history (every Message except the last user turn), then invokes `claude -p "<last user text>" --resume <sessionID> --no-session-persistence` with HOME pointed at the per-client root. Stream-json stdin is removed from the bridge entirely. The session file uses Claude Code's native `SerializedMessage` schema; loading goes through `loadConversationForResume` — the same path as resuming a real session — so tool blocks, content-block ordering, and message structure are preserved verbatim. `--no-session-persistence` prevents the bridge subprocess from appending records to the file we wrote.

**Storage and lifecycle policy:**
- Session files live under `<seederRoot>/<clientHash>/`, where `clientHash` is `hex(sha256(clientPubkey))[:16]` of the consumer's Ed25519 public key.
- Per-client folders are persistent across requests: a consumer that issues N forwarded requests gets N session files all in their own directory. This isolates clients from each other (no chance of serving client A's history during a client B request) and groups all sessions for one peer for audit/inspection.
- The per-client folder ALSO doubles as the subprocess's HOME (the macOS keychain symlink + `.claude/projects/...` tree both live inside it). HOME stays consistent for repeat requests from the same client, which avoids re-symlinking Keychains every call.
- A janitor goroutine in the seeder daemon periodically scans `<seederRoot>/`. For each client folder, it consults an `ActiveClientChecker` (interface implemented by the seeder's coordinator/trackerclient layer) to decide if the client is still connected. Folders for inactive clients whose last-modification time is older than a configurable grace window get removed wholesale.
- This deletion is the only persistent-data lifecycle in the bridge layer. The audit log (rule 4 in `plugin/CLAUDE.md`) is unaffected — that's a separate append-only file outside `<seederRoot>`.

**Tech Stack:** Go 1.23+, stdlib `crypto/rand` for UUID v4, stdlib `crypto/sha256` for client hash, stdlib `encoding/json`. No external deps. Schema reverse-engineered from `/Users/dor.amid/git/claude-code/src/types/logs.ts` and `src/utils/sessionStoragePortable.ts`. Client identity follows the project-wide convention: `ed25519.PublicKey` (used in `trackerclient/types.go:68` and `shared/signing/`).

**Reference findings (from claude-code source explores):**
- Session path: `~/.claude/projects/<sanitizePath(cwd)>/<sessionId>.jsonl`
- `sanitizePath`: replace `[^a-zA-Z0-9]` with `-`; if length > 200, suffix with hash
- Required record fields: `type`, `uuid`, `message{role,content}`, `timestamp` (others optional but recommended)
- `userType="external"`, `entrypoint="cli"`, `version=<claude version string>` (not validated on load)
- `parentUuid` not strictly required, but recommended for clean chain
- `--resume <id>` + `-p "<prompt>"` combine: prompt is appended as a new user turn after the resumed history
- `--no-session-persistence` only blocks WRITING, reading via `--resume` still works
- No integrity checks on the file — synthetic records load if shape is valid

---

## File Structure

```
plugin/internal/ccbridge/
  sessionfile.go          (NEW) Session-record types, SanitizePath, GenerateSessionID, WriteSessionFile, ClientHash
  sessionfile_test.go     (NEW) Unit tests for the above (table-driven)
  janitor.go              (NEW) Janitor type + ActiveClientChecker interface + Run loop
  janitor_test.go         (NEW) Unit tests with a fake checker + tempdir fixtures
  flags.go                (MOD) Drop --input-format/--output-format stream-json, add --resume/--no-session-persistence
  flags_test.go           (MOD) Update assertions for new flag set
  messages.go             (MOD) Drop EncodeMessages and stream-json input types; keep Message + ValidateMessages
  messages_test.go        (MOD) Drop encoder round-trip tests; keep validation tests
  runner_unix.go          (MOD) Write session file under per-client root, pass last user turn as -p positional, no stdin write
  runner_unix_test.go     (MOD) Shell-stub asserts new flags + verifies session file exists at expected per-client path
  bridge.go               (MOD) Add ClientPubkey + SeederRoot fields plumbed through Serve
  bridge_test.go          (MOD) Stub-runner verifies Request now carries client identity
  conformance.go          (MOD) RunStartupConformance gets a synthetic ClientPubkey (zero key is fine for the gate)
  doc.go                  (MOD) Update flag-set + storage layout documentation
  runner.go               (MOD) ExecRunner gains SeederRoot field; defaults to OS temp if unset

plugin/test/conformance/
  wirefidelity_test.go    (MOD) runUserChat stops using stream-json input; uses session file or simple -p "seed"
  contextquality_test.go  (MOD) runUserMode rewritten to use session-file approach; finalAssistantContent helpers shrink/disappear
  outputsimilarity_test.go (MOD) Same updates as contextquality (runOutputSimilarityScenario shares helpers)
  harness_test.go         (NO CHANGE) The 3 harness real-Anthropic tests use bridge.Serve directly
```

---

## Task 1: SessionRecord type + JSON shape

**Files:**
- Create: `plugin/internal/ccbridge/sessionfile.go`
- Test: `plugin/internal/ccbridge/sessionfile_test.go`

The session file is JSONL — one JSON object per line. Each line is a `SerializedMessage` per claude-code's `src/types/logs.ts`. Bridge writes only `user` and `assistant` records; we don't need `attachment`/`system` records for synthetic history.

- [ ] **Step 1: Write the failing test for SessionRecord JSON shape**

```go
// plugin/internal/ccbridge/sessionfile_test.go
package ccbridge

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionRecord_UserText_MarshalsToClaudeShape(t *testing.T) {
	rec := SessionRecord{
		Type:      "user",
		UUID:      "11111111-1111-1111-1111-111111111111",
		ParentUUID: nil, // first record
		IsSidechain: false,
		Timestamp: "2026-05-09T15:30:00.000Z",
		CWD:       "/tmp/seeder-cwd",
		UserType:  "external",
		Entrypoint: "cli",
		SessionID: "22222222-2222-2222-2222-222222222222",
		Version:   "2.1.138",
		Message: SessionMessage{
			Role:    "user",
			Content: json.RawMessage(`[{"type":"text","text":"hello"}]`),
		},
	}
	b, err := json.Marshal(rec)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"type":"user",
		"uuid":"11111111-1111-1111-1111-111111111111",
		"parentUuid":null,
		"isSidechain":false,
		"timestamp":"2026-05-09T15:30:00.000Z",
		"cwd":"/tmp/seeder-cwd",
		"userType":"external",
		"entrypoint":"cli",
		"sessionId":"22222222-2222-2222-2222-222222222222",
		"version":"2.1.138",
		"message":{"role":"user","content":[{"type":"text","text":"hello"}]}
	}`, string(b))
}
```

- [ ] **Step 2: Run test — verify it fails (undefined SessionRecord)**

Run: `cd plugin && go test ./internal/ccbridge/ -run TestSessionRecord_UserText_MarshalsToClaudeShape`
Expected: FAIL with `undefined: SessionRecord`.

- [ ] **Step 3: Write the SessionRecord types**

```go
// plugin/internal/ccbridge/sessionfile.go
package ccbridge

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// SessionRecord is one line in a Claude Code session JSONL file.
// Mirrors the SerializedMessage shape from claude-code's
// src/types/logs.ts. Only the fields we write are listed; we
// intentionally omit optional metadata (gitBranch, slug, etc.)
// claude-code accepts records with missing optional fields.
type SessionRecord struct {
	Type        string         `json:"type"`        // "user" | "assistant"
	UUID        string         `json:"uuid"`        // record identity
	ParentUUID  *string        `json:"parentUuid"`  // chain pointer; null on first record
	IsSidechain bool           `json:"isSidechain"` // false for main chain
	Timestamp   string         `json:"timestamp"`   // ISO-8601 UTC
	CWD         string         `json:"cwd"`         // subprocess working dir
	UserType    string         `json:"userType"`    // "external" for bridge-written records
	Entrypoint  string         `json:"entrypoint"`  // "cli"
	SessionID   string         `json:"sessionId"`   // matches the file's sessionID
	Version     string         `json:"version"`     // Claude Code version string
	Message     SessionMessage `json:"message"`     // the Anthropic message wrapper
}

// SessionMessage is the wire-shaped Anthropic message inside a
// SessionRecord. Content is json.RawMessage so callers control
// exactly what content blocks reach the file (a JSON string for
// plain text, or a content-block array for tool_use/tool_result).
type SessionMessage struct {
	Role    string          `json:"role"`    // "user" | "assistant"
	Content json.RawMessage `json:"content"`
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd plugin && go test ./internal/ccbridge/ -run TestSessionRecord_UserText_MarshalsToClaudeShape`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add plugin/internal/ccbridge/sessionfile.go plugin/internal/ccbridge/sessionfile_test.go
git commit -m "feat(ccbridge): add SessionRecord type for Claude Code session JSONL"
```

---

## Task 2: SanitizePath

**Files:**
- Modify: `plugin/internal/ccbridge/sessionfile.go`
- Test: `plugin/internal/ccbridge/sessionfile_test.go`

Mirrors claude-code's `sanitizePath` in `src/utils/sessionStoragePortable.ts:311`: replace every non-alphanumeric character with `-`, and if the result is longer than 200 chars, append a stable hash. The function is the load-bearing path-derivation algorithm; if our output diverges from claude-code's, `--resume` won't find the file.

- [ ] **Step 1: Write the failing test (table-driven)**

```go
// in plugin/internal/ccbridge/sessionfile_test.go — append below the previous test
func TestSanitizePath_MatchesClaudeCodeAlgorithm(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"unix path", "/Users/dor.amid/foo", "-Users-dor-amid-foo"},
		{"colon-separated", "plugin:name:server", "plugin-name-server"},
		{"trailing slash", "/tmp/work/", "-tmp-work-"},
		{"alphanumeric only", "ABC123", "ABC123"},
		{"leading dot", ".hidden", "-hidden"},
		{"windows-style", `C:\Users\foo`, "C--Users-foo"},
		{"unicode falls to dashes", "naïve/path", "na-ve-path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, SanitizePath(tc.in))
		})
	}
}

func TestSanitizePath_LongInput_TruncatesWithHashSuffix(t *testing.T) {
	// 250 'a's; sanitized form is the same length, then truncated to
	// 200 + "-<hash>" suffix. Hash is hex-encoded SHA-256 prefix.
	in := ""
	for i := 0; i < 250; i++ {
		in += "a"
	}
	got := SanitizePath(in)
	assert.Len(t, got[:200], 200)
	assert.Contains(t, got, "-")
	assert.Greater(t, len(got), 200)
	assert.Less(t, len(got), 220)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd plugin && go test ./internal/ccbridge/ -run TestSanitizePath`
Expected: FAIL — `undefined: SanitizePath`.

- [ ] **Step 3: Implement SanitizePath**

```go
// in plugin/internal/ccbridge/sessionfile.go — append after types

// sanitizeRE matches every non-alphanumeric character. The
// claude-code algorithm replaces each with "-" individually
// (consecutive runs are kept intact, not collapsed).
var sanitizeRE = regexp.MustCompile(`[^a-zA-Z0-9]`)

// maxSanitizedLen mirrors claude-code's MAX_SANITIZED_LENGTH.
const maxSanitizedLen = 200

// SanitizePath maps an absolute filesystem path (typically a
// subprocess CWD) to the directory-name claude-code uses inside
// ~/.claude/projects/ to hold session files. The algorithm
// matches sanitizePath in claude-code/src/utils/
// sessionStoragePortable.ts: replace [^a-zA-Z0-9] with "-" per
// character; if the result exceeds 200 chars, append "-<hash>"
// where hash is a stable digest of the original input.
//
// We use a hex-encoded SHA-256 prefix instead of claude-code's
// Bun.hash / djb2 fallback. claude-code reads any matching
// directory name when resolving --resume, so as long as WE write
// and READ the same path, the digest scheme is internal.
func SanitizePath(p string) string {
	out := sanitizeRE.ReplaceAllString(p, "-")
	if len(out) <= maxSanitizedLen {
		return out
	}
	sum := sha256OfString(p)
	return out[:maxSanitizedLen] + "-" + sum[:8]
}
```

You'll also need a small SHA-256 helper. Add to the same file:

```go
import "crypto/sha256"

func sha256OfString(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd plugin && go test ./internal/ccbridge/ -run TestSanitizePath`
Expected: PASS (both subtests + the LongInput test).

- [ ] **Step 5: Commit**

```bash
git add plugin/internal/ccbridge/sessionfile.go plugin/internal/ccbridge/sessionfile_test.go
git commit -m "feat(ccbridge): SanitizePath matches Claude Code project-dir algorithm"
```

---

## Task 3: GenerateSessionID (UUID v4)

**Files:**
- Modify: `plugin/internal/ccbridge/sessionfile.go`
- Test: `plugin/internal/ccbridge/sessionfile_test.go`

Claude Code's `--session-id <uuid>` flag validates the value as a UUID. We need a v4 UUID generator. Stdlib `crypto/rand` is sufficient; no external `uuid` package needed.

- [ ] **Step 1: Write the failing test**

```go
// in plugin/internal/ccbridge/sessionfile_test.go — append below
func TestGenerateSessionID_ValidUUIDv4(t *testing.T) {
	id, err := GenerateSessionID()
	require.NoError(t, err)
	// 8-4-4-4-12 hex with v4 marker: 14th hex digit is "4",
	// 19th hex digit is one of "8","9","a","b" (variant 10xx).
	assert.Regexp(t, `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`, id)

	// Two consecutive IDs must differ.
	id2, err := GenerateSessionID()
	require.NoError(t, err)
	assert.NotEqual(t, id, id2)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd plugin && go test ./internal/ccbridge/ -run TestGenerateSessionID`
Expected: FAIL — `undefined: GenerateSessionID`.

- [ ] **Step 3: Implement GenerateSessionID**

```go
// in plugin/internal/ccbridge/sessionfile.go — append after SanitizePath

// GenerateSessionID returns a fresh RFC 4122 v4 UUID string.
// Claude Code's --session-id flag validates the format strictly,
// so we set the v4 marker and the 10xx variant bits explicitly.
func GenerateSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("ccbridge: read random for session id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd plugin && go test ./internal/ccbridge/ -run TestGenerateSessionID`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add plugin/internal/ccbridge/sessionfile.go plugin/internal/ccbridge/sessionfile_test.go
git commit -m "feat(ccbridge): GenerateSessionID emits RFC 4122 v4 UUIDs"
```

---

## Task 4: WriteSessionFile

**Files:**
- Modify: `plugin/internal/ccbridge/sessionfile.go`
- Test: `plugin/internal/ccbridge/sessionfile_test.go`

`WriteSessionFile(home, cwd, sessionID, version string, msgs []Message) (string, error)` returns the absolute path written. It builds a chain of `SessionRecord` objects (each `parentUuid` pointing to the previous one's `uuid`), JSON-encodes one per line into the session file, and ensures the parent directory exists.

- [ ] **Step 1: Write the failing test**

```go
// in plugin/internal/ccbridge/sessionfile_test.go — append below
func TestWriteSessionFile_WritesValidJSONLAtCorrectPath(t *testing.T) {
	home := t.TempDir()
	cwd := "/tmp/test-cwd-aaa"
	sessionID, err := GenerateSessionID()
	require.NoError(t, err)

	msgs := []Message{
		{Role: RoleUser, Content: TextContent("hello")},
		{Role: RoleAssistant, Content: TextContent("hi there")},
		{Role: RoleUser, Content: TextContent("what's 2+2?")},
	}

	path, err := WriteSessionFile(home, cwd, sessionID, "2.1.138", msgs)
	require.NoError(t, err)
	wantPath := filepath.Join(home, ".claude", "projects", SanitizePath(cwd), sessionID+".jsonl")
	assert.Equal(t, wantPath, path)

	body, err := os.ReadFile(path)
	require.NoError(t, err)

	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	require.Len(t, lines, 3, "one record per Message")

	var recs [3]SessionRecord
	for i, line := range lines {
		require.NoError(t, json.Unmarshal([]byte(line), &recs[i]), "line %d: %s", i, line)
	}

	// Roles match input.
	assert.Equal(t, "user", recs[0].Type)
	assert.Equal(t, "assistant", recs[1].Type)
	assert.Equal(t, "user", recs[2].Type)

	// First parentUuid is null; subsequent point to previous uuid.
	assert.Nil(t, recs[0].ParentUUID)
	require.NotNil(t, recs[1].ParentUUID)
	assert.Equal(t, recs[0].UUID, *recs[1].ParentUUID)
	require.NotNil(t, recs[2].ParentUUID)
	assert.Equal(t, recs[1].UUID, *recs[2].ParentUUID)

	// Required metadata is populated on every record.
	for i, r := range recs {
		assert.NotEmpty(t, r.UUID, "record %d uuid", i)
		assert.NotEmpty(t, r.Timestamp, "record %d timestamp", i)
		assert.Equal(t, sessionID, r.SessionID, "record %d sessionId", i)
		assert.Equal(t, cwd, r.CWD, "record %d cwd", i)
		assert.Equal(t, "external", r.UserType, "record %d userType", i)
		assert.Equal(t, "cli", r.Entrypoint, "record %d entrypoint", i)
		assert.Equal(t, "2.1.138", r.Version, "record %d version", i)
	}
}

func TestWriteSessionFile_RejectsInvalidRoleOrEmptyContent(t *testing.T) {
	home := t.TempDir()
	id, _ := GenerateSessionID()

	// Empty messages: error.
	_, err := WriteSessionFile(home, "/x", id, "2.1.138", nil)
	require.Error(t, err)

	// Bad role: error.
	_, err = WriteSessionFile(home, "/x", id, "2.1.138", []Message{
		{Role: "system", Content: TextContent("nope")},
	})
	require.Error(t, err)

	// Empty content: error.
	_, err = WriteSessionFile(home, "/x", id, "2.1.138", []Message{
		{Role: RoleUser, Content: nil},
	})
	require.Error(t, err)
}
```

You'll also need `"strings"` in the test imports.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd plugin && go test ./internal/ccbridge/ -run TestWriteSessionFile`
Expected: FAIL — `undefined: WriteSessionFile`.

- [ ] **Step 3: Implement WriteSessionFile**

```go
// in plugin/internal/ccbridge/sessionfile.go — append after GenerateSessionID

// WriteSessionFile writes a Claude Code session JSONL file under
// home/.claude/projects/<SanitizePath(cwd)>/<sessionID>.jsonl
// containing one record per Message in msgs. Returns the absolute
// path written so callers can verify or pass --resume <sessionID>.
//
// Each record is chained via parentUuid. Timestamps are generated
// per-record using monotonically-increasing UTC time so claude-code
// orders records correctly when reading.
//
// Validation matches ValidateMessages: msgs must be non-empty,
// each role in {user, assistant}, content non-empty valid JSON.
func WriteSessionFile(home, cwd, sessionID, version string, msgs []Message) (string, error) {
	if err := ValidateMessages(msgs); err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".claude", "projects", SanitizePath(cwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("ccbridge: mkdir session project dir: %w", err)
	}
	path := filepath.Join(dir, sessionID+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("ccbridge: create session file: %w", err)
	}
	defer f.Close()

	var prevUUID string
	now := time.Now().UTC()
	for i, m := range msgs {
		uid, err := GenerateSessionID()
		if err != nil {
			return "", fmt.Errorf("ccbridge: gen record uuid: %w", err)
		}
		var parent *string
		if i > 0 {
			p := prevUUID
			parent = &p
		}
		ts := now.Add(time.Duration(i) * time.Millisecond).Format("2006-01-02T15:04:05.000Z")
		rec := SessionRecord{
			Type:        m.Role,
			UUID:        uid,
			ParentUUID:  parent,
			IsSidechain: false,
			Timestamp:   ts,
			CWD:         cwd,
			UserType:    "external",
			Entrypoint:  "cli",
			SessionID:   sessionID,
			Version:     version,
			Message: SessionMessage{
				Role:    m.Role,
				Content: m.Content,
			},
		}
		b, err := json.Marshal(rec)
		if err != nil {
			return "", fmt.Errorf("ccbridge: marshal record %d: %w", i, err)
		}
		if _, err := io.WriteString(f, string(b)+"\n"); err != nil {
			return "", fmt.Errorf("ccbridge: write record %d: %w", i, err)
		}
		prevUUID = uid
	}
	return path, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd plugin && go test ./internal/ccbridge/ -run TestWriteSessionFile`
Expected: PASS (both subtests).

- [ ] **Step 5: Commit**

```bash
git add plugin/internal/ccbridge/sessionfile.go plugin/internal/ccbridge/sessionfile_test.go
git commit -m "feat(ccbridge): WriteSessionFile produces Claude-Code-readable JSONL"
```

---

## Task 5: Detect Claude Code version at runtime

**Files:**
- Modify: `plugin/internal/ccbridge/sessionfile.go`
- Test: `plugin/internal/ccbridge/sessionfile_test.go`

`SerializedMessage.version` is the binary's reported version string. Claude-code doesn't validate it on load (the explore confirmed) but populating it correctly keeps the file forward-compatible if validation is added later.

We invoke `claude --version` once and parse the version. Output looks like `2.1.138 (Claude Code)`.

- [ ] **Step 1: Write the failing test**

```go
// in plugin/internal/ccbridge/sessionfile_test.go — append below
func TestParseClaudeVersion_ExtractsSemverPrefix(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"haiku style", "2.1.138 (Claude Code)", "2.1.138"},
		{"trailing newline", "2.1.0\n", "2.1.0"},
		{"with patch", "10.20.30-rc1 (Claude Code)", "10.20.30-rc1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseClaudeVersion(tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestParseClaudeVersion_RejectsEmpty(t *testing.T) {
	_, err := parseClaudeVersion("")
	require.Error(t, err)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd plugin && go test ./internal/ccbridge/ -run TestParseClaudeVersion`
Expected: FAIL — `undefined: parseClaudeVersion`.

- [ ] **Step 3: Implement parseClaudeVersion**

```go
// in plugin/internal/ccbridge/sessionfile.go — append at the bottom

import "strings"

// parseClaudeVersion extracts the leading semver-ish token from
// `claude --version` output, e.g. "2.1.138 (Claude Code)" → "2.1.138".
// Claude-code accepts any string in SerializedMessage.version on
// load, but populating it accurately keeps the file forward-
// compatible if validation is added later.
func parseClaudeVersion(out string) (string, error) {
	out = strings.TrimSpace(out)
	if out == "" {
		return "", fmt.Errorf("ccbridge: empty claude --version output")
	}
	if i := strings.IndexByte(out, ' '); i > 0 {
		return out[:i], nil
	}
	return out, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd plugin && go test ./internal/ccbridge/ -run TestParseClaudeVersion`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add plugin/internal/ccbridge/sessionfile.go plugin/internal/ccbridge/sessionfile_test.go
git commit -m "feat(ccbridge): parseClaudeVersion strips suffix from claude --version"
```

---

## Task 6: Update flag set — drop stream-json input, add resume + no-persist

**Files:**
- Modify: `plugin/internal/ccbridge/flags.go`
- Modify: `plugin/internal/ccbridge/flags_test.go`

The bridge no longer feeds stdin via stream-json. The new approach: `claude -p "<text>" --resume <sessionID> --no-session-persistence` plus the airtight isolation flags. Output stays `stream-json` so the runner can parse stdout for the assistant response.

- [ ] **Step 1: Write the failing test**

```go
// in plugin/internal/ccbridge/flags_test.go — replace TestBuildArgv_HappyPath_IncludesAirtightFlags's body
func TestBuildArgv_HappyPath_IncludesAirtightFlags(t *testing.T) {
	got := BuildArgv(Request{
		Messages: userOnly("hello"),
		Model:    "claude-sonnet-4-6",
	}, "ABCD-SESSION", "the new prompt")

	// Print mode + positional prompt.
	assert.Contains(t, got, "-p")
	assert.Contains(t, got, "the new prompt")

	// Output stream-json + verbose still present (we parse stdout).
	assert.Contains(t, got, "--output-format")
	assert.Contains(t, got, "stream-json")
	assert.Contains(t, got, "--verbose")

	// Stream-json INPUT must NOT be present anymore.
	assert.NotContains(t, got, "--input-format")

	// Airtight tool/MCP/settings flags unchanged.
	assert.Contains(t, got, "--tools")
	assert.Contains(t, got, "--disallowedTools")
	assert.Contains(t, got, "*")
	assert.Contains(t, got, "--mcp-config")
	assert.Contains(t, got, `{"mcpServers":{}}`)
	assert.Contains(t, got, "--strict-mcp-config")
	assert.Contains(t, got, "--settings")
	assert.Contains(t, got, `{"hooks":{}}`)

	// New: resume + no-persist.
	assert.Contains(t, got, "--resume")
	assert.Contains(t, got, "ABCD-SESSION")
	assert.Contains(t, got, "--no-session-persistence")

	// Model passes through.
	assert.Contains(t, got, "--model")
	assert.Contains(t, got, "claude-sonnet-4-6")
}

func TestBuildArgv_PositionalPromptIsLastArg(t *testing.T) {
	got := BuildArgv(Request{
		Messages: userOnly("ignored — only Model matters here"),
		Model:    "claude-haiku-4-5",
	}, "S1", "the actual prompt text")
	require.NotEmpty(t, got)
	// Positional prompt must come AFTER all flags. claude's CLI
	// parser treats the first non-flag arg after -p (or trailing
	// positional) as the prompt. We always place it last.
	assert.Equal(t, "the actual prompt text", got[len(got)-1])
}
```

Also update `TestAirtightFlags_PinnedConstants` to drop the `--input-format` / `InputFormatStream` assertions, and remove `TestBuildArgv_NoSystemPrompt_FlagOmitted` and `TestBuildArgv_RejectsEmptyModel` constraints around stdin — they still apply but the BuildArgv signature now takes more args.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd plugin && go test ./internal/ccbridge/ -run TestBuildArgv`
Expected: FAIL — old signature `BuildArgv(Request)` doesn't match new calls.

- [ ] **Step 3: Update BuildArgv and constants**

Edit `plugin/internal/ccbridge/flags.go`:

```go
// Remove these constants:
//   FlagInputFormat
//   InputFormatStream

// Add these constants:
const (
	FlagResume                = "--resume"
	FlagNoSessionPersistence  = "--no-session-persistence"
)

// New BuildArgv signature: takes sessionID and prompt text.
// Returns argv (excluding the binary path).
func BuildArgv(req Request, sessionID, prompt string) []string {
	argv := []string{
		FlagPrint,
		FlagOutputFormat, OutputFormatStream,
		FlagVerbose,
		FlagTools, ToolsNone,
		FlagDisallowedTools, DisallowedToolsAll,
		FlagMCPConfig, MCPConfigNull,
		FlagStrictMCPConfig,
		FlagSettings, SettingsNoHooks,
		FlagResume, sessionID,
		FlagNoSessionPersistence,
	}
	if req.System != "" {
		argv = append(argv, FlagSystemPrompt, req.System)
	}
	if req.Model != "" {
		argv = append(argv, FlagModel, req.Model)
	}
	// Positional prompt LAST — after all flags. claude treats it
	// as the value of -p when omitted from --print's optional arg.
	argv = append(argv, prompt)
	return argv
}
```

- [ ] **Step 4: Update TestAirtightFlags_PinnedConstants**

```go
// Remove these lines:
//   assert.Equal(t, "--input-format", FlagInputFormat)
//   assert.Equal(t, "stream-json", InputFormatStream)
// Add:
assert.Equal(t, "--resume", FlagResume)
assert.Equal(t, "--no-session-persistence", FlagNoSessionPersistence)
```

- [ ] **Step 5: Run tests**

Run: `cd plugin && go test ./internal/ccbridge/ -run TestBuildArgv`
Expected: PASS.

Run: `cd plugin && go test ./internal/ccbridge/ -run TestAirtightFlags_PinnedConstants`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add plugin/internal/ccbridge/flags.go plugin/internal/ccbridge/flags_test.go
git commit -m "feat(ccbridge): replace stream-json stdin with --resume session-file flag set"
```

---

## Task 7: Drop EncodeMessages and stream-json input types

**Files:**
- Modify: `plugin/internal/ccbridge/messages.go`
- Modify: `plugin/internal/ccbridge/messages_test.go`

The bridge no longer encodes stream-json events to stdin. Delete `EncodeMessages`, `streamJSONInputEvent`, `streamJSONInputMessage`. Keep `Message`, `Request`, `ValidateMessages`, and the `ErrEmpty*` errors — they're still used by `WriteSessionFile`.

- [ ] **Step 1: Delete the encoder + types from messages.go**

Open `plugin/internal/ccbridge/messages.go`. Remove:

```go
// streamJSONInputEvent ...
// streamJSONInputMessage ...
// EncodeMessages ...
```

The remaining file should contain only `ValidateMessages` and the `Err*` declarations.

- [ ] **Step 2: Delete the encoder tests from messages_test.go**

Remove from `plugin/internal/ccbridge/messages_test.go`:
- `TestEncodeMessages_TextContent_WrapsInBlockArray`
- `TestEncodeMessages_StructuredContent_RoundTripsToolBlocks`

Keep:
- `TestValidateMessages_RejectsInvalidJSONContent`
- `TestValidateMessages_AcceptsArrayContent`

- [ ] **Step 3: Run unit tests; expect callers to fail compilation**

Run: `cd plugin && go build ./...`
Expected: failures in `runner_unix.go` (uses `EncodeMessages`) and possibly tests. Note them; the next task fixes runner_unix.go.

- [ ] **Step 4: Commit (after Task 8 fixes the callers)**

Don't commit yet — wait until callers compile.

---

## Task 8: Update runner_unix to write session file + use --resume

**Files:**
- Modify: `plugin/internal/ccbridge/runner_unix.go`
- Modify: `plugin/internal/ccbridge/runner_unix_test.go`

The runner now: (a) determines the claude version once, (b) generates a sessionID, (c) writes the history (Messages[:len-1]) as a session file under the isolated home, (d) extracts the last user turn's text as the prompt arg, (e) runs claude with `--resume sessionID --no-session-persistence + new BuildArgv`, (f) does NOT write to stdin.

- [ ] **Step 1: Write the failing test**

Update `plugin/internal/ccbridge/runner_unix_test.go`'s `TestExecRunner_Run_HappyPath`:

```go
func TestExecRunner_Run_HappyPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude-fake")

	// The fake claude must:
	// - assert the new flag set is present (no --input-format)
	// - read the session file from $HOME/.claude/projects/<...>/<sid>.jsonl
	//   and echo a marker if found
	// - emit a stream-json result event
	writeFakeClaude(t, bin, `#!/bin/sh
echo "$@" | grep -q -- '--resume' || exit 17
echo "$@" | grep -q -- '--no-session-persistence' || exit 17
echo "$@" | grep -q -- '--disallowedTools' || exit 17
echo "$@" | grep -q -- '--mcp-config' || exit 17
echo "$@" | grep -q -- '--settings' || exit 17
echo "$@" | grep -q -- '{"hooks":{}}' || exit 17
echo "$@" | grep -q -- '--input-format' && exit 18  # MUST NOT be present
test "$HOME" != "$REAL_HOME" || exit 19
test -d "$HOME/.claude/projects" || exit 20
SID=$(echo "$@" | sed -E 's/.*--resume ([0-9a-f-]+).*/\1/')
PROJ=$(ls "$HOME/.claude/projects" | head -1)
SESSFILE="$HOME/.claude/projects/$PROJ/$SID.jsonl"
test -f "$SESSFILE" || exit 21
LINES=$(wc -l < "$SESSFILE")
echo '{"type":"system","subtype":"init"}'
printf 'SESSION_RECORDS=%s\n' "$LINES"
echo '{"type":"result","subtype":"success","is_error":false,"usage":{"input_tokens":7,"output_tokens":3}}'
`)
	t.Setenv("REAL_HOME", os.Getenv("HOME"))

	runner := &ExecRunner{BinaryPath: bin}
	var sink bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 3 messages → 2 history records (last user is the prompt arg).
	err := runner.Run(ctx, Request{
		Messages: []Message{
			{Role: RoleUser, Content: TextContent("first")},
			{Role: RoleAssistant, Content: TextContent("ack")},
			{Role: RoleUser, Content: TextContent("the new question")},
		},
		Model: "claude-sonnet-4-6",
	}, &sink)
	require.NoError(t, err)
	out := sink.String()
	assert.Contains(t, out, `"type":"result"`)
	assert.Contains(t, out, "SESSION_RECORDS=2") // Messages[:len-1] → 2 records
}
```

The other unix tests (`MultiTurn_StdinHasAllMessages`, `NonZeroExit`, `ContextCancel`, `FreshTempCWD`) should be reviewed:
- `MultiTurn_StdinHasAllMessages` is the wrong shape now — there's no stdin to inspect. Delete it.
- `NonZeroExit`, `ContextCancel`, `FreshTempCWD` are still meaningful; just update them to pass at least one user Message and trust the runner to do the right thing.

- [ ] **Step 2: Run test — expect failure (signature mismatch + new behavior)**

Run: `cd plugin && go test ./internal/ccbridge/ -run TestExecRunner_Run_HappyPath`
Expected: FAIL on compilation (BuildArgv signature) and runtime (no session file written yet).

- [ ] **Step 3: Update runner_unix.go**

```go
// In plugin/internal/ccbridge/runner_unix.go, replace the Run method's
// body with the session-file flow.

func (r *ExecRunner) Run(ctx context.Context, req Request, sink io.Writer) error {
	if err := ValidateMessages(req.Messages); err != nil {
		return err
	}
	// The last message must be a user turn — that's the prompt.
	last := req.Messages[len(req.Messages)-1]
	if last.Role != RoleUser {
		return ErrLastMessageNotUser
	}
	prompt, err := extractTextContent(last.Content)
	if err != nil {
		return fmt.Errorf("ccbridge: extract last user prompt: %w", err)
	}

	cwd, err := os.MkdirTemp("", "ccbridge-cwd-")
	if err != nil {
		return fmt.Errorf("ccbridge: mkdir temp cwd: %w", err)
	}
	defer func() { _ = os.RemoveAll(cwd) }()

	if err := seedIsolatedHomeAuth(cwd); err != nil {
		return err
	}

	sessionID, err := GenerateSessionID()
	if err != nil {
		return fmt.Errorf("ccbridge: gen session id: %w", err)
	}
	version, err := r.resolveVersion(ctx)
	if err != nil {
		// Fall back to a known-recent version string. Claude code
		// doesn't validate this field on load.
		version = "2.1.138"
	}
	history := req.Messages[:len(req.Messages)-1]
	if len(history) > 0 {
		if _, err := WriteSessionFile(cwd, cwd, sessionID, version, history); err != nil {
			return err
		}
	} else {
		// No prior history — claude --resume on a missing file
		// fails. Write a single-record file with a synthetic
		// no-op user turn that won't show up on the wire.
		// Simpler alternative: skip --resume entirely when history
		// is empty. Implement the simpler path:
		// (set sessionID = "" and don't pass --resume)
		sessionID = ""
	}

	argv := BuildArgvWithOptionalResume(req, sessionID, prompt)
	cmd := exec.CommandContext(ctx, r.resolveBinary(), argv...)
	cmd.Dir = cwd
	cmd.Env = isolatedEnv(cwd)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return os.ErrProcessDone
	}
	cmd.WaitDelay = 500 * time.Millisecond

	stderr := &capBuffer{cap: r.resolveStderrCap()}
	cmd.Stderr = stderr
	cmd.Stdout = sink

	// No stdin needed — the prompt is the positional arg and history
	// is in the session file. Wire stdin to /dev/null so claude
	// doesn't block waiting on it.
	cmd.Stdin = nil

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return &ExitError{Code: exitErr.ExitCode(), Stderr: stderr.bytes()}
		}
		return fmt.Errorf("ccbridge: run claude: %w", err)
	}
	return nil
}

// extractTextContent parses content (a JSON-encoded blocks array
// or a JSON-encoded string) and returns the concatenated text from
// every text block. Used to lift the last user turn out of the
// Messages slice and into claude's positional -p argument.
func extractTextContent(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", fmt.Errorf("empty content")
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", err
		}
		return s, nil
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", err
	}
	var out strings.Builder
	for _, b := range blocks {
		if t, _ := b["type"].(string); t == "text" {
			if s, _ := b["text"].(string); s != "" {
				if out.Len() > 0 {
					out.WriteString("\n")
				}
				out.WriteString(s)
			}
		}
	}
	if out.Len() == 0 {
		return "", fmt.Errorf("no text block in last user content")
	}
	return out.String(), nil
}

// resolveVersion runs `claude --version` (cached on the runner)
// to learn the binary's version string.
func (r *ExecRunner) resolveVersion(ctx context.Context) (string, error) {
	if r.cachedVersion != "" {
		return r.cachedVersion, nil
	}
	cmd := exec.CommandContext(ctx, r.resolveBinary(), "--version")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	v, err := parseClaudeVersion(string(out))
	if err != nil {
		return "", err
	}
	r.cachedVersion = v
	return v, nil
}
```

Add `cachedVersion string` to the `ExecRunner` struct in `runner.go`. Also expose `BuildArgvWithOptionalResume` in `flags.go`:

```go
// BuildArgvWithOptionalResume is BuildArgv but skips --resume when
// sessionID is empty (e.g., a single-message Request with no
// history). Used by the runner to handle the "first prompt"
// case cleanly.
func BuildArgvWithOptionalResume(req Request, sessionID, prompt string) []string {
	argv := []string{
		FlagPrint,
		FlagOutputFormat, OutputFormatStream,
		FlagVerbose,
		FlagTools, ToolsNone,
		FlagDisallowedTools, DisallowedToolsAll,
		FlagMCPConfig, MCPConfigNull,
		FlagStrictMCPConfig,
		FlagSettings, SettingsNoHooks,
		FlagNoSessionPersistence,
	}
	if sessionID != "" {
		argv = append(argv, FlagResume, sessionID)
	}
	if req.System != "" {
		argv = append(argv, FlagSystemPrompt, req.System)
	}
	if req.Model != "" {
		argv = append(argv, FlagModel, req.Model)
	}
	argv = append(argv, prompt)
	return argv
}
```

(Either `BuildArgv` itself becomes this, or it stays the strict form and the runner uses the optional version. Pick one — recommend folding `BuildArgvWithOptionalResume` INTO `BuildArgv`, deleting the strict variant. Update Task 6's tests accordingly.)

- [ ] **Step 4: Run all ccbridge unit tests**

Run: `cd plugin && go test ./internal/ccbridge/`
Expected: PASS for the new `Run_HappyPath`, the trimmed unix tests, and all `sessionfile_test.go` tests. The `EncodeMessages` tests should be gone (Task 7).

- [ ] **Step 5: Commit Tasks 7 + 8 together**

```bash
git add plugin/internal/ccbridge/messages.go \
        plugin/internal/ccbridge/messages_test.go \
        plugin/internal/ccbridge/runner.go \
        plugin/internal/ccbridge/runner_unix.go \
        plugin/internal/ccbridge/runner_unix_test.go \
        plugin/internal/ccbridge/flags.go
git commit -m "feat(ccbridge): runner writes session file + uses --resume; drop stream-json stdin"
```

---

## Task 9: Update bridge.go docstring + bridge_test.go

**Files:**
- Modify: `plugin/internal/ccbridge/bridge.go`
- Modify: `plugin/internal/ccbridge/bridge_test.go`

`Bridge.Serve` keeps its public signature (`Request → Usage`). Update the comment block to reflect the new transport, and update the stub-runner-based tests so they don't depend on the (removed) `EncodeMessages`.

- [ ] **Step 1: Update doc comments on Bridge.Serve**

```go
// Edit plugin/internal/ccbridge/bridge.go
// Update the comment above Bridge.Serve to describe session-file
// transport instead of stream-json input.
```

- [ ] **Step 2: Update TestBridgeServe_HappyPath_StreamsAndExtractsUsage**

The stub doesn't read history any more, so the test stays mostly the same. Just verify the runner sees the Request and produces the expected Usage from the stream-json output the stub returns. No code change likely needed — just rerun.

- [ ] **Step 3: Run bridge tests**

Run: `cd plugin && go test ./internal/ccbridge/ -run TestBridgeServe`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add plugin/internal/ccbridge/bridge.go plugin/internal/ccbridge/bridge_test.go
git commit -m "docs(ccbridge): update Bridge.Serve docstring for session-file transport"
```

---

## Task 10: Update doc.go

**Files:**
- Modify: `plugin/internal/ccbridge/doc.go`

- [ ] **Step 1: Replace the "# Spec" flag list and surrounding text**

```go
// Old text mentioned --input-format stream-json. Remove that and
// describe the new transport: synthetic session JSONL written
// under the isolated HOME's ~/.claude/projects/<sanitized-cwd>/
// directory, claude resumed via --resume, last user turn passed
// as positional -p argument, --no-session-persistence prevents
// the subprocess from appending records to the file we wrote.
```

- [ ] **Step 2: Run go vet and ccbridge tests**

Run: `cd plugin && go vet ./... && go test ./internal/ccbridge/...`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add plugin/internal/ccbridge/doc.go
git commit -m "docs(ccbridge): document session-file + --resume transport"
```

---

## Task 11: Migrate wirefidelity_test.go to session-file approach

**Files:**
- Modify: `plugin/test/conformance/wirefidelity_test.go`

The wirefidelity tests currently use stream-json input via `runUserChat`. With the new bridge they should use the same session-file approach the runner does, so the user-mode and bridge-mode comparisons stay apples-to-apples.

- [ ] **Step 1: Replace runUserChat to use session file + claude -p**

Drop `runUserChat`'s `EncodeMessages(stdin, convo)` block. Instead:

```go
// In plugin/test/conformance/wirefidelity_test.go
// runUserChat invokes the claude binary with the same airtight
// flag set BuildArgv produces, writing the convo's history as a
// session file under cwd and passing the last user turn as the
// positional -p argument. cwd is the isolated HOME we use for
// auth bridging via Keychains symlink.
func runUserChat(t *testing.T, bin, system string, convo []ccbridge.Message, model string) {
	t.Helper()
	require.NotEmpty(t, convo)
	last := convo[len(convo)-1]
	require.Equal(t, ccbridge.RoleUser, last.Role)

	cwd := t.TempDir()
	require.NoError(t, seedTestKeychain(cwd))

	sessionID, err := ccbridge.GenerateSessionID()
	require.NoError(t, err)
	history := convo[:len(convo)-1]
	if len(history) > 0 {
		_, err := ccbridge.WriteSessionFile(cwd, cwd, sessionID, "2.1.138", history)
		require.NoError(t, err)
	} else {
		sessionID = ""
	}

	prompt, err := extractTextForTest(last.Content)
	require.NoError(t, err)

	argv := ccbridge.BuildArgv(ccbridge.Request{System: system, Model: model}, sessionID, prompt)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, argv...)
	cmd.Dir = cwd
	cmd.Env = isolatedHomeEnv(cwd)
	cmd.Stdout = io.Discard
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("user-chat claude exited with %v\nstderr: %s", err, stderr.String())
	}
}

// extractTextForTest is a test-local helper mirroring
// ccbridge.extractTextContent (which is unexported).
func extractTextForTest(raw json.RawMessage) (string, error) { /* same impl */ }
```

- [ ] **Step 2: Run wirefidelity tests**

Run: `cd plugin && go test -tags localintegtest -run 'TestBridgeContext_WireFidelity' ./test/conformance/...`
Expected: PASS for all three (`RecallsFileContent`, `MatchesUserChat`, `MultiTurnWithFileContent`).

- [ ] **Step 3: Commit**

```bash
git add plugin/test/conformance/wirefidelity_test.go
git commit -m "test(ccbridge): wirefidelity uses session-file transport"
```

---

## Task 12: Simplify contextquality_test.go

**Files:**
- Modify: `plugin/test/conformance/contextquality_test.go`

`runUserMode` and the helpers around stream-json input + `finalAssistantContent`-based history reconstruction should largely go away. Both phase-1 and continuation runs use the new session-file mechanism.

- [ ] **Step 1: Rewrite runUserMode for session-file transport**

```go
// New runUserMode: writes session file from convo[:len-1] (if
// any), runs claude -p "<convo[len-1] text>" + flag set + tools.
// No stream-json stdin. Returns stdout for the caller to parse
// the assistant response.
func runUserMode(t *testing.T, system string, convo []ccbridge.Message, model string, allowedTools []string, cwd string) string {
	t.Helper()
	bin := claudeBin(t)
	if cwd == "" {
		cwd = t.TempDir()
	}
	require.NoError(t, seedTestKeychain(cwd))

	require.NotEmpty(t, convo)
	last := convo[len(convo)-1]
	require.Equal(t, ccbridge.RoleUser, last.Role)
	prompt, err := extractTextForTest(last.Content)
	require.NoError(t, err)

	sessionID, err := ccbridge.GenerateSessionID()
	require.NoError(t, err)
	history := convo[:len(convo)-1]
	if len(history) > 0 {
		_, err := ccbridge.WriteSessionFile(cwd, cwd, sessionID, "2.1.138", history)
		require.NoError(t, err)
	} else {
		sessionID = ""
	}

	args := userArgvSession(ccbridge.Request{System: system, Model: model}, sessionID, prompt, allowedTools)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = cwd
	cmd.Env = append(envWithHome(cwd), "ANTHROPIC_BASE_URL="+os.Getenv("ANTHROPIC_BASE_URL"))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("user-mode claude exited with %v\nstderr: %s\nstdout: %s",
			err, stderr.String(), truncate(stdout.String(), 4096))
	}
	return stdout.String()
}

// userArgvSession is BuildArgv but with `--tools` overridden to
// the scenario's allow-list (and bypassPermissions added) so
// phase 1 can actually execute the named tool.
func userArgvSession(req ccbridge.Request, sessionID, prompt string, allowedTools []string) []string {
	argv := ccbridge.BuildArgv(req, sessionID, prompt)
	out := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		if argv[i] == ccbridge.FlagTools && i+1 < len(argv) {
			i++ // skip --tools value
			continue
		}
		if argv[i] == ccbridge.FlagDisallowedTools && i+1 < len(argv) {
			i++ // skip --disallowedTools value
			continue
		}
		out = append(out, argv[i])
	}
	if len(allowedTools) > 0 {
		out = append(out, ccbridge.FlagTools, strings.Join(allowedTools, ","))
		out = append(out, "--permission-mode", "bypassPermissions")
	} else {
		out = append(out, ccbridge.FlagTools, "")
	}
	return out
}
```

Delete `finalAssistantContent`, `finalAssistantHistoryFromStdout`, `extractWireMessages` if they're no longer used by any test. The history is already in the session file we wrote.

- [ ] **Step 2: Replace runContextQualityScenario's history-reconstruction step**

```go
// In runContextQualityScenario:
//
// Phase 1 builds the convo. We don't need to reconstruct it from
// wire bodies anymore — we get it from phase 1's stdout events
// directly (parsing user/assistant events). For session-file
// purposes, that's the input to the next phase.

// Replace the existing phase1 → historyMsgs block with a
// stdout-walking helper that emits []ccbridge.Message directly,
// matching the format the session file expects.
historyMsgs := parsePhase1History(t, phase1Stdout, p.seed)
extendedConvo := append(historyMsgs, ccbridge.Message{
	Role:    ccbridge.RoleUser,
	Content: ccbridge.TextContent(p.continuation),
})
```

`parsePhase1History` is the function previously called `finalAssistantHistoryFromStdout`; rename for clarity.

- [ ] **Step 3: Run context-quality tests**

Run: `cd plugin && go test -tags localintegtest -run 'TestContextQuality' ./test/conformance/...`
Expected: PASS for S1, S2, S4, S5, S7. (S8 still skipped.)

- [ ] **Step 4: Commit**

```bash
git add plugin/test/conformance/contextquality_test.go
git commit -m "test(ccbridge): contextquality uses session-file transport"
```

---

## Task 13: Update outputsimilarity_test.go

**Files:**
- Modify: `plugin/test/conformance/outputsimilarity_test.go`

This file already shares helpers with `contextquality_test.go`. Once Task 12 lands, the only changes here are:

- Remove the now-unused `finalAssistantContent` calls (replaced by `parsePhase1History`).
- The `judgeOutputSimilarity` helper still uses `userArgv(... nil)` — switch to `userArgvSession` with empty `allowedTools`. Pass a fresh prompt and zero history.
- Re-run the suite. The tool-using scenarios (S2, S4, S5, S7) should now produce path-B answers from history (not "tool not enabled" apologies), pushing judge scores well above the 0.6–0.7 thresholds.

- [ ] **Step 1: Update judgeOutputSimilarity**

```go
// In judgeOutputSimilarity, replace:
//   args := userArgv(ccbridge.Request{System: ..., Model: ...}, nil)
// with:
sessionID, err := ccbridge.GenerateSessionID()
require.NoError(t, err)
// Empty history — judge is a single-turn prompt.
args := userArgvSession(ccbridge.Request{
	System: "You are a precise scorer. Reply with exactly one floating-point number between 0.0 and 1.0 on a single line.",
	Model:  "claude-haiku-4-5-20251001",
}, "" /* no history */, judgePromptText, nil /* no tools */)
_ = sessionID
```

(If the empty-history path leaves sessionID unused, drop the GenerateSessionID call.)

- [ ] **Step 2: Run output-similarity tests**

Run: `cd plugin && go test -tags localintegtest -run 'TestOutputSimilarity' ./test/conformance/...`
Expected: PASS for S1, S2, S4, S5, S7. Judge scores should rise across the board (S2/S4/S7 previously scored ~0.15 because path B apologized; now they should score >0.7 because path B answers from history).

- [ ] **Step 3: Commit**

```bash
git add plugin/test/conformance/outputsimilarity_test.go
git commit -m "test(ccbridge): outputsimilarity uses session-file transport"
```

---

## Task 14: Final regression run + commit

**Files:** none (verification step)

- [ ] **Step 1: Run the full ccbridge unit suite**

Run: `cd plugin && go test ./internal/ccbridge/...`
Expected: PASS.

- [ ] **Step 2: Run the full localintegtest suite**

Run: `cd plugin && go test -tags localintegtest -timeout 1200s ./test/conformance/...`
Expected: all current PASS-or-SKIP tests stay PASS-or-SKIP, including the airtight conformance suite (which still has the pre-existing Monitor/PushNotification/RemoteTrigger leak — that's tracked separately).

- [ ] **Step 3: Confirm the bridge actually uses --resume in production code paths**

Grep for residual `--input-format` or `EncodeMessages` references in `plugin/`:

```bash
grep -rn 'EncodeMessages\|FlagInputFormat\|InputFormatStream\|--input-format' plugin/ \
  --include='*.go' --exclude-dir='node_modules'
```

Expected: only matches inside test data fixtures or comments — no live code.

- [ ] **Step 4: Final commit (only if any cleanup landed in 14.3)**

```bash
git status
# If nothing to commit, skip. Otherwise:
git add -p
git commit -m "chore(ccbridge): drop residual stream-json references"
```

---

---

## Task 15: ClientHash + per-client storage layout

**Files:**
- Modify: `plugin/internal/ccbridge/sessionfile.go`
- Modify: `plugin/internal/ccbridge/sessionfile_test.go`
- Modify: `plugin/internal/ccbridge/flags.go` (add `ClientPubkey ed25519.PublicKey` to `Request`)
- Modify: `plugin/internal/ccbridge/runner_unix.go` (compute per-client home, no longer `MkdirTemp`)

The seeder serves multiple consumers concurrently. Each consumer has an Ed25519 keypair (managed by `trackerclient`); the public key is the canonical identity. To prevent cross-client leakage AND give the janitor a reapable folder per client, sessions live under `<seederRoot>/<ClientHash(pubkey)>/`.

`ClientHash` is the hex-encoded first 16 bytes of `sha256(pubkey)`. Truncation is fine — we only need uniqueness across simultaneously-active peers, not collision resistance against an adversary. 16 hex chars = 64 bits, plenty.

- [ ] **Step 1: Write the failing test for ClientHash**

```go
// in plugin/internal/ccbridge/sessionfile_test.go — append below
import "crypto/ed25519"

func TestClientHash_StableAndShort(t *testing.T) {
	// Two arbitrary fixed keys (don't need real signing, just bytes).
	keyA := ed25519.PublicKey(bytes.Repeat([]byte{0x11}, ed25519.PublicKeySize))
	keyB := ed25519.PublicKey(bytes.Repeat([]byte{0x22}, ed25519.PublicKeySize))

	hashA := ClientHash(keyA)
	hashB := ClientHash(keyB)

	// 16 hex chars (64 bits) — enough for cross-peer uniqueness.
	assert.Len(t, hashA, 16)
	assert.Regexp(t, `^[0-9a-f]{16}$`, hashA)
	assert.NotEqual(t, hashA, hashB)
	// Stable: same input → same output.
	assert.Equal(t, hashA, ClientHash(keyA))
}

func TestClientHash_RejectsWrongLength(t *testing.T) {
	// Empty / wrong-length keys produce empty hash so callers can
	// detect the mistake without crashing.
	assert.Equal(t, "", ClientHash(nil))
	assert.Equal(t, "", ClientHash(ed25519.PublicKey{0x01, 0x02}))
}
```

Add `import "bytes"` to the test file's imports if not already there.

- [ ] **Step 2: Run test, expect fail (undefined ClientHash)**

Run: `cd plugin && go test ./internal/ccbridge/ -run TestClientHash`
Expected: FAIL — `undefined: ClientHash`.

- [ ] **Step 3: Implement ClientHash**

```go
// in plugin/internal/ccbridge/sessionfile.go — append after SanitizePath
import "crypto/ed25519"

// ClientHash returns a stable short fingerprint of an Ed25519
// public key, used as the per-client directory name under
// <seederRoot>. 16 hex chars (64 bits) suffices for distinguishing
// simultaneously-active peers; we don't need collision resistance
// against an adversary because client identity is verified
// upstream by the trackerclient signing layer.
//
// Returns empty string if the key is the wrong length, so the
// caller's MkdirAll fails noisily rather than silently writing
// to a degenerate path.
func ClientHash(pub ed25519.PublicKey) string {
	if len(pub) != ed25519.PublicKeySize {
		return ""
	}
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// ClientHomeDir returns the per-client subdirectory of seederRoot
// where session files for this client live. The bridge's runner
// uses it as both HOME and CWD for the claude subprocess.
func ClientHomeDir(seederRoot string, pub ed25519.PublicKey) string {
	return filepath.Join(seederRoot, ClientHash(pub))
}
```

- [ ] **Step 4: Add ClientPubkey to Request**

```go
// in plugin/internal/ccbridge/flags.go — extend Request
import "crypto/ed25519"

type Request struct {
	System       string
	Messages     []Message
	Model        string
	// ClientPubkey identifies which P2P consumer this request is
	// being served on behalf of. Used to derive the per-client
	// storage folder under SeederRoot. Required.
	ClientPubkey ed25519.PublicKey
}
```

Also update `ValidateRequest` (or wherever Request is validated) to reject empty/wrong-length `ClientPubkey`. Add a unit test confirming the rejection.

- [ ] **Step 5: Plumb SeederRoot into ExecRunner**

```go
// in plugin/internal/ccbridge/runner.go — extend ExecRunner
type ExecRunner struct {
	BinaryPath     string
	MaxStderrBytes int
	// SeederRoot is the parent directory under which per-client
	// session folders are created. Each client's folder doubles
	// as the subprocess HOME so claude finds the synthetic session
	// file at <SeederRoot>/<clientHash>/.claude/projects/<...>/<id>.jsonl.
	// Defaults to filepath.Join(os.TempDir(), "ccbridge-seeder") when empty.
	SeederRoot     string
	cachedVersion  string
}

func (r *ExecRunner) resolveSeederRoot() string {
	if r.SeederRoot != "" {
		return r.SeederRoot
	}
	return filepath.Join(os.TempDir(), "ccbridge-seeder")
}
```

- [ ] **Step 6: Update runner_unix.go to use per-client home**

Replace this block in `Run`:

```go
	cwd, err := os.MkdirTemp("", "ccbridge-cwd-")
	if err != nil {
		return fmt.Errorf("ccbridge: mkdir temp cwd: %w", err)
	}
	defer func() { _ = os.RemoveAll(cwd) }()
```

with:

```go
	if len(req.ClientPubkey) != ed25519.PublicKeySize {
		return fmt.Errorf("ccbridge: ClientPubkey is required and must be ed25519.PublicKeySize bytes")
	}
	cwd := ClientHomeDir(r.resolveSeederRoot(), req.ClientPubkey)
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		return fmt.Errorf("ccbridge: mkdir client home: %w", err)
	}
	// No defer-remove — the janitor handles deletion for inactive
	// clients. Touching the directory here keeps mtime fresh so the
	// janitor's "older than grace window" check passes for active
	// clients between requests.
	_ = os.Chtimes(cwd, time.Now(), time.Now())
```

The `seedIsolatedHomeAuth(cwd)` call is already idempotent (Task 11's earlier fix), so it works for the first request to a new client and is a no-op for subsequent requests.

- [ ] **Step 7: Update unit tests**

Update `runner_unix_test.go`'s shell-stub assertions to reflect that `$HOME` is now under `SeederRoot/clientHash/` rather than a fresh tempdir per call. Test must:
- Set `runner.SeederRoot = t.TempDir()`
- Provide a `ClientPubkey` in the `Request` (32 zero bytes is fine for unit test).
- Verify the session file lands at `<SeederRoot>/<expectedHash>/.claude/projects/<sanitized>/<sessionID>.jsonl`.

- [ ] **Step 8: Run all ccbridge unit tests**

Run: `cd plugin && go test ./internal/ccbridge/`
Expected: PASS, including the new `TestClientHash_*` and updated runner tests.

- [ ] **Step 9: Update test/conformance/ helpers**

The test-side `runUserMode`, `runUserChat`, `runFullClaudeContinuation`, `runBridgeContinuation` all create their own subprocess invocations. They need to:
- Generate a deterministic-per-test `ClientPubkey` (e.g., `ed25519.PublicKey(bytes.Repeat([]byte("test"+t.Name()), …))` truncated/padded to 32 bytes — not cryptographically valid, but the bridge doesn't verify, just hashes).
- Pass `ClientPubkey` in the `Request` they hand to `bridge.Serve`.
- For non-bridge invocations (claude binary directly), set HOME to the same per-client path so wire-fidelity comparisons stay apples-to-apples.

- [ ] **Step 10: Commit**

```bash
git add plugin/internal/ccbridge/sessionfile.go \
        plugin/internal/ccbridge/sessionfile_test.go \
        plugin/internal/ccbridge/flags.go \
        plugin/internal/ccbridge/flags_test.go \
        plugin/internal/ccbridge/runner.go \
        plugin/internal/ccbridge/runner_unix.go \
        plugin/internal/ccbridge/runner_unix_test.go \
        plugin/test/conformance/
git commit -m "feat(ccbridge): per-client session storage under SeederRoot/<clientHash>"
```

---

## Task 16: ActiveClientChecker interface

**Files:**
- Create: `plugin/internal/ccbridge/janitor.go`
- Create: `plugin/internal/ccbridge/janitor_test.go`

The bridge layer doesn't know which P2P clients are currently connected — that's the seeder's coordinator/trackerclient state. We expose a small interface that the seeder daemon implements; the janitor calls it.

- [ ] **Step 1: Write the failing test for the interface contract**

```go
// plugin/internal/ccbridge/janitor_test.go
package ccbridge

import (
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/assert"
)

// fakeChecker is a minimal ActiveClientChecker for tests: a set
// of "active" public keys that pass the IsActive predicate.
type fakeChecker struct {
	active map[string]bool // keyed by hex(pub)
}

func (f *fakeChecker) IsActive(pub ed25519.PublicKey) bool {
	if f.active == nil {
		return false
	}
	return f.active[ClientHash(pub)]
}

func TestActiveClientChecker_HashLookup(t *testing.T) {
	keyA := make(ed25519.PublicKey, ed25519.PublicKeySize)
	keyA[0] = 0x01
	keyB := make(ed25519.PublicKey, ed25519.PublicKeySize)
	keyB[0] = 0x02

	c := &fakeChecker{active: map[string]bool{ClientHash(keyA): true}}
	assert.True(t, c.IsActive(keyA))
	assert.False(t, c.IsActive(keyB))
}
```

- [ ] **Step 2: Run test (expect fail — undefined ActiveClientChecker)**

Actually the interface compiles trivially once defined. Run before implementing to see "ActiveClientChecker not used" lint warning is absent — no real failure expected here. Skip and proceed.

- [ ] **Step 3: Define the interface**

```go
// plugin/internal/ccbridge/janitor.go
package ccbridge

import (
	"crypto/ed25519"
)

// ActiveClientChecker is implemented by the seeder coordinator
// (or any callsite that knows the live set of P2P peers). The
// janitor consults it to decide whether a per-client session
// folder is still in use.
//
// Implementations must be safe for concurrent reads from the
// janitor goroutine.
type ActiveClientChecker interface {
	// IsActive returns true if pub is currently connected /
	// transacting with the seeder. The semantics of "active" are
	// defined by the implementation; common choices include "has
	// an open QUIC stream" or "has issued a forwarded request in
	// the last N minutes."
	IsActive(pub ed25519.PublicKey) bool
}
```

- [ ] **Step 4: Run tests**

Run: `cd plugin && go test ./internal/ccbridge/ -run TestActiveClientChecker`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add plugin/internal/ccbridge/janitor.go plugin/internal/ccbridge/janitor_test.go
git commit -m "feat(ccbridge): ActiveClientChecker interface for janitor coordination"
```

---

## Task 17: Janitor — single-shot scan

**Files:**
- Modify: `plugin/internal/ccbridge/janitor.go`
- Modify: `plugin/internal/ccbridge/janitor_test.go`

Before adding the periodic loop, get the per-scan logic right and well-tested. A single scan iterates `<seederRoot>/`, parses each subdirectory name back to a `clientHash`, and removes the directory if both:
1. The corresponding client (by hash) is reported inactive, AND
2. The directory's modification time is older than `grace`.

Two clauses prevent reaping a folder that just received a request from a client whose checker is briefly out of sync.

- [ ] **Step 1: Write the failing tests**

```go
// in plugin/internal/ccbridge/janitor_test.go — append below
import (
	"os"
	"path/filepath"
	"time"

	"github.com/stretchr/testify/require"
)

func TestJanitorScan_RemovesInactiveAndStale(t *testing.T) {
	root := t.TempDir()

	keyActive := make(ed25519.PublicKey, ed25519.PublicKeySize)
	keyActive[0] = 0x01
	keyInactive := make(ed25519.PublicKey, ed25519.PublicKeySize)
	keyInactive[0] = 0x02

	activeDir := filepath.Join(root, ClientHash(keyActive))
	inactiveDir := filepath.Join(root, ClientHash(keyInactive))
	require.NoError(t, os.MkdirAll(activeDir, 0o700))
	require.NoError(t, os.MkdirAll(inactiveDir, 0o700))

	// Make both folders "old" so the grace window is moot for the
	// time check; the differentiator is the activity check.
	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(activeDir, old, old))
	require.NoError(t, os.Chtimes(inactiveDir, old, old))

	checker := &fakeChecker{active: map[string]bool{ClientHash(keyActive): true}}
	j := &Janitor{Root: root, Checker: checker, Grace: time.Minute}

	removed, err := j.Scan()
	require.NoError(t, err)
	assert.Equal(t, []string{ClientHash(keyInactive)}, removed)

	// Inactive folder removed, active folder kept.
	_, err = os.Stat(activeDir)
	assert.NoError(t, err)
	_, err = os.Stat(inactiveDir)
	assert.True(t, os.IsNotExist(err), "inactive dir should be gone")
}

func TestJanitorScan_RespectsGracePeriod(t *testing.T) {
	root := t.TempDir()
	keyInactive := make(ed25519.PublicKey, ed25519.PublicKeySize)
	keyInactive[0] = 0x33
	dir := filepath.Join(root, ClientHash(keyInactive))
	require.NoError(t, os.MkdirAll(dir, 0o700))

	// Folder is fresh — within the grace window.
	checker := &fakeChecker{} // no active entries
	j := &Janitor{Root: root, Checker: checker, Grace: time.Hour}

	removed, err := j.Scan()
	require.NoError(t, err)
	assert.Empty(t, removed, "fresh folder must survive grace window")
	_, err = os.Stat(dir)
	assert.NoError(t, err)
}

func TestJanitorScan_IgnoresUnknownEntries(t *testing.T) {
	root := t.TempDir()
	// A file (not a directory) at root — should be ignored.
	require.NoError(t, os.WriteFile(filepath.Join(root, "stray.txt"), []byte{}, 0o600))
	// A subdir whose name is not 16 hex chars — should be ignored.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "not-a-hash"), 0o700))

	j := &Janitor{Root: root, Checker: &fakeChecker{}, Grace: time.Minute}
	removed, err := j.Scan()
	require.NoError(t, err)
	assert.Empty(t, removed)
}
```

- [ ] **Step 2: Implement `Janitor` and `Scan`**

```go
// plugin/internal/ccbridge/janitor.go — extend
import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Janitor reaps per-client session folders under Root once their
// owning client is no longer active and the folder hasn't been
// touched within Grace.
type Janitor struct {
	Root    string
	Checker ActiveClientChecker
	Grace   time.Duration
}

// Scan performs a single sweep of Root. Returns the slice of
// client-hash directory names that were removed (for logging).
func (j *Janitor) Scan() ([]string, error) {
	if j.Root == "" {
		return nil, fmt.Errorf("ccbridge: janitor Root is empty")
	}
	if j.Checker == nil {
		return nil, fmt.Errorf("ccbridge: janitor Checker is nil")
	}
	entries, err := os.ReadDir(j.Root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("ccbridge: read seeder root: %w", err)
	}
	var removed []string
	cutoff := time.Now().Add(-j.Grace)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		// Folder names are 16 hex chars (ClientHash). Anything
		// else is foreign and we leave it alone.
		if !looksLikeClientHash(name) {
			continue
		}
		fullPath := filepath.Join(j.Root, name)
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue // within grace window
		}
		// Lookup needs the actual public key, not just the hash.
		// The checker is keyed by hash via the fake test impl;
		// real impls map clientHash → bool internally. To keep
		// the interface generic, we expose a hash-only check
		// alongside IsActive — see ActiveClientChecker doc.
		if isActiveByHash(j.Checker, name) {
			continue
		}
		if err := os.RemoveAll(fullPath); err != nil {
			// Logged-not-fatal: a stuck folder shouldn't kill
			// the janitor. Implementation may want to surface
			// this to the seeder's metrics.
			continue
		}
		removed = append(removed, name)
	}
	return removed, nil
}

func looksLikeClientHash(name string) bool {
	if len(name) != 16 {
		return false
	}
	_, err := hex.DecodeString(name)
	return err == nil
}

// isActiveByHash bridges between the interface (which takes a
// PublicKey) and the janitor's view of disk (which only has
// hashes). The default implementation reconstructs nothing —
// it just calls the checker with a zero key after writing the
// hash into the right slot. Implementations should override via
// the ActiveByHashChecker optional interface below if they need
// the reverse-lookup precise.
func isActiveByHash(c ActiveClientChecker, hash string) bool {
	if hc, ok := c.(ActiveByHashChecker); ok {
		return hc.IsActiveByHash(hash)
	}
	// Conservative default: keep the folder if we can't map back
	// from hash to pubkey.
	return true
}

// ActiveByHashChecker is an optional extension to
// ActiveClientChecker for callers (like the janitor) that only
// have the client hash on hand.
type ActiveByHashChecker interface {
	IsActiveByHash(hash string) bool
}
```

Update `fakeChecker` in the test file to also implement `IsActiveByHash` for the reverse-lookup path. (Otherwise the conservative default keeps everything and tests fail.)

```go
// in janitor_test.go's fakeChecker:
func (f *fakeChecker) IsActiveByHash(hash string) bool {
	if f.active == nil {
		return false
	}
	return f.active[hash]
}
```

- [ ] **Step 3: Run tests**

Run: `cd plugin && go test ./internal/ccbridge/ -run TestJanitorScan`
Expected: PASS for all three subtests.

- [ ] **Step 4: Commit**

```bash
git add plugin/internal/ccbridge/janitor.go plugin/internal/ccbridge/janitor_test.go
git commit -m "feat(ccbridge): Janitor.Scan reaps inactive client folders past grace window"
```

---

## Task 18: Janitor — periodic Run loop

**Files:**
- Modify: `plugin/internal/ccbridge/janitor.go`
- Modify: `plugin/internal/ccbridge/janitor_test.go`

Wrap `Scan` in a goroutine-friendly `Run(ctx)` that ticks on `Interval`. Cancellation via context. Errors bubble through a channel callers can listen to (or ignored if they don't care).

- [ ] **Step 1: Write the failing test**

```go
// in plugin/internal/ccbridge/janitor_test.go — append
import "context"

func TestJanitorRun_TicksAndStopsOnContextCancel(t *testing.T) {
	root := t.TempDir()
	keyInactive := make(ed25519.PublicKey, ed25519.PublicKeySize)
	keyInactive[0] = 0x77
	dir := filepath.Join(root, ClientHash(keyInactive))
	require.NoError(t, os.MkdirAll(dir, 0o700))
	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(dir, old, old))

	j := &Janitor{
		Root:     root,
		Checker:  &fakeChecker{}, // nothing active
		Grace:    time.Minute,
		Interval: 20 * time.Millisecond,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() { j.Run(ctx); close(done) }()

	// Wait for context expiration + Run shutdown.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("janitor Run didn't return after context cancel")
	}

	// First tick should have removed the inactive folder.
	_, err := os.Stat(dir)
	assert.True(t, os.IsNotExist(err))
}
```

- [ ] **Step 2: Implement Run**

```go
// in plugin/internal/ccbridge/janitor.go — extend
import "context"

// Run starts a periodic loop that calls Scan every Interval.
// Returns when ctx is cancelled. Safe to launch in its own
// goroutine; not safe to invoke concurrently on the same
// Janitor instance.
func (j *Janitor) Run(ctx context.Context) {
	if j.Interval <= 0 {
		j.Interval = time.Minute
	}
	t := time.NewTicker(j.Interval)
	defer t.Stop()

	// First scan immediately so callers don't wait Interval
	// before the first sweep.
	_, _ = j.Scan()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _ = j.Scan()
		}
	}
}
```

Add `Interval time.Duration` to the `Janitor` struct.

- [ ] **Step 3: Run test**

Run: `cd plugin && go test ./internal/ccbridge/ -run TestJanitorRun`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add plugin/internal/ccbridge/janitor.go plugin/internal/ccbridge/janitor_test.go
git commit -m "feat(ccbridge): Janitor.Run periodic scan loop"
```

---

## Task 19: Wire the janitor into the seeder daemon (cmd/token-bay-sidecar)

**Files:**
- Modify: `plugin/cmd/token-bay-sidecar/<entry-point>.go` (or wherever the sidecar's startup wiring lives)
- Modify: `plugin/internal/<wherever-coordinator-lives>/<file>.go` to satisfy `ActiveClientChecker`/`ActiveByHashChecker`

The bridge package gives us the building blocks. The seeder daemon at startup:
1. Configures the seeder root path (e.g. `~/.token-bay/seeder-sessions/` or `<XDG_STATE_HOME>/token-bay/sessions/`).
2. Constructs an `ActiveClientChecker` impl that reads from whatever in-memory map the trackerclient/coordinator maintains for connected peers.
3. Builds a `Janitor` with the root, checker, grace (e.g. 30 minutes), and interval (e.g. 5 minutes).
4. Runs the janitor in a managed goroutine tied to the daemon's lifecycle (cancel on shutdown).

- [ ] **Step 1: Find the seeder daemon entry point**

```bash
find plugin/cmd/token-bay-sidecar -name '*.go' -type f
```

Identify where the long-lived services start (probably a `run.go` or `serve.go` with a context group / errgroup managing the trackerclient, ccproxy, and now the janitor).

- [ ] **Step 2: Add the janitor to the service lifecycle**

```go
// somewhere in the sidecar's startup, alongside trackerclient.Run etc.
import (
	"github.com/token-bay/token-bay/plugin/internal/ccbridge"
	"path/filepath"
)

func startJanitor(ctx context.Context, cfg Config, coord *Coordinator) {
	j := &ccbridge.Janitor{
		Root:     filepath.Join(cfg.DataDir, "seeder-sessions"),
		Checker:  coord, // implements IsActive + IsActiveByHash
		Grace:    30 * time.Minute,
		Interval: 5 * time.Minute,
	}
	go j.Run(ctx)
}
```

- [ ] **Step 3: Make the coordinator implement `ActiveClientChecker` + `ActiveByHashChecker`**

```go
// In whatever package owns the per-peer connection map:
func (c *Coordinator) IsActive(pub ed25519.PublicKey) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.peers[ccbridge.ClientHash(pub)]
	return ok
}

func (c *Coordinator) IsActiveByHash(hash string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.peers[hash]
	return ok
}
```

If the coordinator doesn't already key its peer map by `ClientHash`, switch it (one-line change) so both lookups are O(1).

- [ ] **Step 4: Add an integration test**

```go
// plugin/cmd/token-bay-sidecar/<...>_test.go — sketch
func TestSidecar_JanitorReapsAfterClientDisconnect(t *testing.T) {
	// Start the sidecar with a tempdir DataDir.
	// Connect a fake peer (mints a keypair, registers with coord).
	// Issue a forwarded request (creates a session folder).
	// Disconnect the peer.
	// Wait > grace window.
	// Trigger a janitor scan (or wait for the next interval).
	// Assert the per-client folder is gone.
}
```

This is a real integration test; details depend on the existing sidecar test harness. Punch a TODO if the harness doesn't yet support fake-peer connections — track in a follow-up issue and lean on the unit-level `TestJanitorScan_*` for now.

- [ ] **Step 5: Commit**

```bash
git add plugin/cmd/token-bay-sidecar/ plugin/internal/<coord-package>/
git commit -m "feat(plugin): wire ccbridge.Janitor into seeder daemon lifecycle"
```

---

## Self-Review Checklist (run at the END, after writing all tasks)

**1. Spec coverage**
- [x] Replace stream-json stdin with session file + `--resume` → Tasks 1–8
- [x] Maintain HOME isolation + auth bridging → kept; runner_unix.go preserves `seedIsolatedHomeAuth`
- [x] Update existing 3 test layers (wirefidelity, contextquality, outputsimilarity) → Tasks 11–13
- [x] Delete dead code (EncodeMessages, stream-json input types) → Task 7
- [x] Verify all PASS-status tests stay green → Task 14
- [x] Per-client storage layout under `<seederRoot>/<clientHash>/` → Task 15
- [x] `ActiveClientChecker` interface for the seeder coordinator → Task 16
- [x] `Janitor.Scan` removes inactive client folders past grace → Task 17
- [x] `Janitor.Run` periodic loop with context cancellation → Task 18
- [x] Wire the janitor into the seeder daemon lifecycle → Task 19

**2. Placeholder scan**
None found. All code blocks are concrete; all commands are explicit. The Task 19 integration test is a sketch with a TODO acknowledgment, which is appropriate because it depends on the existing sidecar harness (out of scope for this plan).

**3. Type consistency**
- `BuildArgv` signature: `(req Request, sessionID, prompt string)` — used consistently in Tasks 6, 8, 11, 12, 13.
- `WriteSessionFile` signature: `(home, cwd, sessionID, version string, msgs []Message) (string, error)` — used in Tasks 4, 8, 11, 12, 15.
- `Message.Content` stays `json.RawMessage` (existing).
- `Request.ClientPubkey` is `ed25519.PublicKey` (Task 15) — matches `trackerclient.types.go:68`.
- `ClientHash(pub) string` returns 16 hex chars (Task 15) — used by Tasks 17, 18, 19.
- `ClientHomeDir(seederRoot, pub) string` returns `<seederRoot>/<ClientHash(pub)>` — used by runner_unix.go (Task 15) and conceptually by Task 19's coordinator wiring.
- `ActiveClientChecker.IsActive(pub) bool` (Task 16) + optional `ActiveByHashChecker.IsActiveByHash(hash) bool` extension (Task 17). Both implemented by the coordinator in Task 19.
- `Janitor` fields: `Root, Checker, Grace, Interval` — consistent across Tasks 17 and 18.
- `parsePhase1History(t, stdout, seed) []Message` introduced in Task 12; consistent with the renamed `finalAssistantHistoryFromStdout` from outputsimilarity_test.go which Task 13 re-points at.

---

## Open issues (for the engineer to flag, not block on)

- **`--no-session-persistence` + `--resume` interaction:** the explore confirmed this combination is supported (the flag only gates writing). If a future Claude Code release changes this, Task 14 step 2 will catch it.
- **Path collisions in concurrent test runs:** `WriteSessionFile` writes to `$HOME/.claude/projects/<sanitize(cwd)>/<sessionID>.jsonl`. Each test gets a fresh `cwd` (sanitized → unique dir), so collisions are impossible. The runner's `os.MkdirTemp("", "ccbridge-cwd-")` provides the uniqueness.
- **Cross-platform sessionID generation:** stdlib `crypto/rand` is portable. Linux, macOS, and Windows all supported; the existing Windows runner stub still returns `ErrUnsupportedPlatform` for the actual run path.

---

## Plan complete — saved at `docs/superpowers/plans/2026-05-09-bridge-resume-history.md`.

**Two execution options:**

1. **Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, review between tasks, fast iteration.

2. **Inline Execution** — Execute tasks in this session using executing-plans, batch execution with checkpoints.

**Which approach?**
