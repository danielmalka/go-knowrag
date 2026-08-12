package eval

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/danielmalka/go-knowrag/internal/retrieval"
)

// RecallStat is one recall figure with the interval around it. Numerator and denominator are kept
// rather than only the ratio, because 4/5 and 40/50 are the same number and not the same evidence.
type RecallStat struct {
	Hits  int     `json:"hits"`
	Total int     `json:"total"`
	Lo    float64 `json:"lo"`
	Hi    float64 `json:"hi"`
}

// Recall is Hits/Total, and 0 when nothing was measured. A reader must not take that zero for a
// measurement — Report.Complete and the rendered text are what say whether anything was measured.
func (s RecallStat) Recall() float64 {
	if s.Total == 0 {
		return 0
	}
	return float64(s.Hits) / float64(s.Total)
}

// Report is one golden run, aggregated.
type Report struct {
	Global  RecallStat            `json:"global"`
	PerArea map[string]RecallStat `json:"per_area"`

	// Failed is every measured question that missed, each keeping its expected uid and its actual
	// top-K so the renderer can show both without a second run.
	Failed []QuestionResult `json:"failed"`

	// Errored is every question that could not be asked. These are outside Global entirely — see
	// Complete.
	Errored []QuestionResult `json:"errored"`

	// Complete is false when any question errored. It is the field that keeps this package honest:
	// recall computed over the questions that happened to answer is not the recall of the golden
	// set, and a gate that treats a partial run as a pass is the "orphans: not scanned" failure in
	// a different costume. GoldenGate requires it (eval.go).
	Complete bool `json:"complete"`

	// Mode and K describe the run these numbers came from. Two reports are only comparable when
	// both agree, which is what makes the hybrid-vs-dense comparison (hybrid_compare.go) and the
	// baseline delta (baseline.go) mean anything.
	Mode string `json:"mode"`
	K    int    `json:"k"`

	// GoldenSetCommit and Stale are provenance (provenance.go), filled by the caller that knows the
	// file's path. Empty means nobody looked, and the renderer says so rather than printing nothing.
	GoldenSetCommit string       `json:"golden_set_commit,omitempty"`
	Stale           []StaleEntry `json:"stale,omitempty"`

	// CoverageWarning is ValidateCoverage's output when the run went ahead anyway (S10 open
	// question 4: warn at run time, strict at authoring time).
	CoverageWarning string `json:"coverage_warning,omitempty"`
}

// Aggregate turns per-question results into global and per-area recall with a Wilson interval at
// 95% — the level is Z95 here and nowhere else, so no caller can pick a different one by passing an
// argument.
func Aggregate(results []QuestionResult) Report {
	r := Report{PerArea: map[string]RecallStat{}, Complete: true}

	perArea := map[string]*RecallStat{}
	for _, res := range results {
		if !res.Measured() {
			r.Complete = false
			r.Errored = append(r.Errored, res)
			continue
		}

		stat, ok := perArea[res.Question.Area]
		if !ok {
			stat = &RecallStat{}
			perArea[res.Question.Area] = stat
		}
		stat.Total++
		r.Global.Total++
		if res.Hit {
			stat.Hits++
			r.Global.Hits++
		} else {
			r.Failed = append(r.Failed, res)
		}
	}

	r.Global.Lo, r.Global.Hi = WilsonInterval(r.Global.Hits, r.Global.Total, Z95)
	for area, stat := range perArea {
		stat.Lo, stat.Hi = WilsonInterval(stat.Hits, stat.Total, Z95)
		r.PerArea[area] = *stat
	}
	return r
}

// RenderReport writes the Markdown an operator reads.
//
// Every interval printed carries ConfidenceMethod next to it. A confidence bound with no method and
// no level is a number with no procedure behind it, and a reader cannot tell a 95% Wilson bound
// from a 99% normal one by looking.
func RenderReport(r Report) string {
	var b strings.Builder

	b.WriteString("# Recall report\n\n")
	fmt.Fprintf(&b, "- Mode: %s\n", orUnknown(r.Mode))
	fmt.Fprintf(&b, "- Cut-off: Recall@%d\n", r.K)
	fmt.Fprintf(&b, "- Global recall: %d/%d = %.4f (%s CI %.4f–%.4f)\n",
		r.Global.Hits, r.Global.Total, r.Global.Recall(), ConfidenceMethod, r.Global.Lo, r.Global.Hi)

	if !r.Complete {
		fmt.Fprintf(&b, "\n**INCOMPLETE RUN: %d question(s) could not be asked.** They are excluded "+
			"from the numbers above, so the recall printed here is over the questions that answered, "+
			"not over the golden set. This is not a passing evaluation.\n", len(r.Errored))
	}

	b.WriteString("\n## Per area\n\n")
	b.WriteString("| Area | Hits | Total | Recall | " + ConfidenceMethod + " CI |\n")
	b.WriteString("|---|---:|---:|---:|---|\n")
	for _, area := range slices.Sorted(maps.Keys(r.PerArea)) {
		s := r.PerArea[area]
		fmt.Fprintf(&b, "| %s | %d | %d | %.4f | %.4f–%.4f |\n",
			area, s.Hits, s.Total, s.Recall(), s.Lo, s.Hi)
	}

	b.WriteString("\n## Failed questions\n\n")
	if len(r.Failed) == 0 {
		b.WriteString("No failures: every measured question returned its expected note in the top-" +
			fmt.Sprint(r.K) + ".\n")
	}
	for _, f := range r.Failed {
		fmt.Fprintf(&b, "- %q (area %s)\n  - expected uid: %s%s\n  - actual top-%d: %s\n",
			f.Question.Question, f.Question.Area, f.Question.UID, chunkSuffix(f.Question), r.K,
			joinUIDs(f.TopK))
	}

	if len(r.Errored) > 0 {
		b.WriteString("\n## Questions that could not be asked\n\n")
		for _, e := range r.Errored {
			fmt.Fprintf(&b, "- %q (area %s): %s\n", e.Question.Question, e.Question.Area, e.Error)
		}
	}

	renderProvenance(&b, r)

	if r.CoverageWarning != "" {
		b.WriteString("\n## Coverage warning\n\n")
		fmt.Fprintf(&b, "The golden set does not satisfy its own coverage table. The run went ahead "+
			"anyway (coverage is an authoring gate, not a run-time one):\n\n```\n%s\n```\n",
			r.CoverageWarning)
	}

	return b.String()
}

func renderProvenance(b *strings.Builder, r Report) {
	b.WriteString("\n## Provenance\n\n")
	if r.GoldenSetCommit == "" {
		b.WriteString("Golden-set commit: **not resolved** — nobody looked, so this report cannot " +
			"say which version of the golden set it measured.\n")
		return
	}
	fmt.Fprintf(b, "Golden-set commit: %s\n", r.GoldenSetCommit)
	if len(r.Stale) == 0 {
		b.WriteString("\nNo entry postdates the baseline this run evaluates.\n")
		return
	}
	fmt.Fprintf(b, "\n%d entry/entries were authored after the baseline this run evaluates, or have "+
		"no resolvable commit:\n\n", len(r.Stale))
	for _, s := range r.Stale {
		fmt.Fprintf(b, "- %q — %s\n", s.Question, s.Reason)
	}
}

func chunkSuffix(q GoldenQuestion) string {
	if q.ChunkIndex == nil {
		return " (any chunk)"
	}
	return fmt.Sprintf(" (chunk %d exactly)", *q.ChunkIndex)
}

// joinUIDs renders what actually came back. The uid is the identity the golden set names, so it is
// what a reader compares against; the chunk index is appended because an entry pinned to a chunk
// misses on a hit of the same note, and without the index that line reads as an inexplicable miss.
func joinUIDs(results []retrieval.Result) string {
	if len(results) == 0 {
		return "(nothing came back)"
	}
	parts := make([]string, 0, len(results))
	for _, r := range results {
		parts = append(parts, fmt.Sprintf("%s#%d", r.UID, r.ChunkIndex))
	}
	return strings.Join(parts, ", ")
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
