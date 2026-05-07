package broker

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// fakePusher is a minimal PushService stub for offer_loop tests.
type fakePusher struct {
	offerCh chan *tbproto.OfferDecision
	ok      bool
}

func (f *fakePusher) PushOfferTo(ids.IdentityID, *tbproto.OfferPush) (<-chan *tbproto.OfferDecision, bool) {
	return f.offerCh, f.ok
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
		[32]byte{0xAA}, 1500*time.Millisecond)
	require.NoError(t, err)
	require.True(t, accept)
	require.Len(t, pub, 32)
}

func TestRunOffer_Reject(t *testing.T) {
	ch := make(chan *tbproto.OfferDecision, 1)
	p := &fakePusher{offerCh: ch, ok: true}
	ch <- &tbproto.OfferDecision{Accept: false, RejectReason: "busy"}
	accept, _, err := runOffer(context.Background(), p, ids.IdentityID{1},
		&tbproto.EnvelopeBody{Model: "x"}, [32]byte{}, time.Second)
	require.NoError(t, err)
	require.False(t, accept)
}

func TestRunOffer_Unreachable(t *testing.T) {
	p := &fakePusher{offerCh: nil, ok: false}
	_, _, err := runOffer(context.Background(), p, ids.IdentityID{1},
		&tbproto.EnvelopeBody{Model: "x"}, [32]byte{}, time.Second)
	require.Error(t, err)
}

func TestRunOffer_Timeout(t *testing.T) {
	ch := make(chan *tbproto.OfferDecision)
	p := &fakePusher{offerCh: ch, ok: true}
	accept, _, err := runOffer(context.Background(), p, ids.IdentityID{1},
		&tbproto.EnvelopeBody{Model: "x"}, [32]byte{}, 10*time.Millisecond)
	require.NoError(t, err) // timeout returns accept=false, not an error
	require.False(t, accept)
}

func TestRunOffer_CtxCancel(t *testing.T) {
	ch := make(chan *tbproto.OfferDecision)
	p := &fakePusher{offerCh: ch, ok: true}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(5 * time.Millisecond); cancel() }()
	_, _, err := runOffer(ctx, p, ids.IdentityID{1},
		&tbproto.EnvelopeBody{Model: "x"}, [32]byte{}, time.Hour)
	require.ErrorIs(t, err, context.Canceled)
}
