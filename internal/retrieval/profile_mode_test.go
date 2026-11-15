package retrieval

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/embed"
	"github.com/danielmalka/go-knowrag/internal/schema"
)

// PR 3 of the embedding migration: the query shape follows the model profile. A dense-only profile
// (EmbeddingGemma 2) gets ONE dense search with no prefetch legs and no RRF; the default profile keeps
// the hybrid, byte for byte.

func eg2Profile(t *testing.T) schema.ModelProfile {
	t.Helper()
	p, ok := schema.ProfileByID("embeddinggemma-2")
	if !ok {
		t.Fatal("embeddinggemma-2 not registered")
	}
	return p
}

func TestResolveMode(t *testing.T) {
	bge, eg2 := schema.DefaultProfile(), eg2Profile(t)
	cases := []struct {
		name    string
		mode    SearchMode
		profile schema.ModelProfile
		want    SearchMode
		wantErr error
	}{
		{"auto on a hybrid profile", SearchModeAuto, bge, SearchModeHybrid, nil},
		{"auto on a dense-only profile", SearchModeAuto, eg2, SearchModeDenseOnly, nil},
		{"hybrid on a hybrid profile", SearchModeHybrid, bge, SearchModeHybrid, nil},
		{"hybrid asked of a dense-only profile", SearchModeHybrid, eg2, SearchModeHybrid, ErrHybridUnavailable},
		{"dense-only on a hybrid profile", SearchModeDenseOnly, bge, SearchModeDenseOnly, nil},
		{"dense-only on a dense-only profile", SearchModeDenseOnly, eg2, SearchModeDenseOnly, nil},
		{"not a mode", SearchMode(9), bge, SearchMode(9), ErrInvalidSearchMode},
	}
	for _, tc := range cases {
		got, err := ResolveMode(tc.mode, tc.profile)
		if !errors.Is(err, tc.wantErr) || (err == nil && got != tc.want) {
			t.Errorf("%s: ResolveMode = %v, %v; want %v, %v", tc.name, got, err, tc.want, tc.wantErr)
		}
	}
}

func searcherFor(t *testing.T, p schema.ModelProfile) (*Searcher, *spyExecutor) {
	t.Helper()
	fake, err := embed.NewFakeEmbedderFor(p)
	if err != nil {
		t.Fatal(err)
	}
	x := &spyExecutor{}
	cfg := DefaultConfig()
	cfg.Profile = p
	return NewSearcher(fake, x, cfg), x
}

// The whole point of the PR on the query side: no Prefetch, no RRF, one dense vector of the profile's
// width, the same filter, the same window.
func TestSearch_DenseOnlyProfile_SendsOneDenseQuery(t *testing.T) {
	s, x := searcherFor(t, eg2Profile(t))
	q := validQuery() // Mode left at its zero value: Auto
	if _, err := s.Search(context.Background(), q); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(x.requests) != 1 {
		t.Fatalf("%d executor calls, want 1", len(x.requests))
	}
	req := x.requests[0]
	if len(req.Prefetch) != 0 || req.FusionRRF {
		t.Errorf("a dense-only profile sent %d prefetch leg(s), fusion=%t; want neither", len(req.Prefetch), req.FusionRRF)
	}
	if len(req.Dense) != 768 || req.DenseVector != schema.DenseVectorName {
		t.Errorf("Dense has %d elements on vector %q, want 768 on %q", len(req.Dense), req.DenseVector, schema.DenseVectorName)
	}
	if !req.Acorn {
		t.Error("the dense query lost the ACORN setting the legs of the hybrid carry")
	}
}

// The filter is the tenant-isolation boundary: dense-only must carry exactly the filter the hybrid's
// legs carry.
func TestSearch_DenseOnlyProfile_KeepsTheSameFilterAsHybrid(t *testing.T) {
	q := validQuery()
	q.Area, q.Tags = "saude", []string{"a"}

	dense, dx := searcherFor(t, eg2Profile(t))
	if _, err := dense.Search(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	hybrid, hx := searcherFor(t, schema.DefaultProfile())
	if _, err := hybrid.Search(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	d, h := dx.requests[0], hx.requests[0]
	if !reflect.DeepEqual(d.Filter, h.Filter) {
		t.Errorf("filters differ:\n dense  %+v\n hybrid %+v", d.Filter, h.Filter)
	}
	for i, leg := range h.Prefetch {
		if !reflect.DeepEqual(leg.Filter, d.Filter) {
			t.Errorf("hybrid leg %d filter differs from the dense-only one", i)
		}
	}
	if d.Limit != h.Limit || d.Offset != h.Offset {
		t.Errorf("window differs: dense %d+%d, hybrid %d+%d", d.Limit, d.Offset, h.Limit, h.Offset)
	}
}

func TestSearch_HybridAskedOfADenseOnlyProfile_FailsBeforeAnyIO(t *testing.T) {
	s, x := searcherFor(t, eg2Profile(t))
	q := validQuery()
	q.Mode = SearchModeHybrid
	if _, err := s.Search(context.Background(), q); !errors.Is(err, ErrHybridUnavailable) {
		t.Fatalf("Search = %v, want ErrHybridUnavailable", err)
	}
	if x.calls != 0 {
		t.Errorf("the executor was called %d time(s) for a query that cannot run", x.calls)
	}
}

// The default profile is untouched: Auto and explicit Hybrid build the identical hybrid request.
func TestSearch_DefaultProfile_AutoEqualsHybrid(t *testing.T) {
	auto, ax := searcherFor(t, schema.DefaultProfile())
	explicit, ex := searcherFor(t, schema.DefaultProfile())
	qa, qh := validQuery(), validQuery()
	qh.Mode = SearchModeHybrid
	if _, err := auto.Search(context.Background(), qa); err != nil {
		t.Fatal(err)
	}
	if _, err := explicit.Search(context.Background(), qh); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ax.requests[0], ex.requests[0]) {
		t.Errorf("Auto and Hybrid differ on the default profile:\n%+v\n%+v", ax.requests[0], ex.requests[0])
	}
	if got := ax.requests[0]; len(got.Prefetch) != 2 || !got.FusionRRF {
		t.Errorf("the default profile is no longer hybrid: %d legs, fusion=%t", len(got.Prefetch), got.FusionRRF)
	}
	// and a zero Config still means the default profile
	zero := NewSearcher(embed.FakeEmbedder{}, &spyExecutor{}, Config{PrefetchMultiplier: 4})
	if zero.Profile().ID != schema.DefaultProfileID {
		t.Errorf("a zero Config.Profile resolves to %q", zero.Profile().ID)
	}
}
