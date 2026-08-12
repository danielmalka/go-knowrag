package eval

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danielmalka/go-knowrag/internal/retrieval"
)

var errUnreachable = errors.New("qdrant is unreachable")

// modeSearcher answers differently per mode, which is the only way a comparison test can produce
// two different recall numbers from one question set.
type modeSearcher struct {
	byMode map[retrieval.SearchMode][]retrieval.Result
	modes  []retrieval.SearchMode
	err    error
}

func (m *modeSearcher) Search(_ context.Context, q retrieval.Query) ([]retrieval.Result, error) {
	m.modes = append(m.modes, q.Mode)
	if m.err != nil {
		return nil, m.err
	}
	return m.byMode[q.Mode], nil
}

func TestCompareHybridVsDense_RunsBothModesOverTheSameQuestions(t *testing.T) {
	questions := []GoldenQuestion{question("a", uidA, nil), question("b", uidB, nil)}
	s := &modeSearcher{byMode: map[retrieval.SearchMode][]retrieval.Result{
		// Hybrid finds uidA only; dense-only finds both, so dense-only wins 2/2 against 1/2.
		retrieval.SearchModeHybrid:    {result(uidA, 0, 0.9)},
		retrieval.SearchModeDenseOnly: {result(uidA, 0, 0.9), result(uidB, 0, 0.8)},
	}}

	hybrid, dense, err := CompareHybridVsDense(t.Context(), s, questions, runConfig(5))
	if err != nil {
		t.Fatalf("CompareHybridVsDense: %v", err)
	}

	if hybrid.Global.Hits != 1 || hybrid.Global.Total != 2 {
		t.Errorf("hybrid = %d/%d, want 1/2", hybrid.Global.Hits, hybrid.Global.Total)
	}
	if dense.Global.Hits != 2 || dense.Global.Total != 2 {
		t.Errorf("dense-only = %d/%d, want 2/2", dense.Global.Hits, dense.Global.Total)
	}
	if hybrid.Mode != "hybrid" || dense.Mode != "dense-only" {
		t.Errorf("the reports are labelled %q and %q", hybrid.Mode, dense.Mode)
	}
	if hybrid.K != 5 || dense.K != 5 {
		t.Errorf("the two runs used K %d and %d, so the numbers are not comparable", hybrid.K, dense.K)
	}

	// Both modes reached the searcher, twice each: two questions per mode. Without this the whole
	// comparison could be one mode run twice.
	counts := map[retrieval.SearchMode]int{}
	for _, mode := range s.modes {
		counts[mode]++
	}
	if counts[retrieval.SearchModeHybrid] != 2 || counts[retrieval.SearchModeDenseOnly] != 2 {
		t.Errorf("searches per mode: hybrid %d, dense-only %d; want 2 each",
			counts[retrieval.SearchModeHybrid], counts[retrieval.SearchModeDenseOnly])
	}
}

func TestWriteHybridVsDenseReport_StatesDecisionOutcome(t *testing.T) {
	date := time.Date(2026, 8, 11, 9, 30, 0, 0, time.UTC)

	cases := map[string]struct {
		hybridHits, denseHits int
		want, absent          string
	}{
		"dense-only wins": {1, 2, OutcomeDenseWins, OutcomeHybridWins},
		"hybrid wins":     {2, 1, OutcomeHybridWins, OutcomeDenseWins},
		// A tie keeps the hybrid: equal measured recall is no evidence for changing the default
		// PRD-contrato §2.3b specifies.
		"a tie keeps the hybrid": {2, 2, OutcomeHybridWins, OutcomeDenseWins},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			hybrid := Aggregate(hitPattern("alfa", tc.hybridHits, 3-tc.hybridHits))
			hybrid.Mode, hybrid.K = "hybrid", 5
			dense := Aggregate(hitPattern("alfa", tc.denseHits, 3-tc.denseHits))
			dense.Mode, dense.K = "dense-only", 5

			path := filepath.Join(t.TempDir(), "eval", "hybrid-vs-dense.md")
			if err := WriteHybridVsDenseReport(path, hybrid, dense, date); err != nil {
				t.Fatalf("WriteHybridVsDenseReport: %v", err)
			}
			data, err := os.ReadFile(path) // #nosec G304 -- a path under t.TempDir()
			if err != nil {
				t.Fatalf("reading what was written: %v", err)
			}
			doc := string(data)

			if !strings.Contains(doc, tc.want) {
				t.Errorf("the document does not state %q:\n%s", tc.want, doc)
			}
			// The absent half matters as much: a document containing both sentences states nothing,
			// and T11's drift guard parses one of them out of this format.
			if strings.Contains(doc, tc.absent) {
				t.Errorf("the document also states the opposite outcome %q:\n%s", tc.absent, doc)
			}
			for _, want := range []string{"2026-08-11", "hybrid", "dense-only", ConfidenceMethod} {
				if !strings.Contains(doc, want) {
					t.Errorf("the document does not carry %q:\n%s", want, doc)
				}
			}
		})
	}
}

// TestWriteHybridVsDenseReport_RefusesARunThatDidNotFinish is the same rule as everywhere else,
// applied where it is easiest to forget: this document is read later as the evidence for a shipped
// default, and nothing downstream could ever learn from the file that some questions never ran.
func TestWriteHybridVsDenseReport_RefusesARunThatDidNotFinish(t *testing.T) {
	complete := Aggregate(hitPattern("alfa", 3, 0))
	incomplete := Aggregate(append(hitPattern("alfa", 3, 0),
		QuestionResult{Question: question("unreachable", uidA, nil), Error: "qdrant is down"}))
	mismatched := Aggregate(hitPattern("alfa", 2, 0))

	cases := map[string]struct{ hybrid, dense Report }{
		"the hybrid run did not finish":        {incomplete, complete},
		"the dense-only run did not finish":    {complete, incomplete},
		"the two runs measured different sets": {complete, mismatched},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hybrid-vs-dense.md")
			if err := WriteHybridVsDenseReport(path, tc.hybrid, tc.dense, time.Now()); err == nil {
				t.Fatalf("%s and the document was written anyway", name)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Error("the refused document was written to disk")
			}
		})
	}
}

// TestCompareHybridVsDense_SearcherFailureDoesNotProduceADecision closes the loop between the
// runner and the writer: a failing searcher yields two incomplete reports, and the writer refuses
// them, so no decision document can come out of a run that never reached the index.
func TestCompareHybridVsDense_SearcherFailureDoesNotProduceADecision(t *testing.T) {
	s := &modeSearcher{err: errUnreachable}
	hybrid, dense, err := CompareHybridVsDense(t.Context(), s,
		[]GoldenQuestion{question("a", uidA, nil)}, runConfig(5))
	if err != nil {
		t.Fatalf("CompareHybridVsDense: %v", err)
	}
	if hybrid.Complete || dense.Complete {
		t.Fatalf("a run against an unreachable searcher reports complete=%t/%t",
			hybrid.Complete, dense.Complete)
	}
	if werr := WriteHybridVsDenseReport(filepath.Join(t.TempDir(), "d.md"), hybrid, dense, time.Now()); werr == nil {
		t.Error("a decision document was written from a run that reached nothing")
	}
}
