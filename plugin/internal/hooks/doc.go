// Package hooks parses Claude Code hook payloads (the four events the
// plugin observes per spec §2.1) and dispatches them to a pluggable Sink.
//
// Responsibilities:
//   - Define typed payload structs for SessionStart, SessionEnd, and
//     UserPromptSubmit (events.go). The StopFailure payload lives in
//     plugin/internal/ratelimit and is reused.
//   - Define the Response type that hook subprocesses write to stdout to
//     control the host Claude Code's behavior (response.go).
//   - Define the Sink interface that downstream wiring (the running sidecar
//     supervisor) will implement to react to events (sink.go).
//   - Provide a Dispatcher that reads a hook event off an io.Reader, calls
//     the appropriate Sink method, and writes a Response to an io.Writer
//     (dispatcher.go).
//
// This package performs no I/O at the package boundary — callers pass
// io.Reader and io.Writer in. The cmd layer is responsible for wiring
// stdin/stdout of the hook subprocess.
//
// Authoritative spec: docs/superpowers/specs/plugin/2026-04-22-plugin-design.md
// §2.1 (the four hook events). Schemas pinned to upstream Claude Code source
// at src/entrypoints/sdk/coreSchemas.ts (line numbers cited per file).
package hooks

// EventName* constants enumerate the Claude Code hook events the plugin
// binds to. Strings are the literal values of hook_event_name in the
// upstream schemas (src/entrypoints/sdk/coreSchemas.ts:484, 493, 529, 758).
const (
	EventNameStopFailure      = "StopFailure"
	EventNameSessionStart     = "SessionStart"
	EventNameSessionEnd       = "SessionEnd"
	EventNameUserPromptSubmit = "UserPromptSubmit"
)
