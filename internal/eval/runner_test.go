package eval

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/danielmalka/go-knowrag/internal/retrieval"
	"github.com/danielmalka/go-knowrag/internal/schema"
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

// TestRunGolden_TiedScores_TieBreakIsStableAcrossRuns is S10 T3's RED test.
//
// Six results all scoring 0.5 with K=5: which five make the cut is decided entirely by the
// tie-break, so a runner that kept the arrival order would answer differently for the two orders
// below. Reversing the input is what makes the assertion mean "stable", not "the fake is
// deterministic".
func TestRunGolden_TiedScores_TieBreakIsStableAcrossRuns(t *testing.T) {
	tied := []retrieval.Result{
		result(uidA, 0, 0.5), result(uidA, 1, 0.5), result(uidB, 0, 0.5),
		result(uidB, 1, 0.5), result(uidC, 0, 0.5), result(uidC, 1, 0.5),
	}
	reversed := slices.Clone(tied)
	slices.Reverse(reversed)

	q := question("which note covers the restart procedure", uidC, intPtr(1))
	s := &fakeSearcher{answers: [][]retrieval.Result{tied, reversed}}

	first, err := RunGolden(t.Context(), s, []GoldenQuestion{q}, runConfig(5))
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	second, err := RunGolden(t.Context(), s, []GoldenQuestion{q}, runConfig(5))
	if err != nil {
		t.Fatalf("second run: %v", err)
	}

	if first[0].Hit != second[0].Hit {
		t.Errorf("the two runs disagree on hit/miss (%t vs %t) over the same six tied results",
			first[0].Hit, second[0].Hit)
	}
	if len(first[0].TopK) != 5 || len(second[0].TopK) != 5 {
		t.Fatalf("top-K lengths %d and %d, want 5 each", len(first[0].TopK), len(second[0].TopK))
	}
	for i := range first[0].TopK {
		if first[0].TopK[i] != second[0].TopK[i] {
			t.Errorf("rank %d differs between runs: %+v vs %+v", i+1, first[0].TopK[i], second[0].TopK[i])
		}
	}

	// The tie-break is not a stable sort over arrival order: it is a function of the point IDs, so
	// the sixth result — whichever one it is — is the same one both times.
	if first[0].TopK[4] != second[0].TopK[4] {
		t.Error("the result that counted as rank 5 is not the same across the two runs")
	}
}

// TestSortDeterministically_OrdersByScoreThenPointID proves what the tie-break actually is, which
// the test above cannot: it would pass over any deterministic order at all, including arrival order
// from a fake that happens to be consistent.
func TestSortDeterministically_OrdersByScoreThenPointID(t *testing.T) {
	hits := []retrieval.Result{result(uidA, 0, 0.1), result(uidB, 0, 0.9), result(uidC, 0, 0.9)}

	ordered, err := sortDeterministically(hits, "tenant-a")
	if err != nil {
		t.Fatalf("sortDeterministically: %v", err)
	}
	if ordered[2].Score != 0.1 {
		t.Errorf("the lowest score is at rank %d, want last — the sort is not score-descending",
			slices.IndexFunc(ordered, func(r retrieval.Result) bool { return r.Score == 0.1 })+1)
	}

	// Between the two 0.9s the winner is the smaller point ID, computed here the same way the runner
	// computes it — schema.PointID(tenant, uid, chunkIndex), internal/schema/identity.go. Recomputing
	// it rather than hardcoding an expected order is deliberate: the ordering is a property of that
	// formula, and a hardcoded uid would freeze today's hash output into this test.
	pointOf := func(uid string) uuid.UUID {
		return schema.PointID("tenant-a", uuid.MustParse(uid), 0)
	}
	pb, pc := pointOf(uidB), pointOf(uidC)
	wantFirst, wantSecond := uidB, uidC
	if bytes.Compare(pc[:], pb[:]) < 0 {
		wantFirst, wantSecond = uidC, uidB
	}
	if ordered[0].UID != wantFirst || ordered[1].UID != wantSecond {
		t.Errorf("the two tied results ordered %s, %s; by ascending point ID they are %s, %s — the "+
			"tie-break is not keyed on schema.PointID", ordered[0].UID, ordered[1].UID, wantFirst, wantSecond)
	}
}

// TestSortDeterministically_LeavesTheCallersSliceAlone is the trap the "two runs agree" test would
// fall into: if the sort worked in place, the first run would reorder the fake's own backing array
// and the second run would receive an already-sorted list. The two runs would then agree for a
// reason that has nothing to do with the tie-break.
func TestSortDeterministically_LeavesTheCallersSliceAlone(t *testing.T) {
	hits := []retrieval.Result{result(uidA, 0, 0.1), result(uidB, 0, 0.9)}
	before := slices.Clone(hits)

	if _, err := sortDeterministically(hits, "tenant-a"); err != nil {
		t.Fatalf("sortDeterministically: %v", err)
	}
	if !slices.Equal(hits, before) {
		t.Errorf("the caller's slice was reordered: %+v, was %+v", hits, before)
	}
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

// TestRunGolden_AsksBeyondKSoTiesAtTheBoundaryCanBeReordered pins the one thing TieBreakMargin is
// for. Without the margin the searcher returns exactly K results and a tie at rank K is decided by
// whatever Qdrant cut, with nothing left over to reorder.
func TestRunGolden_AsksBeyondKSoTiesAtTheBoundaryCanBeReordered(t *testing.T) {
	s := &fakeSearcher{answers: [][]retrieval.Result{{result(uidA, 0, 0.9)}}}

	if _, err := RunGolden(t.Context(), s, []GoldenQuestion{question("q", uidA, nil)}, runConfig(5)); err != nil {
		t.Fatalf("RunGolden: %v", err)
	}
	if len(s.queries) != 1 {
		t.Fatalf("%d search(es), want 1", len(s.queries))
	}
	if want := 5 + TieBreakMargin; s.queries[0].TopK != want {
		t.Errorf("the search asked for TopK %d, want K+TieBreakMargin = %d", s.queries[0].TopK, want)
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

	t.Run("returned point", func(t *testing.T) {
		s := &fakeSearcher{answers: [][]retrieval.Result{{result("point-7", 0, 0.9)}}}
		results, err := RunGolden(t.Context(), s, []GoldenQuestion{question("q", uidA, nil)}, runConfig(5))
		if err != nil {
			t.Fatalf("RunGolden: %v", err)
		}
		if results[0].Measured() {
			t.Errorf("a result with no point ID was ranked anyway: %+v", results[0])
		}
		if !strings.Contains(results[0].Error, "not written by this pipeline") {
			t.Errorf("the error %q does not say what is wrong with the point", results[0].Error)
		}
	})
}
