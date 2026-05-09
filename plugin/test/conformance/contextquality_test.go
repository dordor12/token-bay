//go:build localintegtest

// Context-quality integration tests for the ccbridge.
//
// Goal: prove that routing a continuation prompt through the seeder
// bridge produces a /v1/messages HTTP body byte-canonical-equal to
// the body the user would have seen if they'd typed the same prompt
// in their own claude-code session. The seeder must be "one more
// prompt in the same conversation" — nothing more, nothing less,
// across a representative set of conversation shapes (text-only,
// single tool, parallel tools, multi-turn with mixed tools).
//
// Each test follows the same pattern:
//
//  1. Phase 1 — build a real conversation history by running claude
//     with the user-mode flag set against a forwarding proxy that
//     relays /v1/messages to api.anthropic.com verbatim. claude
//     actually executes tools (Read, Grep, etc.) on real files in
//     a tempdir; the history is reconstructed from phase-1 stdout
//     stream-json events via parsePhase1History.
//
//  2. Phase 2 — canonical "user continues their session". Re-run
//     claude with the bridge's flag set + phase-1 history written
//     as a synthetic session file, last user turn as positional -p.
//     Capture the resulting POST body. This is the reference.
//
//  3. Phase 3 — bridge takeover. bridge.Serve(Request{Messages:
//     phase1 + new_user_turn}). Capture the bridge's POST body.
//
//  4. Compare phase 2 == phase 3 via the canonical JSONEq compare
//     already used by the wirefidelity tests.
package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/ccbridge"
)

// realAnthropicURL is where the forwarding proxy relays POSTs to.
// Tests will skip if the developer's environment can't reach it.
const realAnthropicURL = "https://api.anthropic.com"

// forwardingProxy stands up a local HTTP server that relays
// /v1/messages POSTs to api.anthropic.com verbatim (URL, method,
// headers, body), streams the upstream SSE response back to claude,
// and records every inbound body. Same conceptual shape as the
// production internal/ccproxy.
type forwardingProxy struct {
	server   *httptest.Server
	upstream *url.URL
	mu       sync.Mutex
	bodies   [][]byte
}

func newForwardingProxy(t *testing.T) *forwardingProxy {
	t.Helper()
	u, err := url.Parse(realAnthropicURL)
	require.NoError(t, err)
	p := &forwardingProxy{upstream: u}
	p.server = httptest.NewServer(http.HandlerFunc(p.handle))
	t.Cleanup(p.server.Close)
	return p
}

func (p *forwardingProxy) URL() string { return p.server.URL }

func (p *forwardingProxy) drainBodies() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.bodies
	p.bodies = nil
	return out
}

func (p *forwardingProxy) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if r.URL.Path == "/v1/messages" && r.Method == http.MethodPost {
		p.mu.Lock()
		p.bodies = append(p.bodies, body)
		p.mu.Unlock()
	}

	// Build the upstream request: same path/method/headers, fresh
	// body reader. Strip Host so the http client sets it correctly
	// for api.anthropic.com.
	upstreamURL := *p.upstream
	upstreamURL.Path = r.URL.Path
	upstreamURL.RawQuery = r.URL.RawQuery
	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL.String(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for k, vv := range r.Header {
		if strings.EqualFold(k, "Host") {
			continue
		}
		for _, v := range vv {
			upReq.Header.Add(k, v)
		}
	}

	resp, err := http.DefaultClient.Do(upReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// Stream the SSE response back as bytes arrive — claude is
	// reading event-by-event and won't tolerate buffered delivery.
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			return
		}
	}
}

// requireRealAnthropic skips the test if the developer's claude is
// not authenticated (claude auth status --json reports loggedIn=
// false). The bridge runner symlinks ~/Library/Keychains into the
// isolated home so OAuth-via-keychain works under HOME isolation;
// if the developer is in an environment without that, we skip.
func requireRealAnthropic(t *testing.T) {
	t.Helper()
	bin := claudeBin(t)
	cmd := exec.Command(bin, "auth", "status", "--json")
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("claude auth status failed: %v — skipping real-Anthropic test", err)
	}
	var status struct {
		LoggedIn bool `json:"loggedIn"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		t.Skipf("claude auth status not parseable: %v", err)
	}
	if !status.LoggedIn {
		t.Skip("claude not logged in — skipping real-Anthropic test")
	}
}

// userArgv is a compatibility shim used by judgeOutputSimilarity in
// outputsimilarity_test.go. Returns the base argv without a positional
// prompt; callers append the prompt themselves.
//
// The tool flags use --tools=<list> / --disallowedTools=<list> /
// --permission-mode=<mode> equals-separated syntax. claude's --tools
// is variadic, so writing it as two tokens "--tools" "" would let the
// CLI consume the next positional argument (the appended prompt) as
// a tool name, breaking the invocation. Equals-binding makes the value
// unambiguous and lets the prompt sit unambiguously at the end.
func userArgv(req ccbridge.Request, allowedTools []string) []string {
	full := ccbridge.BuildArgv(req, "", "")
	argv := full[:len(full)-1] // drop the trailing "" positional
	out := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		if argv[i] == ccbridge.FlagTools && i+1 < len(argv) {
			i++
			continue
		}
		if argv[i] == ccbridge.FlagDisallowedTools && i+1 < len(argv) {
			i++
			continue
		}
		out = append(out, argv[i])
	}
	if len(allowedTools) > 0 {
		out = append(out, ccbridge.FlagTools+"="+strings.Join(allowedTools, ","))
		out = append(out, "--permission-mode=bypassPermissions")
	} else {
		out = append(out, ccbridge.FlagTools+"=")
	}
	return out
}

// userArgvSession is BuildArgv with `--tools` swapped for the
// scenario's allow-list and bypassPermissions added so the named
// tool can actually execute under -p mode.
//
// Tool flags use equals-binding so the variadic --tools doesn't
// consume the trailing positional prompt as a tool name. See the
// comment on userArgv for the reasoning.
func userArgvSession(req ccbridge.Request, sessionID, prompt string, allowedTools []string) []string {
	argv := ccbridge.BuildArgv(req, sessionID, prompt)
	out := make([]string, 0, len(argv)+4)
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
		out = append(out, ccbridge.FlagTools+"="+strings.Join(allowedTools, ","))
		out = append(out, "--permission-mode=bypassPermissions")
	} else {
		out = append(out, ccbridge.FlagTools+"=")
	}
	return out
}

// runUserMode invokes the claude binary with the airtight flag
// set the bridge uses, but with `--tools` overridden to the
// scenario's allow-list (so phase 1 can actually execute Read/
// Grep/etc.). Writes convo[:len-1] as a synthetic session file
// under cwd; runs claude with --resume + the last user turn as
// positional -p. Returns stdout (line-delimited stream-json
// events) so callers can parse out the final assistant message.
//
// If cwd is empty, a fresh tempdir is created. HOME is rewritten
// to cwd via envWithHome; Keychains is symlinked in via
// seedTestKeychain.
func runUserMode(t *testing.T, system string, convo []ccbridge.Message, model string, allowedTools []string, cwd string) string {
	t.Helper()
	bin := claudeBin(t)
	if cwd == "" {
		cwd = t.TempDir()
	}
	// On macOS t.TempDir() returns a path under /var/folders/… which
	// is a symlink to /private/var/folders/…. Resolve to the real path
	// so SanitizePath produces the same directory the subprocess sees.
	if real, rerr := filepath.EvalSymlinks(cwd); rerr == nil {
		cwd = real
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
		_, werr := ccbridge.WriteSessionFile(cwd, cwd, sessionID, "2.1.138", history)
		require.NoError(t, werr)
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

// parsePhase1History reconstructs the full conversation history from
// phase-1 stdout events alone — no forwarding proxy required. The
// seed user turn is known (we sent it); we walk stream-json events
// to extract intermediate assistant turns (with tool_use), tool_result
// user turns, and the final assistant text reply.
//
// Output is a []Message ready to feed into WriteSessionFile for
// phase 2/3. Thinking blocks are stripped (Anthropic refuses replayed
// thinking signatures in the latest assistant message).
func parsePhase1History(t *testing.T, stdout, seed string) []ccbridge.Message {
	t.Helper()
	out := []ccbridge.Message{
		{Role: ccbridge.RoleUser, Content: ccbridge.TextContent(seed)},
	}

	type ev struct {
		Type    string `json:"type"`
		Message struct {
			ID      string            `json:"id"`
			Role    string            `json:"role"`
			Content []json.RawMessage `json:"content"`
		} `json:"message"`
	}

	// Group consecutive same-id assistant events; emit a Message
	// when we see a different id or a non-assistant event.
	var curID string
	var curBlocks []json.RawMessage
	flushAssistant := func() {
		if len(curBlocks) == 0 {
			return
		}
		merged, err := json.Marshal(curBlocks)
		if err == nil {
			stripped := stripNonReplayableBlocks(merged)
			if len(stripped) > 2 { // not just "[]"
				out = append(out, ccbridge.Message{
					Role:    ccbridge.RoleAssistant,
					Content: stripped,
				})
			}
		}
		curID = ""
		curBlocks = nil
	}

	for _, line := range strings.Split(stdout, "\n") {
		if line == "" || line[0] != '{' {
			continue
		}
		var e ev
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		switch e.Type {
		case "assistant":
			if e.Message.ID != curID {
				flushAssistant()
				curID = e.Message.ID
			}
			curBlocks = append(curBlocks, e.Message.Content...)
		case "user":
			flushAssistant()
			// User events from stream-json output represent
			// tool_results from local tool execution. Their
			// content is already a content-block array; ship it
			// verbatim.
			if len(e.Message.Content) == 0 {
				continue
			}
			merged, err := json.Marshal(e.Message.Content)
			if err == nil {
				out = append(out, ccbridge.Message{
					Role:    ccbridge.RoleUser,
					Content: merged,
				})
			}
		default:
			flushAssistant()
		}
	}
	flushAssistant()
	return out
}

// envWithHome returns os.Environ() with HOME rewritten to home.
// Mirrors ccbridge.runner_unix.isolatedEnv (which is unexported).
func envWithHome(home string) []string {
	base := os.Environ()
	out := make([]string, 0, len(base)+1)
	seen := false
	for _, kv := range base {
		if strings.HasPrefix(kv, "HOME=") {
			out = append(out, "HOME="+home)
			seen = true
			continue
		}
		out = append(out, kv)
	}
	if !seen {
		out = append(out, "HOME="+home)
	}
	return out
}

// seedTestKeychain symlinks ~/Library/Keychains into the test's
// isolated home so claude can authenticate with the developer's
// existing OAuth login (macOS-only — Linux uses ~/.claude/
// credentials.json which is a different bridging path; see
// runner_unix.seedIsolatedHomeAuth).
//
// Idempotent: callable multiple times on the same home (the
// scenario reuses one cwd across phase 1 and path A).
func seedTestKeychain(home string) error {
	realHome, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	src := realHome + "/Library/Keychains"
	if _, err := os.Stat(src); err != nil {
		return nil
	}
	if err := os.MkdirAll(home+"/Library", 0o700); err != nil {
		return err
	}
	dst := home + "/Library/Keychains"
	if _, err := os.Lstat(dst); err == nil {
		// Already seeded by a prior call on the same cwd.
		return nil
	}
	return os.Symlink(src, dst)
}

// stripNonReplayableBlocks removes content blocks the bridge can't
// faithfully reproduce on a re-run, and masks per-call identifiers
// inside the blocks that survive:
//
//   - thinking / redacted_thinking blocks: the model regenerates
//     them on every API call with a fresh signature and slightly
//     different prose. Drop entirely.
//   - tool_use.id and tool_result.tool_use_id: a fresh "toolu_…"
//     identifier is minted on every API call. Replace with a
//     constant so the surrounding shape can still be compared.
//
// text and tool_use payloads (name, input) and tool_result content
// strings are deterministic transmission inputs — they participate
// in the equality check.
func stripNonReplayableBlocks(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || raw[0] != '[' {
		return raw
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return raw
	}
	const maskedID = "MASKED"
	kept := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		t, _ := b["type"].(string)
		if t == "thinking" || t == "redacted_thinking" {
			continue
		}
		if t == "tool_use" {
			b["id"] = maskedID
		}
		if t == "tool_result" {
			b["tool_use_id"] = maskedID
		}
		kept = append(kept, b)
	}
	out, err := json.Marshal(kept)
	if err != nil {
		return raw
	}
	return out
}

// assertContextQualityBodiesEqual is the load-bearing assertion
// shared by every context-quality scenario. Decodes the user-chat
// and bridge POST bodies, masks per-session noise (cch digest in
// the system block), strips non-replayable thinking blocks, and
// compares system/model/messages with canonical-JSON equality.
func assertContextQualityBodiesEqual(t *testing.T, label string, userBody, bridgeBody []byte) {
	t.Helper()
	var u, b decodedRequest
	require.NoError(t, json.Unmarshal(userBody, &u), "%s: decode user body", label)
	require.NoError(t, json.Unmarshal(bridgeBody, &b), "%s: decode bridge body", label)

	assert.Equal(t, flattenSystem(u.System), flattenSystem(b.System),
		"%s: system differs", label)
	assert.Equal(t, u.Model, b.Model, "%s: model differs", label)
	require.Equal(t, len(u.Messages), len(b.Messages),
		"%s: messages length differs (user=%d bridge=%d)\nuser body: %s\nbridge body: %s",
		label, len(u.Messages), len(b.Messages), string(userBody), string(bridgeBody))

	for i := range u.Messages {
		assert.Equal(t, u.Messages[i].Role, b.Messages[i].Role,
			"%s: message %d role differs", label, i)
		uc := stripNonReplayableBlocks(u.Messages[i].Content)
		bc := stripNonReplayableBlocks(b.Messages[i].Content)
		assert.JSONEq(t, string(uc), string(bc),
			"%s: message %d content differs", label, i)
	}
}

// scenario describes one context-quality test case. setup runs first
// and returns the prompts/anchors so it can use t.TempDir() and other
// per-test state.
type scenario struct {
	name         string
	allowedTools []string // tools enabled for phase 1 (so claude can execute them)
	setup        func(t *testing.T) scenarioPrompts
}

type scenarioPrompts struct {
	system       string
	seed         string   // phase-1 user prompt — drives the tool-use cycle(s)
	continuation string   // new user turn appended in phase 2/3
	anchors      []string // strings that must appear in the user-chat last body
	// phase1Cwd, if non-empty, is the working directory phase 1's
	// claude subprocess runs in. Used by tools that gate paths to
	// "allowed working directories" (Grep, Glob, etc.) — putting
	// the test fixture file inside this cwd lets the tool execute
	// instead of erroring with a security block. Phase 2 and 3 use
	// ephemeral cwds; the first-POST comparison is input-determined
	// and doesn't depend on cwd state.
	phase1Cwd string
}

// runContextQualityScenario implements the three-phase comparison.
func runContextQualityScenario(t *testing.T, sc scenario) {
	requireRealAnthropic(t)
	p := sc.setup(t)

	proxy := newForwardingProxy(t)
	t.Setenv("ANTHROPIC_BASE_URL", proxy.URL())

	model := "claude-haiku-4-5-20251001"

	// Phase 1: build conversation history with real tool execution.
	// Drain the proxy after phase 1 to keep the counter clean for
	// phases 2/3; we don't use the wire bodies for history any more.
	seedConvo := []ccbridge.Message{{Role: ccbridge.RoleUser, Content: ccbridge.TextContent(p.seed)}}
	phase1Stdout := runUserMode(t, p.system, seedConvo, model, sc.allowedTools, p.phase1Cwd)
	proxy.drainBodies() // discard phase-1 bodies; history comes from stdout

	// Reconstruct the handoff history directly from phase-1 stdout
	// events (user + assistant turns). This is the same information
	// the session file will carry into phase 2/3, with no reliance on
	// proxy wire bodies. The returned slice starts with the seed user
	// turn and ends with the final assistant reply.
	historyMsgs := parsePhase1History(t, phase1Stdout, p.seed)
	extendedConvo := append(historyMsgs,
		ccbridge.Message{Role: ccbridge.RoleUser, Content: ccbridge.TextContent(p.continuation)})

	// Phase 2: canonical continuation — claude binary directly with
	// the bridge's flag set (no allowed tools). The point isn't
	// "match a user's full plugin-loaded session" (impossible —
	// bridge intentionally hides plugins for safety); it's "claude
	// binary with bridge flags + this convo on stdin produces a
	// well-defined wire body." Phase 3 must match.
	runUserMode(t, p.system, extendedConvo, model, nil /* no tools */, "")
	userContBodies := proxy.drainBodies()
	require.NotEmpty(t, userContBodies, "phase 2 produced no /v1/messages bodies")
	// claude may emit a session title-generation POST before the
	// conversation POST — we want the conversation one. Pick the
	// FIRST POST whose body carries the continuation text, which is
	// fully determined by the input we feed (independent of the
	// model's later non-deterministic choice between text reply and
	// another tool_use cycle, which would fork the LAST POST).
	userContFirst := pickConversationBody(t, userContBodies, p.continuation)

	// Phase 3: bridge takeover — same convo through bridge.Serve.
	bridge := ccbridge.NewBridge(&ccbridge.ExecRunner{BinaryPath: claudeBin(t), SeederRoot: t.TempDir()})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	_, err := bridge.Serve(ctx, ccbridge.Request{
		System:       p.system,
		Messages:     extendedConvo,
		Model:        model,
		ClientPubkey: testClientPubkey(t.Name()),
	}, io.Discard)
	require.NoError(t, err)
	bridgeBodies := proxy.drainBodies()
	require.NotEmpty(t, bridgeBodies, "phase 3 produced no /v1/messages bodies")
	bridgeFirst := pickConversationBody(t, bridgeBodies, p.continuation)

	// The headline assertion: input-determined POST bodies match.
	assertContextQualityBodiesEqual(t, sc.name, userContFirst, bridgeFirst)

	// Anchors: each must appear in the user-chat conversation body.
	// Body equality carries them over to the bridge.
	for _, a := range p.anchors {
		assert.Contains(t, string(userContFirst), a,
			"%s: anchor %q missing from user-chat conversation body", sc.name, a)
	}
}

// pickConversationBody returns the input-determined conversation
// POST among bodies. Filters out title-generation POSTs (whose
// system block includes a fixed claude-code marker) and bodies
// missing the continuation marker, then returns the one with the
// fewest messages. The input-determined POST is always shortest;
// any subsequent POST adds the model's response cycle (assistant
// turn + tool_result), which would only make the messages array
// longer.
//
// This shape-based selector is robust against Claude Code merging
// pending user turns into tool_result content via system-reminder
// injection (which would defeat a "last user message is the
// continuation" check) and against the model non-deterministically
// choosing text-only or another tool_use cycle (which would defeat
// a "first body" check).
func pickConversationBody(t *testing.T, bodies [][]byte, marker string) []byte {
	t.Helper()
	type wireBody struct {
		Messages []json.RawMessage `json:"messages"`
		System   json.RawMessage   `json:"system"`
	}
	const titleGenMarker = "Generate a concise, sentence-case title"
	var best []byte
	bestLen := -1
	for _, b := range bodies {
		if !bytes.Contains(b, []byte(marker)) {
			continue
		}
		var wb wireBody
		if err := json.Unmarshal(b, &wb); err != nil {
			continue
		}
		if bytes.Contains(wb.System, []byte(titleGenMarker)) {
			continue
		}
		if best == nil || len(wb.Messages) < bestLen {
			best = b
			bestLen = len(wb.Messages)
		}
	}
	if best == nil {
		t.Fatalf("no conversation /v1/messages body found containing %q (scanned %d bodies)", marker, len(bodies))
	}
	return best
}

// S1: TextOnly — baseline. No tools used in phase 1; just plain
// text turns. Confirms the forwarding proxy + comparison harness
// works before we exercise tool-use cycles.
func TestContextQuality_S1_TextOnly(t *testing.T) {
	runContextQualityScenario(t, scenario{
		name: "S1_TextOnly",
		setup: func(t *testing.T) scenarioPrompts {
			return scenarioPrompts{
				system:       "You are concise. Respond with at most one short sentence.",
				seed:         "Reply with the literal token PING-ALPHA-S1 and nothing else.",
				continuation: "Now reply with the literal token PONG-BETA-S1 and nothing else.",
				anchors:      []string{"PING-ALPHA-S1", "PONG-BETA-S1"},
			}
		},
	})
}

// S2: ReadTool — first scenario with a real tool-use cycle. Phase 1
// asks claude to Read a temp file; claude emits a tool_use, executes
// Read locally, sees the file contents in the tool_result, and
// answers in text. The post-handoff convo therefore contains a real
// tool_use + tool_result pair. Phase 2/3 continuation re-fed via
// stream-json must produce equal wire bodies.
func TestContextQuality_S2_ReadTool(t *testing.T) {
	const fileToken = "MAGIC-S2-7K3F"
	runContextQualityScenario(t, scenario{
		name:         "S2_ReadTool",
		allowedTools: []string{"Read"},
		setup: func(t *testing.T) scenarioPrompts {
			tmpdir := t.TempDir()
			fpath := tmpdir + "/secret.txt"
			require.NoError(t, os.WriteFile(fpath, []byte("magic_token="+fileToken+"\nfiller_line\n"), 0o600))
			return scenarioPrompts{
				system:       "You are a precise file inspector. Use Read once when given a path, then answer from what you've already read.",
				seed:         "Use Read on " + fpath + " once, then state the magic_token value in plain text.",
				continuation: "Without using any tool, recall and repeat the magic_token value from your previous reply.",
				anchors:      []string{fileToken},
			}
		},
	})
}

// S4: GlobTool — claude lists files matching a pattern in a tempdir;
// tool_result contains a path list. Tests another structured tool
// output shape.
func TestContextQuality_S4_GlobTool(t *testing.T) {
	const fileToken = "GLOB-S4-PROBE"
	runContextQualityScenario(t, scenario{
		name:         "S4_GlobTool",
		allowedTools: []string{"Glob"},
		setup: func(t *testing.T) scenarioPrompts {
			cwd := t.TempDir()
			// Multiple .txt files; one has a unique prefix. Files
			// must live in cwd so Glob's path gate accepts them.
			require.NoError(t, os.WriteFile(cwd+"/alpha.txt", []byte("a"), 0o600))
			require.NoError(t, os.WriteFile(cwd+"/beta.txt", []byte("b"), 0o600))
			require.NoError(t, os.WriteFile(cwd+"/"+fileToken+".txt", []byte("c"), 0o600))
			require.NoError(t, os.WriteFile(cwd+"/skip.md", []byte("d"), 0o600))
			return scenarioPrompts{
				system:       "You are a file lister. Use Glob when given a pattern, then answer from what you found. Never call any tool a second time.",
				seed:         "Use Glob with pattern '*.txt' in " + cwd + " and tell me how many .txt files there are.",
				continuation: "Reply with just one digit: the count from your previous answer. Plain text only.",
				anchors:      []string{fileToken},
				phase1Cwd:    cwd,
			}
		},
	})
}

// S5: WriteTool — claude writes a file with a specific payload;
// tool_use.input carries the file body verbatim, tool_result is a
// short confirmation. Tests fidelity for write-modifying tools.
func TestContextQuality_S5_WriteTool(t *testing.T) {
	const filePhrase = "PHRASE-S5-ZULU-7D"
	runContextQualityScenario(t, scenario{
		name:         "S5_WriteTool",
		allowedTools: []string{"Write"},
		setup: func(t *testing.T) scenarioPrompts {
			tmpdir := t.TempDir()
			fpath := tmpdir + "/out.txt"
			return scenarioPrompts{
				system:       "You are a precise scribe. Use Write when given a path and content.",
				seed:         "Use Write to create " + fpath + " containing the line: " + filePhrase,
				continuation: "Without using any tool, recall the file path and the content you wrote in your previous reply.",
				anchors:      []string{filePhrase},
			}
		},
	})
}

// S7: ParallelToolUses — phase 1 has the model issue two Read tool
// calls in one assistant turn (one assistant content array with two
// tool_use blocks; one user content array with two tool_result
// blocks). Tests order/structure preservation when an assistant
// turn carries multiple tool_use blocks.
func TestContextQuality_S7_ParallelToolUses(t *testing.T) {
	const tokenA = "PAR-S7-ALPHA"
	const tokenB = "PAR-S7-BRAVO"
	runContextQualityScenario(t, scenario{
		name:         "S7_ParallelToolUses",
		allowedTools: []string{"Read"},
		setup: func(t *testing.T) scenarioPrompts {
			cwd := t.TempDir()
			a := cwd + "/a.txt"
			b := cwd + "/b.txt"
			require.NoError(t, os.WriteFile(a, []byte("magic_token="+tokenA+"\n"), 0o600))
			require.NoError(t, os.WriteFile(b, []byte("magic_token="+tokenB+"\n"), 0o600))
			return scenarioPrompts{
				system:       "You are a parallel inspector. When given multiple files, Read them in parallel (one assistant turn with multiple tool_use blocks). Never call any tool a second time.",
				seed:         "In parallel, Read " + a + " and " + b + ", then state both magic_token values in plain text.",
				continuation: "Reply with just the two values, comma-separated. Plain text only.",
				anchors:      []string{tokenA, tokenB},
				phase1Cwd:    cwd,
			}
		},
	})
}

// S8: MultiTurnMixedTools — three sequential user turns, each
// triggering a different tool: Read, then Bash, then a final
// recall question. Tests long accumulated history and cache_control
// marker placement across diverse tool kinds.
//
// SKIPPED: building a realistic multi-turn-with-different-tools
// convo requires either (a) running phase 1 across multiple
// runUserMode invocations with the same cwd (currently each
// invocation gets its own ephemeral cwd), or (b) feeding all three
// user turns at once via stream-json and trusting claude to drive
// the cycles in order. (a) needs cwd plumbing; (b) is fragile
// because claude may rearrange or merge the turns. Track via an
// issue when the bridge supports persisted-cwd runs.
func TestContextQuality_S8_MultiTurnMixedTools(t *testing.T) {
	t.Skip("multi-turn-with-different-tools needs persisted-cwd runs across phase-1 turns")
}
