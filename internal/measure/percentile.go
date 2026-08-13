// Package measure turns raw timing samples from a real run — an ingestion, a batch of searches —
// into the verdict an operator needs against a named NFR, and a durable report of how it was
// computed. It holds no production logic of its own: cmd/measure-ingest and cmd/measure-search call
// the real internal/ingest, internal/retrieval, internal/vault and internal/embed packages to do the
// actual work, and only the arithmetic that turns their timings into a pass/fail lives here.
package measure

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Percentile returns the p-th percentile (0 < p <= 1) of durations, using nearest-rank: the sample
// at index ceil(p*n)-1 of the sorted slice. Nearest-rank needs no interpolation, which matters here
// because a sample count in the tens (T5's task doc asks for "≥30 real queries") is too small for
// interpolation to mean anything extra.
//
// PRD.md names the thresholds and says nothing about how a percentile is computed — the choice is
// this file's, not a rule read from there.
//
// The ceiling is load-bearing and was a real defect here, so it gets a sentence rather than a symbol.
// Truncating instead — `int(p*n)`, which is what this did — agrees with ceil whenever p*n is a whole
// number, and every test used n=100, where p50/p95/p99 all land whole. At n=30, the sample size this
// tool defaults to, `int(0.99*30)` is 29: p99 becomes the 29th of 30 samples and steps over the single
// worst one. That is the query the measurement exists to find, and the report would have called it a
// pass.
//
// durations is not mutated; Percentile sorts a copy.
func Percentile(durations []time.Duration, p float64) time.Duration {
	if len(durations) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), durations...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	rank := int(math.Ceil(p * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// Stats is p50/p95/p99 for one column of measured runs.
type Stats struct {
	P50, P95, P99 time.Duration
}

func computeStats(durations []time.Duration) Stats {
	return Stats{
		P50: Percentile(durations, 0.50),
		P95: Percentile(durations, 0.95),
		P99: Percentile(durations, 0.99),
	}
}

func (s Stats) String() string {
	return fmt.Sprintf("p50=%s p95=%s p99=%s", s.P50, s.P95, s.P99)
}
