package detect

import (
	"math"
	"slices"
	"testing"
)

func TestMedian(t *testing.T) {
	for _, tc := range []struct {
		xs   []float64
		want float64
	}{
		{[]float64{3, 1, 2}, 2},
		{[]float64{4, 1, 3, 2}, 2.5},
		{[]float64{7}, 7},
		{nil, 0},
	} {
		if got := Median(tc.xs); got != tc.want {
			t.Errorf("Median(%v) = %v, want %v", tc.xs, got, tc.want)
		}
	}
}

func TestMedianDoesNotReorderInput(t *testing.T) {
	xs := []float64{3, 1, 2}
	Median(xs)
	if !slices.Equal(xs, []float64{3, 1, 2}) {
		t.Fatalf("input reordered to %v", xs)
	}
}

func TestRobustZ_Basic(t *testing.T) {
	got := RobustZ([]float64{9, 10, 10, 10, 11, 50}, 5, 0)
	if math.Abs(got-53.96) > 0.01 {
		t.Fatalf("z = %v, want about 53.96", got)
	}
}

func TestRobustZ_MADZeroUsesFloor(t *testing.T) {
	got := RobustZ([]float64{0, 0, 0, 0, 0.03}, 4, 0.005)
	if math.IsInf(got, 0) || math.IsNaN(got) {
		t.Fatalf("z = %v", got)
	}
	if math.Abs(got-4.047) > 0.001 {
		t.Fatalf("z = %v, want about 4.047", got)
	}
}

func TestRobustZ_FloorOnlyAppliesWhenMADIsSmaller(t *testing.T) {
	got := RobustZ([]float64{9, 10, 10, 10, 11, 50}, 5, 0.1)
	if math.Abs(got-53.96) > 0.01 {
		t.Fatalf("z = %v, want the real MAD of 0.5 to be used", got)
	}
}

func TestRobustZ_NoSpreadAndNoFloorIsZero(t *testing.T) {
	for _, xs := range [][]float64{{1, 1, 1}, {0, 0, 5}} {
		got := RobustZ(xs, 2, 0)
		if math.IsInf(got, 0) || math.IsNaN(got) || got != 0 {
			t.Errorf("RobustZ(%v) = %v, want 0", xs, got)
		}
	}
}

func TestRobustZ_BelowMedianIsNegative(t *testing.T) {
	if got := RobustZ([]float64{10, 10, 10, 11, 1}, 4, 1); got >= 0 {
		t.Fatalf("z = %v, want negative", got)
	}
}

// One extreme value must not be able to hide itself by dragging the baseline,
// which is the reason for using a median rather than a mean.
func TestRobustZ_ResistsTheOutlierItself(t *testing.T) {
	small := RobustZ([]float64{10, 10, 10, 10, 10, 10, 10, 10, 11, 100}, 9, 1)
	huge := RobustZ([]float64{10, 10, 10, 10, 10, 10, 10, 10, 11, 100000}, 9, 1)
	if huge <= small {
		t.Fatalf("z for 100000 = %v, not above z for 100 = %v", huge, small)
	}
}
