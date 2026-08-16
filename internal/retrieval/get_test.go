package retrieval

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/schema"
	"github.com/google/uuid"
)

// validNoteQuery is a lookup, not a search: it names the note and the scope, and nothing else.
func validNoteQuery() Query {
	return Query{
		Collection: "interno",
		TenantID:   "tenant-a",
		UID:        "0198a7f2-4b31-7c42-9e15-3d8a92c47b6a",
	}
}

func TestGetByUID_InvalidQueryTouchesNeitherDependency(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Query)
		want   error
	}{
		{"no tenant", func(q *Query) { q.TenantID = "" }, ErrEmptyTenant},
		{"no collection", func(q *Query) { q.Collection = "" }, ErrEmptyCollection},
		{"no uid", func(q *Query) { q.UID = "" }, ErrEmptyUID},
		{"blank uid", func(q *Query) { q.UID = "   \n" }, ErrEmptyUID},
		{"not a uuid", func(q *Query) { q.UID = "not-a-uid" }, ErrInvalidUID},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, x := &spyEmbedder{}, &spyExecutor{}
			s := NewSearcher(e, x, DefaultConfig())

			q := validNoteQuery()
			tc.mutate(&q)

			got, err := s.GetByUID(t.Context(), q)
			if !errors.Is(err, tc.want) {
				t.Fatalf("GetByUID = (%v, %v), want %v", got, err, tc.want)
			}
			if e.calls != 0 || x.calls != 0 {
				t.Errorf("an invalid lookup reached the dependencies: %d embed call(s), %d execute call(s)",
					e.calls, x.calls)
			}
		})
	}
}

func TestGetByUID_EmbedsNothing(t *testing.T) {
	s, e, x := newSpySearcher()
	if _, err := s.GetByUID(t.Context(), validNoteQuery()); err != nil {
		t.Fatalf("GetByUID: %v", err)
	}
	if e.calls != 0 {
		t.Errorf("the embedder was called %d time(s); a uid lookup is a filtered read", e.calls)
	}
	if x.calls != 1 {
		t.Fatalf("the executor was called %d time(s), want 1", x.calls)
	}
}

func TestGetByUID_FilterIsTenantPlusUIDUnderTheSearchGuards(t *testing.T) {
	s, _, x := newSpySearcher()
	q := validNoteQuery()
	// Area/type/tags are search facets. A lookup that already names the uid must not inherit them:
	// a caller that found the note under one area and then opened it must still get the note.
	q.Area, q.Type, q.Tags = "infra", "lesson", []string{"golang"}

	if _, err := s.GetByUID(t.Context(), q); err != nil {
		t.Fatalf("GetByUID: %v", err)
	}
	req := x.requests[0]

	if req.Collection != q.Collection {
		t.Errorf("collection = %q, want %q", req.Collection, q.Collection)
	}
	if !hasCondition(req.Filter.Must, fieldTenantID, q.TenantID) {
		t.Error("the lookup filter carries no tenant condition")
	}
	if countCondition(req.Filter.Must, fieldTenantID) != 1 {
		t.Errorf("tenant condition appears %d time(s), want exactly 1", countCondition(req.Filter.Must, fieldTenantID))
	}
	parsed, err := uuid.Parse(q.UID)
	if err != nil {
		t.Fatalf("test uid: %v", err)
	}
	if !hasCondition(req.Filter.Must, fieldUID, parsed.String()) {
		t.Errorf("the lookup filter has no uid condition for %s: %+v", parsed, req.Filter.Must)
	}
	if !hasCondition(req.Filter.MustNot, fieldStatus, schema.StatusArchived().String()) ||
		!hasCondition(req.Filter.MustNot, fieldVisibility, schema.VisibilityPrivate().String()) {
		t.Errorf("the guards are not the archived/private pair: %+v", req.Filter.MustNot)
	}
	for _, field := range []string{fieldArea, fieldType, fieldVault, fieldTags} {
		for _, c := range req.Filter.Must {
			if c.Field == field {
				t.Errorf("the lookup filter carries a search facet %s=%q", field, c.Value)
			}
		}
	}
	if len(req.Prefetch) != 0 || req.FusionRRF || len(req.Dense) != 0 {
		t.Errorf("the lookup is a ranked search: prefetch=%d fusion=%v dense=%d",
			len(req.Prefetch), req.FusionRRF, len(req.Dense))
	}
	if req.Limit != noteLookupLimit {
		t.Errorf("limit = %d, want %d", req.Limit, noteLookupLimit)
	}
	if !slices.Equal(req.PayloadFields, resultPayloadFields()) {
		t.Errorf("payload fields = %v, want the search projection", req.PayloadFields)
	}
}

func TestGetByUID_CanonicalizesTheUIDSpelling(t *testing.T) {
	s, _, x := newSpySearcher()
	q := validNoteQuery()
	q.UID = strings.ToUpper(strings.ReplaceAll(q.UID, "-", ""))

	if _, err := s.GetByUID(t.Context(), q); err != nil {
		t.Fatalf("GetByUID: %v", err)
	}
	want, err := uuid.Parse(validNoteQuery().UID)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !hasCondition(x.requests[0].Filter.Must, fieldUID, want.String()) {
		t.Errorf("filter uid is not the canonical spelling %s: %+v", want, x.requests[0].Filter.Must)
	}
}

func TestGetByUID_ReturnsChunksInDocumentOrder(t *testing.T) {
	points := []ScoredPoint{
		notePoint("p2", 2, 0.1),
		notePoint("p0", 0, 0.9),
		notePoint("p1", 1, 0.5),
	}
	e, x := &spyEmbedder{}, &spyExecutor{points: points}
	s := NewSearcher(e, x, DefaultConfig())

	got, err := s.GetByUID(t.Context(), validNoteQuery())
	if err != nil {
		t.Fatalf("GetByUID: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("%d result(s), want 3", len(got))
	}
	for i, r := range got {
		if r.ChunkIndex != i {
			t.Errorf("result %d has chunk_index %d, want document order", i, r.ChunkIndex)
		}
		if !r.Untrusted {
			t.Errorf("result %d is not marked untrusted", i)
		}
	}
}

func TestGetByUID_HitsTheLookupCap_IsAnErrorNotATruncation(t *testing.T) {
	points := make([]ScoredPoint, noteLookupLimit)
	for i := range points {
		points[i] = notePoint("p", i, 0)
	}
	e, x := &spyEmbedder{}, &spyExecutor{points: points}
	s := NewSearcher(e, x, DefaultConfig())

	got, err := s.GetByUID(t.Context(), validNoteQuery())
	if !errors.Is(err, ErrNoteTooLarge) {
		t.Fatalf("GetByUID = (%d hits, %v), want ErrNoteTooLarge", len(got), err)
	}
	if got != nil {
		t.Errorf("a capped lookup returned %d result(s) alongside the error", len(got))
	}
}

func TestGetByUID_ExecutorErrorIsWrapped(t *testing.T) {
	sentinel := errors.New("qdrant unreachable")
	s := NewSearcher(&spyEmbedder{}, &spyExecutor{err: sentinel}, DefaultConfig())

	_, err := s.GetByUID(t.Context(), validNoteQuery())
	if !errors.Is(err, sentinel) {
		t.Fatalf("GetByUID = %v, want it to wrap %v", err, sentinel)
	}
}

func TestGetByUID_UnwiredSearcher(t *testing.T) {
	for name, s := range map[string]*Searcher{
		"nil searcher":  nil,
		"no executor":   NewSearcher(&spyEmbedder{}, nil, DefaultConfig()),
		"nothing wired": NewSearcher(nil, nil, DefaultConfig()),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.GetByUID(t.Context(), validNoteQuery()); err == nil {
				t.Error("GetByUID returned no error from an unwired Searcher")
			}
		})
	}
}

func TestGetByUID_NoEmbedderStillLooksUp(t *testing.T) {
	x := &spyExecutor{}
	s := NewSearcher(nil, x, DefaultConfig())
	if _, err := s.GetByUID(t.Context(), validNoteQuery()); err != nil {
		t.Fatalf("GetByUID with no embedder: %v", err)
	}
	if x.calls != 1 {
		t.Errorf("executor calls = %d, want 1", x.calls)
	}
}

func TestSearch_DoesNotFilterOnUID(t *testing.T) {
	s, _, x := newSpySearcher()
	q := validQuery()
	q.UID = validNoteQuery().UID

	if _, err := s.Search(t.Context(), q); err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, c := range x.requests[0].Filter.Must {
		if c.Field == fieldUID {
			t.Errorf("Search treated UID as a filter: %+v", c)
		}
	}
}

func notePoint(id string, index int, score float32) ScoredPoint {
	p := scoredPoint(id, score)
	p.Payload[fieldChunkIndex] = index
	return p
}
