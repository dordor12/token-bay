//go:build unix

package ccbridge

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// isolatedEnv returns a copy of the parent environment with HOME
// rewritten to point at home. Claude Code resolves ~/.claude/ from
// HOME, so an empty home directory hides plugins, skills, hooks,
// settings.json, CLAUDE.md project memory, and SessionStart-hook
// output from the seeder subprocess. Other env vars (PATH, locale,
// ANTHROPIC_*) pass through unchanged.
//
// Auth is preserved by seedIsolatedHomeAuth (called separately):
// claude reads OAuth tokens from $HOME/.claude/credentials.json,
// which would also be hidden by a naïve HOME override. We symlink
// just that one file into the isolated home so the seeder can keep
// using its existing claude login without the package ever touching
// credential bytes.
func isolatedEnv(home string) []string {
	base := os.Environ()
	out := make([]string, 0, len(base)+1)
	seenHome := false
	for _, kv := range base {
		if strings.HasPrefix(kv, "HOME=") {
			out = append(out, "HOME="+home)
			seenHome = true
			continue
		}
		out = append(out, kv)
	}
	if !seenHome {
		out = append(out, "HOME="+home)
	}
	return out
}

// seedIsolatedHomeAuth makes the seeder's claude OAuth tokens
// visible inside the isolated home so the subprocess can authenticate
// with Anthropic without the package ever touching credential bytes.
// The bridge never reads the credential material; OS-level symlinks
// hand the auth path through to claude verbatim, preserving the
// package-wide "no inspection of auth credentials" rule.
//
// macOS: Claude Code stores OAuth tokens in the user's keychain
// (~/Library/Keychains/login.keychain-db). The path resolves through
// HOME, so HOME isolation alone hides it. Symlinking the Keychains
// directory restores claude's view of the keychain.
//
// Linux: Claude Code stores credentials in ~/.claude/credentials.json
// (HOME-relative file). Symlinking that single file is enough.
//
// Plugins, skills, hooks, settings.json, CLAUDE.md, and ~/.claude.json
// all remain hidden — only the auth-relevant paths cross into the
// isolated view.
//
// Returns nil silently when the source isn't present (caller's claude
// is not logged in); the subprocess will surface "Not logged in"
// itself.
func seedIsolatedHomeAuth(home string) error {
	realHome, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	switch runtime.GOOS {
	case "darwin":
		src := filepath.Join(realHome, "Library", "Keychains")
		if _, err := os.Stat(src); err != nil {
			return nil
		}
		dstParent := filepath.Join(home, "Library")
		if err := os.MkdirAll(dstParent, 0o700); err != nil {
			return fmt.Errorf("ccbridge: mkdir isolated Library: %w", err)
		}
		dst := filepath.Join(dstParent, "Keychains")
		if _, err := os.Lstat(dst); err == nil {
			// Already seeded; idempotent.
			return nil
		}
		if err := os.Symlink(src, dst); err != nil {
			return fmt.Errorf("ccbridge: symlink Keychains: %w", err)
		}
	default:
		src := filepath.Join(realHome, ".claude", "credentials.json")
		if _, err := os.Stat(src); err != nil {
			return nil
		}
		dstDir := filepath.Join(home, ".claude")
		if err := os.MkdirAll(dstDir, 0o700); err != nil {
			return fmt.Errorf("ccbridge: mkdir isolated .claude: %w", err)
		}
		dst := filepath.Join(dstDir, "credentials.json")
		if _, err := os.Lstat(dst); err == nil {
			// Already seeded; idempotent.
			return nil
		}
		if err := os.Symlink(src, dst); err != nil {
			return fmt.Errorf("ccbridge: symlink credentials: %w", err)
		}
	}
	return nil
}

// extractTextContent extracts the plain-text string from a message
// content field. It handles two forms:
//
//   - JSON string: "hello" → "hello"
//   - Content-block array: [{"type":"text","text":"..."},...]
//     All text-type blocks are concatenated (space-separated).
//
// Other block types (tool_use, tool_result) are skipped. Returns an
// error if raw is not valid JSON or contains no text content.
func extractTextContent(raw json.RawMessage) (string, error) {
	// Try plain JSON string first.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}

	// Try content-block array.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", fmt.Errorf("ccbridge: extractTextContent: not a string or block array: %w", err)
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("ccbridge: extractTextContent: no text blocks found")
	}
	return strings.Join(parts, " "), nil
}

// resolveVersion returns the claude CLI version string. The result is
// cached on r.cachedVersion so subsequent calls are free. Falls back
// to "2.1.138" on any error.
//
// The probe uses its own short timeout independent of ctx. The
// session-file Version field is informational (claude-code does not
// validate it on load), so a missing or malformed `claude --version`
// must not block or fail the request — a fake/test binary that
// ignores --version and hangs would otherwise stall the entire Run.
func (r *ExecRunner) resolveVersion(ctx context.Context) (string, error) {
	if r.cachedVersion != "" {
		return r.cachedVersion, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(probeCtx, r.resolveBinary(), "--version").Output()
	if err != nil {
		r.cachedVersion = "2.1.138"
		return r.cachedVersion, nil
	}
	v, err := parseClaudeVersion(string(out))
	if err != nil {
		r.cachedVersion = "2.1.138"
		return r.cachedVersion, nil
	}
	r.cachedVersion = v
	return r.cachedVersion, nil
}

// Run implements Runner on Unix platforms.
//
// The conversation history (req.Messages[:len-1]) is written as a
// synthetic Claude Code session JSONL file under
// $HOME/.claude/projects/<sanitized-cwd>/<sessionID>.jsonl. The
// subprocess is invoked with --resume <sessionID> and the final user
// turn as a positional -p argument. No stdin is written; claude reads
// history from the session file via its own loadConversationForResume
// path. On context cancel the entire process group is SIGKILLed.
func (r *ExecRunner) Run(ctx context.Context, req Request, sink io.Writer) error {
	if err := ValidateMessages(req.Messages); err != nil {
		return fmt.Errorf("ccbridge: invalid request: %w", err)
	}

	// Extract the last user turn's text — this becomes the positional
	// -p prompt argument.
	last := req.Messages[len(req.Messages)-1]
	prompt, err := extractTextContent(last.Content)
	if err != nil {
		return fmt.Errorf("ccbridge: extract prompt text: %w", err)
	}

	if len(req.ClientPubkey) != ed25519.PublicKeySize {
		return fmt.Errorf("ccbridge: ClientPubkey is required and must be %d bytes", ed25519.PublicKeySize)
	}
	rawCwd := ClientHomeDir(r.resolveSeederRoot(), req.ClientPubkey)
	if err := os.MkdirAll(rawCwd, 0o700); err != nil {
		return fmt.Errorf("ccbridge: mkdir client home: %w", err)
	}
	// No defer-remove — the janitor handles deletion for inactive
	// clients. Keep mtime fresh so the janitor's grace window resets
	// on every active request.
	now := time.Now()
	_ = os.Chtimes(rawCwd, now, now)

	// On macOS os.MkdirAll may create a path under /var/folders/…
	// which is a symlink to /private/var/folders/…. The subprocess
	// resolves its cwd to the real path via the OS, so
	// SanitizePath must be applied to the same real path to produce
	// the matching project directory for --resume.
	cwd, err := filepath.EvalSymlinks(rawCwd)
	if err != nil {
		return fmt.Errorf("ccbridge: eval symlinks for cwd: %w", err)
	}

	if err := seedIsolatedHomeAuth(cwd); err != nil {
		return err
	}

	sessionID, err := GenerateSessionID()
	if err != nil {
		return fmt.Errorf("ccbridge: generate session id: %w", err)
	}

	version, err := r.resolveVersion(ctx)
	if err != nil {
		return fmt.Errorf("ccbridge: resolve version: %w", err)
	}

	// Write history (all messages except the last user turn) to the
	// session file. If there is only one message (the prompt itself),
	// skip --resume so BuildArgv omits it.
	history := req.Messages[:len(req.Messages)-1]
	if len(history) > 0 {
		if _, err := WriteSessionFile(cwd, cwd, sessionID, version, history); err != nil {
			return fmt.Errorf("ccbridge: write session file: %w", err)
		}
	} else {
		// No history to resume; clear sessionID so --resume is omitted.
		sessionID = ""
	}

	argv := BuildArgv(req, sessionID, prompt)
	cmd := exec.CommandContext(ctx, r.resolveBinary(), argv...)
	cmd.Dir = cwd
	cmd.Env = isolatedEnv(cwd)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return os.ErrProcessDone
	}
	cmd.WaitDelay = 500 * time.Millisecond

	stderr := &capBuffer{cap: r.resolveStderrCap()}
	cmd.Stderr = stderr
	cmd.Stdout = sink
	cmd.Stdin = nil

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return &ExitError{Code: exitErr.ExitCode(), Stderr: stderr.bytes()}
		}
		return fmt.Errorf("ccbridge: run claude: %w", err)
	}
	return nil
}

// capBuffer is a bounded byte buffer used for stderr capture.
type capBuffer struct {
	cap  int
	data []byte
}

func (b *capBuffer) Write(p []byte) (int, error) {
	if len(b.data) >= b.cap {
		return len(p), nil
	}
	room := b.cap - len(b.data)
	if room >= len(p) {
		b.data = append(b.data, p...)
	} else {
		b.data = append(b.data, p[:room]...)
	}
	return len(p), nil
}

func (b *capBuffer) bytes() []byte { return b.data }
