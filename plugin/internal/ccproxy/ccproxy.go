// Package ccproxy provides the Anthropic-compatible HTTP proxy endpoint
// that receives Claude Code's /v1/messages traffic when the Token-Bay
// sidecar has activated mid-session redirect (plugin spec §2.5).
//
// v1 scope:
//   - HTTP server on 127.0.0.1 with /v1/messages and /token-bay/health
//   - Session-mode state store (in-memory, thread-safe)
//   - Pre-flight auth check via `claude auth status --json`
//   - PassThroughRouter (forwards to Anthropic byte-for-byte)
//   - NetworkRouter stub (returns 501)
//
// Real network-mode routing lands in a subsequent feature plan that
// integrates shared/proto envelope types and internal/tunnel.
package ccproxy

// sessionIDHeader is the header Claude Code's Anthropic SDK client sets
// on every /v1/messages call. Pinned against
// src/services/api/client.ts:108 in the upstream Claude Code repo.
const sessionIDHeader = "X-Claude-Code-Session-Id"
