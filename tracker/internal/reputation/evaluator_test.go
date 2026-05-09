package reputation

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMedianMAD_Empty(t *testing.T) {
	median, mad := medianMAD(nil)
	require.Equal(t, 0.0, median)
	require.Equal(t, 0.0, mad)
}

func TestMedianMAD_Single(t *testing.T) {
	median, mad := medianMAD([]float64{3.0})
	require.InDelta(t, 3.0, median, 0.0001)
	require.InDelta(t, 0.0, mad, 0.0001)
}

func TestMedianMAD_OddCount(t *testing.T) {
	median, mad := medianMAD([]float64{1, 2, 3, 4, 5})
	require.InDelta(t, 3.0, median, 0.0001)
	// |xi - median| = [2,1,0,1,2] → MAD median = 1.0.
	require.InDelta(t, 1.0, mad, 0.0001)
}

func TestMedianMAD_EvenCount(t *testing.T) {
	median, mad := medianMAD([]float64{1, 2, 3, 4})
	require.InDelta(t, 2.5, median, 0.0001)
	// |xi - 2.5| = [1.5, 0.5, 0.5, 1.5] → MAD median = 1.0.
	require.InDelta(t, 1.0, mad, 0.0001)
}

func TestZScore_ZeroMAD(t *testing.T) {
	// All samples equal → MAD = 0; z must be defined-as-zero, not Inf.
	require.Equal(t, 0.0, zScore(5.0, 5.0, 0.0))
}

func TestZScore_NormalCase(t *testing.T) {
	z := zScore(7.0, 3.0, 2.0)
	require.InDelta(t, 2.0, z, 0.0001)
}

func TestZScore_NegativeOutlier(t *testing.T) {
	z := zScore(-1.0, 3.0, 2.0)
	if !math.Signbit(z) {
		t.Errorf("expected negative z, got %f", z)
	}
}
