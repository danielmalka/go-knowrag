package eval

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/goldenset"
	"github.com/danielmalka/go-knowrag/internal/retrieval"
)

// TestGoldenGate_RefusesExplicitly is the rule S06b's report established and this package inherits:
// not having looked is never allowed to render as having found nothing. A gate that could not run
// must not answer with a zero-valued Outcome and a nil error, because the zero value of Passed is
// false today and would become a silent "the gate ran and failed" the moment somebody changed the
// field's meaning.
//
// Golden alone, because it holds the only refusal left. Both harnesses exist — S10 wired this one,
// S11 the isolation suite — so neither gate is pending, and the only way to reach a refusal here is
// to call GoldenGate with nothing to search. The isolation side is TestIsolationGate_Measures.
func TestGoldenGate_RefusesExplicitly(t *testing.T) {
	outcome, err := GoldenGate(context.Background(), Options{Collection: "interno", TenantID: "tenant-a"})

	if err == nil {
		t.Fatalf("the golden mode returned %+v and no error, which reads as a gate that ran", outcome)
	}
	if !errors.Is(err, ErrNoSearcher) {
		t.Errorf("the refusal does not carry ErrNoSearcher, so no caller can recognise it without "+
			"matching prose: %v", err)
	}
	if outcome.Passed {
		t.Error("the refusal came back with Passed set, which is the one thing it must never say")
	}
	if !strings.Contains(err.Error(), "Nothing was measured") {
		t.Errorf("the refusal %q does not say that nothing was measured", err)
	}
}

// TestGoldenGate_RefusalIsNotAPendingHarness is the whole reason ErrNoSearcher exists as a separate
// value, and it guards a green CI job over a gate that measured nothing.
//
// scripts/ci/eval-gate.sh recognises ErrNotImplemented as "pending, warn and exit 0" for any mode on
// its pending list. The golden harness is not pending — S10 built it and cmd/cli/eval.go wires it —
// so a golden run that fails for any reason at all must be a failure. If GoldenGate answered with
// ErrNotImplemented, adding a mode to that list would be enough to turn a broken wiring green.
func TestGoldenGate_RefusalIsNotAPendingHarness(t *testing.T) {
	_, err := GoldenGate(context.Background(), Options{Collection: "interno", TenantID: "tenant-a"})
	if err == nil {
		t.Fatal("GoldenGate with no searcher returned no error")
	}
	if errors.Is(err, ErrNotImplemented) {
		t.Errorf("GoldenGate answers with ErrNotImplemented (%v), which scripts/ci/eval-gate.sh "+
			"reads as a pending harness and exits 0 on. The golden harness exists; a golden gate "+
			"that cannot run is a failure, not a pending story", err)
	}
	// And the two sentinels are genuinely distinct values, not one aliased to the other — which is
	// the way the assertion above would go vacuous.
	if errors.Is(ErrNoSearcher, ErrNotImplemented) || errors.Is(ErrNotImplemented, ErrNoSearcher) {
		t.Error("ErrNoSearcher and ErrNotImplemented match each other, so telling them apart is " +
			"impossible for the script and for every caller")
	}
}

// The claim that the two modes' refusals are distinguishable was asserted here, by comparing the two
// error messages. It cannot be: only one mode refuses now, and the test dereferenced the other one's
// error, so with the isolation suite in place it panics rather than fails. What replaced it is
// narrower and still runnable — the isolation gate must not produce the pending sentinel at all,
// below.

// TestIsolationGate_Measures is the other half, and it is the one that changed: the isolation gate
// has a suite now, so it answers with a verdict instead of a refusal.
func TestIsolationGate_Measures(t *testing.T) {
	outcome, err := IsolationGate(context.Background(), Options{Collection: "clientes", TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("IsolationGate: %v", err)
	}
	if outcome.Mode != "isolation" {
		t.Errorf("Mode = %q, want %q", outcome.Mode, "isolation")
	}
	if !outcome.Passed {
		t.Errorf("the isolation suite did not pass on a clean build:\n%s", outcome.Summary)
	}
	// The one field shape S11's task document makes a requirement: no score, ever. An isolation
	// suite reportable as a percentage is one somebody argues down before a release.
	if outcome.Score != nil {
		t.Errorf("the isolation gate reported a score (%v); this suite has no partial credit", *outcome.Score)
	}
	if outcome.Summary == "" {
		t.Error("the isolation gate reported no summary, so an operator sees a verdict and no cases")
	}
}

// TestIsolationGate_NeverReportsAPendingHarness is the guard the CI script depends on.
//
// scripts/ci/eval-gate.sh treats eval.ErrNotImplemented's message as "pending, warn and exit 0".
// The isolation suite exists now, so that string must not reach a caller from this path at all — an
// isolation gate that failed and printed it would go green in CI with nothing measured.
func TestIsolationGate_NeverReportsAPendingHarness(t *testing.T) {
	outcome, err := IsolationGate(context.Background(), Options{})
	if err != nil && errors.Is(err, ErrNotImplemented) {
		t.Fatalf("the isolation gate answers with ErrNotImplemented: %v", err)
	}
	if strings.Contains(outcome.Summary, ErrNotImplemented.Error()) {
		t.Errorf("the isolation summary carries %q, which the CI script reads as a pending harness:\n%s",
			ErrNotImplemented, outcome.Summary)
	}
}

// The "score is absent, not zero" claim used to be asserted here, by building two Outcome values by
// hand and checking that a Go pointer can be nil. That test could not fail at run time: it called
// nothing, and the only thing that would have broken it was a type change that stops this package
// compiling. "The defect is not representable" and "this test is worth running" are different
// claims, and only the first one was true of it.
//
// The claim that is worth running is the round trip, because `"score": 0` and `"score": null` are
// what a gate script actually reads, and it lives where those bytes are produced:
// TestEvalCmd_JSON_ScoreIsAbsentNotZero in cmd/cli/eval_test.go.

// goldenGateFixture is a golden set committed to a throwaway repository, which is what GoldenGate
// needs to resolve provenance. It returns the path and the questions in it.
func goldenGateFixture(t *testing.T) (string, []goldenset.GoldenQuestion) {
	t.Helper()
	repo := newGitRepo(t)
	first := goldenset.GoldenQuestion{Question: "the first gate question", UID: uidA, Area: "alfa"}
	second := goldenset.GoldenQuestion{Question: "the second gate question", UID: uidB, Area: "alfa"}

	body := fixtureCoverage + "questions:\n" +
		entry(first.Question, first.UID, "alfa") + entry(second.Question, second.UID, "alfa")
	repo.commit(body, "add the golden set")
	return repo.path, []goldenset.GoldenQuestion{first, second}
}

// TestGoldenGate_MeasuresAgainstTheThreshold is the wiring S09 left the seam for: the CLI-facing
// gate calls the harness and renders it into the Outcome cmd/cli reads.
func TestGoldenGate_MeasuresAgainstTheThreshold(t *testing.T) {
	path, _ := goldenGateFixture(t)
	// One hit out of two, so recall is 0.5 and the two thresholds below straddle it.
	s := &fakeSearcher{answers: [][]retrieval.Result{
		{result(uidA, 0, 0.9)}, {result(uidC, 0, 0.9)},
	}}

	below, err := GoldenGate(t.Context(), Options{
		Collection: "interno", TenantID: "tenant-a", Searcher: s, GoldenSetPath: path, MinRecall: 0.8,
	})
	if err != nil {
		t.Fatalf("GoldenGate: %v", err)
	}
	if below.Passed {
		t.Error("recall 0.5 passed a 0.8 threshold")
	}
	if below.Score == nil || *below.Score != 0.5 {
		t.Errorf("Score = %v, want 0.5 — the golden gate has a number and must report it", below.Score)
	}
	if below.Mode != "golden" {
		t.Errorf("Mode = %q, want %q", below.Mode, "golden")
	}
	for _, want := range []string{"1/2", ConfidenceMethod, "the second gate question"} {
		if !strings.Contains(below.Summary, want) {
			t.Errorf("the summary does not carry %q:\n%s", want, below.Summary)
		}
	}

	s2 := &fakeSearcher{answers: [][]retrieval.Result{{result(uidA, 0, 0.9)}, {result(uidB, 0, 0.9)}}}
	above, err := GoldenGate(t.Context(), Options{
		Collection: "interno", TenantID: "tenant-a", Searcher: s2, GoldenSetPath: path, MinRecall: 0.8,
	})
	if err != nil {
		t.Fatalf("GoldenGate: %v", err)
	}
	if !above.Passed || *above.Score != 1 {
		t.Errorf("recall 1.0 did not pass a 0.8 threshold: %+v", above)
	}
}

// TestGoldenGate_UnsetKMeasuresRecallAt5 pins the cut-off a gate with no explicit K measures at.
//
// It is behavioural rather than a check that the summary says "Recall@5", because the text check
// stayed green through a plant that changed DefaultK to 500 — the fake returned two results either
// way, so no assertion could tell the two cut-offs apart. Here the expected note sits at rank 6 of
// six equally-plausible results: a hit at Recall@6 and above, a miss at Recall@5. A gate quietly
// measuring at a wider cut-off reports a recall the acceptance criterion never asked for, and that
// is a number nobody can compare against the 0.80 gate S12 applies.
func TestGoldenGate_UnsetKMeasuresRecallAt5(t *testing.T) {
	if DefaultK != 5 {
		t.Errorf("DefaultK = %d; every acceptance criterion in this project is written against "+
			"Recall@5", DefaultK)
	}

	path, questions := goldenGateFixture(t)
	// Six descending results per question, with each question's expected note last. Descending and
	// all distinct, so no tie is involved and the only thing deciding hit/miss is where K cuts.
	sixth := func(expected string) []retrieval.Result {
		return []retrieval.Result{
			result(uidC, 0, 0.9), result(uidC, 1, 0.8), result(uidC, 2, 0.7),
			result(uidC, 3, 0.6), result(uidC, 4, 0.5), result(expected, 0, 0.4),
		}
	}
	s := &fakeSearcher{perQuery: map[string][]retrieval.Result{
		questions[0].Question: sixth(questions[0].UID),
		questions[1].Question: sixth(questions[1].UID),
	}}

	outcome, err := GoldenGate(t.Context(), Options{
		Collection: "interno", TenantID: "tenant-a", Searcher: s, GoldenSetPath: path,
	})
	if err != nil {
		t.Fatalf("GoldenGate: %v", err)
	}
	if outcome.Score == nil || *outcome.Score != 0 {
		t.Errorf("Score = %v, want 0: the expected note is at rank 6 of both answers, which is a "+
			"miss at Recall@5 and a hit at any wider cut-off", outcome.Score)
	}
	if !strings.Contains(outcome.Summary, "Recall@5") {
		t.Errorf("the report does not name the cut-off it measured:\n%s", outcome.Summary)
	}
}

// TestGoldenGate_IncompleteRunCannotPass is the rule that makes the threshold mean something. Two
// questions where one is unaskable gives recall 1/1 over what answered — a number that clears every
// threshold while half the golden set was never measured.
func TestGoldenGate_IncompleteRunCannotPass(t *testing.T) {
	path, _ := goldenGateFixture(t)
	s := &fakeSearcher{perQuery: map[string][]retrieval.Result{
		"the first gate question": {result(uidA, 0, 0.9)},
	}, errFor: "the second gate question"}

	outcome, err := GoldenGate(t.Context(), Options{
		Collection: "interno", TenantID: "tenant-a", Searcher: s, GoldenSetPath: path, MinRecall: 0.0,
	})
	if err != nil {
		t.Fatalf("GoldenGate: %v", err)
	}
	if outcome.Passed {
		t.Error("a run that could not ask half its questions passed a threshold of zero, which is " +
			"an evaluation that did not run rendering as one that passed")
	}
	if !strings.Contains(outcome.Summary, "INCOMPLETE RUN") {
		t.Errorf("the summary does not say the run was incomplete:\n%s", outcome.Summary)
	}
}

// TestGoldenGate_RecordsTheGoldenSetCommit is the provenance half of AC2, at the level the gate is
// responsible for: a report that cannot say which version of the golden set produced its number is
// a number nobody can reproduce.
func TestGoldenGate_RecordsTheGoldenSetCommit(t *testing.T) {
	path, _ := goldenGateFixture(t)
	s := &fakeSearcher{answers: [][]retrieval.Result{{result(uidA, 0, 0.9)}}}

	outcome, err := GoldenGate(t.Context(), Options{
		Collection: "interno", TenantID: "tenant-a", Searcher: s, GoldenSetPath: path,
	})
	if err != nil {
		t.Fatalf("GoldenGate: %v", err)
	}
	if strings.Contains(outcome.Summary, "not resolved") {
		t.Errorf("the gate did not resolve the golden-set commit for a committed file:\n%s", outcome.Summary)
	}
	if !strings.Contains(outcome.Summary, "Golden-set commit: ") {
		t.Errorf("the summary carries no golden-set commit:\n%s", outcome.Summary)
	}
}

// TestGoldenGate_CoverageIsAWarningNotAGate is S10 open question 4, decided: a temporarily
// out-of-range golden set must not stop somebody from measuring recall, and must not be silent
// about it either.
func TestGoldenGate_CoverageIsAWarningNotAGate(t *testing.T) {
	path, _ := goldenGateFixture(t)
	// fixtureCoverage requires a minimum of 1 in both alfa and beta; the fixture has no beta entry.
	s := &fakeSearcher{answers: [][]retrieval.Result{{result(uidA, 0, 0.9)}, {result(uidB, 0, 0.9)}}}

	outcome, err := GoldenGate(t.Context(), Options{
		Collection: "interno", TenantID: "tenant-a", Searcher: s, GoldenSetPath: path, MinRecall: 0.8,
	})
	if err != nil {
		t.Fatalf("a coverage shortfall failed the run: %v", err)
	}
	if !outcome.Passed {
		t.Error("a coverage shortfall failed the gate; coverage is an authoring concern")
	}
	if !strings.Contains(outcome.Summary, "Coverage warning") || !strings.Contains(outcome.Summary, `"beta"`) {
		t.Errorf("the coverage shortfall was silent:\n%s", outcome.Summary)
	}
}

// TestGoldenGate_MissingGoldenSetIsNotAPass covers the loader's error reaching the gate intact.
func TestGoldenGate_MissingGoldenSetIsNotAPass(t *testing.T) {
	outcome, err := GoldenGate(t.Context(), Options{
		Collection: "interno", TenantID: "tenant-a",
		Searcher: &fakeSearcher{}, GoldenSetPath: filepath.Join(t.TempDir(), "nope.yaml"),
	})
	if err == nil {
		t.Fatal("a missing golden set produced an outcome")
	}
	if !errors.Is(err, goldenset.ErrGoldenSetMissing) {
		t.Errorf("the error %v is not goldenset.ErrGoldenSetMissing", err)
	}
	if outcome.Passed || outcome.Score != nil {
		t.Errorf("the refusal carries a verdict: %+v", outcome)
	}
}

// TestIsolationGate_ReportsAFailingSuite closes the last link: a failing case has to reach the
// process exit through the gate, not only through the suite's own Report.
//
// It is driven by the architecture case's real precondition — a working directory with no go.mod
// above it — because that is the one suite failure reachable end to end without editing production
// code. It doubles as the proof that a deployed host with no source tree does not quietly report a
// boundary nobody checked.
func TestIsolationGate_ReportsAFailingSuite(t *testing.T) {
	t.Chdir(t.TempDir())

	outcome, err := IsolationGate(context.Background(), Options{})
	if err != nil {
		t.Fatalf("IsolationGate: %v", err)
	}
	if outcome.Passed {
		t.Error("the gate reported a pass from a run where the architecture boundary could not be " +
			"scanned at all")
	}
	if !strings.Contains(outcome.Summary, "FAIL") {
		t.Errorf("the summary does not report the failure:\n%s", outcome.Summary)
	}
	if outcome.Score != nil {
		t.Error("a failing isolation run carries a score")
	}
}
