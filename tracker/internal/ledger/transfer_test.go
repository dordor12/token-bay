package ledger

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	fed "github.com/token-bay/token-bay/shared/federation"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// Deterministic federation tracker ids for transfer-out intents. Values
// are arbitrary — the ledger only uses them to reconstruct the canonical
// intent bytes; it does not validate them against a peer registry.
var (
	testSourceTrackerID = bytes.Repeat([]byte{0xAA}, 32)
	testDestTrackerID   = bytes.Repeat([]byte{0xBB}, 32)
)

// spkiIdentityID derives the enrollment identity for pub: SHA-256 of the
// DER SubjectPublicKeyInfo — the same encoding server.SPKIToIdentityID
// hashes from the client cert's RawSubjectPublicKeyInfo.
func spkiIdentityID(t *testing.T, pub ed25519.PublicKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	require.NoError(t, err)
	sum := sha256.Sum256(der)
	return sum[:]
}

// intentFromRecord reconstructs the TransferProofRequest whose canonical
// pre-sig bytes the consumer signs — mirrors what AppendTransferOut
// rebuilds internally.
func intentFromRecord(r TransferOutRecord) *fed.TransferProofRequest {
	return &fed.TransferProofRequest{
		SourceTrackerId: r.SourceTrackerID,
		DestTrackerId:   r.DestTrackerID,
		IdentityId:      r.ConsumerID,
		Amount:          r.Amount,
		Nonce:           r.TransferRef,
		ConsumerPub:     r.ConsumerPub,
		Timestamp:       r.Timestamp,
	}
}

// signedTransferOutRecord builds a TransferOutRecord for the "consumer"
// test identity whose ConsumerSig is the consumer's Ed25519 signature
// over the canonical transfer-proof-request intent (sequencing
// independent) — matching what the federation layer holds. ConsumerID is
// the SPKI-hash identity of the consumer pubkey, so the binding check
// passes; callers fund that identity before building the record.
func signedTransferOutRecord(t *testing.T, l *Ledger, amount uint64) TransferOutRecord {
	t.Helper()
	cPub, cPriv := labeledKeypair("consumer")
	consumerID := spkiIdentityID(t, cPub)
	prev, seq := nextTipForTest(t, l)
	transferRef := bytes.Repeat([]byte{byte(seq)}, 32)

	rec := TransferOutRecord{
		PrevHash:        prev,
		Seq:             seq,
		ConsumerID:      consumerID,
		Amount:          amount,
		Timestamp:       1714000000 + seq,
		TransferRef:     transferRef,
		SourceTrackerID: testSourceTrackerID,
		DestTrackerID:   testDestTrackerID,
		ConsumerPub:     cPub,
	}
	sig, err := fed.SignTransferProofRequest(cPriv, intentFromRecord(rec))
	require.NoError(t, err)
	rec.ConsumerSig = sig
	return rec
}

func TestAppendTransferOut_VerifiesIntent(t *testing.T) {
	l := openTempLedger(t)
	ctx := context.Background()
	cPub, _ := labeledKeypair("consumer")
	consumerID := spkiIdentityID(t, cPub)

	_, err := l.IssueStarterGrant(ctx, consumerID, 5000)
	require.NoError(t, err)

	rec := signedTransferOutRecord(t, l, 1000)
	e, err := l.AppendTransferOut(ctx, rec)
	require.NoError(t, err)
	require.NotNil(t, e)

	assert.Equal(t, tbproto.EntryKind_ENTRY_KIND_TRANSFER_OUT, e.Body.Kind)
	assert.Equal(t, uint64(2), e.Body.Seq)
	assert.Equal(t, rec.TransferRef, e.Body.Ref, "entry ref is the transfer nonce")
	assert.Equal(t, rec.ConsumerSig, e.ConsumerSig, "intent-domain sig stored verbatim")
	assert.Empty(t, e.SeederSig, "transfer_out has no seeder")
	assert.NotEmpty(t, e.TrackerSig)

	bal, ok, err := l.store.Balance(ctx, consumerID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(4000), bal.Credits)
}

func TestAppendTransferOut_StaleTipRetriesWithSameIntentSig(t *testing.T) {
	l := openTempLedger(t)
	ctx := context.Background()
	cPub, _ := labeledKeypair("consumer")
	consumerID := spkiIdentityID(t, cPub)

	_, err := l.IssueStarterGrant(ctx, consumerID, 5000)
	require.NoError(t, err)

	rec := signedTransferOutRecord(t, l, 1000)

	// Advance tip with another append.
	otherID := bytes.Repeat([]byte{0x22}, 32)
	_, err = l.IssueStarterGrant(ctx, otherID, 100)
	require.NoError(t, err)

	_, err = l.AppendTransferOut(ctx, rec)
	require.ErrorIs(t, err, ErrStaleTip)

	// The consumer sig covers the sequencing-independent intent, so a
	// refresh of (prev_hash, seq) retries with the SAME sig.
	rec.PrevHash, rec.Seq = nextTipForTest(t, l)
	e, err := l.AppendTransferOut(ctx, rec)
	require.NoError(t, err)
	assert.Equal(t, rec.ConsumerSig, e.ConsumerSig)

	bal, ok, err := l.store.Balance(ctx, consumerID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(4000), bal.Credits)
}

func TestAppendTransferOut_IdentityMismatch(t *testing.T) {
	l := openTempLedger(t)
	ctx := context.Background()

	// The victim identity is funded but is NOT the SPKI hash of the
	// attacker's pubkey — the binding check must refuse the debit even
	// though the intent sig itself is valid for the attacker's key.
	victimID := bytes.Repeat([]byte{0x11}, 32)
	_, err := l.IssueStarterGrant(ctx, victimID, 5000)
	require.NoError(t, err)

	cPub, cPriv := labeledKeypair("attacker")
	prev, seq := nextTipForTest(t, l)
	rec := TransferOutRecord{
		PrevHash:        prev,
		Seq:             seq,
		ConsumerID:      victimID,
		Amount:          1000,
		Timestamp:       1714000001,
		TransferRef:     bytes.Repeat([]byte{0x42}, 32),
		SourceTrackerID: testSourceTrackerID,
		DestTrackerID:   testDestTrackerID,
		ConsumerPub:     cPub,
	}
	rec.ConsumerSig, err = fed.SignTransferProofRequest(cPriv, intentFromRecord(rec))
	require.NoError(t, err)

	_, err = l.AppendTransferOut(ctx, rec)
	require.ErrorIs(t, err, ErrTransferIdentityMismatch)

	// No partial application: no entry appended, victim not debited.
	tipSeq, _, hasTip, err := l.Tip(ctx)
	require.NoError(t, err)
	require.True(t, hasTip)
	assert.Equal(t, uint64(1), tipSeq, "only the starter grant is on chain")

	bal, ok, err := l.store.Balance(ctx, victimID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(5000), bal.Credits)
}

func TestAppendTransferOut_RejectsBadConsumerSig(t *testing.T) {
	l := openTempLedger(t)
	ctx := context.Background()
	cPub, _ := labeledKeypair("consumer")
	consumerID := spkiIdentityID(t, cPub)
	_, err := l.IssueStarterGrant(ctx, consumerID, 5000)
	require.NoError(t, err)

	rec := signedTransferOutRecord(t, l, 1000)

	// (a) Intent signed by a different key than ConsumerPub.
	_, attackerPriv := labeledKeypair("attacker")
	attackerSigned := rec
	attackerSigned.ConsumerSig, err = fed.SignTransferProofRequest(attackerPriv, intentFromRecord(rec))
	require.NoError(t, err)
	_, err = l.AppendTransferOut(ctx, attackerSigned)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "consumer_sig invalid")

	// (b) Tampered intent sig.
	tampered := rec
	tampered.ConsumerSig = bytes.Clone(rec.ConsumerSig)
	tampered.ConsumerSig[0] ^= 0x01
	_, err = l.AppendTransferOut(ctx, tampered)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "consumer_sig invalid")

	// (c) Valid sig over a DIFFERENT intent (amount tampered post-sign).
	inflated := rec
	inflated.Amount = 4999
	_, err = l.AppendTransferOut(ctx, inflated)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "consumer_sig invalid")

	// No entry landed; consumer not debited.
	tipSeq, _, _, err := l.Tip(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), tipSeq)
	bal, ok, err := l.store.Balance(ctx, consumerID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(5000), bal.Credits)
}

func TestAppendTransferOut_RejectsZeroAmount(t *testing.T) {
	l := openTempLedger(t)
	cPub, _ := labeledKeypair("consumer")
	_, err := l.AppendTransferOut(context.Background(), TransferOutRecord{
		PrevHash:        make([]byte, 32),
		Seq:             1,
		ConsumerID:      spkiIdentityID(t, cPub),
		Amount:          0,
		Timestamp:       1714000000,
		TransferRef:     bytes.Repeat([]byte{0x77}, 32),
		SourceTrackerID: testSourceTrackerID,
		DestTrackerID:   testDestTrackerID,
		ConsumerSig:     bytes.Repeat([]byte{0x55}, 64),
		ConsumerPub:     cPub,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "amount must be > 0")
}

func TestAppendTransferOut_RejectsMissingConsumerSig(t *testing.T) {
	l := openTempLedger(t)
	_, err := l.AppendTransferOut(context.Background(), TransferOutRecord{
		PrevHash:        make([]byte, 32),
		Seq:             1,
		ConsumerID:      bytes.Repeat([]byte{0x11}, 32),
		Amount:          1000,
		Timestamp:       1714000000,
		TransferRef:     bytes.Repeat([]byte{0x77}, 32),
		SourceTrackerID: testSourceTrackerID,
		DestTrackerID:   testDestTrackerID,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "consumer sig + pubkey")
}

func TestAppendTransferOut_DuplicateRefRejected(t *testing.T) {
	l := openTempLedger(t)
	ctx := context.Background()
	cPub, _ := labeledKeypair("consumer")
	consumerID := spkiIdentityID(t, cPub)

	_, err := l.IssueStarterGrant(ctx, consumerID, 5000)
	require.NoError(t, err)

	rec := signedTransferOutRecord(t, l, 1000)
	_, err = l.AppendTransferOut(ctx, rec)
	require.NoError(t, err)

	// Replay the SAME signed intent with a refreshed (prev, seq). The
	// intent sig is sequencing-independent, so it still verifies — this
	// is exactly the double-debit vector the on-chain ref-uniqueness
	// check closes. The in-memory federation cache is NOT a defense
	// (lost on restart, check-then-act race).
	dup := rec
	dup.PrevHash, dup.Seq = nextTipForTest(t, l)
	_, err = l.AppendTransferOut(ctx, dup)
	require.ErrorIs(t, err, ErrTransferRefExists)

	// Debit applied exactly ONCE: 5000 - 1000, and only one transfer_out
	// on chain (grant at seq 1, transfer_out at seq 2).
	bal, ok, err := l.store.Balance(ctx, consumerID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(4000), bal.Credits, "duplicate ref must NOT double-debit")

	tipSeq, _, hasTip, err := l.Tip(ctx)
	require.NoError(t, err)
	require.True(t, hasTip)
	assert.Equal(t, uint64(2), tipSeq, "second transfer_out must not land")

	// Kind scoping: a transfer_in reusing the same ref (refund / mirrored
	// entry shape) is NOT a duplicate transfer_out and must append fine.
	prev, seq := nextTipForTest(t, l)
	_, err = l.AppendTransferIn(ctx, TransferInRecord{
		PrevHash:    prev,
		Seq:         seq,
		IdentityID:  consumerID,
		Amount:      1000,
		Timestamp:   1714000100,
		TransferRef: rec.TransferRef,
	})
	require.NoError(t, err, "TRANSFER_IN with the same ref is legitimate")
}

func TestAppendTransferOut_InsufficientBalance(t *testing.T) {
	l := openTempLedger(t)
	ctx := context.Background()
	// No starter grant — consumer has zero credits.

	rec := signedTransferOutRecord(t, l, 1000)
	_, err := l.AppendTransferOut(ctx, rec)
	require.ErrorIs(t, err, ErrInsufficientBalance)

	// No partial application: nothing appended.
	_, _, hasTip, err := l.Tip(ctx)
	require.NoError(t, err)
	assert.False(t, hasTip, "no entry must land on an insufficient-balance debit")
}

func TestAppendTransferIn_HappyPath(t *testing.T) {
	l := openTempLedger(t)
	ctx := context.Background()
	identityID := bytes.Repeat([]byte{0x33}, 32)
	transferRef := bytes.Repeat([]byte{0x44}, 32)

	prev, seq := nextTipForTest(t, l)

	e, err := l.AppendTransferIn(ctx, TransferInRecord{
		PrevHash:    prev,
		Seq:         seq,
		IdentityID:  identityID,
		Amount:      1500,
		Timestamp:   1714000000 + seq,
		TransferRef: transferRef,
	})
	require.NoError(t, err)
	require.NotNil(t, e)

	assert.Equal(t, tbproto.EntryKind_ENTRY_KIND_TRANSFER_IN, e.Body.Kind)
	assert.Empty(t, e.ConsumerSig, "transfer_in has no consumer sig")
	assert.Empty(t, e.SeederSig, "transfer_in has no seeder sig")
	assert.NotEmpty(t, e.TrackerSig)

	bal, ok, err := l.store.Balance(ctx, identityID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(1500), bal.Credits)
}

func TestAppendTransferIn_RejectsZeroAmount(t *testing.T) {
	l := openTempLedger(t)
	prev, seq := nextTipForTest(t, l)
	_, err := l.AppendTransferIn(context.Background(), TransferInRecord{
		PrevHash:    prev,
		Seq:         seq,
		IdentityID:  bytes.Repeat([]byte{0x33}, 32),
		Amount:      0,
		Timestamp:   1714000000,
		TransferRef: bytes.Repeat([]byte{0x44}, 32),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "amount must be > 0")
}

func TestAppendTransferIn_StaleTip(t *testing.T) {
	l := openTempLedger(t)
	ctx := context.Background()
	identityID := bytes.Repeat([]byte{0x33}, 32)

	prev, seq := nextTipForTest(t, l)
	rec := TransferInRecord{
		PrevHash:    prev,
		Seq:         seq,
		IdentityID:  identityID,
		Amount:      100,
		Timestamp:   1714000000,
		TransferRef: bytes.Repeat([]byte{0x44}, 32),
	}
	other := bytes.Repeat([]byte{0x22}, 32)
	_, err := l.IssueStarterGrant(ctx, other, 50)
	require.NoError(t, err)

	_, err = l.AppendTransferIn(ctx, rec)
	require.ErrorIs(t, err, ErrStaleTip)
}
