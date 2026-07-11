package seederflow

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestActualCostCredits_MirrorsTrackerDefaultPriceTable pins the mirrored
// table to the tracker's broker.DefaultPriceTable values and arithmetic
// (cost = in_price*input + out_price*output). If this test needs editing,
// the tracker's DefaultPriceTable changed and BOTH sides must move together
// — see the contract comment in pricing.go.
func TestActualCostCredits_MirrorsTrackerDefaultPriceTable(t *testing.T) {
	cases := []struct {
		model    string
		in, out  uint32
		expected uint64
	}{
		// opus: in=15, out=75
		{"claude-opus-4-7", 100, 200, 15*100 + 75*200},
		// sonnet: in=3, out=15
		{"claude-sonnet-4-6", 10, 20, 3*10 + 15*20},
		{"claude-sonnet-4-6", 0, 0, 0},
		// haiku: in=1, out=5
		{"claude-haiku-4-5-20251001", 7, 3, 1*7 + 5*3},
	}
	for _, tc := range cases {
		got, err := actualCostCredits(tc.model, tc.in, tc.out)
		require.NoError(t, err, tc.model)
		require.Equal(t, tc.expected, got, "cost for %s(%d in, %d out)", tc.model, tc.in, tc.out)
	}
}

func TestActualCostCredits_UnknownModelErrors(t *testing.T) {
	_, err := actualCostCredits("gpt-9000", 1, 1)
	require.Error(t, err, "unknown model must not be priced — the tracker would reject the sig anyway")
}
