package eval

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/danielmalka/go-knowrag/internal/goldenset"
	"github.com/danielmalka/go-knowrag/internal/retrieval"
)

// The instrument asks the question production asks, and nothing else.
//
// This used to request K+5 results and re-sort them by (score, point ID) before truncating to K, to
// make ties reproducible. That was wrong in a way that flattered the system, and the correction is
// the reason this comment is long:
//
// TopK is not a client-side display limit. It reaches internal/retrieval/query.go's prefetchLimit,
// which computes (TopK+Offset) × multiplier — so asking for 10 instead of 5 took the hybrid's
// prefetch legs from 20 candidates each to 40, and the outer RRF Limit from 5 to 10. RRF over a
// doubled pool is a different ranking, not the same ranking with more of its tail visible. A note
// landing 8th in that larger fusion — one production would never return — was then truncated into
// the eval's top-5 and counted as a hit. The reported recall came out better than the deployed
// system's, under a heading that said "Recall@5".
//
// Re-sorting compounded it. Production returns Qdrant's order and never reorders (Search and
// formatResults in internal/retrieval), so re-ranking a *wider* window does not just reorder the
// answer — it changes which points are in it.
//
// So: TopK = K exactly, and whatever comes back is the answer. What that gives up is the ability to
// paper over a tie at the rank-K boundary, and giving it up is the point — see tiedAtTheCut.

// Searcher is what the runner needs from internal/retrieval: one search, nothing else.
//
// Declared here, on the consumer side, so a fake in a test is four lines and no Qdrant is involved.
// *retrieval.Searcher satisfies it as written.
type Searcher interface {
	Search(ctx context.Context, q retrieval.Query) ([]retrieval.Result, error)
}

// RunConfig is the scope one golden run happens in.
//
// TenantID is one value for the whole run, not per question: this build populates a single tenant
// per collection, and a per-question tenant would be a field with no caller and one more way to
// measure recall against a scope nobody asked for.
type RunConfig struct {
	Collection string
	TenantID   string
	// K is the cut-off the recall is named after: K=5 makes this Recall@5.
	K    int
	Mode retrieval.SearchMode
}

// QuestionResult is one question's outcome.
//
// Error is a string and not an error because a Report is persisted as the baseline (baseline.go)
// and read back: an `error` field would round-trip as null and a run that failed on ten questions
// would reload as a run that did not.
type QuestionResult struct {
	Question goldenset.GoldenQuestion `json:"question"`
	Hit      bool                     `json:"hit"`
	// Tied marks a hit whose place in the answer was decided among equally-scored candidates, so it
	// may not reproduce on the next run against the same index. See tiedAtTheCut.
	Tied bool `json:"tied,omitempty"`
	// TopK is what the search returned, in the order it returned it, guarded to RunConfig.K.
	TopK []retrieval.Result `json:"top_k"`
	// Error is set when this question could not be asked at all. It is not a miss: a question that
	// errored was not measured, and Aggregate keeps it out of both numerator and denominator rather
	// than reporting an infrastructure failure as a retrieval failure.
	Error string `json:"error,omitempty"`
}

// Measured reports whether this question produced a hit/miss verdict at all.
func (r QuestionResult) Measured() bool { return r.Error == "" }

// RunGolden asks every question and decides hit or miss.
//
// It reorders nothing. The answer is whatever the search returned, which is what production would
// have shown, and the hit is membership in it. Reproducibility across runs is therefore a property
// of the index and of Qdrant's ranking, not something this file manufactures — where the ranking
// leaves it genuinely undecided, tiedAtTheCut says so instead.
//
// A searcher error on one question is recorded on that question and the run continues, because the
// alternative is a whole eval thrown away by one flaky call and no way to see which one it was.
func RunGolden(
	ctx context.Context, s Searcher, questions []goldenset.GoldenQuestion, cfg RunConfig,
) ([]QuestionResult, error) {
	switch {
	case s == nil:
		return nil, errors.New("eval: RunGolden has no Searcher, so nothing would be measured")
	case cfg.K <= 0:
		return nil, fmt.Errorf("eval: RunGolden K = %d, want a positive cut-off", cfg.K)
	case len(questions) == 0:
		return nil, errors.New("eval: RunGolden was given no questions")
	}

	out := make([]QuestionResult, 0, len(questions))
	for _, q := range questions {
		// Cancellation stops the run and is reported as cancellation, not as questions that failed.
		//
		// Without this the loop keeps asking, every remaining search fails against the dead context,
		// and the report fills with errored questions — which reads identically to a searcher that
		// broke. Report.Complete already stops that from passing as a measurement (report.go), so
		// the defect is not a false pass; it is that nobody reading the CI output can tell a
		// timeout from a retrieval regression. Returning here says which one it was.
		if err := ctx.Err(); err != nil {
			return out, fmt.Errorf("eval: the run was cancelled after %d of %d question(s), so this "+
				"is a cancelled run and not a set of failed searches: %w", len(out), len(questions), err)
		}
		out = append(out, runOne(ctx, s, q, cfg))
	}
	return out, nil
}

func runOne(ctx context.Context, s Searcher, q goldenset.GoldenQuestion, cfg RunConfig) QuestionResult {
	res := QuestionResult{Question: q}

	if _, err := uuid.Parse(q.UID); err != nil {
		res.Error = fmt.Sprintf("the expected uid %q is not a UUID: %v", q.UID, err)
		return res
	}

	hits, err := s.Search(ctx, retrieval.Query{
		Collection: cfg.Collection,
		TenantID:   cfg.TenantID,
		Text:       q.Question,
		TopK:       cfg.K,
		Mode:       cfg.Mode,
	})
	if err != nil {
		res.Error = err.Error()
		return res
	}

	// Truncation is a guard, not a policy: the search was asked for K and a well-behaved searcher
	// returns at most K. It is here so a searcher that over-answers cannot widen the window the
	// verdict is taken over — the defect this whole file was just corrected for.
	res.TopK = hits[:min(len(hits), cfg.K)]
	res.Hit = slices.ContainsFunc(res.TopK, func(r retrieval.Result) bool { return answers(q, r) })
	res.Tied = tiedAtTheCut(res.TopK, q, cfg.K)
	return res
}

// tiedAtTheCut reports whether the expected note's place in the answer was decided among equals.
//
// It fires when the answer is full — K results came back, so something was cut — and the expected
// note scores exactly what the last included result scores. That means the expected note and at
// least one excluded candidate were indistinguishable to the ranker, and which of them filled the
// last slot was Qdrant's to decide. The same index can answer differently on the next run without
// anything having changed.
//
// This is what replaces the old client-side re-sort, and it is the opposite move: the re-sort made
// the boundary look stable by quietly picking a winner, this reports that there was no winner to
// pick. Hit stays true, because the note IS in what production returns and the recall has to be
// production's number — the flag says the hit may not reproduce, and the report names how many
// (report.go).
//
// What it can see is bounded, in both directions, and neither bound is closable from K results:
//
//   - An expected note tied just *outside* the cut is invisible. Seeing it needs a wider query,
//     which is the defect this file was corrected for.
//   - An expected note sitting alone at rank K may still be tied with the invisible rank K+1.
//
// So the rule is the one the visible evidence supports: the boundary score has to be shared by at
// least two returned results, one of them the expected note. Flagging a note merely because it
// landed at rank K would fire on most hits at the boundary and turn this section into one nobody
// reads — which is the way a warning stops working.
func tiedAtTheCut(results []retrieval.Result, q goldenset.GoldenQuestion, k int) bool {
	if len(results) == 0 || len(results) < k {
		return false
	}

	boundary := results[len(results)-1].Score
	atBoundary, expectedIsOne := 0, false
	for _, r := range results {
		if r.Score != boundary {
			continue
		}
		atBoundary++
		if answers(q, r) {
			expectedIsOne = true
		}
	}
	return expectedIsOne && atBoundary >= 2
}

// answers reports whether one result is the note the question expected. An entry with no
// chunk_index accepts any chunk of that uid; an entry with one accepts that chunk alone.
func answers(q goldenset.GoldenQuestion, r retrieval.Result) bool {
	if r.UID != q.UID {
		return false
	}
	return q.ChunkIndex == nil || *q.ChunkIndex == r.ChunkIndex
}
