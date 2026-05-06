package broker

import (
	"testing"

	"github.com/stretchr/testify/require"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/config"
)

func TestDefaultPriceTable_KnownModels(t *testing.T) {
	p := DefaultPriceTable()
	body := &tbproto.EnvelopeBody{
		Model: "claude-sonnet-4-6", MaxInputTokens: 1000, MaxOutputTokens: 2000,
	}
	cost, err := p.MaxCost(body)
	require.NoError(t, err)
	require.Equal(t, uint64(1000*3+2000*15), cost)
}

func TestPriceTable_UnknownModel(t *testing.T) {
	p := DefaultPriceTable()
	_, err := p.MaxCost(&tbproto.EnvelopeBody{Model: "gpt-9"})
	require.ErrorIs(t, err, ErrUnknownModel)
	_, err = p.ActualCost("gpt-9", 1, 1)
	require.ErrorIs(t, err, ErrUnknownModel)
}

func TestPriceTable_FromConfig(t *testing.T) {
	p := NewPriceTableFromConfig(config.PricingConfig{
		Models: map[string]config.ModelPriceConfig{
			"x": {InCreditsPerToken: 2, OutCreditsPerToken: 3},
		},
	})
	cost, err := p.ActualCost("x", 4, 5)
	require.NoError(t, err)
	require.Equal(t, uint64(4*2+5*3), cost)
}

func TestPriceTable_NilEnvelope(t *testing.T) {
	p := DefaultPriceTable()
	_, err := p.MaxCost(nil)
	require.Error(t, err)
}
