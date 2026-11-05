// Package detect compares each host to its peers and decides which ones are sick.
package detect

import (
	"math"
	"slices"
)

// madToSigma scales the MAD so a robust z-score reads like a normal z-score.
const madToSigma = 0.6745

// Median returns 0 for empty input and leaves xs in its original order.
func Median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := slices.Sorted(slices.Values(xs))
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

// RobustZ returns how far xs[i] sits above the median of xs. minMAD is a floor
// on the spread, so a perfectly uniform fleet cannot cause a division by zero.
func RobustZ(xs []float64, i int, minMAD float64) float64 {
	median, spread := baseline(xs, minMAD)
	return zScore(xs[i], median, spread)
}

// baseline returns the median of xs and its MAD, raised to at least minMAD.
func baseline(xs []float64, minMAD float64) (median, spread float64) {
	median = Median(xs)
	deviations := make([]float64, len(xs))
	for i, x := range xs {
		deviations[i] = math.Abs(x - median)
	}
	return median, max(Median(deviations), minMAD)
}

// zScore is 0 when there is no spread and no floor, since no distance from the
// median can be measured then.
func zScore(x, median, spread float64) float64 {
	if spread <= 0 {
		return 0
	}
	return madToSigma * (x - median) / spread
}
