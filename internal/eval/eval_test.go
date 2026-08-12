package eval

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/retrieval"
)

// TestModes_RefuseExplicitly is the rule S06b's report established and this package inherits: not
// having looked is never allowed to render as having found nothing. Neither mode may answer with a
// zero-valued Outcome and a nil error, because the zero value of Passed is false today and would
// become a silent "the gate ran and failed" the moment somebody changed the field's meaning.
//
// The two refuse for different reasons since S10 wired the golden harness, and the difference is
// load-bearing rather than cosmetic — see TestGoldenGate_RefusalIsNotAPendingHarness.
func TestModes_RefuseExplicitly(t *testing.T) {
	tests := map[string]struct {
		run      func(context.Context, Options) (Outcome, error)
		sentinel error
		// mentions is what the refusal has to name. A reader must be able to tell whether the gate
		// is missing, misused or broken without opening the source.
		mentions []string
	}{
		// Isolation has no harness at all; S11 builds it, and the message says so.
		"isolation": {run: IsolationGate, sentinel: ErrNotImplemented,
			mentions: []string{"S11", "--isolation", "Nothing was measured"}},
		// Golden has a harness. Called with no searcher and no golden set, it refuses as a misuse.
		"golden": {run: GoldenGate, sentinel: ErrNoSearcher,
			mentions: []string{"Nothing was measured"}},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			outcome, err := tc.run(context.Background(), Options{Collection: "interno", TenantID: "tenant-a"})
			if err == nil {
				t.Fatalf("the %s mode returned %+v and no error, which reads as a gate that ran", name, outcome)
			}
			if !errors.Is(err, tc.sentinel) {
				t.Errorf("the refusal does not carry %v, so no caller can recognise it without "+
					"matching prose: %v", tc.sentinel, err)
			}
			if outcome.Passed {
				t.Error("the refusal came back with Passed set, which is the one thing it must never say")
			}
			for _, want := range tc.mentions {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not mention %q", err, want)
				}
			}
		})
	}
}

// TestGoldenGate_RefusalIsNotAPendingHarness is the whole reason ErrNoSearcher exists as a separate
// value, and it guards a green CI job over a gate that measured nothing.
//
// scripts/ci/eval-gate.sh recognises ErrNotImplemented as "pending, warn and exit 0". The golden
// harness is not pending — S10 built it and cmd/cli/eval.go wires it — so a golden run that fails
// for any reason at all must be a failure. If GoldenGate answered with ErrNotImplemented, a broken
// wiring would print a warning and pass.
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

// TestModes_RefusalsAreDistinguishable covers the half a shared sentinel hides: both modes answer
// with the same error value, so the message is the only thing that says which gate is missing, and
// a reader who ran --isolation must not be sent to read S10's story.
func TestModes_RefusalsAreDistinguishable(t *testing.T) {
	golden, gerr := GoldenGate(context.Background(), Options{})
	isolation, ierr := IsolationGate(context.Background(), Options{})

	if gerr.Error() == ierr.Error() {
		t.Errorf("both modes refuse with the same sentence, so neither says which gate is missing: %v", gerr)
	}
	if golden != isolation {
		t.Errorf("the two refusals returned different Outcomes (%+v vs %+v); both must be the zero "+
			"value, because neither measured anything", golden, isolation)
	}
}

// TestOutcome_ScoreIsAbsentNotZero pins the one field shape S10 and S11 must not collapse.
//
// S11's task document requires its report to carry no numeric score anywhere: a tenant-isolation
// suite that could be reported as 90% passing is one somebody can argue down. A float64 would make
// "no score" indistinguishable from "scored zero", which is the worst possible reading of an
// isolation run. The pointer is what keeps the absence sayable.
func TestOutcome_ScoreIsAbsentNotZero(t *testing.T) {
	zero := 0.0
	scored := Outcome{Score: &zero}
	unscored := Outcome{}

	if unscored.Score != nil {
		t.Error("an Outcome with no score carries one")
	}
	if scored.Score == nil || *scored.Score != 0 {
		t.Error("a score of zero is not representable, so a gate that scored zero reports as unscored")
	}
}

// goldenGateFixture is a golden set committed to a throwaway repository, which is what GoldenGate
// needs to resolve provenance. It returns the path and the questions in it.
func goldenGateFixture(t *testing.T) (string, []GoldenQuestion) {
	t.Helper()
	repo := newGitRepo(t)
	first := GoldenQuestion{Question: "the first gate question", UID: uidA, Area: "alfa"}
	second := GoldenQuestion{Question: "the second gate question", UID: uidB, Area: "alfa"}

	body := fixtureCoverage + "questions:\n" +
		entry(first.Question, first.UID, "alfa") + entry(second.Question, second.UID, "alfa")
	repo.commit(body, "add the golden set")
	return repo.path, []GoldenQuestion{first, second}
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
	if !errors.Is(err, ErrGoldenSetMissing) {
		t.Errorf("the error %v is not ErrGoldenSetMissing", err)
	}
	if outcome.Passed || outcome.Score != nil {
		t.Errorf("the refusal carries a verdict: %+v", outcome)
	}
}
