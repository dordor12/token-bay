package broker

import (
	"errors"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/config"
)

type ModelPrices struct {
	InCreditsPerToken  uint64
	OutCreditsPerToken uint64
}

type PriceTable struct {
	byModel map[string]ModelPrices
}

func DefaultPriceTable() *PriceTable {
	return &PriceTable{byModel: map[string]ModelPrices{
		"claude-opus-4-7":            {InCreditsPerToken: 15, OutCreditsPerToken: 75},
		"claude-sonnet-4-6":          {InCreditsPerToken: 3, OutCreditsPerToken: 15},
		"claude-haiku-4-5-20251001":  {InCreditsPerToken: 1, OutCreditsPerToken: 5},
	}}
}

func NewPriceTableFromConfig(c config.PricingConfig) *PriceTable {
	out := &PriceTable{byModel: make(map[string]ModelPrices, len(c.Models))}
	for name, m := range c.Models {
		out.byModel[name] = ModelPrices{
			InCreditsPerToken:  m.InCreditsPerToken,
			OutCreditsPerToken: m.OutCreditsPerToken,
		}
	}
	return out
}

func (p *PriceTable) MaxCost(env *tbproto.EnvelopeBody) (uint64, error) {
	if env == nil {
		return 0, errors.New("broker: MaxCost called with nil envelope")
	}
	m, ok := p.byModel[env.Model]
	if !ok {
		return 0, ErrUnknownModel
	}
	return m.InCreditsPerToken*env.MaxInputTokens + m.OutCreditsPerToken*env.MaxOutputTokens, nil
}

func (p *PriceTable) ActualCost(model string, in, out uint32) (uint64, error) {
	m, ok := p.byModel[model]
	if !ok {
		return 0, ErrUnknownModel
	}
	return m.InCreditsPerToken*uint64(in) + m.OutCreditsPerToken*uint64(out), nil
}
