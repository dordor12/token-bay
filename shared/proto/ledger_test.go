package proto

import (
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
