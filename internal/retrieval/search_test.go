package retrieval

import (
	"context"
	"errors"
	"reflect"
	"slices"
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

// TestFilterMatchesAnything_ProbesUnderTheSearchsGuards is the whole contract in one test, driven
// the way the real caller drives it: a search under every facet, then a probe under the one facet
// the caller is going to blame.
//
// The two halves are asserted against each other on purpose. What must be identical to the search
// is the scope and the visibility guards — they come out of buildFilter either way, so no probe can
// report a filter alive under rules the search never ran under, and "nothing visible" stays the
// most the caller may claim. What must *not* be identical is the facets: a full-equality assertion
// here would be demanding the bug back, since a dead area+type pair reported as a dead area sends
// the model to drop the wrong filter and the operator to fix a correct MCP_AREAS entry.
func TestFilterMatchesAnything_ProbesUnderTheSearchsGuards(t *testing.T) {
	s, e, x := newSpySearcher()
	q := validQuery()
	q.Area, q.Type, q.Vault, q.Tags = "infra", "lesson", "trabalho", []string{"golang"}

	// What cmd/mcp-server's explainEmptyArea does: the search's Query with the facets its message
	// says nothing about cleared.
	narrowed := q
	narrowed.Type, narrowed.Vault, narrowed.Tags = "", "", nil

	if _, err := s.Search(t.Context(), q); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if _, err := s.FilterMatchesAnything(t.Context(), narrowed); err != nil {
		t.Fatalf("FilterMatchesAnything: %v", err)
	}
	if x.calls != 2 {
		t.Fatalf("the executor was called %d time(s), want the search and the probe", x.calls)
	}
	search, probe := x.requests[0], x.requests[1]

	// Kept, and kept identical to the search: the tenant condition and both guards.
	if !hasCondition(probe.Filter.Must, fieldTenantID, q.TenantID) {
		t.Error("the probe filter carries no tenant condition")
	}
	if !reflect.DeepEqual(probe.Filter.MustNot, search.Filter.MustNot) {
		t.Errorf("the probe runs under guards %+v, the search ran under %+v",
			probe.Filter.MustNot, search.Filter.MustNot)
	}
	if !hasCondition(probe.Filter.MustNot, fieldStatus, schema.StatusArchived().String()) ||
		!hasCondition(probe.Filter.MustNot, fieldVisibility, schema.VisibilityPrivate().String()) {
		t.Errorf("the guards are not the archived/private pair, so this proves nothing: %+v",
			probe.Filter.MustNot)
	}
	if probe.Collection != q.Collection {
		t.Errorf("the probe ran against %q, want %q", probe.Collection, q.Collection)
	}

	// Kept because the narrowed Query still names it.
	if !hasCondition(probe.Filter.Must, fieldArea, q.Area) {
		t.Error("the probe filter dropped the one facet the caller kept")
	}
	// Dropped because it no longer does — and present on the search, so the difference is real.
	for _, field := range []string{fieldType, fieldVault, fieldTags} {
		for _, c := range probe.Filter.Must {
			if c.Field == field {
				t.Errorf("the probe filter carries a %s condition the Query no longer names: %+v", field, c)
			}
		}
		if !slices.ContainsFunc(search.Filter.Must, func(c Condition) bool { return c.Field == field }) {
			t.Errorf("the search filter carries no %s condition, so dropping it proves nothing", field)
		}
	}

	// The rest is what makes it cheap: no embedding, no prefetch leg to fuse, one point, and a
	// projection that leaves the chunk text on the server.
	if e.calls != 1 {
		t.Errorf("the embedder was called %d time(s); the probe embeds nothing", e.calls)
	}
	if len(probe.Prefetch) != 0 || probe.FusionRRF {
		t.Errorf("the probe carries %d prefetch leg(s) and FusionRRF=%v, want a plain filtered read",
			len(probe.Prefetch), probe.FusionRRF)
	}
	if probe.Limit != 1 {
		t.Errorf("the probe asks for %d points, want 1", probe.Limit)
	}
	if len(probe.PayloadFields) == 0 || slices.Contains(probe.PayloadFields, fieldText) {
		t.Errorf("the probe payload projection is %v, want a minimal one without the chunk text",
			probe.PayloadFields)
	}
}

func TestFilterMatchesAnything_AnswersFromWhatCameBack(t *testing.T) {
	for name, tc := range map[string]struct {
		points []ScoredPoint
		want   bool
	}{
		"one point matches":  {[]ScoredPoint{scoredPoint("p1", 0.9)}, true},
		"the filter is dead": {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			e, x := &spyEmbedder{}, &spyExecutor{points: tc.points}
			s := NewSearcher(e, x, DefaultConfig())

			got, err := s.FilterMatchesAnything(t.Context(), validQuery())
			if err != nil {
				t.Fatalf("FilterMatchesAnything: %v", err)
			}
			if got != tc.want {
				t.Errorf("FilterMatchesAnything = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFilterMatchesAnything_RejectsWhatItCannotProbe keeps a failed probe distinguishable from a
// dead filter: an empty tenant would otherwise build a filter matching nothing and report it as an
// area that does not exist.
func TestFilterMatchesAnything_RejectsWhatItCannotProbe(t *testing.T) {
	sentinel := errors.New("qdrant unreachable")
	for name, tc := range map[string]struct {
		mutate func(*Query)
		exec   *spyExecutor
		want   error
		// wantCalls is asserted rather than implied: the two rejections above must cost no round
		// trip at all, and a fixture that happens to be unconfigured proves nothing about that.
		wantCalls int
	}{
		"no tenant":     {func(q *Query) { q.TenantID = "" }, &spyExecutor{}, ErrEmptyTenant, 0},
		"no collection": {func(q *Query) { q.Collection = "" }, &spyExecutor{}, ErrEmptyCollection, 0},
		"executor down": {func(*Query) {}, &spyExecutor{err: sentinel}, sentinel, 1},
	} {
		t.Run(name, func(t *testing.T) {
			s := NewSearcher(&spyEmbedder{}, tc.exec, DefaultConfig())
			q := validQuery()
			tc.mutate(&q)

			got, err := s.FilterMatchesAnything(t.Context(), q)
			if !errors.Is(err, tc.want) {
				t.Fatalf("FilterMatchesAnything = (%v, %v), want %v", got, err, tc.want)
			}
			if got {
				t.Error("a failed probe reported that the filter matches something")
			}
			if tc.exec.calls != tc.wantCalls {
				t.Errorf("the executor was called %d time(s), want %d", tc.exec.calls, tc.wantCalls)
			}
		})
	}
}

// TestFilterMatchesAnything_UnwiredSearcher is TestSearch_UnwiredSearcher for the probe. The
// embedder is absent from the table on purpose: the probe embeds nothing, so a Searcher with no
// embedder is a Searcher that can still probe.
func TestFilterMatchesAnything_UnwiredSearcher(t *testing.T) {
	for name, s := range map[string]*Searcher{
		"nil searcher":  nil,
		"no executor":   NewSearcher(&spyEmbedder{}, nil, DefaultConfig()),
		"nothing wired": NewSearcher(nil, nil, DefaultConfig()),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.FilterMatchesAnything(t.Context(), validQuery()); err == nil {
				t.Error("FilterMatchesAnything returned no error from an unwired Searcher")
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
