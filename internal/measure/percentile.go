// Package measure turns raw timing samples from a real run — an ingestion, a batch of searches —
// into the verdict an operator needs against a named NFR, and a durable report of how it was
// computed. It holds no production logic of its own: cmd/measure-ingest and cmd/measure-search call
// the real internal/ingest, internal/retrieval, internal/vault and internal/embed packages to do the
// actual work, and only the arithmetic that turns their timings into a pass/fail lives here.
package measure

import (
	"fmt"
	"sort"
	"time"
)

// Percentile returns the p-th percentile (0 < p <= 1) of durations, using nearest-rank: the sample
// at index ceil(p*n)-1 of the sorted slice. That is the same rule PRD.md's own p95/p99 language
// assumes and it needs no interpolation, which matters here because a sample count in the tens (T5's
// task doc asks for "≥30 real queries") is too small for interpolation to mean anything extra.
//
// durations is not mutated; Percentile sorts a copy.
func Percentile(durations []time.Duration, p float64) time.Duration {
	if len(durations) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), durations...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	rank := int(p * float64(len(sorted)))
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
