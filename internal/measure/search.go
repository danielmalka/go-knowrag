package measure

import (
	"fmt"
	"time"

	"github.com/danielmalka/go-knowrag/internal/retrieval"
)

// SearchReport is NFR-1b measured over N real queries: the decomposition's percentiles, and the
// verdict against NFR-1's own gate — NFR-1b carries no gate of its own (PRD.md's NFR-1b row is "—"
// in the target column; it exists only to decompose the number NFR-1 gates). PRD.md's NFR-1 row sets
// p95 <= 3s and p99 <= 5s end-to-end.
type SearchReport struct {
	N                       int
	Embed, Qdrant, Overhead Stats
	Total                   Stats
	P95Gate, P99Gate        time.Duration
	Pass                    bool
}

// EvaluateSearch computes the report from one measured batch of per-query legs.
//
// Pass is gated on Total's percentiles, never on a sum of a subset of the legs — Total is what
// retrieval.SearchTimed measures independently of Embed and Qdrant (internal/retrieval/search_timed.go),
// so Overhead is counted whether or not this function's author remembered it exists.
// TestEvaluateSearch_SlowOverheadAloneFailsTheGate proves that: a verdict built from Embed+Qdrant
// alone would pass the case it constructs, and this one does not.
func EvaluateSearch(legs []retrieval.Timing, p95Gate, p99Gate time.Duration) SearchReport {
	n := len(legs)
	embed := make([]time.Duration, n)
	qdrant := make([]time.Duration, n)
	overhead := make([]time.Duration, n)
	total := make([]time.Duration, n)
	for i, l := range legs {
		embed[i], qdrant[i], overhead[i], total[i] = l.Embed, l.Qdrant, l.Overhead, l.Total
	}

	totalStats := computeStats(total)
	return SearchReport{
		N:        n,
		Embed:    computeStats(embed),
		Qdrant:   computeStats(qdrant),
		Overhead: computeStats(overhead),
		Total:    totalStats,
		P95Gate:  p95Gate,
		P99Gate:  p99Gate,
		Pass:     totalStats.P95 <= p95Gate && totalStats.P99 <= p99Gate,
	}
}

// Resolution is the sentence a percentile needs when the sample is small, or the empty string when
// it does not.
//
// Nearest-rank puts p99 at index ceil(0.99*n)-1, so every n up to 100 puts it on the last sample:
// "p99" and "the worst one we saw" are the same number, and one outlier is the whole statistic. The
// number is still worth reporting — it is a real ceiling on what was observed — but printed beside
// p50 and p95 it borrows their appearance of resolution, and an operator reading a passing p99 off
// 30 queries would be reading one query.
//
// p95 crosses the same line at n=20. Both thresholds follow from the rank formula in
// percentile.go rather than from a convention chosen here.
func (r SearchReport) Resolution() string {
	switch {
	case r.N <= 0:
		return ""
	case r.N <= 20:
		return fmt.Sprintf("p95 and p99 are both the slowest of the %d queries — "+
			"at this sample size they are one observation, not a distribution", r.N)
	case r.N <= 100:
		return fmt.Sprintf("p99 is the slowest of the %d queries — at this sample size it is one "+
			"observation, not a distribution", r.N)
	}
	return ""
}

func (r SearchReport) String() string {
	verdict := "FAILED"
	if r.Pass {
		verdict = "passed"
	}
	note := ""
	if res := r.Resolution(); res != "" {
		note = "\nnote: " + res
	}
	return fmt.Sprintf(
		"NFR-1b search latency decomposition, n=%d queries (PRD.md NFR-1: p95<=%s p99<=%s)\n"+
			"  embed    %s\n"+
			"  qdrant   %s\n"+
			"  overhead %s\n"+
			"  total    %s\n"+
			"verdict: %s%s",
		r.N, r.P95Gate, r.P99Gate, r.Embed, r.Qdrant, r.Overhead, r.Total, verdict, note,
	)
}
