// Package ccbridge invokes `claude -p "<prompt>"` with the airtight
// tool-disabling flag set documented in plugin spec §6.2, conveys
// prior conversation history via a synthetic session JSONL file, and
// parses Claude Code's stream-json output. It is the seeder-side
// bridge between an inbound forwarded request and the seeder's local
// Claude Code account.
//
// # Spec
//
// Plugin design §6.2 ("The Claude Code CLI bridge") locks in the
// non-negotiable flag set:
//
//	-p "<prompt>"
//	--model <requested>
//	--output-format stream-json
//	--verbose
//	--tools ""
//	--disallowedTools "*"
//	--mcp-config '{"mcpServers":{}}'
//	--strict-mcp-config
//	--settings '{"hooks":{}}'
//	--resume <sessionID>           (when prior history exists)
//	--no-session-persistence
//
// # Transport
//
// Stream-json stdin is no longer used to convey conversation history.
// Instead, the bridge writes a synthetic Claude Code session JSONL
// file at $HOME/.claude/projects/<sanitized-cwd>/<sessionID>.jsonl
// before exec. The file uses Claude Code's native SerializedMessage
// schema — one record per Message in Request.Messages[:len-1]. Claude
// loads the file through the same loadConversationForResume code path
// real session resumes use, preserving tool blocks and content-block
// shape verbatim. The last user turn in Request.Messages becomes the
// positional -p prompt argument. --no-session-persistence prevents
// claude from appending its own records to the file the bridge wrote,
// keeping the file's contents fully under bridge control.
//
// In addition to the flag set, the subprocess runs with HOME
// rewritten to a fresh tempdir (see ExecRunner.Run). Claude Code
// resolves ~/.claude/ from HOME, so an empty home directory hides
// the seeder's plugins, skills, SessionStart hooks, settings.json,
// and CLAUDE.md project memory. Without the HOME override, the
// subprocess inherits the seeder's ~/.claude installation and leaks
// ambient context (skill registry, plugin SessionStart output, MCP
// instructions) into the /v1/messages body, breaking wire fidelity.
// Authentication still works: macOS keychain entries are per-user,
// not per-HOME, so the seeder's logged-in claude OAuth token
// continues to authorize requests.
//
// All flag strings live in flags.go as named constants. Any change to
// any constant MUST ship with an updated conformance run per spec §12.
//
// # Responsibilities
//
//   - flags.go      — canonical airtight argv (BuildArgv).
//   - runner.go     — Runner interface and ExecRunner default impl.
//   - stream.go     — line-scanner parser; forwards bytes verbatim,
//     extracts Usage from the terminal `result` event.
//   - bridge.go     — Bridge.Serve composes Runner + parser.
//   - conformance.go — RunStartupConformance (boot-time advertise gate).
//
// # Out of scope
//
// This package does not handle the consumer-side `/usage` PTY probe —
// that lives in plugin/internal/ratelimit. It does not assemble
// exhaustion proofs, build broker envelopes, manage tunnels, or
// negotiate with the tracker. It does not interpret the consumer's
// conversation context — Bridge.Serve takes a Request carrying
// Messages and Model; the caller is responsible for assembling
// the message history from the forwarded /v1/messages body.
//
// # Safety argument
//
// With every side-effecting primitive disabled at the flag layer and
// the subprocess running in a freshly-mkdir'd empty CWD, the residual
// risk surface is "Claude Code's flag enforcement is correct" — a
// focused, testable property covered by:
//
//   - plugin/test/conformance (build tag `localintegtest`) — drives
//     the real claude binary against an adversarial corpus. Run via
//     `make -C plugin localintegtest`. Never runs in CI; gated by
//     the lefthook pre-commit hook on relevant path changes.
//   - the in-process subset in conformance.go (RunStartupConformance) —
//     the sidecar's seeder-advertise gate, runs against any Runner.
package ccbridge
