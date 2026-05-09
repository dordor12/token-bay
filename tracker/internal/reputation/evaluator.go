package reputation

import (
	"math"
	"sort"
)

// medianMAD returns the median and the median absolute deviation of
// xs. xs is mutated (sorted) — callers pass a fresh slice or a copy.
// Returns (0, 0) when xs is empty.
func medianMAD(xs []float64) (float64, float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	sort.Float64s(xs)
	median := medianSortedCopy(xs)
	devs := make([]float64, len(xs))
	for i, x := range xs {
		devs[i] = math.Abs(x - median)
	}
	sort.Float64s(devs)
	mad := medianSortedCopy(devs)
	return median, mad
}

// medianSortedCopy returns the median of a sorted slice (does not sort).
func medianSortedCopy(xs []float64) float64 {
	n := len(xs)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return xs[n/2]
	}
	return 0.5 * (xs[n/2-1] + xs[n/2])
}

// zScore returns (sample - median) / mad. Returns 0 when mad is 0 (the
// population is degenerate and z is undefined; treating it as
// non-outlier is the right default for the evaluator).
func zScore(sample, median, mad float64) float64 {
	if mad == 0 {
		return 0
	}
	return (sample - median) / mad
}
