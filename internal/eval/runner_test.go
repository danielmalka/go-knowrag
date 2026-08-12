package eval

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/retrieval"
)

// fakeSearcher replays a canned answer per call, so a test can hand the same set back in two
// different orders and see whether the runner's verdict depends on the order.
type fakeSearcher struct {
	answers  [][]retrieval.Result
	err      error
	calls    int
	queries  []retrieval.Query
	perQuery map[string][]retrieval.Result
	// errFor makes exactly one question fail, which is how a partial run is built: everything else
	// answers normally and the report has to come back incomplete anyway.
	errFor string
}

func (f *fakeSearcher) Search(_ context.Context, q retrieval.Query) ([]retrieval.Result, error) {
	f.calls++
	f.queries = append(f.queries, q)
	if f.err != nil {
		return nil, f.err
	}
	if f.errFor != "" && q.Text == f.errFor {
		return nil, errors.New("qdrant is unreachable")
	}
	if f.perQuery != nil {
		return f.perQuery[q.Text], nil
	}
	if len(f.answers) == 0 {
		return nil, nil
	}
	answer := f.answers[0]
	if len(f.answers) > 1 {
		f.answers = f.answers[1:]
	}
	return answer, nil
}

func result(uid string, chunk int, score float32) retrieval.Result {
	return retrieval.Result{UID: uid, ChunkIndex: chunk, Score: score, Text: "chunk text", Untrusted: true}
}

func question(text, uid string, chunk *int) GoldenQuestion {
	return GoldenQuestion{Question: text, UID: uid, ChunkIndex: chunk, Area: "alfa", Author: "owner", Date: "2026-08-11"}
}

func intPtr(n int) *int { return &n }

func runConfig(k int) RunConfig {
	return RunConfig{Collection: "interno", TenantID: "tenant-a", K: k}
}

func TestRunGolden_HitAndMiss(t *testing.T) {
	// Five results, descending, with the expected note at rank 5 and a sixth beyond it.
	top5 := []retrieval.Result{
		result(uidA, 0, 0.9), result(uidA, 1, 0.8), result(uidB, 0, 0.7),
		result(uidB, 1, 0.6), result(uidC, 0, 0.5), result(uidC, 1, 0.4),
	}

	cases := map[string]struct {
		question GoldenQuestion
		want     bool
	}{
		"expected uid inside the top 5":           {question("q1", uidC, nil), true},
		"expected uid only beyond rank 5":         {question("q2", uidC, intPtr(1)), false},
		"any chunk counts when none is named":     {question("q3", uidA, nil), true},
		"a named chunk requires that exact chunk": {question("q4", uidA, intPtr(1)), true},
		"a named chunk misses on a different one": {question("q5", uidB, intPtr(9)), false},
		"a uid that never came back is a miss":    {question("q6", "44444444-4444-4444-8444-444444444444", nil), false},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := &fakeSearcher{answers: [][]retrieval.Result{top5}}
			results, err := RunGolden(t.Context(), s, []GoldenQuestion{tc.question}, runConfig(5))
			if err != nil {
				t.Fatalf("RunGolden: %v", err)
			}
			if results[0].Hit != tc.want {
				t.Errorf("hit = %t, want %t (top-5 was %v)", results[0].Hit, tc.want, joinUIDs(results[0].TopK))
			}
			if !results[0].Measured() {
				t.Errorf("the question was not measured: %s", results[0].Error)
			}
		})
	}
}

// TestRunGolden_SearcherErrorIsCapturedAndTheRunContinues is T3's last done-when. The error lands on
// the question that had it, the other questions still run, and — the part that matters more — an
// errored question is not silently a miss: Measured() says it was not asked.
func TestRunGolden_SearcherErrorIsCapturedAndTheRunContinues(t *testing.T) {
	boom := errors.New("qdrant is unreachable")
	s := &fakeSearcher{perQuery: map[string][]retrieval.Result{"ok": {result(uidA, 0, 0.9)}}}
	failing := &fakeSearcher{err: boom}

	good, err := RunGolden(t.Context(), s, []GoldenQuestion{question("ok", uidA, nil)}, runConfig(5))
	if err != nil {
		t.Fatalf("RunGolden: %v", err)
	}
	bad, err := RunGolden(t.Context(), failing,
		[]GoldenQuestion{question("first", uidA, nil), question("second", uidB, nil)}, runConfig(5))
	if err != nil {
		t.Fatalf("RunGolden returned a run-level error for a per-question failure: %v", err)
	}

	if !good[0].Hit {
		t.Error("the healthy question did not register its hit")
	}
	if len(bad) != 2 {
		t.Fatalf("%d result(s) after a searcher failure, want both questions still reported", len(bad))
	}
	for i, res := range bad {
		if res.Measured() {
			t.Errorf("question %d reports as measured despite the searcher failing", i+1)
		}
		if res.Hit {
			t.Errorf("question %d reports a hit from a search that never returned", i+1)
		}
		if !strings.Contains(res.Error, boom.Error()) {
			t.Errorf("question %d's error %q does not carry the cause", i+1, res.Error)
		}
	}
}

// TestRunGolden_AsksTheProductionQuery is the assertion the whole instrument rests on, and the one
// whose absence let a defect through: TopK reaches internal/retrieval/query.go's prefetchLimit, so a
// value larger than K changes the prefetch pool and the fusion width, and the eval measures a
// ranking production never computes.
func TestRunGolden_AsksTheProductionQuery(t *testing.T) {
	s := &fakeSearcher{answers: [][]retrieval.Result{{result(uidA, 0, 0.9)}}}

	if _, err := RunGolden(t.Context(), s, []GoldenQuestion{question("q", uidA, nil)}, runConfig(5)); err != nil {
		t.Fatalf("RunGolden: %v", err)
	}
	if len(s.queries) != 1 {
		t.Fatalf("%d search(es), want 1", len(s.queries))
	}
	if s.queries[0].TopK != 5 {
		t.Errorf("the search asked for TopK %d, want exactly K = 5", s.queries[0].TopK)
	}
	// Offset is pinned for the same reason TopK is, and pinning only TopK was this test's own gap:
	// prefetchLimit is (TopK+Offset)×multiplier, so a non-zero offset widens the prefetch pool by
	// exactly the route the margin used to, and an assertion that watched only TopK would stay green
	// through it.
	if s.queries[0].Offset != 0 {
		t.Errorf("the search asked for offset %d; anything but 0 widens the prefetch pool the same "+
			"way the removed margin did (internal/retrieval/query.go, prefetchLimit)", s.queries[0].Offset)
	}
	// The facets are pinned because a filter the operator's search does not carry makes the eval
	// measure a narrower corpus and call the result Recall@5 — the same class as the TopK defect,
	// arriving through the other half of the query.
	if s.queries[0].Area != "" || s.queries[0].Type != "" || s.queries[0].Vault != "" ||
		len(s.queries[0].Tags) != 0 {
		t.Errorf("the search carried facets %+v; the golden run must ask what production asks",
			s.queries[0])
	}
	if s.queries[0].IncludeArchived || s.queries[0].IncludePrivate {
		t.Errorf("the search widened visibility (archived=%t private=%t); production defaults exclude "+
			"both, so a run that includes them measures a corpus the operator cannot see",
			s.queries[0].IncludeArchived, s.queries[0].IncludePrivate)
	}
	if s.queries[0].TenantID != "tenant-a" || s.queries[0].Collection != "interno" {
		t.Errorf("the search ran under %q/%q, not the RunConfig's scope",
			s.queries[0].Collection, s.queries[0].TenantID)
	}
}

// TestRunGolden_ModeReachesEveryQuery is what makes the hybrid-vs-dense comparison a comparison. If
// RunConfig.Mode never got onto the Query, both halves would measure the same hybrid search and the
// decision doc would report a difference of zero as evidence.
func TestRunGolden_ModeReachesEveryQuery(t *testing.T) {
	cfg := runConfig(5)
	cfg.Mode = retrieval.SearchModeDenseOnly
	s := &fakeSearcher{answers: [][]retrieval.Result{{result(uidA, 0, 0.9)}}}

	if _, err := RunGolden(t.Context(), s, []GoldenQuestion{question("a", uidA, nil), question("b", uidB, nil)}, cfg); err != nil {
		t.Fatalf("RunGolden: %v", err)
	}
	for i, q := range s.queries {
		if q.Mode != retrieval.SearchModeDenseOnly {
			t.Errorf("query %d went out in %s mode, want dense-only", i+1, q.Mode)
		}
	}
}

func TestRunGolden_RefusesToMeasureNothing(t *testing.T) {
	q := []GoldenQuestion{question("q", uidA, nil)}
	s := &fakeSearcher{}

	cases := map[string]struct {
		searcher  Searcher
		questions []GoldenQuestion
		k         int
	}{
		"no searcher":  {nil, q, 5},
		"no questions": {s, nil, 5},
		"zero K":       {s, q, 0},
		"negative K":   {s, q, -1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := RunGolden(t.Context(), tc.searcher, tc.questions,
				RunConfig{Collection: "interno", TenantID: "tenant-a", K: tc.k}); err == nil {
				t.Fatalf("RunGolden with %s returned no error, so a run that measured nothing looks "+
					"like a run", name)
			}
		})
	}
}

// TestRunGolden_NonUUIDIsNotSilentlyRanked covers both directions of the same rule: an expected uid
// that is not a UUID, and a returned point whose uid is not one. Neither can be ranked, and neither
// may be folded into "miss" — a miss says retrieval failed, and these say the harness could not ask.
func TestRunGolden_NonUUIDIsNotSilentlyRanked(t *testing.T) {
	t.Run("expected uid", func(t *testing.T) {
		s := &fakeSearcher{answers: [][]retrieval.Result{{result(uidA, 0, 0.9)}}}
		results, err := RunGolden(t.Context(), s, []GoldenQuestion{question("q", "not-a-uuid", nil)}, runConfig(5))
		if err != nil {
			t.Fatalf("RunGolden: %v", err)
		}
		if results[0].Measured() || results[0].Hit {
			t.Errorf("a question with a non-UUID uid was measured: %+v", results[0])
		}
		if s.calls != 0 {
			t.Error("the searcher was called for a question that could never match anything")
		}
	})

	// A returned point whose uid is not a UUID used to be an error here, because the point-ID
	// tie-break had to parse it. Nothing parses it now — the verdict is a uid comparison — so such a
	// point is simply not the note the question expected. It is a miss, measured, and no error: the
	// eval has no opinion about points it did not ask for.
	t.Run("returned point is simply not a match", func(t *testing.T) {
		s := &fakeSearcher{answers: [][]retrieval.Result{{result("point-7", 0, 0.9)}}}
		results, err := RunGolden(t.Context(), s, []GoldenQuestion{question("q", uidA, nil)}, runConfig(5))
		if err != nil {
			t.Fatalf("RunGolden: %v", err)
		}
		if !results[0].Measured() || results[0].Hit {
			t.Errorf("want a measured miss, got %+v", results[0])
		}
	})
}

// TestRunGolden_ReturnsTheSearchersOrderUntouched is the second half of the correction, and the one
// a TopK assertion alone would miss.
//
// Production hands back Qdrant's order and never reorders it (Search and formatResults in
// internal/retrieval). The runner used to re-sort by (score, point ID), which does not merely
// reorder — over a window wider than K it changes which points are inside the cut. Nothing may
// reorder here now, so the results are asserted position by position against what the searcher
// returned, deliberately in an order no sort would produce.
func TestRunGolden_ReturnsTheSearchersOrderUntouched(t *testing.T) {
	// Ascending score, and uidC before uidA at the same score: both a score sort and a uid sort
	// would rearrange this.
	answer := []retrieval.Result{
		result(uidC, 3, 0.1), result(uidA, 0, 0.1), result(uidB, 0, 0.9),
	}
	// The expectation is frozen as a string, and the searcher gets a copy, before anything runs.
	// Comparing against the same slice the searcher handed out is how this test went vacuous once: a
	// sort inside the runner works on that backing array, so both sides of the comparison move
	// together and any reordering at all passes. A defect plant caught exactly that.
	want := joinUIDs(answer)
	s := &fakeSearcher{answers: [][]retrieval.Result{slices.Clone(answer)}}

	results, err := RunGolden(t.Context(), s, []GoldenQuestion{question("q", uidA, nil)}, runConfig(5))
	if err != nil {
		t.Fatalf("RunGolden: %v", err)
	}
	if got := joinUIDs(results[0].TopK); got != want {
		t.Errorf("the runner reordered the answer:\n  got  %s\n  want %s", got, want)
	}
}

// TestRunGolden_TieAtTheCutIsFlaggedNotSmoothedOver covers the third state that replaced the
// re-sort: a hit that landed on the boundary score is still a hit, and is named as one that may not
// reproduce.
func TestRunGolden_TieAtTheCutIsFlaggedNotSmoothedOver(t *testing.T) {
	cases := map[string]struct {
		answer     []retrieval.Result
		expected   string
		k          int
		wantHit    bool
		wantTied   bool
		wantReason string
	}{
		"expected note scores what the last slot scores": {
			answer:   []retrieval.Result{result(uidA, 0, 0.9), result(uidB, 0, 0.5), result(uidC, 0, 0.5)},
			expected: uidC, k: 3, wantHit: true, wantTied: true,
			wantReason: "the cut landed inside a run of equal scores",
		},
		"expected note is the last slot but scores above it alone": {
			answer:   []retrieval.Result{result(uidA, 0, 0.9), result(uidB, 0, 0.8), result(uidC, 0, 0.7)},
			expected: uidC, k: 3, wantHit: true, wantTied: false,
			wantReason: "no two results share the boundary score",
		},
		"expected note is well above the boundary": {
			answer:   []retrieval.Result{result(uidA, 0, 0.9), result(uidB, 0, 0.5), result(uidC, 0, 0.5)},
			expected: uidA, k: 3, wantHit: true, wantTied: false,
			wantReason: "the expected note does not score what the boundary scores",
		},
		// Fewer results than K means nothing was cut, so there is no boundary to be tied at. Without
		// this the flag would fire on every short answer whose scores happen to match.
		"a short answer has no boundary": {
			answer:   []retrieval.Result{result(uidA, 0, 0.5), result(uidB, 0, 0.5)},
			expected: uidB, k: 5, wantHit: true, wantTied: false,
			wantReason: "fewer than K results came back, so nothing was excluded",
		},
		"a miss is never flagged": {
			answer:   []retrieval.Result{result(uidA, 0, 0.5), result(uidB, 0, 0.5), result(uidB, 1, 0.5)},
			expected: uidC, k: 3, wantHit: false, wantTied: false,
			wantReason: "the expected note is not in the answer at all",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := &fakeSearcher{answers: [][]retrieval.Result{tc.answer}}
			results, err := RunGolden(t.Context(), s, []GoldenQuestion{question("q", tc.expected, nil)}, runConfig(tc.k))
			if err != nil {
				t.Fatalf("RunGolden: %v", err)
			}
			if results[0].Hit != tc.wantHit {
				t.Errorf("hit = %t, want %t", results[0].Hit, tc.wantHit)
			}
			if results[0].Tied != tc.wantTied {
				t.Errorf("tied = %t, want %t: %s", results[0].Tied, tc.wantTied, tc.wantReason)
			}
		})
	}
}

// TestRunGolden_OverAnsweringSearcherCannotWidenTheWindow is the guard on the shape of the defect
// that was just removed: even if something hands back more than K, the verdict is taken over K.
func TestRunGolden_OverAnsweringSearcherCannotWidenTheWindow(t *testing.T) {
	// The expected note sits at rank 6 of an over-long answer: inside the window only if the runner
	// forgets to cut.
	answer := []retrieval.Result{
		result(uidA, 0, 0.9), result(uidA, 1, 0.8), result(uidA, 2, 0.7),
		result(uidA, 3, 0.6), result(uidA, 4, 0.5), result(uidC, 0, 0.4),
	}
	s := &fakeSearcher{answers: [][]retrieval.Result{answer}}

	results, err := RunGolden(t.Context(), s, []GoldenQuestion{question("q", uidC, nil)}, runConfig(5))
	if err != nil {
		t.Fatalf("RunGolden: %v", err)
	}
	if len(results[0].TopK) != 5 {
		t.Errorf("the verdict was taken over %d result(s), want 5", len(results[0].TopK))
	}
	if results[0].Hit {
		t.Error("a note at rank 6 counted as a hit at Recall@5, which is the defect this file was " +
			"corrected for")
	}
}

// cancellingSearcher cancels the run partway through, the way a CI timeout or a Ctrl-C does: the
// context dies between questions rather than before any of them.
type cancellingSearcher struct {
	cancel  context.CancelFunc
	after   int
	calls   int
	answers []retrieval.Result
}

func (c *cancellingSearcher) Search(ctx context.Context, _ retrieval.Query) ([]retrieval.Result, error) {
	c.calls++
	if c.calls == c.after {
		c.cancel()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.answers, nil
}

// TestRunGolden_CancelledRunSaysItWasCancelled is the distinction a reader of a CI failure needs.
//
// Without the ctx check in the loop, the run keeps asking, every remaining search fails against the
// dead context, and the report comes back with six errored questions — which is byte-identical to a
// searcher that broke. Report.Complete already stops that from passing (report.go), so the defect
// is not a false pass; it is that a timeout and a retrieval regression produce the same output.
func TestRunGolden_CancelledRunSaysItWasCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	questions := make([]GoldenQuestion, 0, 8)
	for i := range 8 {
		questions = append(questions, question("question "+string(rune('a'+i)), uidA, nil))
	}
	s := &cancellingSearcher{cancel: cancel, after: 3, answers: []retrieval.Result{result(uidA, 0, 0.9)}}

	results, err := RunGolden(ctx, s, questions, runConfig(5))

	if err == nil {
		t.Fatal("a cancelled run returned no error, so it is indistinguishable from one that finished")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the error does not unwrap to context.Canceled, so no caller can recognise "+
			"cancellation without matching prose: %v", err)
	}
	for _, want := range []string{"cancelled", "of 8", "not a set of failed searches"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error %q does not mention %q", err, want)
		}
	}

	// It stops rather than grinding through the rest. Eight questions, cancelled on the third: the
	// loop must not ask the remaining five and file them as failures.
	if s.calls > 4 {
		t.Errorf("the searcher was called %d time(s) after cancellation on call 3; the run kept "+
			"asking and would report the rest as failed searches", s.calls)
	}
	if len(results) >= len(questions) {
		t.Errorf("%d result(s) for %d question(s): the run did not stop", len(results), len(questions))
	}
}

// TestRunGolden_AlreadyCancelledContextAsksNothing is the boundary the loop check also covers: a
// context dead before the first question must not produce a single search.
func TestRunGolden_AlreadyCancelledContextAsksNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	s := &fakeSearcher{answers: [][]retrieval.Result{{result(uidA, 0, 0.9)}}}
	results, err := RunGolden(ctx, s, []GoldenQuestion{question("q", uidA, nil)}, runConfig(5))

	if err == nil {
		t.Fatal("a run on a dead context reported no error")
	}
	if s.calls != 0 {
		t.Errorf("the searcher was called %d time(s) on an already-cancelled context", s.calls)
	}
	if len(results) != 0 {
		t.Errorf("%d result(s) came back from a run that asked nothing", len(results))
	}
}

// TestRunGolden_LiveContextIsNotTreatedAsCancelled is the absent half. A check written as
// `ctx.Err() == nil` by mistake, or one that fired on every iteration, would stop every run at the
// first question — and the two tests above would still pass.
func TestRunGolden_LiveContextIsNotTreatedAsCancelled(t *testing.T) {
	s := &fakeSearcher{answers: [][]retrieval.Result{{result(uidA, 0, 0.9)}}}
	questions := []GoldenQuestion{question("a", uidA, nil), question("b", uidA, nil), question("c", uidA, nil)}

	results, err := RunGolden(t.Context(), s, questions, runConfig(5))
	if err != nil {
		t.Fatalf("a run on a live context was refused: %v", err)
	}
	if len(results) != len(questions) {
		t.Errorf("%d result(s) for %d question(s) on a live context", len(results), len(questions))
	}
}
