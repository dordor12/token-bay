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
	return &trackerclient.Offer{
		ConsumerID:      consumerID,
		EnvelopeHash:    envHash,
		Model:           model,
		MaxInputTokens:  4096,
		MaxOutputTokens: 1024,
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
