package measure

import (
	"testing"
	"time"
)

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

func TestPercentile_KnownDataset(t *testing.T) {
	// 1..100 ms, nearest-rank: p50 -> rank 50 -> 50ms, p95 -> rank 95 -> 95ms, p99 -> rank 99 -> 99ms.
	durations := make([]time.Duration, 100)
	for i := range durations {
		durations[i] = ms(i + 1)
	}

	cases := []struct {
		p    float64
		want time.Duration
	}{
		{0.50, ms(50)},
		{0.95, ms(95)},
		{0.99, ms(99)},
	}
	for _, c := range cases {
		if got := Percentile(durations, c.p); got != c.want {
			t.Errorf("Percentile(p=%v) = %v, want %v", c.p, got, c.want)
		}
	}
}

func TestPercentile_Empty(t *testing.T) {
	if got := Percentile(nil, 0.95); got != 0 {
		t.Errorf("Percentile(nil) = %v, want 0", got)
	}
}

func TestPercentile_SingleSample(t *testing.T) {
	durations := []time.Duration{ms(7)}
	for _, p := range []float64{0.50, 0.95, 0.99} {
		if got := Percentile(durations, p); got != ms(7) {
			t.Errorf("Percentile(p=%v) on a single sample = %v, want %v", p, got, ms(7))
		}
	}
}

// TestPercentile_DoesNotMutateInput guards the doc comment's promise: a caller reusing its slice
// after computing three percentiles from it must see the original order.
func TestPercentile_DoesNotMutateInput(t *testing.T) {
	durations := []time.Duration{ms(9), ms(1), ms(5)}
	original := append([]time.Duration(nil), durations...)

	Percentile(durations, 0.95)

	for i := range durations {
		if durations[i] != original[i] {
			t.Fatalf("input mutated at index %d: got %v, want %v", i, durations[i], original[i])
		}
	}
}

// TestPercentile_OutlierMovesP99ButNotP50 is the tail-vs-average distinction the task explicitly
// asks for: "reporte percentis, não média — a média esconde exatamente a cauda que importa." Two
// slow samples in a 100-sample set (the top 2%, so nearest-rank p99's 99th-smallest value lands on
// one of them) must move p99 and must not move p50. A single outlier in 100 samples would not: with
// nearest-rank, the 99th-of-100 value skips a lone outlier sitting only in the 100th slot — which is
// the point being pinned, not an oversight.
func TestPercentile_OutlierMovesP99ButNotP50(t *testing.T) {
	durations := make([]time.Duration, 100)
	for i := range durations {
		durations[i] = ms(10)
	}
	durations[0] = ms(9000)
	durations[1] = ms(9500)

	p50 := Percentile(durations, 0.50)
	p99 := Percentile(durations, 0.99)

	if p50 != ms(10) {
		t.Errorf("p50 = %v, want %v — the outliers must not move the median", p50, ms(10))
	}
	if p99 < ms(9000) {
		t.Errorf("p99 = %v, want it to surface one of the outliers (>= %v)", p99, ms(9000))
	}
}
