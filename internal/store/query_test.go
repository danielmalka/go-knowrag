package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/qdrant/go-client/qdrant"

	"github.com/danielmalka/go-knowrag/internal/embed"
	"github.com/danielmalka/go-knowrag/internal/retrieval"
)

// fakeQueryAPI records the request that went out and replays a canned answer. Everything worth
// testing here is "what reached the wire", which is exactly what it captures.
type fakeQueryAPI struct {
	requests []*qdrant.QueryPoints
	points   []*qdrant.ScoredPoint
	err      error
}

func (f *fakeQueryAPI) Query(_ context.Context, request *qdrant.QueryPoints) ([]*qdrant.ScoredPoint, error) {
	f.requests = append(f.requests, request)
	return f.points, f.err
}

// sampleRequest is the shape internal/retrieval builds: two filtered prefetch legs with ACORN on,
// RRF over them, one filter on the outer stage.
func sampleRequest() retrieval.SearchRequest {
	f := retrieval.Filter{
		Must:    []retrieval.Condition{{Field: "tenant_id", Value: "tenant-a"}},
		MustNot: []retrieval.Condition{{Field: "status", Value: "archived"}},
	}
	return retrieval.SearchRequest{
		Collection: "interno",
		Prefetch: []retrieval.PrefetchLeg{
			{Vector: "dense", Dense: []float32{0.1, 0.2}, Limit: 40, Filter: f, Acorn: true},
			{Vector: "sparse", Sparse: embed.Sparse{Indices: []uint32{2, 5}, Values: []float32{0.3, 0.4}},
				Limit: 40, Filter: f, Acorn: true},
		},
		FusionRRF:     true,
		Filter:        f,
		Limit:         5,
		Offset:        3,
		PayloadFields: []string{"uid", "text"},
	}
}

func executeSample(t *testing.T, req retrieval.SearchRequest) (*fakeQueryAPI, []retrieval.ScoredPoint, error) {
	t.Helper()
	api := &fakeQueryAPI{}
	q, err := NewQuerier(api)
	if err != nil {
		t.Fatalf("NewQuerier: %v", err)
	}
	out, err := q.ExecuteQuery(t.Context(), req)
	return api, out, err
}

func TestExecuteQuery_TranscribesTheRequest(t *testing.T) {
	api, _, err := executeSample(t, sampleRequest())
	if err != nil {
		t.Fatalf("ExecuteQuery: %v", err)
	}
	if len(api.requests) != 1 {
		t.Fatalf("%d request(s) reached the wire, want 1", len(api.requests))
	}
	got := api.requests[0]

	if got.GetCollectionName() != "interno" {
		t.Errorf("CollectionName = %q", got.GetCollectionName())
	}
	if got.GetLimit() != 5 || got.GetOffset() != 3 {
		t.Errorf("Limit/Offset = %d/%d, want 5/3", got.GetLimit(), got.GetOffset())
	}
	if got.GetQuery().GetFusion() != qdrant.Fusion_RRF {
		t.Errorf("Query fusion = %v, want RRF (PRD-contrato §2.3b)", got.GetQuery())
	}
	if got.GetWithVectors().GetEnable() {
		t.Error("the request asks for vectors back; nothing reads them")
	}
	if want := []string{"uid", "text"}; !slices.Equal(got.GetWithPayload().GetInclude().GetFields(), want) {
		t.Errorf("payload projection = %v, want %v", got.GetWithPayload().GetInclude().GetFields(), want)
	}

	if len(got.GetPrefetch()) != 2 {
		t.Fatalf("%d prefetch leg(s), want 2", len(got.GetPrefetch()))
	}
	dense, sparse := got.GetPrefetch()[0], got.GetPrefetch()[1]
	if dense.GetUsing() != "dense" || sparse.GetUsing() != "sparse" {
		t.Errorf("legs use %q and %q, want dense and sparse", dense.GetUsing(), sparse.GetUsing())
	}
	if len(dense.GetQuery().GetNearest().GetDense().GetData()) != 2 {
		t.Error("the dense leg did not carry the dense vector")
	}
	if len(sparse.GetQuery().GetNearest().GetSparse().GetIndices()) != 2 {
		t.Error("the sparse leg did not carry the sparse vector")
	}
	for _, leg := range got.GetPrefetch() {
		if leg.GetLimit() != 40 {
			t.Errorf("leg %q limit = %d, want 40", leg.GetUsing(), leg.GetLimit())
		}
		assertFilter(t, leg.GetUsing(), leg.GetFilter())
	}
	assertFilter(t, "outer", got.GetFilter())
}

func assertFilter(t *testing.T, where string, f *qdrant.Filter) {
	t.Helper()
	if f == nil {
		t.Fatalf("%s: no filter reached the wire", where)
	}
	if len(f.GetMust()) != 1 || f.GetMust()[0].GetField().GetKey() != "tenant_id" ||
		f.GetMust()[0].GetField().GetMatch().GetKeyword() != "tenant-a" {
		t.Errorf("%s: Must = %v, want exactly tenant_id == tenant-a", where, f.GetMust())
	}
	if len(f.GetMustNot()) != 1 || f.GetMustNot()[0].GetField().GetKey() != "status" {
		t.Errorf("%s: MustNot = %v, want exactly the status exclusion", where, f.GetMustNot())
	}
}

// TestExecuteQuery_AcornEnabledOnBothLegsWithoutMaxSelectivity is PRD-contrato §2.3c on the wire.
// Enable must be true on both legs, and MaxSelectivity must be absent: Qdrant decides per request
// from a cardinality estimate this process does not have, so pinning it here would be guessing with
// strictly less information. A future "tuning" commit has to delete this assertion first.
func TestExecuteQuery_AcornEnabledOnBothLegsWithoutMaxSelectivity(t *testing.T) {
	api, _, err := executeSample(t, sampleRequest())
	if err != nil {
		t.Fatalf("ExecuteQuery: %v", err)
	}

	legs := api.requests[0].GetPrefetch()
	if len(legs) != 2 {
		t.Fatalf("%d prefetch leg(s), want 2", len(legs))
	}
	for _, leg := range legs {
		acorn := leg.GetParams().GetAcorn()
		if acorn == nil {
			t.Errorf("leg %q has no Acorn params: ACORN is opt-in per request and defaults to off "+
				"(PRD-contrato §2.3c)", leg.GetUsing())
			continue
		}
		if acorn.Enable == nil || !*acorn.Enable {
			t.Errorf("leg %q has Acorn.Enable = %v, want true", leg.GetUsing(), acorn.Enable)
		}
		if acorn.MaxSelectivity != nil {
			t.Errorf("leg %q pins Acorn.MaxSelectivity = %v; it must stay at the server default "+
				"(PRD-contrato §2.3c)", leg.GetUsing(), *acorn.MaxSelectivity)
		}
	}
}

func TestExecuteQuery_AcornOffIsNotSent(t *testing.T) {
	req := sampleRequest()
	for i := range req.Prefetch {
		req.Prefetch[i].Acorn = false
	}

	api, _, err := executeSample(t, req)
	if err != nil {
		t.Fatalf("ExecuteQuery: %v", err)
	}
	for _, leg := range api.requests[0].GetPrefetch() {
		if leg.GetParams().GetAcorn() != nil {
			t.Errorf("leg %q sent Acorn params for a request that did not ask for them", leg.GetUsing())
		}
	}
}

func TestExecuteQuery_MapsScoredPoints(t *testing.T) {
	api := &fakeQueryAPI{points: []*qdrant.ScoredPoint{{
		Id:      qdrant.NewIDUUID("6f5f0a4e-2f7a-5f6a-9d1e-0d0f6f5f0a4e"),
		Score:   0.75,
		Payload: map[string]*qdrant.Value{"uid": qdrant.NewValueString("u-1"), "chunk_index": qdrant.NewValueInt(3)},
	}}}
	q, err := NewQuerier(api)
	if err != nil {
		t.Fatalf("NewQuerier: %v", err)
	}

	got, err := q.ExecuteQuery(t.Context(), sampleRequest())
	if err != nil {
		t.Fatalf("ExecuteQuery: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("%d point(s), want 1", len(got))
	}
	if got[0].ID != "6f5f0a4e-2f7a-5f6a-9d1e-0d0f6f5f0a4e" {
		t.Errorf("ID = %q", got[0].ID)
	}
	if got[0].Score != 0.75 {
		t.Errorf("Score = %v, want 0.75", got[0].Score)
	}
	if got[0].Payload["uid"] != "u-1" || got[0].Payload["chunk_index"] != 3 {
		t.Errorf("Payload = %v", got[0].Payload)
	}
}

func TestExecuteQuery_NumericPointIDIsRendered(t *testing.T) {
	api := &fakeQueryAPI{points: []*qdrant.ScoredPoint{{Id: qdrant.NewIDNum(42)}}}
	q, _ := NewQuerier(api)

	got, err := q.ExecuteQuery(t.Context(), sampleRequest())
	if err != nil {
		t.Fatalf("ExecuteQuery: %v", err)
	}
	if got[0].ID != "42" {
		t.Errorf("ID = %q, want 42", got[0].ID)
	}
}

func TestExecuteQuery_UnconvertiblePayloadIsAnError(t *testing.T) {
	api := &fakeQueryAPI{points: []*qdrant.ScoredPoint{{
		Id:      qdrant.NewIDUUID("6f5f0a4e-2f7a-5f6a-9d1e-0d0f6f5f0a4e"),
		Payload: map[string]*qdrant.Value{"score": qdrant.NewValueDouble(1.5)},
	}}}
	q, _ := NewQuerier(api)

	if _, err := q.ExecuteQuery(t.Context(), sampleRequest()); err == nil {
		t.Fatal("ExecuteQuery accepted a payload value this pipeline never writes")
	}
}

func TestExecuteQuery_APIErrorIsWrapped(t *testing.T) {
	sentinel := errors.New("unavailable")
	api := &fakeQueryAPI{err: sentinel}
	q, _ := NewQuerier(api)

	_, err := q.ExecuteQuery(t.Context(), sampleRequest())
	if !errors.Is(err, sentinel) {
		t.Fatalf("ExecuteQuery = %v, want it to wrap %v", err, sentinel)
	}
	if !strings.Contains(err.Error(), "interno") {
		t.Errorf("error %q does not name the collection", err)
	}
}

func TestExecuteQuery_LegWithBothOrNeitherVector(t *testing.T) {
	for name, leg := range map[string]retrieval.PrefetchLeg{
		"neither": {Vector: "dense", Limit: 10},
		"both": {Vector: "dense", Limit: 10, Dense: []float32{0.1},
			Sparse: embed.Sparse{Indices: []uint32{1}, Values: []float32{0.5}}},
	} {
		t.Run(name, func(t *testing.T) {
			req := sampleRequest()
			req.Prefetch = []retrieval.PrefetchLeg{leg}

			api, _, err := executeSample(t, req)
			if err == nil {
				t.Fatal("ExecuteQuery accepted a prefetch leg that is not exactly one vector")
			}
			if len(api.requests) != 0 {
				t.Error("a malformed request still reached the wire")
			}
		})
	}
}

func TestExecuteQuery_NoFusionMeansNoQuery(t *testing.T) {
	req := sampleRequest()
	req.FusionRRF = false

	api, _, err := executeSample(t, req)
	if err != nil {
		t.Fatalf("ExecuteQuery: %v", err)
	}
	if api.requests[0].GetQuery() != nil {
		t.Error("a request that did not ask for fusion still carries a Query")
	}
}

func TestExecuteQuery_EmptyFilterIsNotSent(t *testing.T) {
	req := sampleRequest()
	req.Filter = retrieval.Filter{}

	api, _, err := executeSample(t, req)
	if err != nil {
		t.Fatalf("ExecuteQuery: %v", err)
	}
	if api.requests[0].GetFilter() != nil {
		t.Error("an empty filter was sent as an empty Filter message rather than omitted")
	}
}

func TestExecuteQuery_NegativeCountsClampToZero(t *testing.T) {
	req := sampleRequest()
	req.Limit, req.Offset = -1, -1

	api, _, err := executeSample(t, req)
	if err != nil {
		t.Fatalf("ExecuteQuery: %v", err)
	}
	if got := api.requests[0]; got.GetLimit() != 0 || got.GetOffset() != 0 {
		t.Errorf("Limit/Offset = %d/%d, want 0/0 — a negative must not wrap into a full scan",
			got.GetLimit(), got.GetOffset())
	}
}

func TestExecuteQuery_NoPayloadProjectionAsksForEverything(t *testing.T) {
	req := sampleRequest()
	req.PayloadFields = nil

	api, _, err := executeSample(t, req)
	if err != nil {
		t.Fatalf("ExecuteQuery: %v", err)
	}
	if !api.requests[0].GetWithPayload().GetEnable() {
		t.Error("a request with no projection did not ask for the whole payload")
	}
}

func TestNewQuerier_NilAPI(t *testing.T) {
	if _, err := NewQuerier(nil); err == nil {
		t.Fatal("NewQuerier(nil) returned no error")
	}
}

// Compile-time proof of S07 T7's wiring claim: *store.Querier satisfies internal/retrieval's
// executor contract with no adapter type in either package. The assertion lives here rather than in
// internal/retrieval because that package must not import this one — it would drag the Qdrant
// client into a package internal/archtest forbids it in.
var _ interface {
	ExecuteQuery(ctx context.Context, req retrieval.SearchRequest) ([]retrieval.ScoredPoint, error)
} = (*Querier)(nil)

// denseOnlyRequest is what internal/retrieval builds for SearchModeDenseOnly (S10 T7): one
// top-level dense query over the filter, no prefetch leg, no fusion.
func denseOnlyRequest() retrieval.SearchRequest {
	f := retrieval.Filter{
		Must:    []retrieval.Condition{{Field: "tenant_id", Value: "tenant-a"}},
		MustNot: []retrieval.Condition{{Field: "status", Value: "archived"}},
	}
	return retrieval.SearchRequest{
		Collection:    "interno",
		Dense:         []float32{0.1, 0.2},
		DenseVector:   "dense",
		Acorn:         true,
		Filter:        f,
		Limit:         5,
		Offset:        3,
		PayloadFields: []string{"uid", "text"},
	}
}

// TestExecuteQuery_TranscribesADenseOnlyRequest is the wire half of the dense-only mode: the
// vocabulary check lives in internal/retrieval (TestBuildQueryRequest_ModeDenseOnly...), and this is
// the only place that can assert what the Qdrant client is actually handed.
func TestExecuteQuery_TranscribesADenseOnlyRequest(t *testing.T) {
	req := denseOnlyRequest()
	api, _, err := executeSample(t, req)
	if err != nil {
		t.Fatalf("ExecuteQuery: %v", err)
	}
	if len(api.requests) != 1 {
		t.Fatalf("%d request(s) went out, want 1", len(api.requests))
	}
	got := api.requests[0]

	if len(got.GetPrefetch()) != 0 {
		t.Errorf("%d prefetch(es) went out for a dense-only request, want none", len(got.GetPrefetch()))
	}
	if got.GetQuery().GetFusion() != qdrant.Fusion(0) || got.GetQuery().GetNearest() == nil {
		t.Errorf("the query that went out is not a nearest-neighbour dense query: %+v", got.GetQuery())
	}
	if !slices.Equal(got.GetQuery().GetNearest().GetDense().GetData(), req.Dense) {
		t.Error("the dense vector on the wire is not the one the request carried")
	}
	if got.GetUsing() != req.DenseVector {
		t.Errorf("Using = %q, want the named vector %q", got.GetUsing(), req.DenseVector)
	}
	if !got.GetParams().GetAcorn().GetEnable() {
		t.Error("ACORN is not enabled on the top-level search, so the dense-only mode loses the " +
			"filtered traversal the hybrid legs get")
	}
	if got.GetParams().GetAcorn().MaxSelectivity != nil {
		t.Error("MaxSelectivity was pinned; PRD-contrato §2.3c leaves it to the server")
	}
	if got.GetFilter() == nil || len(got.GetFilter().GetMust()) == 0 {
		t.Error("the filter did not reach the wire, so the dense-only search is unscoped")
	}
}

// TestExecuteQuery_RefusesBothAQueryAndPrefetches is the guard on a request that describes two
// different searches. Honouring either one silently would send a query nobody built — the one thing
// this file promises never to do.
func TestExecuteQuery_RefusesBothAQueryAndPrefetches(t *testing.T) {
	req := denseOnlyRequest()
	req.Prefetch = sampleRequest().Prefetch
	req.FusionRRF = true

	api, _, err := executeSample(t, req)
	if err == nil {
		t.Fatal("a request carrying both a top-level dense query and prefetch legs was accepted")
	}
	if len(api.requests) != 0 {
		t.Error("the ambiguous request reached the wire before being refused")
	}
	for _, want := range []string{"dense", "prefetch"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not mention %q, so it does not say what is ambiguous", err, want)
		}
	}
}

// TestExecuteQuery_NoAcornOnAFusedRequest keeps the new top-level Params from leaking into the
// hybrid shape: in hybrid mode the filter runs on the legs, and ACORN belongs there and only there
// (PRD-contrato §2.3c, and TestBuildQueryRequest_EnablesAcornOnBothPrefetches in internal/retrieval).
func TestExecuteQuery_NoAcornOnAFusedRequest(t *testing.T) {
	api, _, err := executeSample(t, sampleRequest())
	if err != nil {
		t.Fatalf("ExecuteQuery: %v", err)
	}
	if api.requests[0].GetParams() != nil {
		t.Errorf("a fused request went out with top-level search params: %+v", api.requests[0].GetParams())
	}
}
