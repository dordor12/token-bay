package ccbridge

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SessionRecord is one line in a Claude Code session JSONL file.
// Mirrors the SerializedMessage shape from claude-code's
// src/types/logs.ts. Only the fields we write are listed; we
// intentionally omit optional metadata (gitBranch, slug, etc.).
// claude-code accepts records with missing optional fields.
type SessionRecord struct {
	Type        string         `json:"type"`        // "user" | "assistant"
	UUID        string         `json:"uuid"`        // record identity
	ParentUUID  *string        `json:"parentUuid"`  // chain pointer; null on first record
	IsSidechain bool           `json:"isSidechain"` // false for main chain
	Timestamp   string         `json:"timestamp"`   // ISO-8601 UTC
	CWD         string         `json:"cwd"`         // subprocess working dir
	UserType    string         `json:"userType"`    // "external" for bridge-written records
	Entrypoint  string         `json:"entrypoint"`  // "cli"
	SessionID   string         `json:"sessionId"`   // matches the file's sessionID
	Version     string         `json:"version"`     // Claude Code version string
	Message     SessionMessage `json:"message"`     // the Anthropic message wrapper
}

// SessionMessage is the wire-shaped Anthropic message inside a
// SessionRecord. Content is json.RawMessage so callers control
// exactly what content blocks reach the file (a JSON string for
// plain text, or a content-block array for tool_use/tool_result).
type SessionMessage struct {
	Role    string          `json:"role"` // "user" | "assistant"
	Content json.RawMessage `json:"content"`
}

// maxSanitizedLen mirrors claude-code's MAX_SANITIZED_LENGTH.
const maxSanitizedLen = 200

// SanitizePath maps an absolute filesystem path (typically a
// subprocess CWD) to the directory-name claude-code uses inside
// ~/.claude/projects/ to hold session files. The algorithm
// matches sanitizePath in claude-code/src/utils/
// sessionStoragePortable.ts: replace [^a-zA-Z0-9] with "-" per
// character; if the result exceeds 200 chars, append "-<hash>"
// where hash is a stable digest of the original input.
//
// We use a hex-encoded SHA-256 prefix instead of claude-code's
// Bun.hash / djb2 fallback. claude-code reads any matching
// directory name when resolving --resume, so as long as WE write
// and READ the same path, the digest scheme is internal.
//
// The algorithm operates at the byte level to match claude-code's
// regex behavior, which means multi-byte UTF-8 sequences have each
// byte replaced individually.
func SanitizePath(p string) string {
	b := []byte(p)
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			out = append(out, c)
		} else {
			out = append(out, '-')
		}
	}
	outStr := string(out)
	if len(outStr) <= maxSanitizedLen {
		return outStr
	}
	sum := sha256OfString(p)
	return outStr[:maxSanitizedLen] + "-" + sum[:8]
}

func sha256OfString(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// GenerateSessionID returns a fresh RFC 4122 v4 UUID string.
// Claude Code's --session-id flag validates the format strictly,
// so we set the v4 marker and the 10xx variant bits explicitly.
func GenerateSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("ccbridge: read random for session id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// validateHistoryMessages checks msgs for shape violations without
// imposing the "last message must be user" constraint. History slices
// can legitimately end on an assistant turn.
func validateHistoryMessages(msgs []Message) error {
	if len(msgs) == 0 {
		return ErrEmptyMessages
	}
	for i, m := range msgs {
		if m.Role != RoleUser && m.Role != RoleAssistant {
			return fmt.Errorf("%w: index %d role %q", ErrInvalidRole, i, m.Role)
		}
		if len(m.Content) == 0 {
			return fmt.Errorf("%w: index %d", ErrEmptyContent, i)
		}
		if !json.Valid(m.Content) {
			return fmt.Errorf("%w: index %d: not valid JSON", ErrEmptyContent, i)
		}
	}
	return nil
}

// WriteSessionFile writes a Claude Code session JSONL file under
// home/.claude/projects/<SanitizePath(cwd)>/<sessionID>.jsonl
// containing one record per Message in msgs. Returns the absolute
// path written so callers can verify or pass --resume <sessionID>.
//
// Each record is chained via parentUuid. Timestamps are generated
// per-record using monotonically-increasing UTC time so claude-code
// orders records correctly when reading.
//
// Validation: msgs must be non-empty, each role in {user, assistant},
// content non-empty and valid JSON. Unlike ValidateMessages, history
// may end on any role (caller controls the slice shape).
func WriteSessionFile(home, cwd, sessionID, version string, msgs []Message) (string, error) {
	if err := validateHistoryMessages(msgs); err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".claude", "projects", SanitizePath(cwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("ccbridge: mkdir session project dir: %w", err)
	}
	path := filepath.Join(dir, sessionID+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("ccbridge: create session file: %w", err)
	}
	defer f.Close()

	var prevUUID string
	now := time.Now().UTC()
	for i, m := range msgs {
		uid, err := GenerateSessionID()
		if err != nil {
			return "", fmt.Errorf("ccbridge: gen record uuid: %w", err)
		}
		var parent *string
		if i > 0 {
			p := prevUUID
			parent = &p
		}
		ts := now.Add(time.Duration(i) * time.Millisecond).Format("2006-01-02T15:04:05.000Z")
		rec := SessionRecord{
			Type:        m.Role,
			UUID:        uid,
			ParentUUID:  parent,
			IsSidechain: false,
			Timestamp:   ts,
			CWD:         cwd,
			UserType:    "external",
			Entrypoint:  "cli",
			SessionID:   sessionID,
			Version:     version,
			Message: SessionMessage{
				Role:    m.Role,
				Content: m.Content,
			},
		}
		b, err := json.Marshal(rec)
		if err != nil {
			return "", fmt.Errorf("ccbridge: marshal record %d: %w", i, err)
		}
		if _, err := io.WriteString(f, string(b)+"\n"); err != nil {
			return "", fmt.Errorf("ccbridge: write record %d: %w", i, err)
		}
		prevUUID = uid
	}
	return path, nil
}

// ClientHash returns a stable short fingerprint of an Ed25519
// public key, used as the per-client directory name under
// <SeederRoot>. 16 hex chars (64 bits) suffices for distinguishing
// simultaneously-active peers; collision resistance against an
// adversary is provided upstream by the trackerclient signing
// layer, not here.
//
// Returns empty string if pub is nil or the wrong length so the
// caller's MkdirAll fails noisily rather than silently writing
// to a degenerate path.
func ClientHash(pub ed25519.PublicKey) string {
	if len(pub) != ed25519.PublicKeySize {
		return ""
	}
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// ClientHomeDir returns the per-client subdirectory of seederRoot
// where session files for this client live. The bridge runner
// uses it as both HOME and CWD for the claude subprocess, so the
// session file we write at <home>/.claude/projects/<sanitize(cwd)>/
// resolves to <seederRoot>/<clientHash>/.claude/projects/<sanitize(<seederRoot>/<clientHash>)>/.
func ClientHomeDir(seederRoot string, pub ed25519.PublicKey) string {
	return filepath.Join(seederRoot, ClientHash(pub))
}

// parseClaudeVersion extracts the leading semver-ish token from
// `claude --version` output, e.g. "2.1.138 (Claude Code)" → "2.1.138".
// Claude-code accepts any string in SerializedMessage.version on
// load, but populating it accurately keeps the file forward-
// compatible if validation is added later.
func parseClaudeVersion(out string) (string, error) {
	out = strings.TrimSpace(out)
	if out == "" {
		return "", fmt.Errorf("ccbridge: empty claude --version output")
	}
	if i := strings.IndexByte(out, ' '); i > 0 {
		return out[:i], nil
	}
	return out, nil
}
