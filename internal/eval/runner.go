package eval

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/danielmalka/go-knowrag/internal/retrieval"
	"github.com/danielmalka/go-knowrag/internal/schema"
)

// TieBreakMargin is how many candidates past K the runner asks for so that a tie straddling rank K
// can be reordered before the cut.
//
// The number lives here and is used two lines below, in requestedTopK. What it buys is bounded and
// the bound is worth stating: reordering can only fix ties inside the window that came back. A tie
// that straddles rank K+TieBreakMargin is cut by Qdrant before this code sees it, and no client-side
// sort can undo that — the margin makes the boundary rarer, it does not remove it. Widening it costs
// payload, since internal/retrieval projects the chunk text on every hit (retrieval/build.go's
// resultPayloadFields).
const TieBreakMargin = 5

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
	Question GoldenQuestion `json:"question"`
	Hit      bool           `json:"hit"`
	// TopK is what the search returned, after the deterministic sort and truncated to RunConfig.K.
	TopK []retrieval.Result `json:"top_k"`
	// Error is set when this question could not be asked at all. It is not a miss: a question that
	// errored was not measured, and Aggregate keeps it out of both numerator and denominator rather
	// than reporting an infrastructure failure as a retrieval failure.
	Error string `json:"error,omitempty"`
}

// Measured reports whether this question produced a hit/miss verdict at all.
func (r QuestionResult) Measured() bool { return r.Error == "" }

// RunGolden asks every question and decides hit or miss, reproducibly.
//
// Determinism has two halves and only one of them is here. Qdrant decides *which* points come back;
// this decides the order they are read in, by sorting the returned window on (Score desc, PointID
// asc) before truncating to K. PointID is the deterministic UUIDv5 from
// internal/schema/identity.go — schema.PointID(tenantID, uid, chunkIndex) — so the tie-break key is
// the point's real identity in the index and not a position the transport happened to hand back.
//
// A searcher error on one question is recorded on that question and the run continues, because the
// alternative is a whole eval thrown away by one flaky call and no way to see which one it was.
func RunGolden(ctx context.Context, s Searcher, questions []GoldenQuestion, cfg RunConfig) ([]QuestionResult, error) {
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
		out = append(out, runOne(ctx, s, q, cfg))
	}
	return out, nil
}

func runOne(ctx context.Context, s Searcher, q GoldenQuestion, cfg RunConfig) QuestionResult {
	res := QuestionResult{Question: q}

	if _, err := uuid.Parse(q.UID); err != nil {
		res.Error = fmt.Sprintf("the expected uid %q is not a UUID: %v", q.UID, err)
		return res
	}

	hits, err := s.Search(ctx, retrieval.Query{
		Collection: cfg.Collection,
		TenantID:   cfg.TenantID,
		Text:       q.Question,
		TopK:       requestedTopK(cfg.K),
		Mode:       cfg.Mode,
	})
	if err != nil {
		res.Error = err.Error()
		return res
	}

	ordered, err := sortDeterministically(hits, cfg.TenantID)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.TopK = ordered[:min(len(ordered), cfg.K)]
	res.Hit = slices.ContainsFunc(res.TopK, func(r retrieval.Result) bool { return answers(q, r) })
	return res
}

// requestedTopK is K plus the margin, clamped to what internal/retrieval accepts. The clamp exists
// because maxTopK is decided in internal/retrieval/query.go, not here: asking for more than that
// package allows would turn a large K into a validation error rather than a large search.
func requestedTopK(k int) int {
	const retrievalMaxTopK = 1_000 // == maxTopK in internal/retrieval/query.go
	return min(k+TieBreakMargin, retrievalMaxTopK)
}

// answers reports whether one result is the note the question expected. An entry with no
// chunk_index accepts any chunk of that uid; an entry with one accepts that chunk alone.
func answers(q GoldenQuestion, r retrieval.Result) bool {
	if r.UID != q.UID {
		return false
	}
	return q.ChunkIndex == nil || *q.ChunkIndex == r.ChunkIndex
}

// sortDeterministically imposes a total order on one search's results.
//
// It returns a fresh slice rather than sorting in place, so a caller's slice is not reordered under
// it — and so a fake searcher handing out the same backing array twice cannot have the first run's
// sort change what the second run receives, which would make a determinism test agree with itself
// for the wrong reason.
func sortDeterministically(hits []retrieval.Result, tenantID string) ([]retrieval.Result, error) {
	type keyed struct {
		result retrieval.Result
		point  uuid.UUID
	}

	rows := make([]keyed, 0, len(hits))
	for _, h := range hits {
		uid, err := uuid.Parse(h.UID)
		if err != nil {
			// Not a fallback to some other ordering: a point whose uid is not a UUID was not written
			// by this pipeline, and ranking it by an invented key would hide that.
			return nil, fmt.Errorf("result uid %q is not a UUID, so it has no point ID to break ties "+
				"on — the point was not written by this pipeline: %w", h.UID, err)
		}
		rows = append(rows, keyed{result: h, point: schema.PointID(tenantID, uid, h.ChunkIndex)})
	}

	slices.SortFunc(rows, func(a, b keyed) int {
		// Score descending first, then the point ID ascending. uuid.UUID is [16]byte, so the byte
		// comparison is a total order over every point the index can hold: no two distinct points
		// can compare equal here, which is what makes the sort a function of the set and not of the
		// order it arrived in.
		if c := cmp.Compare(b.result.Score, a.result.Score); c != 0 {
			return c
		}
		return bytes.Compare(a.point[:], b.point[:])
	})

	out := make([]retrieval.Result, len(rows))
	for i, r := range rows {
		out[i] = r.result
	}
	return out, nil
}
