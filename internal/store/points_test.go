package store

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/qdrant/go-client/qdrant"

	"github.com/danielmalka/go-knowrag/internal/embed"
	"github.com/danielmalka/go-knowrag/internal/ingest"
	"github.com/danielmalka/go-knowrag/internal/schema"
)

// fakePointAPI records the requests it was given and replays canned answers, which is how "which
// call, carrying what filter" is proven without a container.
type fakePointAPI struct {
	upserts []*qdrant.UpsertPoints
	deletes []*qdrant.DeletePoints
	scrolls []*qdrant.ScrollPoints

	upsertResult *qdrant.UpdateResult
	upsertErr    error

	pages     [][]*qdrant.RetrievedPoint
	offsets   []*qdrant.PointId
	scrollErr error
	// scrollErrOnCall makes only the Nth scroll fail (1-based, 0 = never), which is the shape a page
	// that dies mid-pagination has and a blanket scrollErr cannot express.
	scrollErrOnCall int
}

func (f *fakePointAPI) Upsert(_ context.Context, r *qdrant.UpsertPoints) (*qdrant.UpdateResult, error) {
	f.upserts = append(f.upserts, r)
	if f.upsertErr != nil {
		return nil, f.upsertErr
	}
	if f.upsertResult != nil {
		return f.upsertResult, nil
	}
	return &qdrant.UpdateResult{Status: qdrant.UpdateStatus_Completed}, nil
}

func (f *fakePointAPI) Delete(_ context.Context, r *qdrant.DeletePoints) (*qdrant.UpdateResult, error) {
	f.deletes = append(f.deletes, r)
	return &qdrant.UpdateResult{Status: qdrant.UpdateStatus_Completed}, nil
}

func (f *fakePointAPI) ScrollAndOffset(_ context.Context, r *qdrant.ScrollPoints) ([]*qdrant.RetrievedPoint, *qdrant.PointId, error) {
	f.scrolls = append(f.scrolls, r)
	if f.scrollErr != nil {
		return nil, nil, f.scrollErr
	}
	if f.scrollErrOnCall == len(f.scrolls) {
		return nil, nil, errors.New("qdrant dropped the connection mid-scroll")
	}
	i := len(f.scrolls) - 1
	if i >= len(f.pages) {
		return nil, nil, nil
	}
	var next *qdrant.PointId
	if i < len(f.offsets) {
		next = f.offsets[i]
	}
	return f.pages[i], next, nil
}

const (
	testCollection = "interno"
	testTenant     = "interno-tenant"
)

func testUID(t *testing.T) uuid.UUID {
	t.Helper()
	return uuid.MustParse("0198a7f2-4b31-7c42-9e15-3d8a92c47b6a")
}

func newTestClient(t *testing.T, api PointAPI) *Client {
	t.Helper()
	c, err := NewClient(api, testCollection)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func testPoint(index int) ingest.Point {
	return ingest.Point{
		ChunkIndex: index,
		PointHash:  "hash-of-chunk",
		Fields: map[string]any{
			"tenant_id":   testTenant,
			"chunk_index": index,
			"text":        "Alpha\n\nbody",
			"tags":        []string{"golang", "architecture"},
			"oversize":    false,
		},
		Vector: embed.Embedding{
			Dense:  make([]float32, embed.DenseDim),
			Sparse: embed.Sparse{Indices: []uint32{3, 9}, Values: []float32{0.5, 0.25}},
		},
	}
}

// TestClient_UpsertPoints_WritesIdentityVectorsAndPayload pins what one written point carries: the
// deterministic ID, both named vectors S07 queries with, and the full payload including point_hash.
func TestClient_UpsertPoints_WritesIdentityVectorsAndPayload(t *testing.T) {
	api := &fakePointAPI{}
	c := newTestClient(t, api)
	uid := testUID(t)

	outcome, err := c.UpsertPoints(t.Context(), testTenant, uid, []ingest.Point{testPoint(2)})
	if err != nil || outcome != ingest.UpsertConfirmed {
		t.Fatalf("UpsertPoints = %v, %v; want confirmed, nil", outcome, err)
	}
	if len(api.upserts) != 1 {
		t.Fatalf("%d upsert call(s), want 1", len(api.upserts))
	}
	req := api.upserts[0]

	if req.GetCollectionName() != testCollection {
		t.Errorf("collection = %q, want %q", req.GetCollectionName(), testCollection)
	}
	if !req.GetWait() {
		t.Error("wait is not set; insert-then-prune needs the write confirmed before the delete " +
			"is issued (ADR-006 §2)")
	}

	p := req.GetPoints()[0]
	if got, want := p.GetId().GetUuid(), schema.PointID(testTenant, uid, 2).String(); got != want {
		t.Errorf("point ID = %q, want %q — the ID must be uuid5(tenant_id + uid + chunk_index) or "+
			"the upsert stops being idempotent", got, want)
	}

	vectors := p.GetVectors().GetVectors().GetVectors()
	if _, ok := vectors[schema.DenseVectorName]; !ok {
		t.Errorf("no named vector %q; S07's PrefetchQuery has nothing to select", schema.DenseVectorName)
	}
	if _, ok := vectors[schema.SparseVectorName]; !ok {
		t.Errorf("no named vector %q", schema.SparseVectorName)
	}

	payload := p.GetPayload()
	if got := payload["point_hash"].GetStringValue(); got != "hash-of-chunk" {
		t.Errorf("point_hash = %q, want the value the pipeline computed", got)
	}
	if got := payload["tenant_id"].GetStringValue(); got != testTenant {
		t.Errorf("tenant_id = %q; a point without it is invisible to every tenant-scoped search", got)
	}
	if got := payload["chunk_index"].GetIntegerValue(); got != 2 {
		t.Errorf("chunk_index = %v, want the integer 2 — the prune's range filter is refused on a "+
			"keyword index", got)
	}
	if got := len(payload["tags"].GetListValue().GetValues()); got != 2 {
		t.Errorf("tags has %d element(s), want a 2-element list S07 can filter on", got)
	}
}

// TestClient_UpsertPoints_Outcomes: only an explicit Completed confirms. Everything else has to
// leave the caller unable to prune.
func TestClient_UpsertPoints_Outcomes(t *testing.T) {
	cases := []struct {
		name   string
		result *qdrant.UpdateResult
		err    error
		want   ingest.UpsertOutcome
	}{
		{"completed", &qdrant.UpdateResult{Status: qdrant.UpdateStatus_Completed}, nil, ingest.UpsertConfirmed},
		{"wait timeout", &qdrant.UpdateResult{Status: qdrant.UpdateStatus_WaitTimeout}, nil, ingest.UpsertAmbiguous},
		{"acknowledged only", &qdrant.UpdateResult{Status: qdrant.UpdateStatus_Acknowledged}, nil, ingest.UpsertAmbiguous},
		{"rpc error", nil, errors.New("unavailable"), ingest.UpsertFailed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakePointAPI{upsertResult: tc.result, upsertErr: tc.err}
			c := newTestClient(t, api)

			got, err := c.UpsertPoints(t.Context(), testTenant, testUID(t), []ingest.Point{testPoint(0)})
			if got != tc.want {
				t.Errorf("outcome = %v, want %v", got, tc.want)
			}
			if tc.want != ingest.UpsertConfirmed && err == nil {
				t.Error("a non-confirmed outcome came back with no error to report")
			}
		})
	}
}

// TestClient_DeleteByFilter_ScopedByTenantUIDAndChunkIndex is the prune's filter, condition by
// condition. Dropping the tenant_id condition would delete the neighbour tenant's points for the
// same uid, which is legal data since tenant_id is part of the point ID (ADR-006 §2).
func TestClient_DeleteByFilter_ScopedByTenantUIDAndChunkIndex(t *testing.T) {
	api := &fakePointAPI{}
	c := newTestClient(t, api)
	uid := testUID(t)

	if err := c.DeleteByFilter(t.Context(), testTenant, uid, 5); err != nil {
		t.Fatalf("DeleteByFilter: %v", err)
	}
	if len(api.deletes) != 1 {
		t.Fatalf("%d delete call(s), want 1", len(api.deletes))
	}
	req := api.deletes[0]
	if !req.GetWait() {
		t.Error("wait is not set on the prune")
	}

	must := req.GetPoints().GetFilter().GetMust()
	assertKeywordCondition(t, must, "tenant_id", testTenant)
	assertKeywordCondition(t, must, "uid", uid.String())

	var found bool
	for _, cond := range must {
		f := cond.GetField()
		if f.GetKey() != "chunk_index" {
			continue
		}
		found = true
		if got := f.GetRange().GetGte(); got != 5 {
			t.Errorf("chunk_index range gte = %v, want 5", got)
		}
	}
	if !found {
		t.Error("no chunk_index range condition; the prune would delete the whole uid, including the " +
			"points that were just written")
	}
	if len(must) != 3 {
		t.Errorf("the filter carries %d condition(s), want exactly tenant_id + uid + chunk_index", len(must))
	}
}

// TestClient_ScrollByUID_ScopedAndPaginated: the read is scoped to tenant_id + uid, never bare uid,
// asks for no vectors, and follows the offset until the server stops handing one out.
func TestClient_ScrollByUID_ScopedAndPaginated(t *testing.T) {
	uid := testUID(t)
	api := &fakePointAPI{
		pages: [][]*qdrant.RetrievedPoint{
			{retrieved(t, 0, "hash-0"), retrieved(t, 1, "hash-1")},
			{retrieved(t, 2, "hash-2")},
		},
		offsets: []*qdrant.PointId{qdrant.NewIDNum(7), nil},
	}
	c := newTestClient(t, api)

	records, err := c.ScrollByUID(t.Context(), testTenant, uid)
	if err != nil {
		t.Fatalf("ScrollByUID: %v", err)
	}
	if got, want := len(records), 3; got != want {
		t.Fatalf("%d record(s), want %d — a truncated read reports orphans as absent and makes a "+
			"broken note look integral", got, want)
	}

	indices := make([]int, len(records))
	for i, r := range records {
		indices[i] = r.ChunkIndex
	}
	if !slices.Equal(indices, []int{0, 1, 2}) {
		t.Errorf("chunk indices = %v, want 0,1,2", indices)
	}
	if records[0].PointHash != "hash-0" {
		t.Errorf("point_hash = %q, want hash-0", records[0].PointHash)
	}
	if _, ok := records[0].Fields["point_hash"]; ok {
		t.Error("point_hash is inside Fields; condition 4 must not be able to compare it")
	}
	if got := records[0].Fields["tags"]; !slices.Equal(got.([]string), []string{"a", "b"}) {
		t.Errorf("tags read back as %v, want a string list", got)
	}

	must := api.scrolls[0].GetFilter().GetMust()
	assertKeywordCondition(t, must, "tenant_id", testTenant)
	assertKeywordCondition(t, must, "uid", uid.String())
	if len(must) != 2 {
		t.Errorf("the scroll filter carries %d condition(s), want tenant_id + uid", len(must))
	}
	if api.scrolls[0].GetWithVectors().GetEnable() {
		t.Error("the scroll asks for vectors; nothing in the predicate looks at them and they are " +
			"the expensive half of a point")
	}
	if len(api.scrolls) != 2 {
		t.Errorf("%d scroll call(s), want 2 — the second page was never fetched", len(api.scrolls))
	}
}

// TestClient_ScrollByUID_RejectsAPointWithNoHash: a point written under the previous contract has no
// point_hash, and reading it as if it did would make it compare equal to nothing and integral to no
// one. It is named at the read instead.
func TestClient_ScrollByUID_RejectsAPointWithNoHash(t *testing.T) {
	api := &fakePointAPI{pages: [][]*qdrant.RetrievedPoint{{
		{Payload: qdrant.NewValueMap(map[string]any{"chunk_index": 0})},
	}}}
	c := newTestClient(t, api)

	_, err := c.ScrollByUID(t.Context(), testTenant, testUID(t))
	if err == nil {
		t.Fatal("a point with no point_hash was read without complaint")
	}
	if !strings.Contains(err.Error(), "point_hash") {
		t.Errorf("error %q does not name the missing field", err)
	}
}

// TestPayloadRoundTrip is the shared marshal/unmarshal helper's contract: what goes in comes back
// with the same Go types, so condition 4 compares like with like.
func TestPayloadRoundTrip(t *testing.T) {
	fields := map[string]any{
		"tenant_id":   testTenant,
		"chunk_index": 4,
		"oversize":    true,
		"tags":        []string{"golang", "architecture"},
		"headings":    []string{"H1", "H2"},
		"title":       "",
	}

	payload, err := marshalPayload(fields, "the-hash")
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	rec, err := unmarshalPayload(payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}

	if rec.PointHash != "the-hash" {
		t.Errorf("PointHash = %q", rec.PointHash)
	}
	if rec.ChunkIndex != 4 {
		t.Errorf("ChunkIndex = %d, want 4", rec.ChunkIndex)
	}
	for k, want := range fields {
		got := rec.Fields[k]
		if list, ok := want.([]string); ok {
			if !slices.Equal(got.([]string), list) {
				t.Errorf("%s = %v, want %v", k, got, list)
			}
			continue
		}
		if got != want {
			t.Errorf("%s = %v (%T), want %v (%T)", k, got, got, want, want)
		}
	}
}

// TestStore_NoExportedSearchFunction mirrors S07's own acceptance criterion from this side: search
// belongs to internal/retrieval, and a query path growing in here would be a second place the
// mandatory tenant_id filter has to be remembered.
//
// S07 T8 landed the fuller version of this check in architecture_test.go — an allowlist plus a
// signature check, rather than a substring denylist — and sanctioned exactly one exception,
// ExecuteQuery: it transcribes an already-finished retrieval.SearchRequest onto the wire and makes
// no search decision of its own. This test keeps the cruder check as a second net and carries that
// one exception; anything else matching the banned substrings still fails here.
func TestStore_NoExportedSearchFunction(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading internal/store: %v", err)
	}

	fset := token.NewFileSet()
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		scanned++

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !fn.Name.IsExported() || fn.Name.Name == "ExecuteQuery" {
				continue
			}
			for _, banned := range []string{"Search", "Query", "Recommend", "Discover"} {
				if strings.Contains(fn.Name.Name, banned) {
					t.Errorf("%s declares exported %s; search lives in internal/retrieval (S07)",
						name, fn.Name.Name)
				}
			}
		}
	}
	// A walk that looked at nothing would pass forever, which is the failure mode a green
	// architecture check is most likely to have.
	if scanned == 0 {
		t.Fatal("no production file was scanned, so this test proves nothing")
	}
}

func assertKeywordCondition(t *testing.T, must []*qdrant.Condition, key, want string) {
	t.Helper()
	for _, cond := range must {
		f := cond.GetField()
		if f.GetKey() == key {
			if got := f.GetMatch().GetKeyword(); got != want {
				t.Errorf("condition %s = %q, want %q", key, got, want)
			}
			return
		}
	}
	t.Errorf("no %s condition in the filter; the scope must always be tenant_id + uid, never uid "+
		"alone — two tenants may legitimately share a uid", key)
}

func retrieved(t *testing.T, index int, hash string) *qdrant.RetrievedPoint {
	t.Helper()
	return &qdrant.RetrievedPoint{
		// #nosec G115 -- index is a small non-negative test constant.
		Id: qdrant.NewIDNum(uint64(index)),
		Payload: qdrant.NewValueMap(map[string]any{
			"point_hash":  hash,
			"chunk_index": index,
			"tenant_id":   testTenant,
			"tags":        []any{"a", "b"},
			"oversize":    false,
		}),
	}
}

// TestClient_ScrollTenant_PageFailsMidway_DiscardsEverything is a deletion safety property, not a
// tidiness one.
//
// ingest.BulkScroller promises that a nil error means the whole tenant, and ingest.ScanOrphans reads
// every uid *missing* from that map as a note deleted from disk — so returning the first page with a
// nil error after the second one failed would hand --prune a list of live notes to delete. The first
// page here holds a real point precisely so the test can prove it is thrown away rather than
// returned as a smaller truth.
func TestClient_ScrollTenant_PageFailsMidway_DiscardsEverything(t *testing.T) {
	api := &fakePointAPI{
		pages: [][]*qdrant.RetrievedPoint{
			{retrievedWithUID(t, 0, "hash-0", testUID(t))},
			{retrievedWithUID(t, 1, "hash-1", testUID(t))},
		},
		offsets:         []*qdrant.PointId{qdrant.NewIDNum(7), nil},
		scrollErrOnCall: 2,
	}
	c := newTestClient(t, api)

	got, err := c.ScrollTenant(t.Context(), testTenant)
	if err == nil {
		t.Fatalf("ScrollTenant with a failed second page = %v, nil; a partial snapshot returned as "+
			"complete makes every uid it is missing look like a deleted note", got)
	}
	if got != nil {
		t.Errorf("ScrollTenant returned %v alongside its error; the accumulated pages have to be "+
			"discarded, because a caller that ignores the error would prune against them", got)
	}
	if len(api.scrolls) != 2 {
		t.Errorf("%d scroll(s) issued, want 2 — the read has to stop at the failure, not carry on",
			len(api.scrolls))
	}
}

// retrievedWithUID is retrieved() plus the uid ScrollTenant recovers from the payload, which is the
// one read that does not already know which uid it asked for.
func retrievedWithUID(t *testing.T, index int, hash string, uid uuid.UUID) *qdrant.RetrievedPoint {
	t.Helper()
	p := retrieved(t, index, hash)
	p.Payload["uid"] = qdrant.NewValueString(uid.String())
	return p
}
