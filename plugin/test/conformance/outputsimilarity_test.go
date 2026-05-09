//go:build localintegtest

// Output-similarity tests for the ccbridge.
//
// Layered ON TOP OF wirefidelity_test.go (request-byte equality)
// and contextquality_test.go (input-determined wire-body equality
// across user-mode and bridge-mode invocations of claude). This
// layer asks a different, user-facing question: does the seeder
// bridge produce a RESPONSE that's semantically equivalent to the
// user's regular claude session would have produced?
//
// Each scenario:
//
//  1. Phase 1 — same as the context-quality tests; build a real
//     conversation history with tool execution.
//  2. Path A — claude binary directly with the same tools enabled,
//     processing extendedConvo (= phase-1 history + continuation).
//     Captures the model's final assistant text. This is the
//     "user kept typing in their full session" reference.
//  3. Path B — bridge.Serve with extendedConvo. Captures the
//     bridge's final assistant text. This is "consumer fell back
//     to the seeder via Token-Bay."
//  4. Judge — claude -p with a rubric prompt scoring how
//     semantically similar paths A and B's answers are. Returns
//     a float in [0, 1].
//  5. Assert score >= scenario-specific threshold.
//
// Wire-fidelity tests catch byte-level wrapping bugs. This layer
// catches semantic regressions: a bridge change that breaks the
// model's ability to answer faithfully would show up as a low
// judge score even if every wire body is technically valid.
package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/ccbridge"
)

// runFullClaudeContinuation runs the claude binary directly with
// the SAME tool allow-list and permission mode phase 1 used (so
// the model can re-execute tools if it chooses), feeds the convo
// via stream-json stdin, and returns the model's final assistant
// text — the "user keeps typing in their full session" answer.
func runFullClaudeContinuation(t *testing.T, system string, convo []ccbridge.Message, model string, allowedTools []string, cwd string) string {
	t.Helper()
	stdout := runUserMode(t, system, convo, model, allowedTools, cwd)
	return finalAssistantText(stdout)
}

// runBridgeContinuation calls bridge.Serve and returns the
// model's final assistant text — the seeder's answer.
func runBridgeContinuation(t *testing.T, system string, convo []ccbridge.Message, model string) string {
	t.Helper()
	bridge := ccbridge.NewBridge(&ccbridge.ExecRunner{BinaryPath: claudeBin(t)})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var sink bytes.Buffer
	_, err := bridge.Serve(ctx, ccbridge.Request{
		System:   system,
		Messages: convo,
		Model:    model,
	}, &sink)
	require.NoError(t, err, "bridge.Serve failed; sink: %s", sink.String())
	return finalAssistantText(sink.String())
}

// finalAssistantText extracts just the plain-text portion of the
// last assistant message in a stream-json stdout dump. Skips
// thinking blocks and tool_use blocks (they have no surface text the
// user would read). If multiple assistant events share an id, the
// blocks are merged before extraction.
func finalAssistantText(stdout string) string {
	type asstEv struct {
		Type    string `json:"type"`
		Message struct {
			ID      string            `json:"id"`
			Role    string            `json:"role"`
			Content []json.RawMessage `json:"content"`
		} `json:"message"`
	}

	var lastID string
	var rawBlocks []json.RawMessage
	for _, line := range strings.Split(stdout, "\n") {
		if line == "" || line[0] != '{' {
			continue
		}
		var ev asstEv
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Type != "assistant" || ev.Message.Role != "assistant" {
			continue
		}
		if ev.Message.ID != lastID {
			lastID = ev.Message.ID
			rawBlocks = rawBlocks[:0]
		}
		rawBlocks = append(rawBlocks, ev.Message.Content...)
	}
	if len(rawBlocks) == 0 {
		return ""
	}
	merged, err := json.Marshal(rawBlocks)
	if err != nil {
		return ""
	}
	var blocks []map[string]any
	if err := json.Unmarshal(merged, &blocks); err != nil {
		return ""
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
	return out.String()
}

// judgePromptTmpl is the rubric the LLM judge applies. Question +
// both answers go in. The judge MUST reply with a single
// floating-point number on its own line — anything else is a
// parse failure and we surface it loudly.
const judgePromptTmpl = `You are scoring the semantic similarity of two assistant answers to the same user question. Score from 0.0 to 1.0.

Rubric:
- 1.0 — answers convey the same information; a user asking the question would be equally well served by either.
- 0.7 — answers cover the same essential point with different phrasing or detail level.
- 0.5 — answers address the same topic but disagree on a key detail, or one answers fully while the other is partial.
- 0.0 — answers are unrelated, contradictory, or one fails to address the question at all.

Respond with ONLY the score as a single floating-point number on its own line. No commentary, no markdown.

Question:
%s

Answer A:
%s

Answer B:
%s
`

// scoreRE matches a floating-point number anywhere in the judge's
// reply. The rubric asks for a bare number, but the judge
// occasionally adds a sentence; matching anywhere keeps us robust
// without being silently wrong (we still fail the assertion if the
// number is outside [0, 1]).
var scoreRE = regexp.MustCompile(`(?m)([01](?:\.[0-9]+)?|0?\.[0-9]+)`)

// judgeOutputSimilarity runs claude -p with the judge prompt and
// returns the parsed score. The judge model is the same haiku
// used by the scenarios — cheap enough at one call per test, and
// we get to keep all scenario costs under the same model so
// quota patterns are predictable.
func judgeOutputSimilarity(t *testing.T, question, answerA, answerB string) float64 {
	t.Helper()
	bin := claudeBin(t)

	prompt := []ccbridge.Message{
		{Role: ccbridge.RoleUser, Content: ccbridge.TextContent(
			// Format inline so we don't need a Sprintf import dance.
			strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(judgePromptTmpl,
				"%s", question), "%s", answerA), "%s", answerB),
		)},
	}
	// Sprintf was clearer; rewrite cleanly.
	prompt = []ccbridge.Message{
		{Role: ccbridge.RoleUser, Content: ccbridge.TextContent(
			renderJudgePrompt(question, answerA, answerB),
		)},
	}

	cwd := t.TempDir()
	require.NoError(t, seedTestKeychain(cwd))

	// The judge has exactly one user message — use it as the positional
	// -p prompt with no session file (no history to resume).
	require.Len(t, prompt, 1)
	judgePromptText, perr := extractTextForTest(prompt[0].Content)
	require.NoError(t, perr, "extract judge prompt text")

	args := userArgv(ccbridge.Request{
		System: "You are a precise scorer. Reply with exactly one floating-point number between 0.0 and 1.0 on a single line.",
		Model:  "claude-haiku-4-5-20251001",
	}, nil /* no tools */)
	// Append positional prompt (no --resume; single-turn judge has no history).
	args = append(args, judgePromptText)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = cwd
	cmd.Env = append(envWithHome(cwd), "ANTHROPIC_BASE_URL="+os.Getenv("ANTHROPIC_BASE_URL"))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("judge claude exited with %v\nstderr: %s\nstdout: %s",
			err, stderr.String(), truncate(stdout.String(), 2048))
	}

	reply := finalAssistantText(stdout.String())
	if reply == "" {
		t.Fatalf("judge returned no assistant text; stdout: %s", truncate(stdout.String(), 2048))
	}
	m := scoreRE.FindString(reply)
	if m == "" {
		t.Fatalf("judge reply has no parseable score: %q", reply)
	}
	score, err := strconv.ParseFloat(m, 64)
	require.NoError(t, err, "parse judge score %q", m)
	if score < 0 || score > 1 {
		t.Fatalf("judge score out of range [0,1]: %v (reply: %q)", score, reply)
	}
	t.Logf("judge score=%.3f for question=%q", score, truncate(question, 80))
	return score
}

// renderJudgePrompt builds the judge prompt with the four
// substitutions (rubric is fixed, only question/A/B vary).
func renderJudgePrompt(question, answerA, answerB string) string {
	const tmpl = `You are scoring the semantic similarity of two assistant answers to the same user question. Score from 0.0 to 1.0.

Rubric:
- 1.0 — answers convey the same information; a user asking the question would be equally well served by either.
- 0.7 — answers cover the same essential point with different phrasing or detail level.
- 0.5 — answers address the same topic but disagree on a key detail, or one answers fully while the other is partial.
- 0.0 — answers are unrelated, contradictory, or one fails to address the question at all.

Respond with ONLY the score as a single floating-point number on its own line. No commentary, no markdown.

Question:
` + "{{Q}}" + `

Answer A:
` + "{{A}}" + `

Answer B:
` + "{{B}}" + `
`
	r := strings.NewReplacer("{{Q}}", question, "{{A}}", answerA, "{{B}}", answerB)
	return r.Replace(tmpl)
}

// outputSimilarityScenario describes one similarity test. Reuses
// scenario.setup from contextquality_test.go for phase-1 prompts.
type outputSimilarityScenario struct {
	name         string
	allowedTools []string
	setup        func(t *testing.T) scenarioPrompts
	threshold    float64 // judge score must be >= this to pass
}

// runOutputSimilarityScenario does the four-stage flow:
// phase 1 (build history) → path A (full claude) → path B (bridge)
// → judge. Asserts judge score >= sc.threshold.
func runOutputSimilarityScenario(t *testing.T, sc outputSimilarityScenario) {
	requireRealAnthropic(t)
	p := sc.setup(t)

	model := "claude-haiku-4-5-20251001"

	// Phase 1: build history with real tool execution. We don't
	// need a forwarding proxy for output-similarity — claude can
	// hit Anthropic directly for both paths and the judge. The
	// proxy mattered for wire-body capture; here we just want the
	// final assistant text from each path.
	seedConvo := []ccbridge.Message{{Role: ccbridge.RoleUser, Content: ccbridge.TextContent(p.seed)}}
	phase1Stdout := runUserMode(t, p.system, seedConvo, model, sc.allowedTools, p.phase1Cwd)
	historyMsgs := finalAssistantHistoryFromStdout(t, phase1Stdout, p.seed)
	extendedConvo := append(historyMsgs,
		ccbridge.Message{Role: ccbridge.RoleUser, Content: ccbridge.TextContent(p.continuation)})

	// Path A: full claude with tools enabled.
	answerA := runFullClaudeContinuation(t, p.system, extendedConvo, model, sc.allowedTools, p.phase1Cwd)

	// Path B: bridge.
	answerB := runBridgeContinuation(t, p.system, extendedConvo, model)

	require.NotEmpty(t, answerA, "path A produced no assistant text")
	require.NotEmpty(t, answerB, "path B produced no assistant text")

	score := judgeOutputSimilarity(t, p.continuation, answerA, answerB)
	t.Logf("%s: path A=%q\n%s: path B=%q",
		sc.name, truncate(answerA, 200),
		sc.name, truncate(answerB, 200))
	assert.GreaterOrEqual(t, score, sc.threshold,
		"%s: judge score %.3f < threshold %.3f\nA: %s\nB: %s",
		sc.name, score, sc.threshold, answerA, answerB)
}

// finalAssistantHistoryFromStdout reconstructs phase-1 history from
// stdout events alone — sidestepping the forwarding proxy used by
// contextquality_test.go. The seed user turn we know (we sent it),
// and we walk stream-json events to extract intermediate
// assistant turns (with tool_use), tool_result user turns, and
// the final assistant text.
//
// Output is a []Message ready to feed back via stream-json stdin.
// Thinking blocks are stripped (Anthropic refuses replayed
// thinking signatures in the latest assistant message).
func finalAssistantHistoryFromStdout(t *testing.T, stdout, seed string) []ccbridge.Message {
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

// =================== Scenarios ===================

// S1: TextOnly — text-only baseline. Both paths should produce
// nearly identical short replies.
func TestOutputSimilarity_S1_TextOnly(t *testing.T) {
	runOutputSimilarityScenario(t, outputSimilarityScenario{
		name: "S1_TextOnly",
		setup: func(t *testing.T) scenarioPrompts {
			return scenarioPrompts{
				system:       "You are concise. Respond with at most one short sentence.",
				seed:         "Reply with the literal token PING-ALPHA-OS1 and nothing else.",
				continuation: "Now reply with the literal token PONG-BETA-OS1 and nothing else.",
			}
		},
		threshold: 0.85,
	})
}

// S2: ReadTool — phase 1 reads a file with a magic token, the
// continuation asks to recall it from history. Both paths should
// produce the magic token without needing fresh tool access.
func TestOutputSimilarity_S2_ReadTool(t *testing.T) {
	const fileToken = "MAGIC-OS2-7K3F"
	runOutputSimilarityScenario(t, outputSimilarityScenario{
		name:         "S2_ReadTool",
		allowedTools: []string{"Read"},
		setup: func(t *testing.T) scenarioPrompts {
			cwd := t.TempDir()
			fpath := cwd + "/secret.txt"
			require.NoError(t, os.WriteFile(fpath, []byte("magic_token="+fileToken+"\nfiller\n"), 0o600))
			return scenarioPrompts{
				system:       "You are a precise file inspector. Use Read once when given a path, then answer from what you've already read.",
				seed:         "Use Read on " + fpath + " once, then state the magic_token value in plain text.",
				continuation: "Do not use any tool. Based on your previous assistant reply (which already contains the answer), what is the magic_token value? Reply with just the value.",
				phase1Cwd:    cwd,
			}
		},
		threshold: 0.7,
	})
}

// S4: GlobTool — phase 1 globs *.txt; continuation asks for count.
func TestOutputSimilarity_S4_GlobTool(t *testing.T) {
	runOutputSimilarityScenario(t, outputSimilarityScenario{
		name:         "S4_GlobTool",
		allowedTools: []string{"Glob"},
		setup: func(t *testing.T) scenarioPrompts {
			cwd := t.TempDir()
			require.NoError(t, os.WriteFile(cwd+"/alpha.txt", []byte("a"), 0o600))
			require.NoError(t, os.WriteFile(cwd+"/beta.txt", []byte("b"), 0o600))
			require.NoError(t, os.WriteFile(cwd+"/gamma.txt", []byte("c"), 0o600))
			require.NoError(t, os.WriteFile(cwd+"/skip.md", []byte("d"), 0o600))
			return scenarioPrompts{
				system:       "You are a file lister. Use Glob when given a pattern, then answer from what you found.",
				seed:         "Use Glob with pattern '*.txt' in " + cwd + " and tell me how many .txt files there are.",
				continuation: "Do not use any tool. Based on your previous assistant reply (which already states the count), how many .txt files were there? Reply with just the count as a single digit.",
				phase1Cwd:    cwd,
			}
		},
		threshold: 0.7,
	})
}

// S5: WriteTool — phase 1 writes a file with a payload phrase;
// continuation asks for the phrase back.
func TestOutputSimilarity_S5_WriteTool(t *testing.T) {
	const filePhrase = "PHRASE-OS5-ZULU-7D"
	runOutputSimilarityScenario(t, outputSimilarityScenario{
		name:         "S5_WriteTool",
		allowedTools: []string{"Write"},
		setup: func(t *testing.T) scenarioPrompts {
			cwd := t.TempDir()
			fpath := cwd + "/out.txt"
			return scenarioPrompts{
				system:       "You are a precise scribe. Use Write when given a path and content.",
				seed:         "Use Write to create " + fpath + " containing the line: " + filePhrase,
				continuation: "Do not use any tool. Based on your previous assistant reply, what was the exact line you wrote? Reply with just the line.",
				phase1Cwd:    cwd,
			}
		},
		threshold: 0.7,
	})
}

// S7: ParallelToolUses — phase 1 reads two files in parallel; the
// continuation asks for both magic tokens.
func TestOutputSimilarity_S7_ParallelToolUses(t *testing.T) {
	const tokenA = "PAR-OS7-ALPHA"
	const tokenB = "PAR-OS7-BRAVO"
	runOutputSimilarityScenario(t, outputSimilarityScenario{
		name:         "S7_ParallelToolUses",
		allowedTools: []string{"Read"},
		setup: func(t *testing.T) scenarioPrompts {
			cwd := t.TempDir()
			a := cwd + "/a.txt"
			b := cwd + "/b.txt"
			require.NoError(t, os.WriteFile(a, []byte("magic_token="+tokenA+"\n"), 0o600))
			require.NoError(t, os.WriteFile(b, []byte("magic_token="+tokenB+"\n"), 0o600))
			return scenarioPrompts{
				system:       "You are a parallel inspector. When given multiple files, Read them in parallel.",
				seed:         "In parallel, Read " + a + " and " + b + ", then state both magic_token values in plain text.",
				continuation: "Do not use any tool. Based on your previous assistant reply, what were the two magic_token values? Reply with just the two values, comma-separated.",
				phase1Cwd:    cwd,
			}
		},
		threshold: 0.6, // lowest threshold — parallel-tool histories are the trickiest for the bridge
	})
}

// Compile-time guard so the unused-import detector doesn't trip
// on `io` if everything that uses it is removed in a refactor.
var _ = io.Discard
