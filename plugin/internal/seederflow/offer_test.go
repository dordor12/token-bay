package seederflow_test

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/seederflow"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
	"github.com/token-bay/token-bay/shared/ids"
)

func makeOffer(model string) *trackerclient.Offer {
	var consumerID ids.IdentityID
	for i := range consumerID {
		consumerID[i] = byte(i + 1)
	}
	var envHash [32]byte
	for i := range envHash {
		envHash[i] = byte(0xa0 + i)
	}
	consumerPub := make([]byte, ed25519.PublicKeySize)
	for i := range consumerPub {
		consumerPub[i] = byte(0x40 + i)
	}
	return &trackerclient.Offer{
		ConsumerID:           consumerID,
		EnvelopeHash:         envHash,
		Model:                model,
		MaxInputTokens:       4096,
		MaxOutputTokens:      1024,
		ConsumerEphemeralPub: consumerPub,
	}
}

func TestHandleOffer_AcceptsWhenAvailable(t *testing.T) {
	cfg := validConfig(t)
	c, err := seederflow.New(cfg)
	require.NoError(t, err)
	dec, err := c.HandleOffer(context.Background(), makeOffer("claude-sonnet-4-6"))
	require.NoError(t, err)
	require.True(t, dec.Accept)
	require.Len(t, dec.EphemeralPubkey, ed25519.PublicKeySize)
	require.NotEmpty(t, dec.EphemeralPubkey)
}

func TestHandleOffer_RegistersReservation(t *testing.T) {
	cfg := validConfig(t)
	c, err := seederflow.New(cfg)
	require.NoError(t, err)
	o := makeOffer("claude-sonnet-4-6")
	dec, err := c.HandleOffer(context.Background(), o)
	require.NoError(t, err)
	require.True(t, dec.Accept)
	require.True(t, c.HasReservation(o.EnvelopeHash))
}

func TestHandleOffer_RejectsUnsupportedModel(t *testing.T) {
	cfg := validConfig(t)
	cfg.Models = []string{"claude-sonnet-4-6"}
	c, err := seederflow.New(cfg)
	require.NoError(t, err)
	dec, err := c.HandleOffer(context.Background(), makeOffer("gpt-9000"))
	require.NoError(t, err)
	require.False(t, dec.Accept)
	require.Contains(t, dec.RejectReason, "model")
}

func TestHandleOffer_RejectsWhenSessionActive(t *testing.T) {
	cfg := validConfig(t)
	c, err := seederflow.New(cfg)
	require.NoError(t, err)
	require.NoError(t, c.OnSessionStart(context.Background(), time.Now()))
	dec, err := c.HandleOffer(context.Background(), makeOffer("claude-sonnet-4-6"))
	require.NoError(t, err)
	require.False(t, dec.Accept)
	require.NotEmpty(t, dec.RejectReason)
}

func TestHandleOffer_RejectsWhenRateLimited(t *testing.T) {
	cfg := validConfig(t)
	cfg.HeadroomWindow = 15 * time.Minute
	c, err := seederflow.New(cfg)
	require.NoError(t, err)
	c.RecordRateLimit(time.Now())
	dec, err := c.HandleOffer(context.Background(), makeOffer("claude-sonnet-4-6"))
	require.NoError(t, err)
	require.False(t, dec.Accept)
	require.NotEmpty(t, dec.RejectReason)
}

func TestHandleOffer_RejectsMissingConsumerEphemeralPub(t *testing.T) {
	cfg := validConfig(t)
	metrics := &stubOfferMetrics{}
	cfg.Metrics = metrics
	c, err := seederflow.New(cfg)
	require.NoError(t, err)

	o := makeOffer("claude-sonnet-4-6")
	o.ConsumerEphemeralPub = nil

	dec, err := c.HandleOffer(context.Background(), o)
	require.NoError(t, err)
	require.False(t, dec.Accept, "offer with missing consumer ephemeral pubkey must be rejected")
	require.Contains(t, dec.RejectReason, "consumer ephemeral", "reject reason must point at the missing pubkey")
	require.False(t, c.HasReservation(o.EnvelopeHash), "no reservation must be registered on reject")
	require.Equal(t, 1, metrics.Get("no_ephemeral"), "offer_rejected_no_ephemeral counter must increment")
}

func TestHandleOffer_RejectsMalformedConsumerEphemeralPub(t *testing.T) {
	cfg := validConfig(t)
	metrics := &stubOfferMetrics{}
	cfg.Metrics = metrics
	c, err := seederflow.New(cfg)
	require.NoError(t, err)

	o := makeOffer("claude-sonnet-4-6")
	o.ConsumerEphemeralPub = make([]byte, 31)

	dec, err := c.HandleOffer(context.Background(), o)
	require.NoError(t, err)
	require.False(t, dec.Accept, "offer with malformed consumer ephemeral pubkey must be rejected")
	require.False(t, c.HasReservation(o.EnvelopeHash), "no reservation must be registered on reject")
	require.Equal(t, 1, metrics.Get("no_ephemeral"), "malformed pubkey also bumps the same counter")
}

func TestHandleOffer_BindsAcceptorWithKeyPair(t *testing.T) {
	cfg := validConfig(t)
	acc := newStubAcceptor()
	cfg.Acceptor = acc
	c, err := seederflow.New(cfg)
	require.NoError(t, err)

	o := makeOffer("claude-sonnet-4-6")
	dec, err := c.HandleOffer(context.Background(), o)
	require.NoError(t, err)
	require.True(t, dec.Accept)
	require.Len(t, dec.EphemeralPubkey, ed25519.PublicKeySize)

	binds := acc.Bindings()
	require.Len(t, binds, 1, "acceptor must be bound exactly once per accepted offer")
	require.Equal(t, ed25519.PublicKey(o.ConsumerEphemeralPub), binds[0].ConsumerPub,
		"acceptor must receive the consumer's ephemeral pubkey from the offer")
	require.Len(t, binds[0].SeederPriv, ed25519.PrivateKeySize)
	require.Equal(t, ed25519.PublicKey(dec.EphemeralPubkey), binds[0].SeederPriv.Public().(ed25519.PublicKey),
		"acceptor's seeder priv must match the pubkey returned to the consumer")
}

func TestHandleOffer_RejectsWhenAcceptorBindFails(t *testing.T) {
	cfg := validConfig(t)
	acc := newStubAcceptor()
	acc.bindErr = errBindFailed
	cfg.Acceptor = acc
	c, err := seederflow.New(cfg)
	require.NoError(t, err)

	o := makeOffer("claude-sonnet-4-6")
	dec, err := c.HandleOffer(context.Background(), o)
	require.NoError(t, err)
	require.False(t, dec.Accept, "Bind failure must cause reject so we don't promise a tunnel we can't accept")
	require.False(t, c.HasReservation(o.EnvelopeHash), "no reservation when Bind fails")
}
