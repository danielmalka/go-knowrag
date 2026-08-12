package eval

import "math"

// Z95 is the standard-normal quantile for a two-sided 95% interval, and it is the only level this
// package reports at. The value is here, as a const, so the claim "95%" in a rendered report is
// checkable against the same file that produces it (report.go names the method and the level in
// the text it renders).
//
// Wilson rather than the naive normal approximation, decided by the owner: the golden set is 50–100
// questions, and at that size the normal approximation misbehaves near proportions of 0 and 1 — it
// produces bounds outside [0,1] and, at a perfect 100%, an interval of zero width, which reads as
// certainty from a sample of fifty.
const Z95 = 1.96

// ConfidenceMethod is what the reports call the interval. It exists so the label and the constant
// above cannot drift into disagreeing in prose.
const ConfidenceMethod = "95% Wilson"

// WilsonInterval is the Wilson score interval for a binomial proportion.
//
//	centre = (p̂ + z²/2n) / (1 + z²/n)
//	margin = z/(1 + z²/n) · √(p̂(1-p̂)/n + z²/4n²)
//
// n == 0 returns the whole unit interval rather than a point at zero: no observation supports no
// claim, and (0, 0) would read as "measured, and it is zero".
func WilsonInterval(successes, n int, z float64) (lo, hi float64) {
	if n <= 0 {
		return 0, 1
	}

	nf := float64(n)
	p := float64(successes) / nf
	z2 := z * z

	denom := 1 + z2/nf
	centre := (p + z2/(2*nf)) / denom
	margin := (z / denom) * math.Sqrt(p*(1-p)/nf+z2/(4*nf*nf))

	return clampUnit(centre - margin), clampUnit(centre + margin)
}

func clampUnit(v float64) float64 {
	return math.Min(1, math.Max(0, v))
}
