# Plugin ssetranslate Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `plugin/internal/ssetranslate` — a pure adapter that consumes Claude Code's `--output-format stream-json` byte stream and emits an Anthropic-compatible Server-Sent Events (SSE) byte stream. The package has no I/O of its own, no goroutines, no third-party deps; the future seeder accept-loop coordinator wraps `tunnel.Tunnel.ResponseWriter()` with this adapter and hands the result to `bridge.Serve` as the sink.

**Architecture:** A single `Writer` value with an `io.WriteCloser` surface. Internally a buffered line scanner consumes stream-json lines, decodes each into a typed envelope, and drives a small state machine (IDLE → IN_MESSAGE → CLOSING → DONE). The state machine emits SSE events onto an inner writer:

- One `message_start` per logical Anthropic turn (multi-cycle assistant events collapse into the same envelope).
- `content_block_start`/`content_block_delta`/`content_block_stop` for each block; `tool_use` blocks ship at-once in `content_block_start` without `input_json_delta` reconstruction.
- `message_delta` (with `result.usage` folded in) and `message_stop` emitted only after the terminal `result` event arrives. `Close` synthesizes both if the stream ends mid-message.
- `system/init` and `user/tool_result` events are dropped (claude-internal); malformed JSON lines are dropped.

**Tech Stack:** Go 1.25 stdlib (`bufio`, `bytes`, `encoding/json`, `errors`, `fmt`, `io`, `strings`); `github.com/stretchr/testify` (tests). No third-party runtime deps.

**Spec:** [`docs/superpowers/specs/plugin/2026-05-10-ssetranslate-design.md`](../specs/plugin/2026-05-10-ssetranslate-design.md). Parent: `docs/superpowers/specs/plugin/2026-04-22-plugin-design.md` §6.2/§7.2; `docs/superpowers/specs/2026-04-22-token-bay-architecture-design.md` §10.3.

---

## 1. File map

Created in this plan:

| Path | Purpose |
|---|---|
| `plugin/internal/ssetranslate/doc.go` | Package documentation comment with mapping table |
| `plugin/internal/ssetranslate/translate.go` | `Writer` + state machine + line scanner |
| `plugin/internal/ssetranslate/translate_test.go` | Golden-fixture tests + property tests |
| `plugin/internal/ssetranslate/testdata/sj/text_only_single_cycle.jsonl` | Happy-path stream-json |
| `plugin/internal/ssetranslate/testdata/sse/text_only_single_cycle.sse` | Expected SSE bytes |
| `plugin/internal/ssetranslate/testdata/sj/multi_block_text_text.jsonl` | Two text blocks |
| `plugin/internal/ssetranslate/testdata/sse/multi_block_text_text.sse` | Expected SSE for two-block |
| `plugin/internal/ssetranslate/testdata/sj/tool_use_then_text.jsonl` | tool_use + text |
| `plugin/internal/ssetranslate/testdata/sse/tool_use_then_text.sse` | Expected SSE for tool_use case |
| `plugin/internal/ssetranslate/testdata/sj/multi_cycle_collapsed.jsonl` | Two assistant cycles before result |
| `plugin/internal/ssetranslate/testdata/sse/multi_cycle_collapsed.sse` | Expected SSE collapsed |
| `plugin/internal/ssetranslate/testdata/sj/malformed_mid_stream.jsonl` | Garbage line in the middle |
| `plugin/internal/ssetranslate/testdata/sse/malformed_mid_stream.sse` | Expected SSE (malformed line absent) |
| `plugin/internal/ssetranslate/testdata/sj/no_result_event.jsonl` | Stream ends without result |
| `plugin/internal/ssetranslate/testdata/sse/no_result_event.sse` | Expected SSE with synthetic close |

No external files modified.

---

## 2. Conventions used in this plan

- Worktree root: `/Users/dor.amid/.superset/worktrees/token-bay/plugin-sse`. The Go module lives at `plugin/`. All `go test`/`go build` commands run from `/Users/dor.amid/.superset/worktrees/token-bay/plugin-sse/plugin`.
- `PATH="$HOME/.local/share/mise/shims:$PATH"` is required when Go is managed by mise. All shell commands include it.
- One commit per task. Conventional-commit prefixes: `feat(plugin/ssetranslate):`, `test(plugin/ssetranslate):`. Co-Authored-By footer on every commit.
- Module path: `github.com/token-bay/token-bay/plugin`.
- Test fixtures use raw bytes; do not edit them with editors that insert trailing newlines or BOMs. Use the heredoc snippets in this plan verbatim.
- The package imports only Go stdlib + `testify`. No `shared/`, no `internal/tunnel`, no `internal/ccbridge` — pure format adapter.
- TDD discipline: failing test → minimal impl → passing test → commit. Each task is scoped to one observable behavior plus its fixture(s).

---

## Task 1: Package skeleton — doc.go

**Files:**
- Create: `plugin/internal/ssetranslate/doc.go`

- [ ] **Step 1: Confirm directory does not yet exist**

Run: `ls /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse/plugin/internal/ssetranslate 2>&1 || echo "absent"`
Expected: `No such file or directory` or `absent`.

- [ ] **Step 2: Create `plugin/internal/ssetranslate/doc.go`**

```go
// Package ssetranslate is a pure adapter from Claude Code's
// `--output-format stream-json` byte stream to an Anthropic-compatible
// Server-Sent Events (SSE) byte stream.
//
// # Spec
//
// Subsystem design `docs/superpowers/specs/plugin/2026-05-10-ssetranslate-design.md`.
// Plugin design §6.2 / §7.2; architecture spec §10.3.
//
// # Position
//
// The seeder coordinator (separate, future plan) wraps a Tunnel's
// ResponseWriter() with this adapter and hands the result to
// ccbridge.Bridge.Serve as its sink. Bytes leaving the inner writer
// are valid Anthropic SSE; the consumer side io.Copy's them straight
// into Claude Code's open HTTP response.
//
// # Mapping
//
// Each line of stream-json input maps to zero or more SSE events:
//
//	{"type":"system","subtype":"init",...}            → drop
//	{"type":"assistant","message":{id:M,content:[B]}} → first time we
//	    see id M, emit message_start; for each new content block index
//	    in B, emit content_block_start; for text deltas, emit
//	    content_block_delta with type=text_delta; tool_use blocks ship
//	    at-once in content_block_start with full input populated.
//	{"type":"user","message":{tool_result...}}        → drop
//	{"type":"result","usage":{...}}                   → emit
//	    message_delta with stop_reason+usage, then message_stop.
//	(malformed JSON line)                              → drop, continue.
//
// The translator collapses multi-cycle assistant events (multiple
// internal claude-side tool cycles before result) into one logical
// message envelope. Anthropic's SSE for one /v1/messages call exposes
// exactly one message envelope; preserving cycles would leak seeder
// internals. Sequential block indexes carry across cycle boundaries.
//
// # Locked decisions
//
//  1. Translator is a separate package — ccbridge stays "stream-json
//     bytes verbatim into a sink"; tunnel stays format-agnostic.
//  2. tool_use ships at-once in content_block_start; no
//     input_json_delta reconstruction.
//  3. Multi-cycle assistant events collapse to one envelope.
//  4. message_delta is deferred until the terminal result event so
//     usage matches Anthropic's wire-canonical placement.
//
// # Out of scope
//
// This package does not read from a subprocess, does not write to a
// tunnel, does not interpret request bodies, does not validate
// Anthropic responses, does not extract Usage for the tracker
// (ccbridge.ParseStreamJSON does that separately on the same byte
// stream consumed before this adapter — they do not coordinate).
package ssetranslate
```

- [ ] **Step 3: Verify `go vet` passes on the new file**

Run: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" go vet ./internal/ssetranslate/...`
Expected: no output, exit 0.

- [ ] **Step 4: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse
git add plugin/internal/ssetranslate/doc.go
git commit -m "$(cat <<'EOF'
feat(plugin/ssetranslate): package skeleton + doc.go mapping table

Pure adapter from Claude Code stream-json to Anthropic-compatible SSE.
Documents the per-line mapping, four locked decisions (separate
package, tool_use at-once, multi-cycle collapse, deferred
message_delta), and the out-of-scope boundary against ccbridge and
tunnel.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: Fixture loader + first golden case + minimal Writer (combined)

> **NOTE — combined task.** The original plan split this into a TDD red commit (fixtures+test) followed by a green commit (Writer impl). The repo's lefthook pre-commit hook runs `golangci-lint` + `go vet` on staged Go files, which rejects a typecheck-failing test (undefined `NewWriter`). Rather than bypass hooks (`--no-verify` is forbidden by CLAUDE.md), this task lands fixtures + test + minimal Writer in **one commit** that compiles and passes. TDD discipline is preserved — the test is still authored before the implementation, just within the same commit.

This task introduces:
- The first stream-json input fixture and its expected SSE.
- A `loadFixture` helper.
- A test for the text-only single-cycle case.
- A minimal `Writer` + state machine sufficient to make the test pass (the rest of the state-machine semantics — multi-block, multi-cycle, malformed-line tolerance, synthetic close — get added incrementally in Tasks 4–8 as additional fixtures arrive; the Task 3 listing below contains the **complete** implementation, so following Task 3 verbatim already covers those later cases).

**Files:**
- Create: `plugin/internal/ssetranslate/testdata/sj/text_only_single_cycle.jsonl`
- Create: `plugin/internal/ssetranslate/testdata/sse/text_only_single_cycle.sse`
- Create: `plugin/internal/ssetranslate/translate_test.go`

- [ ] **Step 1: Create input fixture `text_only_single_cycle.jsonl`**

Write to `plugin/internal/ssetranslate/testdata/sj/text_only_single_cycle.jsonl`:

```
{"type":"system","subtype":"init","session_id":"s1","model":"claude-sonnet-4-6","tools":[],"mcp_servers":[]}
{"type":"assistant","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"Hello"}],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":12,"output_tokens":1}}}
{"type":"assistant","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":" world"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":12,"output_tokens":2}}}
{"type":"result","subtype":"success","is_error":false,"duration_ms":4200,"result":"Hello world","session_id":"s1","total_cost_usd":0.001,"usage":{"input_tokens":12,"output_tokens":2}}
```

(File ends with one trailing `\n` after the last line.)

- [ ] **Step 2: Create expected SSE fixture `text_only_single_cycle.sse`**

Write to `plugin/internal/ssetranslate/testdata/sse/text_only_single_cycle.sse`. The file must contain exactly these bytes (each event ends with `\n\n`):

```
event: message_start
data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":12,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":12,"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

```

(Note: file ends with `\n\n` after `message_stop` — the SSE event terminator.)

- [ ] **Step 3: Create the test file with the golden-fixture test + helper**

Write to `plugin/internal/ssetranslate/translate_test.go`:

```go
package ssetranslate

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loadPair returns (stream-json input, expected SSE output) for the
// fixture pair under testdata/sj/<name>.jsonl + testdata/sse/<name>.sse.
func loadPair(t *testing.T, name string) ([]byte, []byte) {
	t.Helper()
	in, err := os.ReadFile(filepath.Join("testdata", "sj", name+".jsonl"))
	require.NoError(t, err, "load input %q", name)
	out, err := os.ReadFile(filepath.Join("testdata", "sse", name+".sse"))
	require.NoError(t, err, "load expected %q", name)
	return in, out
}

// runFixture pumps the stream-json through a Writer one chunk at a time
// (whole file as one Write here; per-line splitting is exercised in a
// dedicated property test) and returns the captured SSE bytes.
func runFixture(t *testing.T, in []byte) []byte {
	t.Helper()
	var sink bytes.Buffer
	w := NewWriter(&sink)
	n, err := w.Write(in)
	require.NoError(t, err)
	assert.Equal(t, len(in), n)
	require.NoError(t, w.Close())
	return sink.Bytes()
}

func TestTranslate_TextOnlySingleCycle(t *testing.T) {
	in, want := loadPair(t, "text_only_single_cycle")
	got := runFixture(t, in)
	assert.Equal(t, string(want), string(got))
}
```

- [ ] **Step 4: Add the full `Writer` implementation in `plugin/internal/ssetranslate/translate.go` (Task 3 content, applied in this same commit)**

See Task 3 below for the verbatim listing — copy that file into place as the next step.

- [ ] **Step 5: Run the test, confirm PASS**

Run: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" go test -race ./internal/ssetranslate/...`
Expected: `ok`.

- [ ] **Step 6: Commit fixtures + test + impl together**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse
git add plugin/internal/ssetranslate/testdata/sj/text_only_single_cycle.jsonl \
        plugin/internal/ssetranslate/testdata/sse/text_only_single_cycle.sse \
        plugin/internal/ssetranslate/translate_test.go \
        plugin/internal/ssetranslate/translate.go
git commit -m "$(cat <<'EOF'
feat(plugin/ssetranslate): Writer + state machine for text-only path

First golden-fixture pair (text-only single cycle), the loadPair test
helper, and the minimal io.WriteCloser implementation that drives the
3-state machine (IDLE → IN_MESSAGE → DONE) over line-delimited
stream-json input. Emits message_start / content_block_start /
content_block_delta / content_block_stop / message_delta / message_stop
in Anthropic SSE shape. result.usage is folded into message_delta.

Multi-block, tool_use, multi-cycle, and tolerance fixtures land in
subsequent commits; the implementation already supports them.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: `Writer` implementation (file content for Task 2's commit)

> **NOTE — folded into Task 2.** The Writer implementation here is committed as part of Task 2's combined commit (see Task 2 note above). This section keeps the verbatim file listing for reference; do **not** create a separate commit for it.

**Files:**
- Create: `plugin/internal/ssetranslate/translate.go`

- [ ] **Step 1: Create `plugin/internal/ssetranslate/translate.go`**

```go
package ssetranslate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Writer ingests stream-json bytes and emits SSE bytes on the inner
// writer. Single-flight: one goroutine only.
type Writer struct {
	inner io.Writer

	// Buffered partial line — Write may receive arbitrary chunk
	// boundaries, so we accumulate until we see '\n'.
	buf bytes.Buffer

	state state
	envel envelope // captured between message_start and message_stop

	// firstWriteErr latches the first inner.Write error; subsequent
	// Writes return it without further work.
	firstWriteErr error
	closed        bool
}

type state int

const (
	stateIdle state = iota
	stateInMessage
	stateDone
)

// envelope is the per-message state we accumulate between message_start
// and message_stop. content blocks open as they appear; when the next
// block index appears (or message ends), the previous one closes.
type envelope struct {
	id            string
	openBlockIdx  int  // index of currently-open content block
	hasOpenBlock  bool // is openBlockIdx valid?
	nextBlockIdx  int  // next index to assign for new blocks
	stopReason    *string
	stopSequence  *string
	finalUsage    *usage
}

// NewWriter returns a Writer wrapping inner. Callers should wrap once
// per logical /v1/messages turn.
func NewWriter(inner io.Writer) *Writer {
	return &Writer{inner: inner, state: stateIdle}
}

// Write ingests stream-json bytes. Returns len(p), nil on success even
// when zero SSE events are emitted. A first inner.Write error latches;
// subsequent Writes return that error.
func (w *Writer) Write(p []byte) (int, error) {
	if w.firstWriteErr != nil {
		return 0, w.firstWriteErr
	}
	if w.closed {
		return 0, errors.New("ssetranslate: write after close")
	}

	w.buf.Write(p)
	for {
		line, err := w.buf.ReadBytes('\n')
		if err != nil {
			// No newline yet — put the partial back.
			if len(line) > 0 {
				w.buf.Write(line)
			}
			break
		}
		if errInner := w.handleLine(line); errInner != nil {
			w.firstWriteErr = errInner
			return len(p), errInner
		}
	}
	return len(p), nil
}

// Close finalizes any open envelope. Idempotent.
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if w.firstWriteErr != nil {
		return nil
	}
	if w.state == stateInMessage {
		if err := w.finishMessage(synthStopReason("end_turn"), nil, w.envel.finalUsage); err != nil {
			w.firstWriteErr = err
			return err
		}
	}
	return nil
}

func synthStopReason(s string) *string { return &s }

// handleLine processes one newline-terminated stream-json line.
func (w *Writer) handleLine(line []byte) error {
	body := bytes.TrimRight(line, "\n")
	if len(body) == 0 || body[0] != '{' {
		return nil // tolerate malformed/empty lines
	}
	var head struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return nil
	}
	switch head.Type {
	case "assistant":
		return w.handleAssistant(body)
	case "result":
		return w.handleResult(body)
	default:
		// system, user, tool_result, anything else: drop.
		return nil
	}
}

// --- assistant event handling ---

type assistantEnvelope struct {
	Type    string `json:"type"`
	Message struct {
		ID           string          `json:"id"`
		Type         string          `json:"type"`
		Role         string          `json:"role"`
		Model        string          `json:"model"`
		Content      []json.RawMessage `json:"content"`
		StopReason   *string         `json:"stop_reason"`
		StopSequence *string         `json:"stop_sequence"`
		Usage        *usage          `json:"usage"`
	} `json:"message"`
}

type usage struct {
	InputTokens  uint64 `json:"input_tokens"`
	OutputTokens uint64 `json:"output_tokens"`
}

func (w *Writer) handleAssistant(body []byte) error {
	var ev assistantEnvelope
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil
	}
	if w.state == stateDone {
		// New assistant after we've already closed — defensive ignore.
		return nil
	}
	if w.state == stateIdle {
		if err := w.emitMessageStart(ev); err != nil {
			return err
		}
		w.state = stateInMessage
		w.envel.id = ev.Message.ID
	}
	// Cache rolling stop_reason/stop_sequence/usage from the latest cycle.
	w.envel.stopReason = ev.Message.StopReason
	w.envel.stopSequence = ev.Message.StopSequence
	if ev.Message.Usage != nil {
		w.envel.finalUsage = ev.Message.Usage
	}
	for _, blk := range ev.Message.Content {
		if err := w.handleContentBlock(blk); err != nil {
			return err
		}
	}
	return nil
}

// handleContentBlock handles one block from an assistant event. Each
// block from stream-json is fully formed; we either open+populate+close
// it or extend an already-open block of the same type/index.
func (w *Writer) handleContentBlock(blk json.RawMessage) error {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(blk, &head); err != nil {
		return nil
	}
	switch head.Type {
	case "text":
		return w.handleTextBlock(blk)
	case "tool_use":
		return w.handleToolUseBlock(blk)
	default:
		// Unknown block types (thinking, image, etc.) — ship as
		// content_block_start with the raw block, then immediately
		// content_block_stop, no delta. Future work may expand this.
		return w.shipOpaqueBlock(blk)
	}
}

func (w *Writer) handleTextBlock(blk json.RawMessage) error {
	var b struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(blk, &b); err != nil {
		return nil
	}
	if !w.envel.hasOpenBlock {
		idx := w.envel.nextBlockIdx
		if err := w.emitContentBlockStart(idx, json.RawMessage(`{"type":"text","text":""}`)); err != nil {
			return err
		}
		w.envel.openBlockIdx = idx
		w.envel.hasOpenBlock = true
		w.envel.nextBlockIdx++
	}
	if b.Text != "" {
		if err := w.emitTextDelta(w.envel.openBlockIdx, b.Text); err != nil {
			return err
		}
	}
	return nil
}

func (w *Writer) handleToolUseBlock(blk json.RawMessage) error {
	if w.envel.hasOpenBlock {
		if err := w.emitContentBlockStop(w.envel.openBlockIdx); err != nil {
			return err
		}
		w.envel.hasOpenBlock = false
	}
	idx := w.envel.nextBlockIdx
	if err := w.emitContentBlockStart(idx, blk); err != nil {
		return err
	}
	if err := w.emitContentBlockStop(idx); err != nil {
		return err
	}
	w.envel.nextBlockIdx++
	return nil
}

func (w *Writer) shipOpaqueBlock(blk json.RawMessage) error {
	if w.envel.hasOpenBlock {
		if err := w.emitContentBlockStop(w.envel.openBlockIdx); err != nil {
			return err
		}
		w.envel.hasOpenBlock = false
	}
	idx := w.envel.nextBlockIdx
	if err := w.emitContentBlockStart(idx, blk); err != nil {
		return err
	}
	if err := w.emitContentBlockStop(idx); err != nil {
		return err
	}
	w.envel.nextBlockIdx++
	return nil
}

// --- result event handling ---

type resultEnvelope struct {
	Type    string `json:"type"`
	IsError bool   `json:"is_error"`
	Usage   *usage `json:"usage"`
}

func (w *Writer) handleResult(body []byte) error {
	var ev resultEnvelope
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil
	}
	if w.state != stateInMessage {
		return nil // result without prior assistant — drop
	}
	stopReason := w.envel.stopReason
	if stopReason == nil {
		var def string
		if ev.IsError {
			def = "error"
		} else {
			def = "end_turn"
		}
		stopReason = &def
	}
	useUsage := ev.Usage
	if useUsage == nil {
		useUsage = w.envel.finalUsage
	}
	return w.finishMessage(stopReason, w.envel.stopSequence, useUsage)
}

// finishMessage closes any open block and emits message_delta + message_stop.
func (w *Writer) finishMessage(stopReason, stopSequence *string, u *usage) error {
	if w.envel.hasOpenBlock {
		if err := w.emitContentBlockStop(w.envel.openBlockIdx); err != nil {
			return err
		}
		w.envel.hasOpenBlock = false
	}
	if err := w.emitMessageDelta(stopReason, stopSequence, u); err != nil {
		return err
	}
	if err := w.emitMessageStop(); err != nil {
		return err
	}
	w.state = stateDone
	return nil
}

// --- emit helpers ---

func (w *Writer) emit(event string, data any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		// Marshalling our own types should never fail; if it does, surface.
		return fmt.Errorf("ssetranslate: marshal %s: %w", event, err)
	}
	if _, err := fmt.Fprintf(w.inner, "event: %s\ndata: %s\n\n", event, payload); err != nil {
		return err
	}
	return nil
}

func (w *Writer) emitMessageStart(ev assistantEnvelope) error {
	// message_start carries the message envelope with empty content;
	// per Anthropic SSE, content blocks stream after.
	type startMsg struct {
		ID           string  `json:"id"`
		Type         string  `json:"type"`
		Role         string  `json:"role"`
		Model        string  `json:"model"`
		Content      []any   `json:"content"`
		StopReason   *string `json:"stop_reason"`
		StopSequence *string `json:"stop_sequence"`
		Usage        *usage  `json:"usage"`
	}
	msg := startMsg{
		ID:           ev.Message.ID,
		Type:         ev.Message.Type,
		Role:         ev.Message.Role,
		Model:        ev.Message.Model,
		Content:      []any{},
		StopReason:   ev.Message.StopReason,
		StopSequence: ev.Message.StopSequence,
		Usage:        ev.Message.Usage,
	}
	return w.emit("message_start", map[string]any{
		"type":    "message_start",
		"message": msg,
	})
}

func (w *Writer) emitContentBlockStart(idx int, block json.RawMessage) error {
	return w.emit("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         idx,
		"content_block": json.RawMessage(block),
	})
}

func (w *Writer) emitTextDelta(idx int, text string) error {
	return w.emit("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": idx,
		"delta": map[string]any{
			"type": "text_delta",
			"text": text,
		},
	})
}

func (w *Writer) emitContentBlockStop(idx int) error {
	return w.emit("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": idx,
	})
}

func (w *Writer) emitMessageDelta(stopReason, stopSequence *string, u *usage) error {
	delta := map[string]any{
		"stop_reason":   stopReason,
		"stop_sequence": stopSequence,
	}
	payload := map[string]any{
		"type":  "message_delta",
		"delta": delta,
	}
	if u != nil {
		payload["usage"] = u
	}
	return w.emit("message_delta", payload)
}

func (w *Writer) emitMessageStop() error {
	return w.emit("message_stop", map[string]any{
		"type": "message_stop",
	})
}

// Compile-time interface check.
var _ io.WriteCloser = (*Writer)(nil)
```

- [ ] **Step 2: Run test, confirm PASS for the single-cycle case**

Run: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" go test -race ./internal/ssetranslate/...`
Expected: `ok`. If a byte-mismatch error appears, the most likely cause is JSON key ordering — Go's `json.Marshal` sorts keys alphabetically for `map[string]any`, so the fixture file's JSON must do the same. Verify the fixture and adjust if needed.

- [ ] **Step 3: Lint check**

Run: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" go vet ./internal/ssetranslate/...`
Expected: no output, exit 0.

- [ ] **Step 4: (no separate commit — bundled into Task 2's commit)**

The Writer implementation lands in the same commit as Task 2's fixtures + test. See Task 2 Step 6.

---

## Task 4: Multi-block (text + text) golden case

**Files:**
- Create: `plugin/internal/ssetranslate/testdata/sj/multi_block_text_text.jsonl`
- Create: `plugin/internal/ssetranslate/testdata/sse/multi_block_text_text.sse`
- Modify: `plugin/internal/ssetranslate/translate_test.go`

- [ ] **Step 1: Create input fixture `multi_block_text_text.jsonl`**

This fixture exercises the index-advancement boundary: a text block continues across two assistant events, then a tool_use block opens at index 1, forcing the prior block to close.

Write to `plugin/internal/ssetranslate/testdata/sj/multi_block_text_text.jsonl`:

```
{"type":"system","subtype":"init","session_id":"s2","model":"claude-sonnet-4-6","tools":[],"mcp_servers":[]}
{"type":"assistant","message":{"id":"msg_02","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"first"}],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":1}}}
{"type":"assistant","message":{"id":"msg_02","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":" chunk"}],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":2}}}
{"type":"assistant","message":{"id":"msg_02","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"tool_use","id":"toolu_1","name":"Monitor","input":{"action":"ping"}}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":4}}}
{"type":"result","subtype":"success","is_error":false,"duration_ms":1000,"result":"first chunk","session_id":"s2","total_cost_usd":0.001,"usage":{"input_tokens":5,"output_tokens":4}}
```

- [ ] **Step 2: Create expected SSE fixture `multi_block_text_text.sse`**

Write to `plugin/internal/ssetranslate/testdata/sse/multi_block_text_text.sse`:

```
event: message_start
data: {"type":"message_start","message":{"id":"msg_02","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"first"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" chunk"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"Monitor","input":{"action":"ping"}}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":5,"output_tokens":4}}

event: message_stop
data: {"type":"message_stop"}

```

- [ ] **Step 3: Add a test case for the new fixture**

Append to `plugin/internal/ssetranslate/translate_test.go`:

```go
func TestTranslate_MultiBlock_TextThenToolUse(t *testing.T) {
	in, want := loadPair(t, "multi_block_text_text")
	got := runFixture(t, in)
	assert.Equal(t, string(want), string(got))
}
```

- [ ] **Step 4: Run tests, confirm PASS**

The Task 3 implementation already supports this case (text block continues through assistant events; tool_use block closes the open text block and ships at-once). If a mismatch occurs, inspect the `data:` lines at the boundary — the translator must emit `content_block_stop{index:0}` BEFORE `content_block_start{index:1, content_block:tool_use}`.

Run: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" go test -race ./internal/ssetranslate/...`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse
git add plugin/internal/ssetranslate/testdata/sj/multi_block_text_text.jsonl \
        plugin/internal/ssetranslate/testdata/sse/multi_block_text_text.sse \
        plugin/internal/ssetranslate/translate_test.go
git commit -m "$(cat <<'EOF'
test(plugin/ssetranslate): multi-block text-then-tool_use fixture

Asserts the index-advancement boundary: an open text block closes
before a new tool_use block opens at the next index. tool_use ships
at-once with full input populated in content_block_start, no
input_json_delta reconstruction.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: Pure tool_use + text golden case

A more pointed tool_use coverage: tool_use first, then text — verifies the open/close ordering when tool_use OPENS the envelope.

**Files:**
- Create: `plugin/internal/ssetranslate/testdata/sj/tool_use_then_text.jsonl`
- Create: `plugin/internal/ssetranslate/testdata/sse/tool_use_then_text.sse`
- Modify: `plugin/internal/ssetranslate/translate_test.go`

- [ ] **Step 1: Create input fixture**

Write to `plugin/internal/ssetranslate/testdata/sj/tool_use_then_text.jsonl`:

```
{"type":"system","subtype":"init","session_id":"s3","model":"claude-sonnet-4-6","tools":[],"mcp_servers":[]}
{"type":"assistant","message":{"id":"msg_03","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"tool_use","id":"toolu_2","name":"Monitor","input":{"q":1}}],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":7,"output_tokens":1}}}
{"type":"assistant","message":{"id":"msg_03","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"after"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":7,"output_tokens":3}}}
{"type":"result","subtype":"success","is_error":false,"duration_ms":900,"result":"after","session_id":"s3","total_cost_usd":0.0005,"usage":{"input_tokens":7,"output_tokens":3}}
```

- [ ] **Step 2: Create expected SSE fixture**

Write to `plugin/internal/ssetranslate/testdata/sse/tool_use_then_text.sse`:

```
event: message_start
data: {"type":"message_start","message":{"id":"msg_03","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":7,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_2","name":"Monitor","input":{"q":1}}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"after"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":7,"output_tokens":3}}

event: message_stop
data: {"type":"message_stop"}

```

- [ ] **Step 3: Add test case**

Append to `plugin/internal/ssetranslate/translate_test.go`:

```go
func TestTranslate_ToolUseThenText(t *testing.T) {
	in, want := loadPair(t, "tool_use_then_text")
	got := runFixture(t, in)
	assert.Equal(t, string(want), string(got))
}
```

- [ ] **Step 4: Run, confirm PASS**

Run: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" go test -race ./internal/ssetranslate/...`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse
git add plugin/internal/ssetranslate/testdata/sj/tool_use_then_text.jsonl \
        plugin/internal/ssetranslate/testdata/sse/tool_use_then_text.sse \
        plugin/internal/ssetranslate/translate_test.go
git commit -m "$(cat <<'EOF'
test(plugin/ssetranslate): tool_use-then-text fixture

Verifies tool_use can open the envelope and a subsequent text block
opens at index 1 with content_block_start{text:""} + delta + stop.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: Multi-cycle collapse golden case

Two assistant cycles with the SAME message id occur naturally; the harder case is two assistant cycles with DIFFERENT message ids (a tool-cycle artifact). Both must collapse into one envelope.

**Files:**
- Create: `plugin/internal/ssetranslate/testdata/sj/multi_cycle_collapsed.jsonl`
- Create: `plugin/internal/ssetranslate/testdata/sse/multi_cycle_collapsed.sse`
- Modify: `plugin/internal/ssetranslate/translate_test.go`

- [ ] **Step 1: Create input fixture**

Write to `plugin/internal/ssetranslate/testdata/sj/multi_cycle_collapsed.jsonl`:

```
{"type":"system","subtype":"init","session_id":"s4","model":"claude-sonnet-4-6","tools":[],"mcp_servers":[]}
{"type":"assistant","message":{"id":"msg_04a","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"plan: "}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_3","content":"err","is_error":true}]}}
{"type":"assistant","message":{"id":"msg_04b","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":3}}}
{"type":"result","subtype":"success","is_error":false,"duration_ms":1500,"result":"plan: done","session_id":"s4","total_cost_usd":0.0008,"usage":{"input_tokens":10,"output_tokens":3}}
```

- [ ] **Step 2: Create expected SSE fixture**

Write to `plugin/internal/ssetranslate/testdata/sse/multi_cycle_collapsed.sse`:

```
event: message_start
data: {"type":"message_start","message":{"id":"msg_04a","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"plan: "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":10,"output_tokens":3}}

event: message_stop
data: {"type":"message_stop"}

```

- [ ] **Step 3: Add test case**

Append to `plugin/internal/ssetranslate/translate_test.go`:

```go
func TestTranslate_MultiCycle_Collapsed(t *testing.T) {
	in, want := loadPair(t, "multi_cycle_collapsed")
	got := runFixture(t, in)
	assert.Equal(t, string(want), string(got))
}
```

- [ ] **Step 4: Run, confirm PASS**

This case is already covered by Task 3's logic: `handleAssistant` only emits `message_start` when `state == stateIdle`; subsequent assistant events extend the existing envelope and merge text into the open block. The intervening `user/tool_result` event is dropped by `handleLine`'s default branch.

Run: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" go test -race ./internal/ssetranslate/...`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse
git add plugin/internal/ssetranslate/testdata/sj/multi_cycle_collapsed.jsonl \
        plugin/internal/ssetranslate/testdata/sse/multi_cycle_collapsed.sse \
        plugin/internal/ssetranslate/translate_test.go
git commit -m "$(cat <<'EOF'
test(plugin/ssetranslate): multi-cycle collapse fixture

Two assistant cycles with different message ids and a synthetic
tool_result in between collapse into one Anthropic envelope. The
consumer sees one message_start, one continuous text block, one
message_stop — matching what api.anthropic.com would have emitted
for a /v1/messages call.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 7: Malformed mid-stream line tolerance

**Files:**
- Create: `plugin/internal/ssetranslate/testdata/sj/malformed_mid_stream.jsonl`
- Create: `plugin/internal/ssetranslate/testdata/sse/malformed_mid_stream.sse`
- Modify: `plugin/internal/ssetranslate/translate_test.go`

- [ ] **Step 1: Create input fixture**

Write to `plugin/internal/ssetranslate/testdata/sj/malformed_mid_stream.jsonl`:

```
{"type":"system","subtype":"init","session_id":"s5","model":"claude-sonnet-4-6","tools":[],"mcp_servers":[]}
{"type":"assistant","message":{"id":"msg_05","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":1}}}
this line is not json at all
{"type":"result","subtype":"success","is_error":false,"duration_ms":100,"result":"ok","session_id":"s5","total_cost_usd":0.0001,"usage":{"input_tokens":3,"output_tokens":1}}
```

- [ ] **Step 2: Create expected SSE fixture**

Write to `plugin/internal/ssetranslate/testdata/sse/malformed_mid_stream.sse`:

```
event: message_start
data: {"type":"message_start","message":{"id":"msg_05","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":3,"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

```

- [ ] **Step 3: Add test case**

Append to `plugin/internal/ssetranslate/translate_test.go`:

```go
func TestTranslate_MalformedMidStream_Tolerated(t *testing.T) {
	in, want := loadPair(t, "malformed_mid_stream")
	got := runFixture(t, in)
	assert.Equal(t, string(want), string(got))
}
```

- [ ] **Step 4: Run, confirm PASS**

Already covered by `handleLine`'s `body[0] != '{'` check.

Run: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" go test -race ./internal/ssetranslate/...`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse
git add plugin/internal/ssetranslate/testdata/sj/malformed_mid_stream.jsonl \
        plugin/internal/ssetranslate/testdata/sse/malformed_mid_stream.sse \
        plugin/internal/ssetranslate/translate_test.go
git commit -m "$(cat <<'EOF'
test(plugin/ssetranslate): malformed mid-stream line tolerance

Non-JSON garbage line in the middle of a valid stream is dropped
silently. Mirrors ccbridge.ParseStreamJSON's tolerance — transient
corruption shouldn't abort an in-flight turn.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 8: No-result-event synthetic close

**Files:**
- Create: `plugin/internal/ssetranslate/testdata/sj/no_result_event.jsonl`
- Create: `plugin/internal/ssetranslate/testdata/sse/no_result_event.sse`
- Modify: `plugin/internal/ssetranslate/translate_test.go`

- [ ] **Step 1: Create input fixture**

Write to `plugin/internal/ssetranslate/testdata/sj/no_result_event.jsonl`:

```
{"type":"system","subtype":"init","session_id":"s6","model":"claude-sonnet-4-6","tools":[],"mcp_servers":[]}
{"type":"assistant","message":{"id":"msg_06","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"part"}],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":2,"output_tokens":1}}}
```

- [ ] **Step 2: Create expected SSE fixture**

Write to `plugin/internal/ssetranslate/testdata/sse/no_result_event.sse`:

```
event: message_start
data: {"type":"message_start","message":{"id":"msg_06","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":2,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"part"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":2,"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

```

- [ ] **Step 3: Add test case**

Append to `plugin/internal/ssetranslate/translate_test.go`:

```go
func TestTranslate_NoResultEvent_CloseSynthesizes(t *testing.T) {
	in, want := loadPair(t, "no_result_event")
	got := runFixture(t, in)
	assert.Equal(t, string(want), string(got))
}
```

- [ ] **Step 4: Run, confirm PASS**

Already covered by `Close`'s `state == stateInMessage` branch + `synthStopReason("end_turn")`.

Run: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" go test -race ./internal/ssetranslate/...`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse
git add plugin/internal/ssetranslate/testdata/sj/no_result_event.jsonl \
        plugin/internal/ssetranslate/testdata/sse/no_result_event.sse \
        plugin/internal/ssetranslate/translate_test.go
git commit -m "$(cat <<'EOF'
test(plugin/ssetranslate): no-result-event synthetic close

When the upstream stream ends without a terminal result event, Close
synthesizes message_delta + message_stop carrying stop_reason="end_turn"
and best-effort usage from the last assistant event. The consumer's
claude observes a clean turn boundary and surfaces the truncation as
a normal end-of-stream rather than a parse error.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 9: Property tests — chunk-boundary independence + error-writer latching

**Files:**
- Modify: `plugin/internal/ssetranslate/translate_test.go`

- [ ] **Step 1: Add `errors` to the test file's import block**

Edit `plugin/internal/ssetranslate/translate_test.go` and update the import block to add `errors`:

```go
import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)
```

- [ ] **Step 2: Append the new tests**

Append to `plugin/internal/ssetranslate/translate_test.go`:

```go
// errAt returns a writer that fails on the Nth Write call.
type errAt struct {
	wrap   *bytes.Buffer
	failOn int
	calls  int
	err    error
}

func (e *errAt) Write(p []byte) (int, error) {
	e.calls++
	if e.calls == e.failOn {
		return 0, e.err
	}
	return e.wrap.Write(p)
}

func TestTranslate_ChunkBoundaryIndependence(t *testing.T) {
	// Feed the same input one byte at a time; output must be identical
	// to a one-shot Write of the whole input.
	in, _ := loadPair(t, "text_only_single_cycle")

	var oneShot bytes.Buffer
	w1 := NewWriter(&oneShot)
	_, err := w1.Write(in)
	require.NoError(t, err)
	require.NoError(t, w1.Close())

	var oneByte bytes.Buffer
	w2 := NewWriter(&oneByte)
	for _, b := range in {
		n, err := w2.Write([]byte{b})
		require.NoError(t, err)
		require.Equal(t, 1, n)
	}
	require.NoError(t, w2.Close())

	assert.Equal(t, oneShot.String(), oneByte.String())
}

func TestTranslate_InnerWriteError_Latches(t *testing.T) {
	in, _ := loadPair(t, "text_only_single_cycle")
	wantErr := errors.New("boom")
	sink := &errAt{wrap: &bytes.Buffer{}, failOn: 2, err: wantErr}
	w := NewWriter(sink)

	// First Write triggers fail on the second emit (content_block_start
	// after message_start).
	_, err := w.Write(in)
	require.Error(t, err)
	assert.True(t, errors.Is(err, wantErr))

	// Subsequent Write returns the latched error and writes nothing.
	n, err := w.Write([]byte("x"))
	require.Error(t, err)
	assert.Equal(t, 0, n)
	assert.True(t, errors.Is(err, wantErr))

	// Close returns nil because the error is already latched.
	require.NoError(t, w.Close())
}

func TestTranslate_DoubleClose_Idempotent(t *testing.T) {
	in, _ := loadPair(t, "text_only_single_cycle")
	var sink bytes.Buffer
	w := NewWriter(&sink)
	_, err := w.Write(in)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	require.NoError(t, w.Close())
}

func TestTranslate_WriteAfterClose_Errors(t *testing.T) {
	var sink bytes.Buffer
	w := NewWriter(&sink)
	require.NoError(t, w.Close())
	_, err := w.Write([]byte(`{"type":"system"}` + "\n"))
	require.Error(t, err)
}
```

- [ ] **Step 3: Run, confirm PASS**

Run: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" go test -race ./internal/ssetranslate/...`
Expected: `ok`.

- [ ] **Step 4: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse
git add plugin/internal/ssetranslate/translate_test.go
git commit -m "$(cat <<'EOF'
test(plugin/ssetranslate): chunk-boundary + error-latch + close idempotency

Property tests: byte-by-byte writes produce identical SSE to a
single one-shot write; an inner.Write error latches and propagates;
double-Close is a no-op; Write after Close returns an error.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 10: Final verification — `make check`, `golangci-lint`, race

**Files:** none (verification only)

- [ ] **Step 1: Run the package's full test suite under race**

Run: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/ssetranslate/...`
Expected: `ok`, no skips.

- [ ] **Step 2: Run vet and lint on the package**

Run: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" go vet ./internal/ssetranslate/...`
Expected: exit 0.

Run: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse/plugin && PATH="$HOME/.local/share/mise/shims:$PATH" golangci-lint run ./internal/ssetranslate/...`
Expected: exit 0. If `golangci-lint` flags an unused parameter or an unchecked write error, fix in place before committing.

- [ ] **Step 3: Run repo-wide `make check`**

Run: `cd /Users/dor.amid/.superset/worktrees/token-bay/plugin-sse && PATH="$HOME/.local/share/mise/shims:$PATH" make check`
Expected: tests + lint pass across all modules.

- [ ] **Step 4: If any verification step fails, fix before opening PR**

The fix lands as its own commit (`fix(plugin/ssetranslate): …`); do NOT amend prior commits. Re-run `make check` until green.

- [ ] **Step 5: Open the PR**

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
gh pr create --base main --title "feat(plugin): internal/ssetranslate — stream-json → Anthropic SSE adapter" --body "$(cat <<'EOF'
## Summary

- New package `plugin/internal/ssetranslate`: pure adapter from Claude Code's `--output-format stream-json` byte stream to an Anthropic-compatible SSE byte stream.
- Implements the four locked decisions from the design spec: separate package; tool_use ships at-once in `content_block_start`; multi-cycle assistant events collapse to one envelope; `message_delta` deferred until the terminal `result` event so usage is canonical.
- Six golden-fixture test pairs (text-only, multi-block, tool_use+text, multi-cycle collapse, malformed line tolerance, no-result-event synthetic close) plus property tests (chunk-boundary independence, inner.Write error latching, Close idempotency).
- No third-party deps. No I/O, no goroutines, no `crypto/rand`. Race-clean.

Spec: `docs/superpowers/specs/plugin/2026-05-10-ssetranslate-design.md`.

## Test plan

- [x] `go test -race ./plugin/internal/ssetranslate/...` green.
- [x] `golangci-lint run ./plugin/internal/ssetranslate/...` green.
- [x] `make check` green at repo root.
- [ ] Reviewer spot-checks fixture pairs against the spec's mapping table.

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

- [ ] Spec coverage: every section in `2026-05-10-ssetranslate-design.md` (§3.2 API, §3.3 state machine, §3.4 mapping, §3.5 D1–D8, §5.1 fixtures, §5.2 property tests) maps to at least one task above.
- [ ] No "TBD"/"TODO"/"figure out later" left in code or fixtures.
- [ ] Type and method names match across tasks: `Writer`, `NewWriter`, `Write`, `Close`, `state`, `envelope`, `usage`, `assistantEnvelope`, `resultEnvelope`, `synthStopReason` are consistent throughout.
- [ ] Each fixture file exists in both `testdata/sj/` and `testdata/sse/`.
- [ ] No imports of `internal/tunnel`, `internal/ccbridge`, `shared/`, or any third-party crypto.
