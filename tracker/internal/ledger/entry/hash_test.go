package entry

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// usageBodyFixture mirrors shared/proto's fixtureUsageEntryBody so the
// hash here and the golden-bytes test there exercise the same shape.
// Duplication is intentional: tracker tests don't import shared/proto's
// _test.go fixtures.
func usageBodyFixture() *tbproto.EntryBody {
	return &tbproto.EntryBody{
		PrevHash:     bytes.Repeat([]byte{0x01}, 32),
		Seq:          42,
		Kind:         tbproto.EntryKind_ENTRY_KIND_USAGE,
		ConsumerId:   bytes.Repeat([]byte{0x11}, 32),
		SeederId:     bytes.Repeat([]byte{0x22}, 32),
		Model:        "claude-sonnet-4-6",
		InputTokens:  4096,
		OutputTokens: 1024,
		CostCredits:  3072000,
		Timestamp:    1714000100,
		RequestId:    bytes.Repeat([]byte{0x33}, 16),
		Flags:        0,
		Ref:          make([]byte, 32),
	}
}

func TestHash_MatchesSHA256OfDeterministicMarshal(t *testing.T) {
	body := usageBodyFixture()

	got, err := Hash(body)
	require.NoError(t, err)

	wantBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(body)
	require.NoError(t, err)
	want := sha256.Sum256(wantBytes)

	assert.Equal(t, want, got)
}

func TestHash_StableAcrossRepeatedCalls(t *testing.T) {
	body := usageBodyFixture()
	a, err := Hash(body)
	require.NoError(t, err)
	b, err := Hash(body)
	require.NoError(t, err)
	assert.Equal(t, a, b)
}

func TestHash_DiffersOnFieldChange(t *testing.T) {
	a, err := Hash(usageBodyFixture())
	require.NoError(t, err)

	mutated := usageBodyFixture()
	mutated.CostCredits++
	b, err := Hash(mutated)
	require.NoError(t, err)

	assert.NotEqual(t, a, b)
}

func TestHash_NilReturnsError(t *testing.T) {
	_, err := Hash(nil)
	require.Error(t, err)
}

// TestHash_MarshalError covers the err != nil branch when DeterministicMarshal
// fails (invalid UTF-8 in a proto3 string field).
func TestHash_MarshalError(t *testing.T) {
	bad := &tbproto.EntryBody{Model: "\xff\xfe"}
	_, err := Hash(bad)
	require.Error(t, err)
}

const usageEntryGoldenPath = "testdata/usage_entry.golden.hex"

func TestHash_GoldenBytes(t *testing.T) {
	got, err := Hash(usageBodyFixture())
	require.NoError(t, err)
	gotHex := hex.EncodeToString(got[:])

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		require.NoError(t, os.MkdirAll(filepath.Dir(usageEntryGoldenPath), 0o755))
		require.NoError(t, os.WriteFile(usageEntryGoldenPath, []byte(gotHex+"\n"), 0o644))
		return
	}

	wantBytes, err := os.ReadFile(usageEntryGoldenPath)
	require.NoError(t, err, "missing golden — run: UPDATE_GOLDEN=1 go test ./internal/ledger/entry/... -run TestHash_GoldenBytes")
	wantHex := strings.TrimSpace(string(wantBytes))
	assert.Equal(t, wantHex, gotHex,
		"entry hash differs from golden. If wire schema or marshal lib changed, regenerate with UPDATE_GOLDEN=1.")
}
