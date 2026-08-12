package eval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/danielmalka/go-knowrag/internal/retrieval"
)

// The two sentences the decision doc can end with. They are constants because S10 T11 parses one of
// them back out of the written document to check that the shipped DefaultSearchMode agrees with the
// measurement — a doc-vs-code drift guard is only a guard while both sides spell the outcome the
// same way, and prose reworded by hand is exactly how that stops being true.
const (
	OutcomeHybridWins = "hybrid wins, default stays hybrid"
	OutcomeDenseWins  = "dense-only wins, default switches to dense-only"
)

// CompareHybridVsDense runs the same questions through both modes.
//
// Both runs take the identical RunConfig apart from Mode, so the only thing that differs between
// the two reports is the ranking — same collection, same tenant, same K, same questions, same
// order. Anything else varying would make the comparison a comparison of two setups.
func CompareHybridVsDense(ctx context.Context, s Searcher, questions []GoldenQuestion, cfg RunConfig) (hybrid, dense Report, err error) {
	run := func(mode retrieval.SearchMode) (Report, error) {
		modeCfg := cfg
		modeCfg.Mode = mode
		results, rerr := RunGolden(ctx, s, questions, modeCfg)
		if rerr != nil {
			return Report{}, fmt.Errorf("eval: the %s run: %w", mode, rerr)
		}
		r := Aggregate(results)
		r.Mode = mode.String()
		r.K = modeCfg.K
		return r, nil
	}

	if hybrid, err = run(retrieval.SearchModeHybrid); err != nil {
		return Report{}, Report{}, err
	}
	if dense, err = run(retrieval.SearchModeDenseOnly); err != nil {
		return Report{}, Report{}, err
	}
	return hybrid, dense, nil
}

// DecisionOutcome applies the rule: dense-only has to beat hybrid to displace it.
//
// A tie keeps the hybrid, and that asymmetry is the decision rather than an oversight — the hybrid
// is what PRD-contrato §2.3b specifies, and equal measured recall is no evidence for changing it.
func DecisionOutcome(hybrid, dense Report) string {
	if dense.Global.Recall() > hybrid.Global.Recall() {
		return OutcomeDenseWins
	}
	return OutcomeHybridWins
}

// WriteHybridVsDenseReport writes the comparison document.
//
// It refuses an incomplete run. A document stating two recall numbers is read later as the evidence
// for a default that ships, and a run that could not ask some of its questions produces two numbers
// that are not the recall of the golden set — nothing downstream would ever find that out from the
// file.
func WriteHybridVsDenseReport(path string, hybrid, dense Report, date time.Time) error {
	if !hybrid.Complete || !dense.Complete {
		return fmt.Errorf("eval: refusing to write %s: the hybrid run is complete=%t and the "+
			"dense-only run is complete=%t, so at least one number here would be the recall over "+
			"the questions that answered rather than over the golden set", path, hybrid.Complete, dense.Complete)
	}
	if hybrid.Global.Total != dense.Global.Total {
		return fmt.Errorf("eval: refusing to write %s: the two runs measured %d and %d question(s), "+
			"so the numbers are not comparable", path, hybrid.Global.Total, dense.Global.Total)
	}

	var b strings.Builder
	b.WriteString("# Hybrid vs dense-only\n\n")
	fmt.Fprintf(&b, "- Run date: %s\n", date.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "- Questions: %d, cut-off Recall@%d\n\n", hybrid.Global.Total, hybrid.K)
	fmt.Fprintf(&b, "| Mode | Hits | Total | Recall | %s CI |\n|---|---:|---:|---:|---|\n", ConfidenceMethod)
	for _, r := range []Report{hybrid, dense} {
		fmt.Fprintf(&b, "| %s | %d | %d | %.4f | %.4f–%.4f |\n",
			orUnknown(r.Mode), r.Global.Hits, r.Global.Total, r.Global.Recall(), r.Global.Lo, r.Global.Hi)
	}
	fmt.Fprintf(&b, "\n## Decision\n\n%s\n", DecisionOutcome(hybrid, dense))
	fmt.Fprintf(&b, "\nThe intervals overlap check is left to the reader on purpose: the rule "+
		"applied above is a plain comparison of the two point estimates, and the bounds are printed "+
		"so a reader can see how much evidence is behind it.\n")

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("eval: creating the directory for %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("eval: writing %s: %w", path, err)
	}
	return nil
}
