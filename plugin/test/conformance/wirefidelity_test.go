//go:build localintegtest

// Wire-fidelity integration tests for the ccbridge.
//
// These tests stand up a local HTTP server that records POST
// /v1/messages bodies, set ANTHROPIC_BASE_URL to point at it, and run
// the bridge through a real `claude` binary. The captured request
// body is then compared field-by-field against the Request the
// bridge was given — proving the conversation we put into Request
// flows through to Anthropic's wire byte-for-byte (modulo claude-CLI
// metadata like tool declarations, which differ by design between
// bridge-wrapped and native invocations).
package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/ccbridge"
)

// stubAnthropicSSE returns a minimal valid Anthropic /v1/messages
// streaming response: message_start → one text delta "ok" →
// message_stop. claude consumes this and exits cleanly.
func stubAnthropicSSE() []byte {
	var buf bytes.Buffer
	for _, ev := range []struct{ event, data string }{
		{"message_start", `{"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","content":[],"model":"claude-haiku-4-5","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":0}}}`},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`},
		{"message_stop", `{"type":"message_stop"}`},
	} {
		buf.WriteString("event: ")
		buf.WriteString(ev.event)
		buf.WriteString("\ndata: ")
		buf.WriteString(ev.data)
		buf.WriteString("\n\n")
	}
	return buf.Bytes()
}

// recordingProxy captures POST /v1/messages bodies and returns a
// stub SSE response so claude exits cleanly. Other endpoints (auth,
// telemetry) get a 404 — claude tolerates them in --bare-adjacent
// non-interactive flows.
type recordingProxy struct {
	server *httptest.Server
	mu     sync.Mutex
	bodies [][]byte
}

func newRecordingProxy(t *testing.T) *recordingProxy {
	t.Helper()
	p := &recordingProxy{}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages" && r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			p.mu.Lock()
			p.bodies = append(p.bodies, body)
			p.mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(stubAnthropicSSE())
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(p.server.Close)
	return p
}

func (p *recordingProxy) URL() string { return p.server.URL }

// drainBodies returns and clears all captured bodies. Call AFTER
// the subprocess has exited — by then the proxy has stored
// everything.
func (p *recordingProxy) drainBodies() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.bodies
	p.bodies = nil
	return out
}

// cchHashRE matches the per-session digest claude-code stamps into
// its x-anthropic-billing-header system block. The value differs
// every invocation, so we mask it before comparing system prompts.
var cchHashRE = regexp.MustCompile(`cch=[0-9a-f]+`)

// flattenSystem accepts the loose Anthropic /v1/messages "system"
// field (string OR [{type:"text",text:string}]) and returns a flat
// string for comparison, with per-invocation noise masked out.
func flattenSystem(v interface{}) string {
	var raw string
	switch s := v.(type) {
	case string:
		raw = s
	case []interface{}:
		var out strings.Builder
		for _, item := range s {
			if m, ok := item.(map[string]interface{}); ok {
				if t, _ := m["text"].(string); t != "" {
					out.WriteString(t)
				}
			}
		}
		raw = out.String()
	}
	return cchHashRE.ReplaceAllString(raw, "cch=MASKED")
}

// flattenContent extracts text from a message's content field, which
// may be a string OR a list of content blocks like
// [{type:"text",text:"..."}].
func flattenContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
		return ""
	}
	var blocks []map[string]interface{}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var out strings.Builder
	for _, b := range blocks {
		if b["type"] == "text" {
			if t, _ := b["text"].(string); t != "" {
				out.WriteString(t)
			}
		}
	}
	return out.String()
}

// decodedRequest is the slice of /v1/messages we compare on. We
// ignore tool declarations, mcp config, and any claude-injected
// metadata — only the conversation payload is asserted.
type decodedRequest struct {
	System   interface{} `json:"system"`
	Model    string      `json:"model"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

// assertLastWireBodiesEqual drives the same convo through (a) the
// claude binary directly (HOME-isolated, airtight flag set) and (b)
// the bridge, then asserts the last /v1/messages POST body of each
// is byte-canonical-equal in system/model/messages.
//
// "Byte-canonical-equal" means: system text equal after masking the
// per-session cch digest in the billing-header block; model strings
// equal; messages arrays equal in length, role, and JSON-equivalent
// content blocks (so cache_control markers, content-block structure,
// and any future field claude adds are all caught).
//
// Returns the last user-chat body so the caller can run additional
// anchors (e.g., assertContains for a magic token).
func assertLastWireBodiesEqual(t *testing.T, system string, convo []ccbridge.Message, model string) []byte {
	t.Helper()

	proxy := newRecordingProxy(t)
	t.Setenv("ANTHROPIC_BASE_URL", proxy.URL())
	// HOME-isolation hides ~/.claude/credentials.json, so claude
	// reports apiKeySource="none" without an env override. The stub
	// recording proxy doesn't validate Authorization, so any
	// non-empty key satisfies claude's "I have an auth source"
	// precondition. Real-Anthropic context-quality tests use a
	// different mechanism (credential symlink) that's also OK with
	// the bridge's HOME isolation.
	t.Setenv("ANTHROPIC_API_KEY", "test-stub-key")

	// (1) Regular user chat: claude binary directly, BuildArgv flag
	// set, HOME isolated.
	runUserChat(t, claudeBin(t), system, convo, model)
	userConvBodies := filterConversationBodies(proxy.drainBodies())
	require.NotEmpty(t, userConvBodies, "user chat produced no conversation /v1/messages bodies")
	userLast := userConvBodies[len(userConvBodies)-1]

	// (2) Same convo through the bridge.
	bridge := ccbridge.NewBridge(&ccbridge.ExecRunner{BinaryPath: claudeBin(t)})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := bridge.Serve(ctx, ccbridge.Request{
		System:   system,
		Messages: convo,
		Model:    model,
	}, io.Discard)
	require.NoError(t, err)
	bridgeConvBodies := filterConversationBodies(proxy.drainBodies())
	require.NotEmpty(t, bridgeConvBodies, "bridge produced no conversation /v1/messages bodies")
	bridgeLast := bridgeConvBodies[len(bridgeConvBodies)-1]

	// (3) Compare.
	var u, b decodedRequest
	require.NoError(t, json.Unmarshal(userLast, &u), "decode user body: %s", string(userLast))
	require.NoError(t, json.Unmarshal(bridgeLast, &b), "decode bridge body: %s", string(bridgeLast))

	assert.Equal(t, flattenSystem(u.System), flattenSystem(b.System),
		"system differs between user chat and bridge")
	assert.Equal(t, u.Model, b.Model, "model differs")
	require.Equal(t, len(u.Messages), len(b.Messages),
		"messages length differs (user=%d bridge=%d)\nuser body: %s\nbridge body: %s",
		len(u.Messages), len(b.Messages), string(userLast), string(bridgeLast))

	for i := range u.Messages {
		assert.Equal(t, u.Messages[i].Role, b.Messages[i].Role,
			"message %d role differs", i)
		// JSONEq compares JSON value equality (independent of key
		// order and whitespace) — tighter than flattenContent's
		// text extraction, since it also catches cache_control,
		// content-block structure, and any new fields.
		assert.JSONEq(t,
			string(u.Messages[i].Content), string(b.Messages[i].Content),
			"message %d content differs", i)
	}

	return userLast
}

// TestBridgeContext_WireFidelity_RecallsFileContent runs a single
// user turn that embeds a temp file's content (with a unique magic
// token) through both the claude binary directly and the bridge,
// then asserts the two /v1/messages POST bodies are byte-canonical-
// equal. The magic token serves as a high-signal anchor — its
// presence in the user-chat body proves end-to-end byte transit;
// the body equality then carries that property over to the bridge.
func TestBridgeContext_WireFidelity_RecallsFileContent(t *testing.T) {
	tmpdir := t.TempDir()
	fileToken := "MAGIC-FILE-TOKEN-9F3E"
	fileContent := "magic_token=" + fileToken + "\nseparator_line\n"
	fpath := filepath.Join(tmpdir, "data.txt")
	require.NoError(t, os.WriteFile(fpath, []byte(fileContent), 0o600))

	system := "You are a helpful assistant. Always respond with exactly the magic token from the file when asked."
	userText := "Here is " + fpath + ":\n---\n" + fileContent + "---\nWhat is the magic token?"
	convo := []ccbridge.Message{
		{Role: ccbridge.RoleUser, Content: ccbridge.TextContent(userText)},
	}
	model := "claude-haiku-4-5-20251001"

	userLast := assertLastWireBodiesEqual(t, system, convo, model)

	// Token-survival anchor: the file's magic token must appear in
	// the user-chat wire body. By body-equality with the bridge, it
	// also appears in the bridge's body.
	assert.Contains(t, string(userLast), fileToken,
		"file token did not round-trip into the /v1/messages body")
}

// TestBridgeContext_WireFidelity_MatchesUserChat simulates a regular
// multi-turn user chat against the claude binary, captures the LAST
// /v1/messages POST (the one that carries the full accumulated
// conversation), then runs the same conversation through the
// seeder's ccbridge and asserts the bridge's POST body matches.
//
// The "last POST" is the load-bearing one because each user turn
// triggers its own /v1/messages call; the final call carries the
// full accumulated history (u1, a_ok, u2, a_ok, u3). If the bridge
// and a regular user chat agree on that body, the seeder's
// "quality identical to a regular chat" promise holds at the wire.
func TestBridgeContext_WireFidelity_MatchesUserChat(t *testing.T) {
	system := "Test system prompt."
	convo := []ccbridge.Message{
		{Role: ccbridge.RoleUser, Content: ccbridge.TextContent("regular-chat turn-1 ALPHA")},
		{Role: ccbridge.RoleUser, Content: ccbridge.TextContent("regular-chat turn-2 BRAVO")},
		{Role: ccbridge.RoleUser, Content: ccbridge.TextContent("regular-chat turn-3 CHARLIE")},
	}
	model := "claude-haiku-4-5-20251001"

	assertLastWireBodiesEqual(t, system, convo, model)
}

// TestBridgeContext_WireFidelity_MultiTurnWithFileContent combines
// the multi-turn shape of MatchesUserChat with the file-content
// anchor of RecallsFileContent: a 3-turn conversation in which one
// user turn embeds a temp file's bytes (with a unique magic token).
// Asserts the LAST /v1/messages POST from a regular user chat and
// from the bridge are byte-canonical-equal, and that the magic
// token survives end-to-end.
//
// This is the strongest of the three wire-fidelity tests — it
// covers content-block structure, cache_control marker placement
// across multiple turns, and the embedded-file payload all in one
// scenario. A regression in any of those surfaces here.
func TestBridgeContext_WireFidelity_MultiTurnWithFileContent(t *testing.T) {
	tmpdir := t.TempDir()
	fileToken := "MAGIC-FILE-TOKEN-MULTI-7C2D"
	fileContent := "magic_token=" + fileToken + "\nseparator_line\nextra_payload=ZULU\n"
	fpath := filepath.Join(tmpdir, "multi.txt")
	require.NoError(t, os.WriteFile(fpath, []byte(fileContent), 0o600))

	system := "You are a helpful assistant. Cite values exactly as they appear in shared file content."
	convo := []ccbridge.Message{
		{Role: ccbridge.RoleUser, Content: ccbridge.TextContent("I'm going to share " + fpath + " in the next turn.")},
		{Role: ccbridge.RoleUser, Content: ccbridge.TextContent("Here is " + fpath + ":\n---\n" + fileContent + "---")},
		{Role: ccbridge.RoleUser, Content: ccbridge.TextContent("What is the magic token from the file I just shared?")},
	}
	model := "claude-haiku-4-5-20251001"

	userLast := assertLastWireBodiesEqual(t, system, convo, model)

	assert.Contains(t, string(userLast), fileToken,
		"file token did not round-trip into the last /v1/messages body")
}

// titleGenMarker is the fixed string claude-code places in the system
// block of its automatic session-title generation POST. Any body whose
// raw bytes contain this string is a housekeeping call, not the
// conversation POST we want to compare.
const titleGenMarker = "Generate a concise, sentence-case title"

// filterConversationBodies strips title-generation POSTs from a slice
// of captured /v1/messages bodies. The remaining bodies are the actual
// conversation POSTs.
func filterConversationBodies(bodies [][]byte) [][]byte {
	out := bodies[:0:len(bodies)]
	for _, b := range bodies {
		if !bytes.Contains(b, []byte(titleGenMarker)) {
			out = append(out, b)
		}
	}
	return out
}

// runUserChat invokes the claude binary with the same airtight
// flag set BuildArgv produces, writing convo[:len-1] as a
// synthetic session file under cwd and passing the last user
// turn as the positional -p argument. cwd is the isolated HOME
// (also the subprocess CWD). HOME is rewritten to cwd via
// isolatedHomeEnv so claude reads ~/.claude/ from cwd, and
// Keychains is symlinked in via seedTestKeychain to preserve OAuth.
func runUserChat(t *testing.T, bin, system string, convo []ccbridge.Message, model string) {
	t.Helper()
	require.NotEmpty(t, convo)
	last := convo[len(convo)-1]
	require.Equal(t, ccbridge.RoleUser, last.Role)

	rawCwd := t.TempDir()
	// On macOS t.TempDir() returns a path under /var/folders/… which
	// is a symlink to /private/var/folders/…. The subprocess resolves
	// its cwd to the real path, so SanitizePath must be applied to the
	// same real path to produce a matching project directory.
	cwd, err := filepath.EvalSymlinks(rawCwd)
	require.NoError(t, err)

	require.NoError(t, seedTestKeychain(cwd))

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

// extractTextForTest mirrors ccbridge.extractTextContent (which is
// unexported) — pulls a plain text string out of a content field
// that may be either a JSON string or a content-block array.
func extractTextForTest(raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", fmt.Errorf("extractTextForTest: not string or block array: %w", err)
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("extractTextForTest: no text block")
	}
	return strings.Join(parts, " "), nil
}

// isolatedHomeEnv mirrors ccbridge.runner_unix.isolatedEnv: copy the
// parent environment and rewrite HOME so claude can't see ~/.claude/.
// Inlined here because the runner helper is unexported.
func isolatedHomeEnv(home string) []string {
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
