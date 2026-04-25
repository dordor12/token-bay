package entry

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tbproto "github.com/token-bay/token-bay/shared/proto"
)

func b32(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }
func b16(b byte) []byte { return bytes.Repeat([]byte{b}, 16) }

func allZeroBytes(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

func TestBuildUsageEntry_Valid(t *testing.T) {
	body, err := BuildUsageEntry(UsageInput{
		PrevHash:     b32(0x01),
		Seq:          42,
		ConsumerID:   b32(0x11),
		SeederID:     b32(0x22),
		Model:        "claude-sonnet-4-6",
		InputTokens:  4096,
		OutputTokens: 1024,
		CostCredits:  3072000,
		Timestamp:    1714000100,
		RequestID:    b16(0x33),
	})
	require.NoError(t, err)
	require.NoError(t, tbproto.ValidateEntryBody(body))
	assert.Equal(t, tbproto.EntryKind_ENTRY_KIND_USAGE, body.Kind)
	assert.Equal(t, uint32(0), body.Flags)
	assert.True(t, allZeroBytes(body.Ref))
}

func TestBuildUsageEntry_WithConsumerSigMissingFlag(t *testing.T) {
	body, err := BuildUsageEntry(UsageInput{
		PrevHash:           b32(0x01),
		Seq:                42,
		ConsumerID:         b32(0x11),
		SeederID:           b32(0x22),
		Model:              "claude-sonnet-4-6",
		Timestamp:          1714000100,
		RequestID:          b16(0x33),
		ConsumerSigMissing: true,
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(1), body.Flags)
}

func TestBuildUsageEntry_RejectsEmptyModel(t *testing.T) {
	_, err := BuildUsageEntry(UsageInput{
		PrevHash: b32(0x01), Seq: 42,
		ConsumerID: b32(0x11), SeederID: b32(0x22),
		Model: "", Timestamp: 1714000100, RequestID: b16(0x33),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "model")
}

func TestBuildUsageEntry_RejectsBadHash(t *testing.T) {
	_, err := BuildUsageEntry(UsageInput{
		PrevHash:   make([]byte, 16),
		Seq:        42,
		ConsumerID: b32(0x11),
		SeederID:   b32(0x22),
		Model:      "claude-sonnet-4-6",
		Timestamp:  1714000100,
		RequestID:  b16(0x33),
	})
	require.Error(t, err)
}

func TestBuildTransferOutEntry_Valid(t *testing.T) {
	body, err := BuildTransferOutEntry(TransferOutInput{
		PrevHash:    b32(0x01),
		Seq:         43,
		ConsumerID:  b32(0x11),
		Amount:      1000,
		Timestamp:   1714000200,
		TransferRef: b32(0x77),
	})
	require.NoError(t, err)
	require.NoError(t, tbproto.ValidateEntryBody(body))
	assert.Equal(t, tbproto.EntryKind_ENTRY_KIND_TRANSFER_OUT, body.Kind)
	assert.Empty(t, body.Model)
	assert.True(t, allZeroBytes(body.SeederId))
	assert.True(t, allZeroBytes(body.RequestId))
}

func TestBuildTransferOutEntry_RejectsBadConsumer(t *testing.T) {
	_, err := BuildTransferOutEntry(TransferOutInput{
		PrevHash:    b32(0x01),
		Seq:         43,
		ConsumerID:  make([]byte, 16),
		Amount:      1000,
		Timestamp:   1714000200,
		TransferRef: b32(0x77),
	})
	require.Error(t, err)
}

func TestBuildTransferInEntry_Valid(t *testing.T) {
	body, err := BuildTransferInEntry(TransferInInput{
		PrevHash:    b32(0x01),
		Seq:         44,
		Amount:      1000,
		Timestamp:   1714000300,
		TransferRef: b32(0x88),
	})
	require.NoError(t, err)
	require.NoError(t, tbproto.ValidateEntryBody(body))
	assert.True(t, allZeroBytes(body.ConsumerId))
	assert.True(t, allZeroBytes(body.SeederId))
	assert.True(t, allZeroBytes(body.RequestId))
}

func TestBuildTransferInEntry_RejectsBadTransferRef(t *testing.T) {
	_, err := BuildTransferInEntry(TransferInInput{
		PrevHash:    b32(0x01),
		Seq:         44,
		Amount:      1000,
		Timestamp:   1714000300,
		TransferRef: make([]byte, 16),
	})
	require.Error(t, err)
}

func TestBuildStarterGrantEntry_Valid(t *testing.T) {
	body, err := BuildStarterGrantEntry(StarterGrantInput{
		PrevHash:   b32(0x01),
		Seq:        1,
		ConsumerID: b32(0x11),
		Amount:     100000,
		Timestamp:  1714000000,
	})
	require.NoError(t, err)
	require.NoError(t, tbproto.ValidateEntryBody(body))
	assert.True(t, allZeroBytes(body.SeederId))
	assert.True(t, allZeroBytes(body.RequestId))
	assert.True(t, allZeroBytes(body.Ref))
}

func TestBuildStarterGrantEntry_RejectsBadConsumer(t *testing.T) {
	_, err := BuildStarterGrantEntry(StarterGrantInput{
		PrevHash:   b32(0x01),
		Seq:        1,
		ConsumerID: make([]byte, 16),
		Amount:     100000,
		Timestamp:  1714000000,
	})
	require.Error(t, err)
}
