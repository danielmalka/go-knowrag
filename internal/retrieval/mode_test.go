package retrieval

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/schema"
)

// TestSearchMode_HybridIsTheZeroValue is what keeps every Query written before the toggle existed
// meaning what it meant. If SearchModeDenseOnly ever became the zero value, every caller that never
// heard of the field would silently switch ranking.
func TestSearchMode_HybridIsTheZeroValue(t *testing.T) {
	var zero SearchMode
	if zero != SearchModeHybrid {
		t.Errorf("the zero SearchMode is %d, want SearchModeHybrid (%d)", zero, SearchModeHybrid)
	}
	if validQuery().Mode != SearchModeHybrid {
		t.Error("a Query built without naming a Mode is not in hybrid mode")
	}
	if SearchModeHybrid.String() != "hybrid" || SearchModeDenseOnly.String() != "dense-only" {
		t.Errorf("the modes render as %q and %q; the eval reports and the hybrid-vs-dense document "+
			"carry these strings", SearchModeHybrid, SearchModeDenseOnly)
	}
}

// TestBuildQueryRequest_ModeDenseOnly_SkipsSparsePrefetchAndRRF is S10 T7's RED test, adapted to
// the shapes internal/retrieval actually has.
//
// The task document's version asserts on *qdrant.PrefetchQuery and NewQueryRRF inside this package.
// Those types cannot appear here: internal/archtest's TestArch_QdrantClientConfinedToStore forbids
// importing the Qdrant client outside internal/store. The equivalent assertion on this package's own
// vocabulary is Prefetch and FusionRRF — internal/store/query.go is where they become the client
// types, and store's own test covers that leg.
func TestBuildQueryRequest_ModeDenseOnly_SkipsSparsePrefetchAndRRF(t *testing.T) {
	q := validQuery()
	q.TopK, q.Offset, q.Mode = 5, 3, SearchModeDenseOnly
	emb := fakeEmbedding()

	req := buildQueryRequest(q, emb, calibratedPrefetchLimit(q, DefaultPrefetchMultiplier))

	if len(req.Prefetch) != 0 {
		t.Errorf("%d prefetch leg(s) in dense-only mode, want none", len(req.Prefetch))
	}
	if req.FusionRRF {
		t.Error("FusionRRF is on in dense-only mode; there is one list, so there is nothing to fuse")
	}
	if !slices.Equal(req.Dense, emb.Dense) {
		t.Error("the request does not carry the dense half of the embedding as its top-level query")
	}
	if req.DenseVector != schema.DenseVectorName {
		t.Errorf("DenseVector = %q, want internal/schema's %q", req.DenseVector, schema.DenseVectorName)
	}
	if !req.Acorn {
		t.Error("ACORN is off; in dense-only the filtered traversal happens on this search, so this " +
			"is where PRD-contrato §2.3c's opt-in has to be (it is on both legs in hybrid mode)")
	}
}

// TestBuildQueryRequest_ModeDoesNotChangeTheFilterOrTheWindow is the half of the toggle that must
// not move. A comparison of hybrid against dense-only is only a comparison of ranking if everything
// else — the tenant condition, the archived/private exclusions, the facets, the limit, the offset
// and the payload projection — is byte-identical between the two.
func TestBuildQueryRequest_ModeDoesNotChangeTheFilterOrTheWindow(t *testing.T) {
	base := validQuery()
	base.TopK, base.Offset = 5, 3
	base.Area, base.Type, base.Vault, base.Tags = "alfa", "concept", "pessoal", []string{"golang"}

	dense := base
	dense.Mode = SearchModeDenseOnly

	hybridReq := buildQueryRequest(base, fakeEmbedding(), 32)
	denseReq := buildQueryRequest(dense, fakeEmbedding(), 32)

	if !reflect.DeepEqual(hybridReq.Filter, denseReq.Filter) {
		t.Errorf("the two modes build different filters:\n hybrid: %+v\n  dense: %+v",
			hybridReq.Filter, denseReq.Filter)
	}
	if hybridReq.Limit != denseReq.Limit || hybridReq.Offset != denseReq.Offset {
		t.Errorf("the two modes ask for different windows: hybrid limit/offset %d/%d, dense %d/%d",
			hybridReq.Limit, hybridReq.Offset, denseReq.Limit, denseReq.Offset)
	}
	if !slices.Equal(hybridReq.PayloadFields, denseReq.PayloadFields) {
		t.Errorf("the two modes project different payloads: %v vs %v",
			hybridReq.PayloadFields, denseReq.PayloadFields)
	}
	// The tenant condition specifically, spelled out rather than left to DeepEqual: it is the one
	// condition this package exists to guarantee, and a DeepEqual over two equally-broken filters
	// would agree.
	if !slices.Contains(denseReq.Filter.Must, Condition{Field: fieldTenantID, Value: base.TenantID}) {
		t.Errorf("the dense-only filter has no tenant condition: %+v", denseReq.Filter)
	}
}

// TestQuery_Validate_RejectsAnUndefinedMode keeps a garbage int from falling into the hybrid branch
// by default. Folding it there would make "somebody passed SearchMode(7)" and "somebody asked for
// hybrid" the same run, and the eval could not say which one it measured.
func TestQuery_Validate_RejectsAnUndefinedMode(t *testing.T) {
	q := validQuery()
	q.Mode = SearchMode(7)

	err := q.Validate(DefaultPrefetchMultiplier)
	if !errors.Is(err, ErrInvalidSearchMode) {
		t.Fatalf("Validate returned %v, want ErrInvalidSearchMode", err)
	}
	for _, defined := range []SearchMode{SearchModeHybrid, SearchModeDenseOnly} {
		q.Mode = defined
		if err := q.Validate(DefaultPrefetchMultiplier); err != nil {
			t.Errorf("Validate rejected the defined mode %s: %v", defined, err)
		}
	}
}

// TestSearch_ModeReachesTheExecutor closes the gap between "buildQueryRequest branches" and "the
// mode a caller set is the mode that goes out". The builder is unexported and every test above
// calls it directly; without this one, Search could ignore Query.Mode entirely and all of them
// would still pass.
func TestSearch_ModeReachesTheExecutor(t *testing.T) {
	s, _, x := newSpySearcher()
	q := validQuery()
	q.Mode = SearchModeDenseOnly

	if _, err := s.Search(t.Context(), q); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(x.requests) != 1 {
		t.Fatalf("%d request(s) reached the executor, want 1", len(x.requests))
	}
	if len(x.requests[0].Prefetch) != 0 || x.requests[0].FusionRRF {
		t.Errorf("Search sent a hybrid request for a dense-only Query: %d leg(s), fusion=%t",
			len(x.requests[0].Prefetch), x.requests[0].FusionRRF)
	}
	if len(x.requests[0].Dense) == 0 {
		t.Error("the request that went out carries no top-level dense query")
	}
}
