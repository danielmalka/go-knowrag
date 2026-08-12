package eval

import (
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/retrieval"
)

// hitPattern builds hits hits and misses misses in one area. Each question's text is unique so a
// rendered report can be searched for one of them.
func hitPattern(area string, hits, misses int) []QuestionResult {
	var out []QuestionResult
	for i := range hits {
		q := question(area+" hit "+string(rune('a'+i)), uidA, nil)
		q.Area = area
		out = append(out, QuestionResult{Question: q, Hit: true, TopK: []retrieval.Result{result(uidA, 0, 0.9)}})
	}
	for i := range misses {
		q := question(area+" miss "+string(rune('a'+i)), uidB, nil)
		q.Area = area
		out = append(out, QuestionResult{Question: q, TopK: []retrieval.Result{result(uidC, 2, 0.9)}})
	}
	return out
}

func TestAggregate_GlobalAndPerArea(t *testing.T) {
	results := append(hitPattern("alfa", 3, 1), hitPattern("beta", 1, 3)...)

	r := Aggregate(results)

	if r.Global.Hits != 4 || r.Global.Total != 8 {
		t.Errorf("global = %d/%d, want 4/8", r.Global.Hits, r.Global.Total)
	}
	if r.Global.Recall() != 0.5 {
		t.Errorf("global recall = %v, want 0.5", r.Global.Recall())
	}
	if got := r.PerArea["alfa"]; got.Hits != 3 || got.Total != 4 {
		t.Errorf("alfa = %d/%d, want 3/4", got.Hits, got.Total)
	}
	if got := r.PerArea["beta"]; got.Hits != 1 || got.Total != 4 {
		t.Errorf("beta = %d/%d, want 1/4", got.Hits, got.Total)
	}
	if len(r.Failed) != 4 {
		t.Errorf("%d failed question(s) retained, want 4", len(r.Failed))
	}
	if !r.Complete {
		t.Error("a run where every question was asked reports as incomplete")
	}
	// The failures keep what the report needs: expected uid and the actual top-K, so nothing has to
	// be re-run to render them.
	if len(r.Failed[0].TopK) == 0 || r.Failed[0].Question.UID == "" {
		t.Errorf("a failed result lost its expected/actual pair: %+v", r.Failed[0])
	}
}

// TestAggregate_ErroredQuestionsAreNotMisses is the rule the whole package is built around, applied
// to the numbers: a question that could not be asked was not measured, so counting it as a miss
// would report an unreachable Qdrant as a retrieval failure — and, worse, a run that reached
// nothing would report recall 0/N and look measured.
func TestAggregate_ErroredQuestionsAreNotMisses(t *testing.T) {
	results := hitPattern("alfa", 3, 1)
	results = append(results, QuestionResult{Question: question("unreachable", uidA, nil), Error: "qdrant is down"})

	r := Aggregate(results)

	if r.Global.Total != 4 {
		t.Errorf("global denominator = %d, want 4 — the errored question is not a measurement",
			r.Global.Total)
	}
	if r.Complete {
		t.Error("a run with an unaskable question reports as complete, so a gate could pass on it")
	}
	if len(r.Errored) != 1 {
		t.Fatalf("%d errored question(s) retained, want 1", len(r.Errored))
	}
	for _, f := range r.Failed {
		if f.Error != "" {
			t.Errorf("the errored question was also filed as a failure: %+v", f)
		}
	}
}

func TestRenderReport_ListsFailedQuestionsWithExpectedAndActual(t *testing.T) {
	r := Aggregate(hitPattern("alfa", 1, 1))
	r.K, r.Mode = 5, "hybrid"

	out := RenderReport(r)

	for _, want := range []string{
		"alfa miss a", // the question text
		uidB,          // the expected uid
		uidC,          // the uid that actually came back
		"1/2",         // numerator/denominator
		ConfidenceMethod,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not contain %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "Recall@5") {
		t.Errorf("the report does not name the cut-off it measured:\n%s", out)
	}
	if !strings.Contains(out, "| Area |") {
		t.Errorf("the report has no per-area table:\n%s", out)
	}
}

// TestRenderReport_NamesTheMethodWhereverBoundsAppear is why ConfidenceMethod exists. A bound with
// no method and no level is a number with no procedure behind it, and a reader cannot tell 95%
// Wilson from 99% normal by looking at 0.6698.
func TestRenderReport_NamesTheMethodWhereverBoundsAppear(t *testing.T) {
	r := Aggregate(append(hitPattern("alfa", 3, 1), hitPattern("beta", 2, 2)...))
	out := RenderReport(r)

	// Once in the global line and once in the per-area table header — two places bounds are printed.
	if n := strings.Count(out, ConfidenceMethod); n < 2 {
		t.Errorf("%q appears %d time(s); bounds are printed in the global line and in the per-area "+
			"table, and both have to be labelled:\n%s", ConfidenceMethod, n, out)
	}
}

func TestRenderReport_EmptyFailureListSaysSo(t *testing.T) {
	r := Aggregate(hitPattern("alfa", 3, 0))
	r.K = 5

	out := RenderReport(r)
	if !strings.Contains(out, "No failures") {
		t.Errorf("a clean run does not render a no-failures line:\n%s", out)
	}
	if strings.Contains(out, "expected uid") {
		t.Errorf("a clean run rendered a failure entry:\n%s", out)
	}
}

// TestRenderReport_IncompleteRunCannotBeMisreadAsAPass is the rendered half of the same rule
// Aggregate enforces on the numbers. An operator reading only the recall line must not come away
// thinking the golden set was measured.
func TestRenderReport_IncompleteRunCannotBeMisreadAsAPass(t *testing.T) {
	results := append(hitPattern("alfa", 4, 0),
		QuestionResult{Question: question("unreachable", uidA, nil), Error: "qdrant is down"})

	out := RenderReport(Aggregate(results))

	for _, want := range []string{"INCOMPLETE RUN", "not a passing evaluation", "qdrant is down"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report of an incomplete run does not say %q:\n%s", want, out)
		}
	}
}

// TestRenderReport_UnresolvedProvenanceIsStatedNotOmitted keeps the report from having a clean-
// looking provenance section when nobody looked. An absent hash printed as nothing is
// indistinguishable from an entry list that happened to be empty.
func TestRenderReport_UnresolvedProvenanceIsStatedNotOmitted(t *testing.T) {
	unresolved := RenderReport(Aggregate(hitPattern("alfa", 2, 0)))
	if !strings.Contains(unresolved, "not resolved") {
		t.Errorf("a report with no golden-set commit does not say so:\n%s", unresolved)
	}

	r := Aggregate(hitPattern("alfa", 2, 0))
	r.GoldenSetCommit = "abc1234"
	r.Stale = []StaleEntry{{Identity: "deadbeef", Question: "a late question", Reason: "introduced after the baseline"}}

	resolved := RenderReport(r)
	for _, want := range []string{"abc1234", "a late question", "introduced after the baseline"} {
		if !strings.Contains(resolved, want) {
			t.Errorf("the report does not carry %q:\n%s", want, resolved)
		}
	}
	if strings.Contains(resolved, "No entry postdates") {
		t.Errorf("a report with flagged entries also claims none:\n%s", resolved)
	}
}

func TestRenderReport_CoverageWarningIsCarried(t *testing.T) {
	r := Aggregate(hitPattern("alfa", 2, 0))
	r.CoverageWarning = `area "beta" has 0 question(s), below the minimum of 4`

	out := RenderReport(r)
	if !strings.Contains(out, "below the minimum of 4") {
		t.Errorf("the coverage warning did not reach the report:\n%s", out)
	}
	if !strings.Contains(RenderReport(Aggregate(hitPattern("alfa", 2, 0))), "## Per area") {
		t.Error("the per-area section vanished when there was no warning")
	}
	if strings.Contains(RenderReport(Aggregate(hitPattern("alfa", 2, 0))), "Coverage warning") {
		t.Error("a report with no coverage problem renders a coverage-warning section anyway")
	}
}

// TestRecallStat_ZeroDenominatorIsNotZeroRecall guards the division. Recall() answering 0 for an
// empty stat is unavoidable arithmetic; what must not happen is that value reading as a
// measurement, which is why Complete and the rendered text carry that meaning instead.
func TestRecallStat_ZeroDenominatorIsNotZeroRecall(t *testing.T) {
	if got := (RecallStat{}).Recall(); got != 0 {
		t.Errorf("an empty RecallStat divided by zero: %v", got)
	}
	empty := Aggregate(nil)
	if empty.Global.Lo != 0 || empty.Global.Hi != 1 {
		t.Errorf("a run that measured nothing reports the interval (%.4f, %.4f); with no "+
			"observations the honest answer is the whole unit interval", empty.Global.Lo, empty.Global.Hi)
	}
}

// tiedHit is a hit whose place was decided among equals, as RunGolden marks it.
func tiedHit(area, text string) QuestionResult {
	q := question(text, uidA, nil)
	q.Area = area
	return QuestionResult{Question: q, Hit: true, Tied: true,
		TopK: []retrieval.Result{result(uidB, 0, 0.5), result(uidA, 0, 0.5)}}
}

// TestAggregate_TiedHitsCountAndAreNamed pins the decision the tie flag encodes: a tied hit is a
// hit, because it is what a search returns today and the recall has to be production's number — and
// it is listed, because that part of the recall may not survive the next run against an unchanged
// index.
func TestAggregate_TiedHitsCountAndAreNamed(t *testing.T) {
	results := append(hitPattern("alfa", 2, 1), tiedHit("alfa", "a hit decided by a coin toss"))

	r := Aggregate(results)

	if r.Global.Hits != 3 || r.Global.Total != 4 {
		t.Errorf("global = %d/%d, want 3/4 — a tied hit is still a hit", r.Global.Hits, r.Global.Total)
	}
	if len(r.Tied) != 1 {
		t.Fatalf("%d tied hit(s) retained, want 1", len(r.Tied))
	}
	if r.Tied[0].Question.Question != "a hit decided by a coin toss" {
		t.Errorf("the wrong result was filed as tied: %+v", r.Tied[0])
	}
	// A tied hit is not also a failure. Filing it in both would double-count it in the report.
	for _, f := range r.Failed {
		if f.Tied {
			t.Errorf("a tied hit was also filed as a failure: %+v", f)
		}
	}
	// And an ordinary run carries no tie section at all.
	if len(Aggregate(hitPattern("alfa", 2, 1)).Tied) != 0 {
		t.Error("a run with no tied hits reported some")
	}
}

func TestRenderReport_NamesTiedHits(t *testing.T) {
	r := Aggregate(append(hitPattern("alfa", 2, 0), tiedHit("beta", "the tied question")))
	r.K = 5

	out := RenderReport(r)
	for _, want := range []string{"tie at rank 5", "the tied question", "not reproducible"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not carry %q:\n%s", want, out)
		}
	}

	// The absent half: a run with no ties must not print the section, or a reader learns to skip a
	// heading that is always there.
	clean := RenderReport(Aggregate(hitPattern("alfa", 2, 0)))
	if strings.Contains(clean, "tie at rank") {
		t.Errorf("a report with no tied hits rendered the tie section anyway:\n%s", clean)
	}
}
