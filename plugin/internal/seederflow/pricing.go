package seederflow

import "fmt"

// modelPrices holds per-model credit prices, mirroring the tracker's
// broker.ModelPrices shape.
type modelPrices struct {
	inCreditsPerToken  uint64
	outCreditsPerToken uint64
}

// usagePriceTable mirrors tracker/internal/broker.DefaultPriceTable.
//
// CROSS-COMPONENT CONTRACT (v1): these values MUST stay byte-for-byte in
// lock-step with the tracker's DefaultPriceTable, because cost_credits is
// part of the canonical usage-assertion both the seeder and the tracker
// sign/verify (spec §3 P4). The tracker recomputes the cost with its own
// table (broker.PriceTable.ActualCost) and rejects the seeder's signature
// with SEEDER_SIG_INVALID if the numbers differ. The plugin cannot import
// the tracker module, so v1 mirrors the defaults here; a real deployment
// would source pricing from the tracker instead of a static mirror.
var usagePriceTable = map[string]modelPrices{
	"claude-opus-4-7":           {inCreditsPerToken: 15, outCreditsPerToken: 75},
	"claude-sonnet-4-6":         {inCreditsPerToken: 3, outCreditsPerToken: 15},
	"claude-haiku-4-5-20251001": {inCreditsPerToken: 1, outCreditsPerToken: 5},
}

// actualCostCredits replicates the tracker's broker.PriceTable.ActualCost
// arithmetic exactly: cost = in_price*input_tokens + out_price*output_tokens.
// Unknown models error out — signing a guessed cost would only produce a
// report the tracker rejects.
func actualCostCredits(model string, in, out uint32) (uint64, error) {
	p, ok := usagePriceTable[model]
	if !ok {
		return 0, fmt.Errorf("seederflow: no mirrored price for model %q", model)
	}
	return p.inCreditsPerToken*uint64(in) + p.outCreditsPerToken*uint64(out), nil
}
