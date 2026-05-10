# SSE Translator (`plugin/internal/ssetranslate`) — Subsystem Design Spec

| Field | Value |
|---|---|
| Parent | [Plugin design](./2026-04-22-plugin-design.md) §6.2/§7.2, [Architecture](../2026-04-22-token-bay-architecture-design.md) §10.3 |
| Status | Design draft — 2026-05-10 |
| Date | 2026-05-10 |
| Scope | Seeder-side translator that reads Claude Code's `--output-format stream-json` byte stream and emits an Anthropic-compatible Server-Sent Events (SSE) byte stream onto an `io.Writer`. Pure adapter: no I/O of its own, no knowledge of `internal/tunnel` or `internal/ccbridge`. The future seeder accept-loop composes this between `bridge.Serve` and `tunnel.ResponseWriter()`. |

## 1. Purpose

Plugin design §6.2 commits the seeder to invoking `claude -p --output-format stream-json` and §7.2 commits the seeder→consumer half of the tunnel to "Anthropic SSE chunks forwarded verbatim where possible". Stream-json is *not* SSE; it is line-delimited JSON with claude-internal envelopes around the assistant's content. The translator bridges the two formats so that:

- The bytes leaving the seeder's tunnel writer are byte-identical (in shape, not content) to what `api.anthropic.com` would have produced for the same `/v1/messages` POST.
- The consumer side can `io.Copy` from the tunnel into Claude Code's open HTTP response without any consumer-side parsing. Tunnel/doc.go already locks the wire to "verbatim Anthropic SSE byte stream"; this package is what makes that promise true.

## 2. Non-goals (explicit)

- The seeder accept loop. Reading the consumer's request body off the tunnel, decoding it into a `ccbridge.Request`, calling `bridge.Serve`, signaling `tunnel.SendOK`/`SendError`, and reporting `usage_report` to the tracker all live in a follow-on plan (the "data-path coordinator" tunnel/doc.go references). This package is invoked by that coordinator; it does not orchestrate it.
- Anthropic API authentication. The translator never sees credentials; it sees stream-json bytes from a subprocess that holds the seeder's auth.
- Usage extraction for the tracker's `usage_report`. `ccbridge.ParseStreamJSON` already extracts canonical `Usage` from the terminal `result` event for that purpose. The translator separately folds `result.usage` into the `message_delta` event because Anthropic's wire requires it there; the two consumers of the same `result` event do not coordinate.
- Reverse direction (consumer-side SSE → stream-json). Out of scope; not a use case anywhere in Token-Bay.
- Validation of consumer-supplied request bodies. The seeder coordinator parses `/v1/messages` JSON before calling the bridge; the translator has nothing to do with the request side.

## 3. Architecture

### 3.1 Position in the seeder pipeline

```
                     ┌────────────────────────────────────────────────┐
                     │  seeder coordinator (separate, future plan)    │
                     │   1. tunnel.Listener.Accept                    │
                     │   2. tun.ReadRequest → parse /v1/messages JSON │
                     │   3. tun.SendOK                                │
                     │   4. bridge.Serve(ctx, req, sink)              │
                     │      where sink = ssetranslate.NewWriter(      │
                     │              tun.ResponseWriter())             │
                     │   5. tun.CloseWrite                            │
                     │   6. tracker.UsageReport                       │
                     └────────────────────────────────────────────────┘
                                          │
                                          ▼
                     ┌────────────────────────────────────────────────┐
                     │  ssetranslate.Writer  (this package)           │
                     │   • io.Writer surface                          │
                     │   • line-delimited stream-json scanner         │
                     │   • per-message state machine                  │
                     │   • emits SSE bytes verbatim onto inner writer │
                     └────────────────────────────────────────────────┘
                                          │
                                          ▼
                     ┌────────────────────────────────────────────────┐
                     │  tunnel.Tunnel.ResponseWriter() io.Writer      │
                     │  (raw bytes go on the wire after status 0x00)  │
                     └────────────────────────────────────────────────┘
```

### 3.2 Public API

```go
package ssetranslate

// NewWriter returns an io.Writer that consumes line-delimited stream-json
// bytes and emits Anthropic-compatible SSE bytes onto inner. The writer
// is single-flight: writes from a single goroutine only. Callers SHOULD
// wrap inner once per logical /v1/messages turn — the writer holds
// state for one message envelope at a time and surfaces an error on a
// second `message_start` after `message_stop` has been emitted.
//
// The returned writer's Write returns len(p), nil when bytes are
// successfully ingested, even if zero SSE bytes are emitted as a
// result. A non-nil error from inner.Write is propagated; subsequent
// writes are no-ops.
//
// The writer does not flush on its own — callers wrapping an
// http.ResponseWriter should wrap with their own flushing layer if
// per-event delivery to the consumer is required. Tunnel-side QUIC
// streams flush opportunistically; no extra layer is needed.
func NewWriter(inner io.Writer) *Writer

// Close finalizes any in-flight message. If a message_start was emitted
// without a matching message_stop (e.g. the upstream stream was cut
// short), Close emits a synthetic message_delta + message_stop carrying
// stop_reason="end_turn" and the best-effort usage observed so far.
// Returns an error iff inner.Write fails during finalization.
func (*Writer) Close() error
```

`Writer` also implements `io.WriteCloser`. It is safe to call `Close` multiple times; the second call is a no-op.

### 3.3 Internal state machine

```
                ┌──────────┐  first assistant event with new message.id
                │  IDLE    ├──────────────────────────────────────────────┐
                └──────────┘                                              ▼
                                                            ┌────────────────────────┐
                                                            │  IN_MESSAGE            │
                  result event                              │  • message.id pinned   │
                ┌──────────────────────────────────────────►│  • next content_block  │
                │                                           │    index = 0           │
                │   ┌───────────────────────────────────────┴──────┐                 │
                │   │ assistant event with N content blocks (N≥1)  │                 │
                │   └─────────────┬────────────────────────────────┘                 │
                │                 ▼                                                  │
                │  ┌─────────────────────────────────┐                               │
                │  │ for each new block index seen:  │                               │
                │  │   emit content_block_start{idx} │                               │
                │  │ emit content_block_delta{idx}*  │                               │
                │  │ if same block index already     │                               │
                │  │ open: emit content_block_stop{} │                               │
                │  │ when next block index appears:  │                               │
                │  │   emit content_block_stop{prev} │                               │
                │  └─────────────────────────────────┘                               │
                │                                                                    │
                │  another assistant event, same message.id ────────────────────────►│
                │  another assistant event, NEW message.id (multi-cycle collapse) ──►│
                ▼
        ┌──────────────────────┐
        │  CLOSING             │
        │  • emit any pending  │
        │    content_block_stop│
        │  • emit message_delta│
        │    {stop_reason,     │
        │      usage}          │
        │  • emit message_stop │
        └──────────────────────┘
                ▼
            ┌──────┐
            │ DONE │
            └──────┘
```

### 3.4 Stream-json → SSE event mapping

| Stream-json input | SSE output |
|---|---|
| `{"type":"system","subtype":"init",...}` | (drop — claude-internal) |
| First `{"type":"assistant","message":{...}}` per turn | `event: message_start`<br>`data: {"type":"message_start","message":<msg with content=[]>}` |
| Each new content-block index in an assistant event | `event: content_block_start`<br>`data: {"type":"content_block_start","index":N,"content_block":<block>}` |
| Text portion of a `text` block | `event: content_block_delta`<br>`data: {"type":"content_block_delta","index":N,"delta":{"type":"text_delta","text":"…"}}` |
| `tool_use` block (delivered with full `input`) | `event: content_block_start` carrying full `input`; no `input_json_delta` events |
| End of a content block (next block appears or message ends) | `event: content_block_stop`<br>`data: {"type":"content_block_stop","index":N}` |
| Multi-cycle collapse (additional assistant events, same OR new message.id, before `result`) | (extend the SAME envelope — append blocks under sequential indexes; do NOT emit a second message_start) |
| `{"type":"user","message":{tool_result…}}` events | (drop — seeder runs `--tools=""`; these are synthetic SDK-builtin echoes; preserving them leaks seeder internals) |
| `{"type":"result","usage":{...},"is_error":...}` | `event: message_delta`<br>`data: {"type":"message_delta","delta":{"stop_reason":"<derived>"},"usage":<from result>}`<br>`event: message_stop`<br>`data: {"type":"message_stop"}` |
| Malformed JSON line | (drop, do not error — bridge's `ParseStreamJSON` is also tolerant) |

`stop_reason` derivation: prefer the last assistant event's `stop_reason` field if non-null; else `"end_turn"` for `result.is_error=false`; else `"error"` for `result.is_error=true`.

### 3.5 Locked design decisions

| # | Decision | Choice | Rationale |
|---|---|---|---|
| D1 | Translator location | New package `plugin/internal/ssetranslate` | Pure adapter; ccbridge stays "stream-json bytes verbatim into a sink", tunnel stays format-agnostic, sidecar stays no-business-logic. |
| D2 | tool_use input encoding | Full `input` in `content_block_start`; no `input_json_delta` reconstruction | Stream-json delivers the full input atomically. Anthropic's SDK accepts populated-input start events. Reconstructing per-character JSON deltas adds risk without observable benefit — the consumer's claude assembles the same final block either way. |
| D3 | Multi-cycle assistant events | Collapse to one logical message envelope | Anthropic's SSE for one `/v1/messages` call exposes exactly one envelope. Stream-json's per-tool-cycle assistant events are an internal claude-side artifact. Preserving them leaks "the seeder ran multiple internal cycles" and confuses consumer-side message-boundary heuristics. |
| D4 | Usage placement | Defer `message_delta` until `result` event seen, fold `result.usage` in | Anthropic puts canonical usage in the terminal `message_delta`. Per-cycle assistant `usage` is partial. Buffering one event of state is cheap. |
| D5 | Drop user/tool_result events | Yes | Seeder runs `--tools=""`; user/tool_result events here are SDK-builtin echoes (e.g. Monitor leak). Forwarding them fragments the message envelope on the consumer side. |
| D6 | Malformed line tolerance | Drop and continue | Mirrors `ccbridge.ParseStreamJSON`; transient corruption (rare) shouldn't abort a streamed turn already in flight. |
| D7 | Flush behavior | None | The package writes raw bytes; flush is the inner writer's concern. Tunnel/QUIC opportunistic flush is sufficient for the consumer to see tokens as they arrive. |
| D8 | Error contract | First `inner.Write` error makes subsequent writes no-op; returned from current Write call | Mid-stream tunnel close should not amplify into per-line errors. Caller observes error on the next syscall. |

## 4. File map

```
plugin/internal/ssetranslate/
├── doc.go                         — package doc, mapping table, lock-ins
├── translate.go                   — Writer + state machine
├── translate_test.go              — unit tests against golden fixtures
└── testdata/
    ├── sj/text_only_single_cycle.jsonl
    ├── sj/multi_block_text_text.jsonl
    ├── sj/tool_use_then_text.jsonl
    ├── sj/multi_cycle_collapsed.jsonl
    ├── sj/malformed_mid_stream.jsonl
    ├── sj/no_result_event.jsonl
    ├── sse/text_only_single_cycle.sse
    ├── sse/multi_block_text_text.sse
    ├── sse/tool_use_then_text.sse
    ├── sse/multi_cycle_collapsed.sse
    ├── sse/malformed_mid_stream.sse
    └── sse/no_result_event.sse
```

## 5. Test strategy

### 5.1 Unit tests (golden fixture pairs)

Each case has matched `.jsonl` (input) and `.sse` (expected output). The test loads both, runs the input through `Writer`, and asserts byte-equal output.

| Fixture | Coverage |
|---|---|
| `text_only_single_cycle` | Happy path: init + one assistant event + result. |
| `multi_block_text_text` | Two text blocks with index advancement → two `content_block_start`/`stop` pairs. |
| `tool_use_then_text` | Mixed-block: tool_use block (full input) + text block, single cycle. |
| `multi_cycle_collapsed` | Two assistant cycles before `result` — verifies one envelope, sequential indexes. |
| `malformed_mid_stream` | One non-JSON line in the middle — output identical to the same fixture without the bad line. |
| `no_result_event` | Stream ends without `result` — `Close` emits synthetic `message_delta`+`message_stop` with `stop_reason="end_turn"`. |

### 5.2 Property-style coverage

In addition to golden cases:
- A `bytes.Buffer` sink wrapped in an "every byte counted" decorator asserts no out-of-band bytes (e.g. spurious `\r`) appear.
- `Writer.Write([]byte("garbage"))` followed by valid stream-json: the garbage line is dropped; subsequent valid output is byte-identical to a clean run.
- Inner-writer error: a fake `errWriter` that fails on the third call returns the error from `Writer.Write`; no panics.

### 5.3 What this package does NOT test

- End-to-end through `tunnel`. Covered by Deliverable 2's `network_dialer_test.go` (a real `tunnel.Listener` with a hand-rolled "fake seeder" that uses this writer to produce SSE).
- End-to-end through real `claude -p`. Covered by the future seeder-coordinator plan's `localintegtest` conformance.

## 6. Open questions

None at design time. The bridge's `ParseStreamJSON` already locks the input format; Anthropic's public SSE schema locks the output format. The only material judgment calls are the four locked in §3.5; revisiting them needs a follow-up spec amendment, not a code change.

## 7. Acceptance criteria

This subsystem is "done" when:

- [ ] `go test ./plugin/internal/ssetranslate/...` passes with no skips and the race detector clean.
- [ ] Each fixture in §5.1 is present and matches.
- [ ] `golangci-lint run ./plugin/internal/ssetranslate/...` passes.
- [ ] The package's `doc.go` documents the mapping table and the four locked decisions, so a future reader does not have to consult this spec to understand intent.
- [ ] No production import of `crypto/rand`, no filesystem I/O, no goroutines (the package is a pure adapter).
- [ ] PR is merged green; no localintegtest dependency.
