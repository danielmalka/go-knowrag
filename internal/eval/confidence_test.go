package eval

import (
	"math"
	"testing"
)

const tolerance = 1e-6

// TestWilsonInterval_KnownProportion_MatchesReferenceValue checks the formula against hand-computed
// values.
//
// The expected values were not taken from the same closed form confidence.go implements, which
// would only prove the code agrees with itself. They are the roots of the score equation the Wilson
// interval is defined as — (p̂ − p)² = z²·p(1−p)/n, solved as a quadratic in p — computed
// independently and checked to agree with the closed form to 1e-12 on every case below. 40/50 is
// the worked one: a = 1 + z²/n, b = −(2p̂ + z²/n), c = p̂², giving (0.669626, 0.887564).
//
// 50/50 and 0/50 are the cases the naive normal approximation gets wrong — it returns a zero-width
// interval at a perfect proportion and bounds outside [0,1] near the ends — which is the reason the
// owner picked Wilson (confidence.go). Wilson's upper bound at 50/50 is exactly 1 by algebra, not
// by the clamp: centre + margin = (1 + z²/n)/(1 + z²/n).
func TestWilsonInterval_KnownProportion_MatchesReferenceValue(t *testing.T) {
	cases := []struct {
		name           string
		successes, n   int
		wantLo, wantHi float64
	}{
		{"40 of 50", 40, 50, 0.669626, 0.887564},
		{"perfect 50 of 50", 50, 50, 0.928650, 1.000000},
		{"zero of 50", 0, 50, 0.000000, 0.071350},
		{"half of 100", 50, 100, 0.403830, 0.596170},
		{"8 of 10", 8, 10, 0.490157, 0.943319},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lo, hi := WilsonInterval(tc.successes, tc.n, Z95)
			if math.Abs(lo-tc.wantLo) > tolerance || math.Abs(hi-tc.wantHi) > tolerance {
				t.Errorf("WilsonInterval(%d, %d, Z95) = (%.4f, %.4f), want (%.4f, %.4f)",
					tc.successes, tc.n, lo, hi, tc.wantLo, tc.wantHi)
			}
			if lo < 0 || hi > 1 || lo > hi {
				t.Errorf("the interval (%.4f, %.4f) is not a subinterval of [0,1]", lo, hi)
			}
		})
	}
}

// TestWilsonInterval_NoObservationsSupportNoClaim is the n=0 case. Returning (0, 0) would read as
// "measured, and it is zero", which is the same class of lie as reporting an unscanned corpus as
// having no orphans.
func TestWilsonInterval_NoObservationsSupportNoClaim(t *testing.T) {
	lo, hi := WilsonInterval(0, 0, Z95)
	if lo != 0 || hi != 1 {
		t.Errorf("WilsonInterval(0, 0, Z95) = (%.4f, %.4f), want the whole unit interval", lo, hi)
	}
}

// TestWilsonInterval_WiderConfidenceIsAWiderInterval is the sanity check that the z argument is
// used at all: with the formula's z hardcoded internally, every level would return the same bounds
// and the test above would still pass.
func TestWilsonInterval_WiderConfidenceIsAWiderInterval(t *testing.T) {
	lo95, hi95 := WilsonInterval(40, 50, Z95)
	lo99, hi99 := WilsonInterval(40, 50, 2.5758)

	if !(lo99 < lo95 && hi99 > hi95) {
		t.Errorf("the 99%% interval (%.4f, %.4f) does not contain the 95%% one (%.4f, %.4f), so the "+
			"z argument is not reaching the formula", lo99, hi99, lo95, hi95)
	}
}

// TestWilsonInterval_DefaultConfidenceIs95Percent is S10 T4's second RED test, and it asserts the
// half that matters: not that Z95 holds 1.96, but that the aggregation path actually uses it.
//
// A test that only checked the constant would stay green through an Aggregate that hardcoded 2.5758
// — CLAUDE.md's "um teste que conferia o número escolhido sem conferir se ele é usado". So the
// interval Aggregate reports is compared against WilsonInterval at Z95, and against a different z,
// and it has to match the first and differ from the second.
func TestWilsonInterval_DefaultConfidenceIs95Percent(t *testing.T) {
	if Z95 != 1.96 {
		t.Errorf("Z95 = %v, want 1.96 — the level named in every rendered report", Z95)
	}
	if ConfidenceMethod != "95% Wilson" {
		t.Errorf("ConfidenceMethod = %q; the reports print this next to every bound", ConfidenceMethod)
	}

	// 8 hits out of 10, so the interval is well away from the [0,1] edges where any two z values
	// would clamp to the same numbers and the difference check below would go vacuous.
	report := Aggregate(hitPattern("alfa", 8, 2))

	wantLo, wantHi := WilsonInterval(8, 10, Z95)
	if math.Abs(report.Global.Lo-wantLo) > tolerance || math.Abs(report.Global.Hi-wantHi) > tolerance {
		t.Errorf("Aggregate reported (%.4f, %.4f); the 95%% Wilson interval for 8/10 is (%.4f, %.4f)",
			report.Global.Lo, report.Global.Hi, wantLo, wantHi)
	}

	otherLo, otherHi := WilsonInterval(8, 10, 2.5758)
	if math.Abs(report.Global.Lo-otherLo) < tolerance && math.Abs(report.Global.Hi-otherHi) < tolerance {
		t.Error("Aggregate's interval is indistinguishable from a 99% one, so this test would not " +
			"notice the reported confidence level changing")
	}

	// Per-area intervals go through the same path, and it is the one an edit is most likely to miss.
	area := report.PerArea["alfa"]
	if math.Abs(area.Lo-wantLo) > tolerance || math.Abs(area.Hi-wantHi) > tolerance {
		t.Errorf("the per-area interval (%.4f, %.4f) is not the 95%% Wilson one (%.4f, %.4f)",
			area.Lo, area.Hi, wantLo, wantHi)
	}
}
