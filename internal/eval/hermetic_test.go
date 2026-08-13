package eval

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/goldenset"
)

// The hermetic fixture: a synthetic corpus and the golden set paired with it, both under testdata/
// at the repository root.
const (
	hermeticGoldenSet = "../../testdata/eval/hermetic/golden-set.yaml"
	hermeticCorpus    = "../../testdata/eval/hermetic/corpus.yaml"
	ciWorkflow        = "../../.github/workflows/ci.yml"

	// hermeticTenant is the tenant the fixture's answerable chunks belong to. The value is decided
	// in cmd/cli/ingest.go (defaultTenantID), not here, and it is spelled again because
	// internal/eval cannot import cmd/cli. TestEvalCmd_CorpusRunNeverDials drives the real command
	// against the same fixture, so a change to defaultTenantID fails there rather than turning this
	// fixture into a silent recall of zero.
	//
	// The corpus also holds an `outro` chunk, which exists to be excluded — see
	// TestHermeticCorpus_TenantScopeIsApplied.
	hermeticTenant = "interno"
)

// hermeticExpectedRecall is what the fixture produces: 6 hits out of 8 questions.
//
// A fixed number and not a floor, per S10 T12's acceptance criterion. It is written as the division
// rather than as 0.75 so the numerator and denominator stay visible — 6/8 is the claim, and a
// fixture edit that changes either one has to change this line too.
const hermeticExpectedRecall = 6.0 / 8.0

func loadHermetic(t *testing.T) ([]goldenset.GoldenQuestion, goldenset.CoverageTable, *CorpusSearcher) {
	t.Helper()
	set, err := goldenset.LoadGoldenSet(hermeticGoldenSet)
	if err != nil {
		t.Fatalf("loading the hermetic golden set: %v", err)
	}
	corpus, err := LoadCorpus(hermeticCorpus)
	if err != nil {
		t.Fatalf("loading the hermetic corpus: %v", err)
	}
	return set.Questions, set.Coverage, NewCorpusSearcher(corpus)
}

func runHermetic(t *testing.T) Report {
	t.Helper()
	questions, _, searcher := loadHermetic(t)
	results, err := RunGolden(t.Context(), searcher, questions, RunConfig{
		Collection: "hermetic", TenantID: hermeticTenant, K: DefaultK,
	})
	if err != nil {
		t.Fatalf("RunGolden over the hermetic fixture: %v", err)
	}
	return Aggregate(results)
}

// TestHermeticGoldenGate_FixtureCorpus_AchievesExpectedRecall is S10 T12: the fixture's recall is a
// fixed, asserted number, not a loose threshold.
//
// Exact equality is the point. A `>=` here would let the fixture drift upward unnoticed, and an
// upward drift is the same defect as a downward one — it means the pair no longer measures what the
// number in the workflow says it measures.
func TestHermeticGoldenGate_FixtureCorpus_AchievesExpectedRecall(t *testing.T) {
	report := runHermetic(t)

	if !report.Complete {
		t.Fatalf("the hermetic run did not finish: %d question(s) could not be asked (%+v)",
			len(report.Errored), report.Errored)
	}
	if report.Global.Hits != 6 || report.Global.Total != 8 {
		t.Fatalf("the fixture scored %d/%d, want 6/8 — the corpus and the golden set have drifted "+
			"apart, and .github/workflows/ci.yml still gates on the old number",
			report.Global.Hits, report.Global.Total)
	}
	if report.Global.Recall() != hermeticExpectedRecall {
		t.Errorf("recall = %v, want exactly %v", report.Global.Recall(), hermeticExpectedRecall)
	}

	// The two misses are misses, not errors. A fixture whose failures were all unaskable questions
	// would never exercise the path that counts a miss, and the day that path broke this gate would
	// report the same green.
	if len(report.Failed) != 2 {
		t.Errorf("%d failed question(s), want 2 — the deliberate misses are what prove the runner "+
			"counts a miss at all", len(report.Failed))
	}
}

// TestHermeticGoldenGate_IsDeterministic is the property the whole gate rests on: same fixture,
// same answer, every run. Anything keyed on map iteration or on slice order would show up here.
func TestHermeticGoldenGate_IsDeterministic(t *testing.T) {
	first, second := runHermetic(t), runHermetic(t)

	if first.Global != second.Global {
		t.Errorf("two runs over the same fixture disagree: %+v vs %+v", first.Global, second.Global)
	}
	if len(first.Failed) != len(second.Failed) {
		t.Fatalf("two runs failed %d and %d question(s)", len(first.Failed), len(second.Failed))
	}
	for i := range first.Failed {
		if first.Failed[i].Question.Question != second.Failed[i].Question.Question {
			t.Errorf("failure %d differs between runs: %q vs %q", i+1,
				first.Failed[i].Question.Question, second.Failed[i].Question.Question)
		}
		if joinUIDs(first.Failed[i].TopK) != joinUIDs(second.Failed[i].TopK) {
			t.Errorf("failure %d came back with different results:\n  %s\n  %s", i+1,
				joinUIDs(first.Failed[i].TopK), joinUIDs(second.Failed[i].TopK))
		}
	}
}

// TestHermeticCorpus_TenantScopeIsApplied is why the corpus carries a second tenant at all.
//
// Its chunk is the best lexical match in the file for the beacon question, so a searcher that
// dropped the tenant condition would rank it first and this would notice. Without a second tenant
// in the fixture, the CI gate would be equally green with the scope filter deleted.
func TestHermeticCorpus_TenantScopeIsApplied(t *testing.T) {
	questions, _, searcher := loadHermetic(t)
	question := questions[0].Question

	scoped, err := searcher.Search(t.Context(), retrievalQuery(question, hermeticTenant))
	if err != nil {
		t.Fatalf("searching as %s: %v", hermeticTenant, err)
	}
	other, err := searcher.Search(t.Context(), retrievalQuery(question, "outro"))
	if err != nil {
		t.Fatalf("searching as the excluded tenant: %v", err)
	}

	if len(other) == 0 {
		t.Fatal("the second tenant's chunk answers nothing, so it cannot prove the scope is applied")
	}
	for _, hit := range scoped {
		for _, foreign := range other {
			if hit.UID == foreign.UID {
				t.Errorf("an %s search returned %s, which belongs to the other tenant", hermeticTenant, hit.UID)
			}
		}
	}
}

// TestHermeticFixture_SatisfiesItsOwnCoverageTable keeps the CI job's output clean of a warning it
// would teach everybody to ignore. Coverage is warn-only at run time (S10 open question 4), so a
// fixture out of range would not fail the gate — it would print a warning on every push forever.
func TestHermeticFixture_SatisfiesItsOwnCoverageTable(t *testing.T) {
	questions, table, _ := loadHermetic(t)
	if err := goldenset.ValidateCoverage(questions, table); err != nil {
		t.Errorf("the hermetic golden set violates the coverage table it declares, so every CI run "+
			"prints a warning nobody can act on: %v", err)
	}
}

// minRecallRe reads the number the workflow gates on.
var minRecallRe = regexp.MustCompile(`--min-recall\s+([0-9.]+)`)

// TestCIWorkflow_HermeticJobGatesOnTheMeasuredRecall is the anchor that keeps the chain from having
// a loose end.
//
// The number 0.75 lives in two files that cannot check each other: a YAML workflow and this test.
// Rather than assert the workflow says "0.75" — which would only prove one string matches another —
// this compares it against the recall the fixture actually produces, computed a few lines up. So a
// fixture edit that moves the recall fails here, and so does a workflow edit that relaxes the gate.
//
// It also asserts --corpus is passed, which is what makes the job hermetic: without it the golden
// gate dials Qdrant (cmd/cli/eval.go, openEvalSearcher) and the job stops being what its name says.
func TestCIWorkflow_HermeticJobGatesOnTheMeasuredRecall(t *testing.T) {
	workflow := readFile(t, ciWorkflow)
	measured := runHermetic(t).Global.Recall()

	match := minRecallRe.FindStringSubmatch(workflow)
	if match == nil {
		t.Fatalf("%s passes no --min-recall, so the hermetic job records a number and gates on "+
			"nothing", ciWorkflow)
	}
	gate, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		t.Fatalf("--min-recall %q in %s is not a number: %v", match[1], ciWorkflow, err)
	}
	if gate != measured {
		t.Errorf("the workflow gates on --min-recall %v; the fixture measures %v. A gate below the "+
			"measured value passes a regression, and one above fails every push", gate, measured)
	}

	for _, want := range []string{
		"--corpus testdata/eval/hermetic/corpus.yaml",
		"--file testdata/eval/hermetic/golden-set.yaml",
	} {
		if !strings.Contains(workflow, want) {
			t.Errorf("the hermetic job does not pass %q; without it the golden gate searches the "+
				"configured Qdrant collection and the job is not hermetic", want)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}
