package ccbridge

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionRecord_UserText_MarshalsToClaudeShape(t *testing.T) {
	rec := SessionRecord{
		Type:        "user",
		UUID:        "11111111-1111-1111-1111-111111111111",
		ParentUUID:  nil, // first record
		IsSidechain: false,
		Timestamp:   "2026-05-09T15:30:00.000Z",
		CWD:         "/tmp/seeder-cwd",
		UserType:    "external",
		Entrypoint:  "cli",
		SessionID:   "22222222-2222-2222-2222-222222222222",
		Version:     "2.1.138",
		Message: SessionMessage{
			Role:    "user",
			Content: json.RawMessage(`[{"type":"text","text":"hello"}]`),
		},
	}
	b, err := json.Marshal(rec)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"type":"user",
		"uuid":"11111111-1111-1111-1111-111111111111",
		"parentUuid":null,
		"isSidechain":false,
		"timestamp":"2026-05-09T15:30:00.000Z",
		"cwd":"/tmp/seeder-cwd",
		"userType":"external",
		"entrypoint":"cli",
		"sessionId":"22222222-2222-2222-2222-222222222222",
		"version":"2.1.138",
		"message":{"role":"user","content":[{"type":"text","text":"hello"}]}
	}`, string(b))
}

func TestSanitizePath_MatchesClaudeCodeAlgorithm(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"unix path", "/Users/dor.amid/foo", "-Users-dor-amid-foo"},
		{"colon-separated", "plugin:name:server", "plugin-name-server"},
		{"trailing slash", "/tmp/work/", "-tmp-work-"},
		{"alphanumeric only", "ABC123", "ABC123"},
		{"leading dot", ".hidden", "-hidden"},
		{"windows-style", `C:\Users\foo`, "C--Users-foo"},
		{"unicode falls to dashes", "naïve/path", "na--ve-path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, SanitizePath(tc.in))
		})
	}
}

func TestSanitizePath_LongInput_TruncatesWithHashSuffix(t *testing.T) {
	// 250 'a's; sanitized form is the same length, then truncated to
	// 200 + "-<hash>" suffix. Hash is hex-encoded SHA-256 prefix.
	in := strings.Repeat("a", 250)
	got := SanitizePath(in)
	assert.Len(t, got[:200], 200)
	assert.Contains(t, got, "-")
	assert.Greater(t, len(got), 200)
	assert.Less(t, len(got), 220)
}

func TestGenerateSessionID_ValidUUIDv4(t *testing.T) {
	id, err := GenerateSessionID()
	require.NoError(t, err)
	// 8-4-4-4-12 hex with v4 marker: 14th hex digit is "4",
	// 19th hex digit is one of "8","9","a","b" (variant 10xx).
	assert.Regexp(t, `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`, id)

	// Two consecutive IDs must differ.
	id2, err := GenerateSessionID()
	require.NoError(t, err)
	assert.NotEqual(t, id, id2)
}

func TestWriteSessionFile_WritesValidJSONLAtCorrectPath(t *testing.T) {
	home := t.TempDir()
	cwd := "/tmp/test-cwd-aaa"
	sessionID, err := GenerateSessionID()
	require.NoError(t, err)

	msgs := []Message{
		{Role: RoleUser, Content: TextContent("hello")},
		{Role: RoleAssistant, Content: TextContent("hi there")},
		{Role: RoleUser, Content: TextContent("what's 2+2?")},
	}

	path, err := WriteSessionFile(home, cwd, sessionID, "2.1.138", msgs)
	require.NoError(t, err)
	wantPath := filepath.Join(home, ".claude", "projects", SanitizePath(cwd), sessionID+".jsonl")
	assert.Equal(t, wantPath, path)

	body, err := os.ReadFile(path)
	require.NoError(t, err)

	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	require.Len(t, lines, 3, "one record per Message")

	var recs [3]SessionRecord
	for i, line := range lines {
		require.NoError(t, json.Unmarshal([]byte(line), &recs[i]), "line %d: %s", i, line)
	}

	// Roles match input.
	assert.Equal(t, "user", recs[0].Type)
	assert.Equal(t, "assistant", recs[1].Type)
	assert.Equal(t, "user", recs[2].Type)

	// First parentUuid is null; subsequent point to previous uuid.
	assert.Nil(t, recs[0].ParentUUID)
	require.NotNil(t, recs[1].ParentUUID)
	assert.Equal(t, recs[0].UUID, *recs[1].ParentUUID)
	require.NotNil(t, recs[2].ParentUUID)
	assert.Equal(t, recs[1].UUID, *recs[2].ParentUUID)

	// Required metadata is populated on every record.
	for i, r := range recs {
		assert.NotEmpty(t, r.UUID, "record %d uuid", i)
		assert.NotEmpty(t, r.Timestamp, "record %d timestamp", i)
		assert.Equal(t, sessionID, r.SessionID, "record %d sessionId", i)
		assert.Equal(t, cwd, r.CWD, "record %d cwd", i)
		assert.Equal(t, "external", r.UserType, "record %d userType", i)
		assert.Equal(t, "cli", r.Entrypoint, "record %d entrypoint", i)
		assert.Equal(t, "2.1.138", r.Version, "record %d version", i)
	}
}

func TestWriteSessionFile_RejectsInvalidRoleOrEmptyContent(t *testing.T) {
	home := t.TempDir()
	id, _ := GenerateSessionID()

	// Empty messages: error.
	_, err := WriteSessionFile(home, "/x", id, "2.1.138", nil)
	require.Error(t, err)

	// Bad role: error.
	_, err = WriteSessionFile(home, "/x", id, "2.1.138", []Message{
		{Role: "system", Content: TextContent("nope")},
	})
	require.Error(t, err)

	// Empty content: error.
	_, err = WriteSessionFile(home, "/x", id, "2.1.138", []Message{
		{Role: RoleUser, Content: nil},
	})
	require.Error(t, err)
}

func TestParseClaudeVersion_ExtractsSemverPrefix(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"haiku style", "2.1.138 (Claude Code)", "2.1.138"},
		{"trailing newline", "2.1.0\n", "2.1.0"},
		{"with patch", "10.20.30-rc1 (Claude Code)", "10.20.30-rc1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseClaudeVersion(tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestParseClaudeVersion_RejectsEmpty(t *testing.T) {
	_, err := parseClaudeVersion("")
	require.Error(t, err)
}

func TestClientHash_StableAndShort(t *testing.T) {
	keyA := ed25519.PublicKey(bytes.Repeat([]byte{0x11}, ed25519.PublicKeySize))
	keyB := ed25519.PublicKey(bytes.Repeat([]byte{0x22}, ed25519.PublicKeySize))

	hashA := ClientHash(keyA)
	hashB := ClientHash(keyB)

	assert.Len(t, hashA, 16)
	assert.Regexp(t, `^[0-9a-f]{16}$`, hashA)
	assert.NotEqual(t, hashA, hashB)
	assert.Equal(t, hashA, ClientHash(keyA)) // stable
}

func TestClientHash_RejectsWrongLength(t *testing.T) {
	assert.Equal(t, "", ClientHash(nil))
	assert.Equal(t, "", ClientHash(ed25519.PublicKey{0x01, 0x02}))
}

func TestClientHomeDir_BuildsExpectedPath(t *testing.T) {
	key := ed25519.PublicKey(bytes.Repeat([]byte{0x42}, ed25519.PublicKeySize))
	got := ClientHomeDir("/srv/seeder", key)
	want := filepath.Join("/srv/seeder", ClientHash(key))
	assert.Equal(t, want, got)
}
