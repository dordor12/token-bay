// Package ccbridge invokes `claude -p "<prompt>"` with the airtight
// tool-disabling flag set documented in plugin spec §6.2 and parses
// Claude Code's stream-json output. It is the seeder-side bridge
// between an inbound forwarded request and the seeder's local Claude
// Code account.
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
//	--disallowedTools "*"
//	--mcp-config /dev/null
//	--settings '{"hooks":{}}'
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
// conversation context — Bridge.Serve takes a flat Prompt string and
// the caller is responsible for prompt construction.
//
// # Safety argument
//
// With every side-effecting primitive disabled at the flag layer and
// the subprocess running in a freshly-mkdir'd empty CWD, the residual
// risk surface is "Claude Code's flag enforcement is correct" — a
// focused, testable property covered by the conformance suite at
// plugin/test/conformance and the in-process subset in conformance.go.
package ccbridge
