package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/qdrant/go-client/qdrant"

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

// sampleRequest is the shape internal/retrieval builds: one top-level dense query over a filter, with
// ACORN on.
func sampleRequest() retrieval.SearchRequest {
	f := retrieval.Filter{
		Must:    []retrieval.Condition{{Field: "tenant_id", Value: "tenant-a"}},
		MustNot: []retrieval.Condition{{Field: "status", Value: "archived"}},
	}
	return retrieval.SearchRequest{
		Collection:    "interno_eg2",
		Dense:         []float32{0.1, 0.2},
		DenseVector:   "dense",
		Acorn:         true,
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
	req := sampleRequest()
	api, _, err := executeSample(t, req)
	if err != nil {
		t.Fatalf("ExecuteQuery: %v", err)
	}
	if len(api.requests) != 1 {
		t.Fatalf("%d request(s) reached the wire, want 1", len(api.requests))
	}
	got := api.requests[0]

	if got.GetCollectionName() != "interno_eg2" {
		t.Errorf("CollectionName = %q", got.GetCollectionName())
	}
	if got.GetLimit() != 5 || got.GetOffset() != 3 {
		t.Errorf("Limit/Offset = %d/%d, want 5/3", got.GetLimit(), got.GetOffset())
	}
	if got.GetWithVectors().GetEnable() {
		t.Error("the request asks for vectors back; nothing reads them")
	}
	if want := []string{"uid", "text"}; !slices.Equal(got.GetWithPayload().GetInclude().GetFields(), want) {
		t.Errorf("payload projection = %v, want %v", got.GetWithPayload().GetInclude().GetFields(), want)
	}
	if len(got.GetPrefetch()) != 0 {
		t.Errorf("%d prefetch(es) went out, want none: this is one dense search", len(got.GetPrefetch()))
	}
	if got.GetQuery().GetNearest() == nil || got.GetQuery().GetFusion() != qdrant.Fusion(0) {
		t.Errorf("the query that went out is not a nearest-neighbour dense query: %+v", got.GetQuery())
	}
	if !slices.Equal(got.GetQuery().GetNearest().GetDense().GetData(), req.Dense) {
		t.Error("the dense vector on the wire is not the one the request carried")
	}
	if got.GetUsing() != req.DenseVector {
		t.Errorf("Using = %q, want the named vector %q", got.GetUsing(), req.DenseVector)
	}
	assertFilter(t, "request", got.GetFilter())
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

// TestExecuteQuery_AcornEnabledWithoutMaxSelectivity is PRD-contrato §2.3c on the wire. Enable must be
// true, and MaxSelectivity must be absent: Qdrant decides per request from a cardinality estimate this
// process does not have, so pinning it here would be guessing with strictly less information. A future
// "tuning" commit has to delete this assertion first.
func TestExecuteQuery_AcornEnabledWithoutMaxSelectivity(t *testing.T) {
	api, _, err := executeSample(t, sampleRequest())
	if err != nil {
		t.Fatalf("ExecuteQuery: %v", err)
	}
	acorn := api.requests[0].GetParams().GetAcorn()
	if acorn == nil {
		t.Fatal("no Acorn params: ACORN is opt-in per request and defaults to off (PRD-contrato §2.3c)")
	}
	if acorn.Enable == nil || !*acorn.Enable {
		t.Errorf("Acorn.Enable = %v, want true", acorn.Enable)
	}
	if acorn.MaxSelectivity != nil {
		t.Errorf("Acorn.MaxSelectivity = %v is pinned; it must stay at the server default (PRD-contrato §2.3c)",
			*acorn.MaxSelectivity)
	}
}

func TestExecuteQuery_AcornOffIsNotSent(t *testing.T) {
	req := sampleRequest()
	req.Acorn = false

	api, _, err := executeSample(t, req)
	if err != nil {
		t.Fatalf("ExecuteQuery: %v", err)
	}
	if api.requests[0].GetParams().GetAcorn() != nil {
		t.Error("Acorn params were sent for a request that did not ask for them")
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
	if !strings.Contains(err.Error(), "interno_eg2") {
		t.Errorf("error %q does not name the collection", err)
	}
}

// A request with no vector is the filter alone (retrieval.Searcher.FilterMatchesAnything): the query
// is left unset and Qdrant reads the filter.
func TestExecuteQuery_NoVectorMeansNoQuery(t *testing.T) {
	req := sampleRequest()
	req.Dense, req.DenseVector, req.Acorn = nil, "", false

	api, _, err := executeSample(t, req)
	if err != nil {
		t.Fatalf("ExecuteQuery: %v", err)
	}
	if api.requests[0].GetQuery() != nil || api.requests[0].Using != nil {
		t.Error("a request with no vector still carries a Query or a vector name")
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
