package ledger

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
	"github.com/token-bay/token-bay/tracker/internal/ledger/entry"
)

func signedTransferOutRecord(t *testing.T, l *Ledger, consumerID []byte, amount uint64) (TransferOutRecord, *tbproto.EntryBody) {
	t.Helper()
	prev, seq := nextTipForTest(t, l)
	transferRef := bytes.Repeat([]byte{byte(seq)}, 32)
	cPub, cPriv := labeledKeypair("consumer")

	body, err := entry.BuildTransferOutEntry(entry.TransferOutInput{
		PrevHash:    prev,
		Seq:         seq,
		ConsumerID:  consumerID,
		Amount:      amount,
		Timestamp:   1714000000 + seq,
		TransferRef: transferRef,
	})
	require.NoError(t, err)

	sig, err := signing.SignEntry(cPriv, body)
	require.NoError(t, err)

	return TransferOutRecord{
		PrevHash:    prev,
		Seq:         seq,
		ConsumerID:  consumerID,
		Amount:      amount,
		Timestamp:   1714000000 + seq,
		TransferRef: transferRef,
		ConsumerSig: sig,
		ConsumerPub: cPub,
	}, body
}

func TestAppendTransferOut_HappyPath(t *testing.T) {
	l := openTempLedger(t)
	ctx := context.Background()
	consumerID := bytes.Repeat([]byte{0x11}, 32)

	_, err := l.IssueStarterGrant(ctx, consumerID, 5000)
	require.NoError(t, err)

	rec, _ := signedTransferOutRecord(t, l, consumerID, 1000)
	e, err := l.AppendTransferOut(ctx, rec)
	require.NoError(t, err)
	require.NotNil(t, e)

	assert.Equal(t, tbproto.EntryKind_ENTRY_KIND_TRANSFER_OUT, e.Body.Kind)
	assert.Equal(t, uint64(2), e.Body.Seq)
	assert.NotEmpty(t, e.ConsumerSig)
	assert.Empty(t, e.SeederSig, "transfer_out has no seeder")
	assert.NotEmpty(t, e.TrackerSig)

	bal, ok, err := l.store.Balance(ctx, consumerID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(4000), bal.Credits)
}

func TestAppendTransferOut_StaleTip(t *testing.T) {
	l := openTempLedger(t)
	ctx := context.Background()
	consumerID := bytes.Repeat([]byte{0x11}, 32)

	_, err := l.IssueStarterGrant(ctx, consumerID, 5000)
	require.NoError(t, err)

	rec, _ := signedTransferOutRecord(t, l, consumerID, 1000)

	// Advance tip with another append.
	otherID := bytes.Repeat([]byte{0x22}, 32)
	_, err = l.IssueStarterGrant(ctx, otherID, 100)
	require.NoError(t, err)

	_, err = l.AppendTransferOut(ctx, rec)
	require.ErrorIs(t, err, ErrStaleTip)
}

func TestAppendTransferOut_RejectsBadConsumerSig(t *testing.T) {
	l := openTempLedger(t)
	ctx := context.Background()
	consumerID := bytes.Repeat([]byte{0x11}, 32)
	_, err := l.IssueStarterGrant(ctx, consumerID, 5000)
	require.NoError(t, err)

	cPub, _ := labeledKeypair("consumer")
	_, otherPriv := labeledKeypair("attacker")
	prev, seq := nextTipForTest(t, l)

	body, err := entry.BuildTransferOutEntry(entry.TransferOutInput{
		PrevHash: prev, Seq: seq,
		ConsumerID:  consumerID,
		Amount:      1000,
		Timestamp:   1714000000,
		TransferRef: bytes.Repeat([]byte{0x77}, 32),
	})
	require.NoError(t, err)
	badSig, err := signing.SignEntry(otherPriv, body)
	require.NoError(t, err)

	rec := TransferOutRecord{
		PrevHash: prev, Seq: seq,
		ConsumerID: consumerID, Amount: 1000,
		Timestamp:   1714000000,
		TransferRef: bytes.Repeat([]byte{0x77}, 32),
		ConsumerSig: badSig, ConsumerPub: cPub,
	}
	_, err = l.AppendTransferOut(ctx, rec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "consumer_sig invalid")
}

func TestAppendTransferOut_RejectsZeroAmount(t *testing.T) {
	l := openTempLedger(t)
	cPub, _ := labeledKeypair("consumer")
	_, err := l.AppendTransferOut(context.Background(), TransferOutRecord{
		PrevHash:    make([]byte, 32),
		Seq:         1,
		ConsumerID:  bytes.Repeat([]byte{0x11}, 32),
		Amount:      0,
		Timestamp:   1714000000,
		TransferRef: bytes.Repeat([]byte{0x77}, 32),
		ConsumerSig: bytes.Repeat([]byte{0x55}, 64),
		ConsumerPub: cPub,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "amount must be > 0")
}

func TestAppendTransferOut_RejectsMissingConsumerSig(t *testing.T) {
	l := openTempLedger(t)
	_, err := l.AppendTransferOut(context.Background(), TransferOutRecord{
		PrevHash:    make([]byte, 32),
		Seq:         1,
		ConsumerID:  bytes.Repeat([]byte{0x11}, 32),
		Amount:      1000,
		Timestamp:   1714000000,
		TransferRef: bytes.Repeat([]byte{0x77}, 32),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "consumer sig + pubkey")
}

func TestAppendTransferOut_InsufficientBalance(t *testing.T) {
	l := openTempLedger(t)
	ctx := context.Background()
	consumerID := bytes.Repeat([]byte{0x11}, 32)
	// No starter grant — consumer has zero credits.

	rec, _ := signedTransferOutRecord(t, l, consumerID, 1000)
	_, err := l.AppendTransferOut(ctx, rec)
	require.ErrorIs(t, err, ErrInsufficientBalance)
}
