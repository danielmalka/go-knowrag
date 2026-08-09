package retrieval

import (
	"context"
	"errors"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/embed"
	"github.com/danielmalka/go-knowrag/internal/schema"
)

// spyEmbedder counts calls and records the text, which is how "no I/O happened" is proven as a
// number rather than inferred from the returned error.
type spyEmbedder struct {
	embed.FakeEmbedder
	calls int
	texts []string
	err   error
}

func (s *spyEmbedder) EmbedQuery(ctx context.Context, text string) (embed.Embedding, error) {
	s.calls++
	s.texts = append(s.texts, text)
	if s.err != nil {
		return embed.Embedding{}, s.err
	}
	return s.FakeEmbedder.EmbedQuery(ctx, text)
}

type spyExecutor struct {
	calls    int
	requests []SearchRequest
	points   []ScoredPoint
	err      error
}

func (s *spyExecutor) ExecuteQuery(_ context.Context, req SearchRequest) ([]ScoredPoint, error) {
	s.calls++
	s.requests = append(s.requests, req)
	return s.points, s.err
}

func newSpySearcher() (*Searcher, *spyEmbedder, *spyExecutor) {
	e, x := &spyEmbedder{}, &spyExecutor{}
	return NewSearcher(e, x, DefaultConfig()), e, x
}

func TestSearch_InvalidQueryTouchesNeitherDependency(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*Query)
		multiplier int
		want       error
	}{
		{"no tenant", func(q *Query) { q.TenantID = "" }, DefaultPrefetchMultiplier, ErrEmptyTenant},
		{"no collection", func(q *Query) { q.Collection = "" }, DefaultPrefetchMultiplier, ErrEmptyCollection},
		{"no text", func(q *Query) { q.Text = "" }, DefaultPrefetchMultiplier, ErrEmptyQuery},
		{"zero top_k", func(q *Query) { q.TopK = 0 }, DefaultPrefetchMultiplier, ErrInvalidTopK},
		{"prefetch floor not cleared", func(q *Query) { q.Offset = 3 }, 0, ErrPrefetchLimitTooLow},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, x := &spyEmbedder{}, &spyExecutor{}
			s := NewSearcher(e, x, Config{PrefetchMultiplier: tc.multiplier})

			q := validQuery()
			tc.mutate(&q)

			got, err := s.Search(t.Context(), q)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Search = (%v, %v), want %v", got, err, tc.want)
			}
			if e.calls != 0 || x.calls != 0 {
				t.Errorf("an invalid query reached the dependencies: %d embed call(s), %d execute call(s)",
					e.calls, x.calls)
			}
		})
	}
}

func TestSearch_EmbedderErrorIsWrappedAndStopsTheFlow(t *testing.T) {
	sentinel := errors.New("backend refused")
	e, x := &spyEmbedder{err: sentinel}, &spyExecutor{}
	s := NewSearcher(e, x, DefaultConfig())

	_, err := s.Search(t.Context(), validQuery())
	if !errors.Is(err, sentinel) {
		t.Fatalf("Search = %v, want it to wrap %v", err, sentinel)
	}
	if x.calls != 0 {
		t.Errorf("the executor ran %d time(s) after the embedder failed", x.calls)
	}
}

func TestSearch_ExecutorErrorIsWrapped(t *testing.T) {
	sentinel := errors.New("qdrant unreachable")
	e, x := &spyEmbedder{}, &spyExecutor{err: sentinel}
	s := NewSearcher(e, x, DefaultConfig())

	_, err := s.Search(t.Context(), validQuery())
	if !errors.Is(err, sentinel) {
		t.Fatalf("Search = %v, want it to wrap %v", err, sentinel)
	}
	// Distinguishable from the embedder's failure: the two are wrapped with different prefixes so a
	// caller reading a log knows which half of the flow broke.
	if errors.Is(err, ErrEmptyTenant) {
		t.Error("an executor failure was reported as a validation failure")
	}
}

func TestSearch_HappyPath(t *testing.T) {
	s, e, x := newSpySearcher()
	q := validQuery()
	q.TopK, q.Offset = 5, 3
	x.points = []ScoredPoint{scoredPoint("p1", 0.9)}

	got, err := s.Search(t.Context(), q)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if e.calls != 1 {
		t.Fatalf("the embedder was called %d time(s), want exactly 1", e.calls)
	}
	if e.texts[0] != q.Text {
		t.Errorf("the embedder got %q, want the query text %q", e.texts[0], q.Text)
	}
	if x.calls != 1 {
		t.Fatalf("the executor was called %d time(s), want exactly 1", x.calls)
	}

	// The request the executor saw is the one the builder produces for this query — same embedding,
	// same calibrated limit, nothing added between the two.
	emb, err := e.FakeEmbedder.EmbedQuery(t.Context(), q.Text)
	if err != nil {
		t.Fatalf("re-embedding for the comparison: %v", err)
	}
	want := buildQueryRequest(q, emb, calibratedPrefetchLimit(q, DefaultPrefetchMultiplier))
	if x.requests[0].Collection != want.Collection || x.requests[0].Limit != want.Limit ||
		x.requests[0].Offset != want.Offset || !x.requests[0].FusionRRF {
		t.Errorf("the executed request %+v does not match the built one %+v", x.requests[0], want)
	}
	if !hasCondition(x.requests[0].Filter.Must, fieldTenantID, q.TenantID) {
		t.Error("the request that reached the executor carries no tenant condition")
	}
	for _, leg := range x.requests[0].Prefetch {
		if !leg.Acorn {
			t.Errorf("prefetch leg %q reached the executor with ACORN off", leg.Vector)
		}
	}
	if len(x.requests[0].Prefetch) != 2 {
		t.Errorf("%d prefetch leg(s) reached the executor, want 2", len(x.requests[0].Prefetch))
	}

	// And the return value is formatResults' output, not a second mapping.
	formatted, err := formatResults(x.points)
	if err != nil {
		t.Fatalf("formatResults: %v", err)
	}
	if len(got) != len(formatted) || got[0] != formatted[0] {
		t.Errorf("Search returned %+v, want formatResults' output %+v", got, formatted)
	}
}

func TestSearch_MalformedPointIsReported(t *testing.T) {
	s, _, x := newSpySearcher()
	broken := scoredPoint("half-written", 0.5)
	delete(broken.Payload, fieldText)
	x.points = []ScoredPoint{broken}

	if _, err := s.Search(t.Context(), validQuery()); err == nil {
		t.Fatal("Search returned no error for a point with no text")
	}
}

func TestSearch_UnwiredSearcher(t *testing.T) {
	for name, s := range map[string]*Searcher{
		"nil searcher":  nil,
		"no embedder":   NewSearcher(nil, &spyExecutor{}, DefaultConfig()),
		"no executor":   NewSearcher(&spyEmbedder{}, nil, DefaultConfig()),
		"nothing wired": NewSearcher(nil, nil, DefaultConfig()),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.Search(t.Context(), validQuery()); err == nil {
				t.Error("Search returned no error from an unwired Searcher")
			}
		})
	}
}

// TestSearch_UsesTheSchemaVectorNames pins the names to internal/schema's constants rather than to
// literals: the collections were provisioned with those, and a search naming anything else asks
// Qdrant for a vector that does not exist.
func TestSearch_UsesTheSchemaVectorNames(t *testing.T) {
	s, _, x := newSpySearcher()
	if _, err := s.Search(t.Context(), validQuery()); err != nil {
		t.Fatalf("Search: %v", err)
	}

	names := make([]string, 0, 2)
	for _, leg := range x.requests[0].Prefetch {
		names = append(names, leg.Vector)
	}
	for _, want := range []string{schema.DenseVectorName, schema.SparseVectorName} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("no prefetch leg uses %q; legs use %v", want, names)
		}
	}
}
