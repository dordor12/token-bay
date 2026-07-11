package broker

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// fakePusher is a minimal PushService stub for offer_loop tests. Some
// tests (e.g. TestSubsystems_Submit_RaceClean) share one instance across
// concurrent Submit calls, so lastPush is guarded by mu.
type fakePusher struct {
	offerCh chan *tbproto.OfferDecision
	ok      bool

	mu       sync.Mutex
	lastPush *tbproto.OfferPush
}

func (f *fakePusher) PushOfferTo(_ ids.IdentityID, push *tbproto.OfferPush) (<-chan *tbproto.OfferDecision, bool) {
	f.mu.Lock()
	f.lastPush = push
	f.mu.Unlock()
	return f.offerCh, f.ok
}

func (f *fakePusher) getLastPush() *tbproto.OfferPush {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastPush
}

func (*fakePusher) PushSettlementTo(ids.IdentityID, *tbproto.SettlementPush) (<-chan *tbproto.SettleAck, bool) {
	return nil, false
}

func bytesAllB(n int, v byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = v
	}
	return b
}

func TestRunOffer_Accept(t *testing.T) {
	ch := make(chan *tbproto.OfferDecision, 1)
	p := &fakePusher{offerCh: ch, ok: true}
	ch <- &tbproto.OfferDecision{Accept: true, EphemeralPubkey: bytesAllB(32, 0xCC)}
	accept, pub, err := runOffer(context.Background(), p, ids.IdentityID{1},
		&tbproto.EnvelopeBody{Model: "x", MaxInputTokens: 1, MaxOutputTokens: 1},
		[32]byte{0xAA}, [16]byte{}, 1500*time.Millisecond)
	require.NoError(t, err)
	require.True(t, accept)
	require.Len(t, pub, 32)
}

func TestRunOffer_Reject(t *testing.T) {
	ch := make(chan *tbproto.OfferDecision, 1)
	p := &fakePusher{offerCh: ch, ok: true}
	ch <- &tbproto.OfferDecision{Accept: false, RejectReason: "busy"}
	accept, _, err := runOffer(context.Background(), p, ids.IdentityID{1},
		&tbproto.EnvelopeBody{Model: "x"}, [32]byte{}, [16]byte{}, time.Second)
	require.NoError(t, err)
	require.False(t, accept)
}

func TestRunOffer_Unreachable(t *testing.T) {
	p := &fakePusher{offerCh: nil, ok: false}
	_, _, err := runOffer(context.Background(), p, ids.IdentityID{1},
		&tbproto.EnvelopeBody{Model: "x"}, [32]byte{}, [16]byte{}, time.Second)
	require.Error(t, err)
}

func TestRunOffer_Timeout(t *testing.T) {
	ch := make(chan *tbproto.OfferDecision)
	p := &fakePusher{offerCh: ch, ok: true}
	accept, _, err := runOffer(context.Background(), p, ids.IdentityID{1},
		&tbproto.EnvelopeBody{Model: "x"}, [32]byte{}, [16]byte{}, 10*time.Millisecond)
	require.NoError(t, err) // timeout returns accept=false, not an error
	require.False(t, accept)
}

func TestRunOffer_CtxCancel(t *testing.T) {
	ch := make(chan *tbproto.OfferDecision)
	p := &fakePusher{offerCh: ch, ok: true}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(5 * time.Millisecond); cancel() }()
	_, _, err := runOffer(ctx, p, ids.IdentityID{1},
		&tbproto.EnvelopeBody{Model: "x"}, [32]byte{}, [16]byte{}, time.Hour)
	require.ErrorIs(t, err, context.Canceled)
}

func TestRunOffer_PopulatesEphemeralAndRequestID(t *testing.T) {
	fp := &fakePusher{offerCh: make(chan *tbproto.OfferDecision, 1), ok: true}
	fp.offerCh <- &tbproto.OfferDecision{Accept: true, EphemeralPubkey: make([]byte, 32)}
	body := &tbproto.EnvelopeBody{Model: "x", ConsumerEphemeralPub: bytes.Repeat([]byte{7}, 32)}
	var reqID [16]byte
	reqID[0] = 0xAB
	_, _, err := runOffer(context.Background(), fp, ids.IdentityID{1}, body, [32]byte{}, reqID, time.Second)
	require.NoError(t, err)
	lastPush := fp.getLastPush()
	require.Equal(t, body.ConsumerEphemeralPub, lastPush.ConsumerEphemeralPub)
	require.Equal(t, reqID[:], lastPush.RequestId)
}
