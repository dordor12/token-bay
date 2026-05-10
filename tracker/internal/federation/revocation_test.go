package federation

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
	"google.golang.org/protobuf/proto"
)

// fakeRevocationArchive is a thread-safe in-memory PeerRevocationArchive.
type fakeRevocationArchive struct {
	mu   sync.Mutex
	rows map[[64]byte]storage.PeerRevocation
}

func newFakeRevocationArchive() *fakeRevocationArchive {
	return &fakeRevocationArchive{rows: map[[64]byte]storage.PeerRevocation{}}
}

func (f *fakeRevocationArchive) key(tr, id []byte) [64]byte {
	var k [64]byte
	copy(k[0:32], tr)
	copy(k[32:64], id)
	return k
}

func (f *fakeRevocationArchive) PutPeerRevocation(_ context.Context, r storage.PeerRevocation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := f.key(r.TrackerID, r.IdentityID)
	if _, exists := f.rows[k]; exists {
		return nil // INSERT OR IGNORE semantics
	}
	f.rows[k] = r
	return nil
}

func (f *fakeRevocationArchive) GetPeerRevocation(_ context.Context, tr, id []byte) (storage.PeerRevocation, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[f.key(tr, id)]
	return r, ok, nil
}

// captureForward records every (kind, payload) sent.
type captureForward struct {
	mu    sync.Mutex
	calls []capturedForward
}

type capturedForward struct {
	kind    fed.Kind
	payload []byte
}

func (c *captureForward) fn(_ context.Context, kind fed.Kind, payload []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, capturedForward{kind: kind, payload: append([]byte(nil), payload...)})
}

func (c *captureForward) snapshot() []capturedForward {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]capturedForward, len(c.calls))
	copy(out, c.calls)
	return out
}

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return pub, priv
}

func newTestCoordinator(t *testing.T, arch PeerRevocationArchive, fwd Forwarder) (*revocationCoordinator, ed25519.PublicKey, ed25519.PrivateKey, ids.TrackerID) {
	t.Helper()
	pub, priv := newKey(t)
	tid := ids.TrackerID(sha256.Sum256(pub))
	rc := newRevocationCoordinator(revocationCoordinatorCfg{
		MyTrackerID: tid,
		MyPriv:      priv,
		Archive:     arch,
		Forward:     fwd,
		PeerPubKey: func(id ids.TrackerID) (ed25519.PublicKey, bool) {
			if id == tid {
				return pub, true
			}
			return nil, false
		},
		Now:        func() time.Time { return time.Unix(1714000123, 0) },
		Invalid:    func(string) {},
		OnEmit:     func() {},
		OnReceived: func(string) {},
	})
	return rc, pub, priv, tid
}

func TestRevocationReasonString_KnownAndUnknown(t *testing.T) {
	assert.Equal(t, fed.RevocationReason_REVOCATION_REASON_ABUSE, mapReputationReasonToProto("freeze_repeat"))
	assert.Equal(t, fed.RevocationReason_REVOCATION_REASON_MANUAL, mapReputationReasonToProto("operator"))
	// Unknown reputation reason defaults to ABUSE (reputation-emitted strings are abuse-class).
	assert.Equal(t, fed.RevocationReason_REVOCATION_REASON_ABUSE, mapReputationReasonToProto("some_unknown_reason"))
}

func TestRevocationCoordinator_OnFreeze_EmitsAndArchives(t *testing.T) {
	arch := newFakeRevocationArchive()
	cap := &captureForward{}
	rc, _, priv, tid := newTestCoordinator(t, arch, cap.fn)

	identity := ids.IdentityID(bytes.Repeat([]byte{0x55}, 32))
	revokedAt := time.Unix(1714000100, 0)
	rc.OnFreeze(context.Background(), identity, "freeze_repeat", revokedAt)

	calls := cap.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, fed.Kind_KIND_REVOCATION, calls[0].kind)

	rev := &fed.Revocation{}
	require.NoError(t, proto.Unmarshal(calls[0].payload, rev))
	tidBytes := tid.Bytes()
	assert.Equal(t, tidBytes[:], rev.TrackerId)
	idArr := identity
	assert.Equal(t, idArr[:], rev.IdentityId)
	assert.Equal(t, fed.RevocationReason_REVOCATION_REASON_ABUSE, rev.Reason)
	assert.Equal(t, uint64(1714000100), rev.RevokedAt)
	require.Len(t, rev.TrackerSig, fed.SigLen)

	canonical, err := fed.CanonicalRevocationPreSig(rev)
	require.NoError(t, err)
	assert.True(t, ed25519.Verify(priv.Public().(ed25519.PublicKey), canonical, rev.TrackerSig))

	got, ok, err := arch.GetPeerRevocation(context.Background(), tidBytes[:], identity[:])
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, uint32(fed.RevocationReason_REVOCATION_REASON_ABUSE), got.Reason)
	assert.Equal(t, uint64(1714000100), got.RevokedAt)
	assert.Equal(t, uint64(1714000123), got.ReceivedAt, "ReceivedAt = coordinator.Now()")
}

func TestRevocationCoordinator_OnFreeze_NilArchive_NoOp(t *testing.T) {
	cap := &captureForward{}
	pub, priv := newKey(t)
	tid := ids.TrackerID(sha256.Sum256(pub))
	rc := newRevocationCoordinator(revocationCoordinatorCfg{
		MyTrackerID: tid,
		MyPriv:      priv,
		Archive:     nil, // disabled
		Forward:     cap.fn,
		PeerPubKey:  func(ids.TrackerID) (ed25519.PublicKey, bool) { return nil, false },
		Now:         time.Now,
		Invalid:     func(string) {},
		OnEmit:      func() {},
		OnReceived:  func(string) {},
	})
	rc.OnFreeze(context.Background(), ids.IdentityID(bytes.Repeat([]byte{0x55}, 32)), "freeze_repeat", time.Now())
	assert.Empty(t, cap.snapshot(), "nil archive disables emit")
}

func TestRevocationCoordinator_OnIncoming_HappyPath(t *testing.T) {
	arch := newFakeRevocationArchive()
	cap := &captureForward{}

	// Issuer identity (a remote peer).
	issuerPub, issuerPriv := newKey(t)
	issuerID := ids.TrackerID(sha256.Sum256(issuerPub))

	// Build a properly-signed Revocation from the issuer.
	identity := bytes.Repeat([]byte{0x77}, 32)
	issuerIDBytes := issuerID.Bytes()
	rev := &fed.Revocation{
		TrackerId:  issuerIDBytes[:],
		IdentityId: identity,
		Reason:     fed.RevocationReason_REVOCATION_REASON_ABUSE,
		RevokedAt:  1714000100,
	}
	canonical, err := fed.CanonicalRevocationPreSig(rev)
	require.NoError(t, err)
	rev.TrackerSig = ed25519.Sign(issuerPriv, canonical)
	payload, err := proto.Marshal(rev)
	require.NoError(t, err)

	// Coordinator that knows issuerPub. Use an explicit cfg here so we
	// can set MyTrackerID independently of the issuer.
	myPub, myPriv := newKey(t)
	myID := ids.TrackerID(sha256.Sum256(myPub))
	rc := newRevocationCoordinator(revocationCoordinatorCfg{
		MyTrackerID: myID,
		MyPriv:      myPriv,
		Archive:     arch,
		Forward:     cap.fn,
		PeerPubKey: func(id ids.TrackerID) (ed25519.PublicKey, bool) {
			if id == issuerID {
				return issuerPub, true
			}
			return nil, false
		},
		Now:        func() time.Time { return time.Unix(1714000200, 0) },
		Invalid:    func(string) {},
		OnEmit:     func() {},
		OnReceived: func(string) {},
	})

	env := &fed.Envelope{
		SenderId:  issuerIDBytes[:],
		Kind:      fed.Kind_KIND_REVOCATION,
		Payload:   payload,
		SenderSig: bytes.Repeat([]byte{0xFF}, fed.SigLen), // dispatcher would have verified
	}
	rc.OnIncoming(context.Background(), env, issuerID)

	got, ok, err := arch.GetPeerRevocation(context.Background(), issuerIDBytes[:], identity)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, uint64(1714000100), got.RevokedAt)
	assert.Equal(t, uint64(1714000200), got.ReceivedAt)

	// Forwarded onward.
	calls := cap.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, fed.Kind_KIND_REVOCATION, calls[0].kind)
	assert.Equal(t, payload, calls[0].payload)
}

func TestRevocationCoordinator_OnIncoming_BadSig_Drops(t *testing.T) {
	arch := newFakeRevocationArchive()
	cap := &captureForward{}

	issuerPub, issuerPriv := newKey(t)
	issuerID := ids.TrackerID(sha256.Sum256(issuerPub))
	issuerIDBytes := issuerID.Bytes()

	rev := &fed.Revocation{
		TrackerId:  issuerIDBytes[:],
		IdentityId: bytes.Repeat([]byte{0x77}, 32),
		Reason:     fed.RevocationReason_REVOCATION_REASON_ABUSE,
		RevokedAt:  1714000100,
	}
	canonical, err := fed.CanonicalRevocationPreSig(rev)
	require.NoError(t, err)
	rev.TrackerSig = ed25519.Sign(issuerPriv, canonical)
	rev.TrackerSig[0] ^= 0xFF // tamper
	payload, err := proto.Marshal(rev)
	require.NoError(t, err)

	myPub, myPriv := newKey(t)
	myID := ids.TrackerID(sha256.Sum256(myPub))
	rc := newRevocationCoordinator(revocationCoordinatorCfg{
		MyTrackerID: myID, MyPriv: myPriv, Archive: arch, Forward: cap.fn,
		PeerPubKey: func(id ids.TrackerID) (ed25519.PublicKey, bool) {
			if id == issuerID {
				return issuerPub, true
			}
			return nil, false
		},
		Now:     func() time.Time { return time.Unix(1714000200, 0) },
		Invalid: func(string) {}, OnEmit: func() {}, OnReceived: func(string) {},
	})

	env := &fed.Envelope{
		SenderId: issuerIDBytes[:], Kind: fed.Kind_KIND_REVOCATION,
		Payload: payload, SenderSig: bytes.Repeat([]byte{0xFF}, fed.SigLen),
	}
	rc.OnIncoming(context.Background(), env, issuerID)

	_, ok, err := arch.GetPeerRevocation(context.Background(), issuerIDBytes[:], rev.IdentityId)
	require.NoError(t, err)
	assert.False(t, ok, "bad sig must not be archived")
	assert.Empty(t, cap.snapshot(), "bad sig must not be forwarded")
}

func TestRevocationCoordinator_OnIncoming_UnknownIssuer_Drops(t *testing.T) {
	arch := newFakeRevocationArchive()
	cap := &captureForward{}

	issuerPub, issuerPriv := newKey(t)
	issuerID := ids.TrackerID(sha256.Sum256(issuerPub))
	issuerIDBytes := issuerID.Bytes()

	rev := &fed.Revocation{
		TrackerId:  issuerIDBytes[:],
		IdentityId: bytes.Repeat([]byte{0x77}, 32),
		Reason:     fed.RevocationReason_REVOCATION_REASON_ABUSE,
		RevokedAt:  1714000100,
	}
	canonical, err := fed.CanonicalRevocationPreSig(rev)
	require.NoError(t, err)
	rev.TrackerSig = ed25519.Sign(issuerPriv, canonical)
	payload, err := proto.Marshal(rev)
	require.NoError(t, err)

	myPub, myPriv := newKey(t)
	myID := ids.TrackerID(sha256.Sum256(myPub))
	rc := newRevocationCoordinator(revocationCoordinatorCfg{
		MyTrackerID: myID, MyPriv: myPriv, Archive: arch, Forward: cap.fn,
		PeerPubKey: func(ids.TrackerID) (ed25519.PublicKey, bool) { return nil, false }, // unknown
		Now:        func() time.Time { return time.Unix(1714000200, 0) },
		Invalid:    func(string) {},
		OnEmit:     func() {},
		OnReceived: func(string) {},
	})

	env := &fed.Envelope{
		SenderId: issuerIDBytes[:], Kind: fed.Kind_KIND_REVOCATION,
		Payload: payload, SenderSig: bytes.Repeat([]byte{0xFF}, fed.SigLen),
	}
	rc.OnIncoming(context.Background(), env, issuerID)

	_, ok, err := arch.GetPeerRevocation(context.Background(), issuerIDBytes[:], rev.IdentityId)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, cap.snapshot())
}

func TestRevocationCoordinator_OnIncoming_FiresOnRevocationObserved(t *testing.T) {
	arch := newFakeRevocationArchive()
	cap := &captureForward{}

	issuerPub, issuerPriv := newKey(t)
	issuerID := ids.TrackerID(sha256.Sum256(issuerPub))
	issuerIDBytes := issuerID.Bytes()
	identity := bytes.Repeat([]byte{0x77}, 32)
	rev := &fed.Revocation{
		TrackerId:  issuerIDBytes[:],
		IdentityId: identity,
		Reason:     fed.RevocationReason_REVOCATION_REASON_ABUSE,
		RevokedAt:  9000,
	}
	canonical, err := fed.CanonicalRevocationPreSig(rev)
	require.NoError(t, err)
	rev.TrackerSig = ed25519.Sign(issuerPriv, canonical)
	payload, err := proto.Marshal(rev)
	require.NoError(t, err)

	var (
		gotIssuer    ids.TrackerID
		gotRevokedAt time.Time
		gotRecvAt    time.Time
		callCount    int
	)
	myPub, myPriv := newKey(t)
	myID := ids.TrackerID(sha256.Sum256(myPub))
	rc := newRevocationCoordinator(revocationCoordinatorCfg{
		MyTrackerID: myID,
		MyPriv:      myPriv,
		Archive:     arch,
		Forward:     cap.fn,
		PeerPubKey: func(id ids.TrackerID) (ed25519.PublicKey, bool) {
			if id == issuerID {
				return issuerPub, true
			}
			return nil, false
		},
		Now:        func() time.Time { return time.Unix(9100, 0) },
		Invalid:    func(string) {},
		OnEmit:     func() {},
		OnReceived: func(string) {},
		OnRevocationObserved: func(issuer ids.TrackerID, revokedAt, recvAt time.Time) {
			callCount++
			gotIssuer = issuer
			gotRevokedAt = revokedAt
			gotRecvAt = recvAt
		},
	})
	env := &fed.Envelope{
		SenderId: issuerIDBytes[:], Kind: fed.Kind_KIND_REVOCATION,
		Payload: payload, SenderSig: bytes.Repeat([]byte{0xFF}, fed.SigLen),
	}
	rc.OnIncoming(context.Background(), env, issuerID)

	require.Equal(t, 1, callCount)
	require.Equal(t, issuerID, gotIssuer)
	require.Equal(t, time.Unix(9000, 0), gotRevokedAt)
	require.Equal(t, time.Unix(9100, 0), gotRecvAt)
}

func TestRevocationCoordinator_OnIncoming_DoesNotFireOnBadSig(t *testing.T) {
	arch := newFakeRevocationArchive()
	cap := &captureForward{}
	issuerPub, _ := newKey(t)
	issuerID := ids.TrackerID(sha256.Sum256(issuerPub))
	issuerIDBytes := issuerID.Bytes()
	rev := &fed.Revocation{
		TrackerId:  issuerIDBytes[:],
		IdentityId: bytes.Repeat([]byte{0x42}, 32),
		Reason:     fed.RevocationReason_REVOCATION_REASON_ABUSE,
		RevokedAt:  9000,
		TrackerSig: bytes.Repeat([]byte{0x00}, 64), // garbage sig
	}
	payload, _ := proto.Marshal(rev)
	called := false
	myPub, myPriv := newKey(t)
	myID := ids.TrackerID(sha256.Sum256(myPub))
	rc := newRevocationCoordinator(revocationCoordinatorCfg{
		MyTrackerID: myID, MyPriv: myPriv, Archive: arch, Forward: cap.fn,
		PeerPubKey: func(ids.TrackerID) (ed25519.PublicKey, bool) { return issuerPub, true },
		Now:        func() time.Time { return time.Unix(9100, 0) },
		Invalid:    func(string) {}, OnEmit: func() {}, OnReceived: func(string) {},
		OnRevocationObserved: func(ids.TrackerID, time.Time, time.Time) { called = true },
	})
	env := &fed.Envelope{
		SenderId: issuerIDBytes[:], Kind: fed.Kind_KIND_REVOCATION,
		Payload: payload, SenderSig: bytes.Repeat([]byte{0xFF}, fed.SigLen),
	}
	rc.OnIncoming(context.Background(), env, issuerID)
	require.False(t, called)
}
