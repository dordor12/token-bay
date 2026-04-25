package proto

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// fixtureUsageEntryBody returns a fully-populated USAGE-kind body — used by
// round-trip, validator, sign/verify, and golden-fixture tests across the
// shared/ packages.
func fixtureUsageEntryBody() *EntryBody {
	return &EntryBody{
		PrevHash:     make32(0x01),
		Seq:          42,
		Kind:         EntryKind_ENTRY_KIND_USAGE,
		ConsumerId:   make32(0x11),
		SeederId:     make32(0x22),
		Model:        "claude-sonnet-4-6",
		InputTokens:  4096,
		OutputTokens: 1024,
		CostCredits:  3072000,
		Timestamp:    1714000100,
		RequestId:    make16(0x33),
		Flags:        0,
		Ref:          make([]byte, 32),
	}
}

func fixtureTransferOutEntryBody() *EntryBody {
	return &EntryBody{
		PrevHash:    make32(0x01),
		Seq:         43,
		Kind:        EntryKind_ENTRY_KIND_TRANSFER_OUT,
		ConsumerId:  make32(0x11),
		SeederId:    make([]byte, 32),
		Model:       "",
		Timestamp:   1714000200,
		RequestId:   make([]byte, 16),
		Ref:         make32(0x77),
		CostCredits: 1000,
	}
}

func fixtureTransferInEntryBody() *EntryBody {
	return &EntryBody{
		PrevHash:    make32(0x01),
		Seq:         44,
		Kind:        EntryKind_ENTRY_KIND_TRANSFER_IN,
		ConsumerId:  make([]byte, 32),
		SeederId:    make([]byte, 32),
		Model:       "",
		Timestamp:   1714000300,
		RequestId:   make([]byte, 16),
		Ref:         make32(0x88),
		CostCredits: 1000,
	}
}

func fixtureStarterGrantEntryBody() *EntryBody {
	return &EntryBody{
		PrevHash:    make32(0x01),
		Seq:         1,
		Kind:        EntryKind_ENTRY_KIND_STARTER_GRANT,
		ConsumerId:  make32(0x11),
		SeederId:    make([]byte, 32),
		Model:       "",
		Timestamp:   1714000000,
		RequestId:   make([]byte, 16),
		Ref:         make([]byte, 32),
		CostCredits: 100000,
	}
}

func TestEntryBody_RoundTrip(t *testing.T) {
	original := fixtureUsageEntryBody()

	first, err := proto.MarshalOptions{Deterministic: true}.Marshal(original)
	require.NoError(t, err)

	var parsed EntryBody
	require.NoError(t, proto.Unmarshal(first, &parsed))

	second, err := proto.MarshalOptions{Deterministic: true}.Marshal(&parsed)
	require.NoError(t, err)

	assert.Equal(t, first, second, "Deterministic marshal must be byte-stable")
	require.True(t, proto.Equal(original, &parsed), "unmarshal must reproduce original exactly")
}

func TestEntry_RoundTrip(t *testing.T) {
	signed := &Entry{
		Body:        fixtureUsageEntryBody(),
		ConsumerSig: make64(0x44),
		SeederSig:   make64(0x55),
		TrackerSig:  make64(0x66),
	}

	first, err := proto.MarshalOptions{Deterministic: true}.Marshal(signed)
	require.NoError(t, err)

	var parsed Entry
	require.NoError(t, proto.Unmarshal(first, &parsed))

	second, err := proto.MarshalOptions{Deterministic: true}.Marshal(&parsed)
	require.NoError(t, err)

	assert.Equal(t, first, second)
	require.True(t, proto.Equal(signed, &parsed), "unmarshal must reproduce original exactly")
}

const ledgerGoldenPath = "testdata/entry_signed.golden.hex"

// derivedKey produces a deterministic Ed25519 key from a base seed and a
// label. Used only by the golden fixture to populate three distinct sig
// slots without committing extra seeds.
func derivedKey(baseSeed []byte, label string) ed25519.PrivateKey {
	h := sha256.Sum256(append(append([]byte{}, baseSeed...), []byte(label)...))
	return ed25519.NewKeyFromSeed(h[:])
}

// TestEntry_GoldenBytes pins the v1 wire bytes for a fully-signed USAGE
// entry. Any unintended change to schema, codegen library, or
// DeterministicMarshal trips this test. Regenerate with UPDATE_GOLDEN=1
// and review the diff manually.
func TestEntry_GoldenBytes(t *testing.T) {
	body := fixtureUsageEntryBody()

	bodyBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(body)
	require.NoError(t, err)

	baseSeed := []byte("token-bay-fixture-seed-v1-000000") // 32 bytes
	consumerPriv := derivedKey(baseSeed, "consumer")
	seederPriv := derivedKey(baseSeed, "seeder")
	trackerPriv := derivedKey(baseSeed, "tracker")

	signed := &Entry{
		Body:        body,
		ConsumerSig: ed25519.Sign(consumerPriv, bodyBytes),
		SeederSig:   ed25519.Sign(seederPriv, bodyBytes),
		TrackerSig:  ed25519.Sign(trackerPriv, bodyBytes),
	}
	wireBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(signed)
	require.NoError(t, err)

	gotHex := hex.EncodeToString(wireBytes)

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		require.NoError(t, os.MkdirAll(filepath.Dir(ledgerGoldenPath), 0o755))
		require.NoError(t, os.WriteFile(ledgerGoldenPath, []byte(gotHex+"\n"), 0o644))
		t.Logf("golden updated at %s", ledgerGoldenPath)
		return
	}

	wantBytes, err := os.ReadFile(ledgerGoldenPath)
	require.NoError(t, err, "missing golden file — run: UPDATE_GOLDEN=1 go test ./proto/... -run TestEntry_GoldenBytes")
	wantHex := strings.TrimSpace(string(wantBytes))

	assert.Equal(t, wantHex, gotHex,
		"Entry wire bytes differ from golden. If schema/lib intentionally changed, "+
			"regenerate via UPDATE_GOLDEN=1 and review the diff manually.")
}
