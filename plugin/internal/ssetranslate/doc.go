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
