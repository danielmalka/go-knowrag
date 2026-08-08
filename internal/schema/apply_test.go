package schema

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/qdrant/go-client/qdrant"
)

// fakeQdrant is the whole Qdrant side of Apply, in memory. Every write is recorded rather than
// applied: the assertions in this file are about which calls Apply makes and what they carry, and
// a fake that mutated its own state would let a test pass by agreeing with itself.
type fakeQdrant struct {
	// info holds the collections that already exist. A name absent from the map does not exist.
	info map[string]*qdrant.CollectionInfo

	created      []*qdrant.CreateCollection
	fieldIndexes []*qdrant.CreateFieldIndexCollection
}

func (f *fakeQdrant) CollectionExists(_ context.Context, name string) (bool, error) {
	_, ok := f.info[name]
	return ok, nil
}

func (f *fakeQdrant) CreateCollection(_ context.Context, req *qdrant.CreateCollection) error {
	f.created = append(f.created, req)
	return nil
}

func (f *fakeQdrant) GetCollectionInfo(_ context.Context, name string) (*qdrant.CollectionInfo, error) {
	info, ok := f.info[name]
	if !ok {
		return nil, errors.New("collection does not exist: " + name)
	}
	return info, nil
}

func (f *fakeQdrant) CreateFieldIndex(_ context.Context, req *qdrant.CreateFieldIndexCollection) (*qdrant.UpdateResult, error) {
	f.fieldIndexes = append(f.fieldIndexes, req)
	return &qdrant.UpdateResult{}, nil
}

// fakeState is an AppliedStateStore that never touches disk and counts its writes, which is how
// "second run writes nothing" is proven rather than assumed.
type fakeState struct {
	state  AppliedState
	writes int
}

func (f *fakeState) Load() (AppliedState, error) { return f.state, nil }

func (f *fakeState) Save(s AppliedState) error {
	f.state = s
	f.writes++
	return nil
}

// liveInfo builds the CollectionInfo a Qdrant that already matches c would report.
func liveInfo(c CollectionManifest) *qdrant.CollectionInfo {
	payload := map[string]*qdrant.PayloadSchemaInfo{}
	for _, idx := range c.Indexes {
		// The reported type follows the manifest entry's own kind: a fake that answered "keyword"
		// for every field would let the integer index pass a drift check it would fail live.
		live := &qdrant.PayloadSchemaInfo{DataType: payloadSchemaTypes[idx.Kind]}
		if idx.Kind == qdrant.FieldType_FieldTypeKeyword {
			live.Params = qdrant.NewPayloadIndexParamsKeyword(&qdrant.KeywordIndexParams{
				IsTenant: qdrant.PtrOf(idx.IsTenant),
			})
		}
		payload[idx.Field] = live
	}
	return &qdrant.CollectionInfo{
		Config: &qdrant.CollectionConfig{
			Params: &qdrant.CollectionParams{
				VectorsConfig: qdrant.NewVectorsConfigMap(map[string]*qdrant.VectorParams{
					c.DenseVectorName: {Size: c.DenseDim, Distance: c.Distance},
				}),
				SparseVectorsConfig: qdrant.NewSparseVectorsConfig(map[string]*qdrant.SparseVectorParams{
					c.SparseVectorName: {},
				}),
			},
		},
		PayloadSchema: payload,
	}
}

// upToDate is the fake pair for "a Qdrant and a state record that already match the manifest".
func upToDate(m []CollectionManifest) (*fakeQdrant, *fakeState) {
	api := &fakeQdrant{info: map[string]*qdrant.CollectionInfo{}}
	state := AppliedState{}
	for _, c := range m {
		api.info[c.Name] = liveInfo(c)
		state = state.with(c.Name, c.ModelRevision)
	}
	return api, &fakeState{state: state}
}

func denseParams(t *testing.T, req *qdrant.CreateCollection, name string) *qdrant.VectorParams {
	t.Helper()
	params := req.GetVectorsConfig().GetParamsMap().GetMap()[name]
	if params == nil {
		t.Fatalf("%s: CreateCollection carries no named dense vector %q", req.GetCollectionName(), name)
	}
	return params
}

func TestApply_CreatesCollectionsFromManifest_WhenNoneExist(t *testing.T) {
	m := Manifest()
	api := &fakeQdrant{}
	state := &fakeState{}

	if _, err := Apply(t.Context(), api, m, state); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if len(api.created) != len(m) {
		t.Fatalf("CreateCollection called %d time(s), want %d — one per manifest entry", len(api.created), len(m))
	}
	for i, req := range api.created {
		want := m[i]
		if req.GetCollectionName() != want.Name {
			t.Errorf("created[%d] name = %q, want %q", i, req.GetCollectionName(), want.Name)
		}
		dense := denseParams(t, req, want.DenseVectorName)
		if dense.GetSize() != want.DenseDim {
			t.Errorf("%s: dense size = %d, want %d", want.Name, dense.GetSize(), want.DenseDim)
		}
		if dense.GetDistance() != want.Distance {
			t.Errorf("%s: dense distance = %v, want %v", want.Name, dense.GetDistance(), want.Distance)
		}
		if _, ok := req.GetSparseVectorsConfig().GetMap()[want.SparseVectorName]; !ok {
			t.Errorf("%s: CreateCollection carries no named sparse vector %q", want.Name, want.SparseVectorName)
		}
	}
}

// TestApply_UsesTheManifestItWasGiven is the mechanical proof that nothing in Apply names a
// collection: given a manifest that contains none of the three real names, it must provision
// exactly what it was handed.
func TestApply_UsesTheManifestItWasGiven(t *testing.T) {
	custom := Manifest()[:1]
	custom[0].Name = "collection-that-is-not-in-the-real-manifest"
	api := &fakeQdrant{}

	if _, err := Apply(t.Context(), api, custom, &fakeState{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if len(api.created) != 1 {
		t.Fatalf("CreateCollection called %d time(s), want 1", len(api.created))
	}
	if got := api.created[0].GetCollectionName(); got != custom[0].Name {
		t.Errorf("created collection %q, want %q", got, custom[0].Name)
	}
}

func TestApply_CreatesCollection_WithStrictModeEnabled(t *testing.T) {
	api := &fakeQdrant{}
	if _, err := Apply(t.Context(), api, Manifest(), &fakeState{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	for _, req := range api.created {
		strict := req.GetStrictModeConfig()
		if !strict.GetEnabled() {
			t.Errorf("%s: strict mode is not enabled", req.GetCollectionName())
		}
		// Strict mode on its own still permits unindexed filtering; these two are what turn a
		// filter on a field with no payload index into a refusal instead of a full scan.
		if strict.GetUnindexedFilteringRetrieve() {
			t.Errorf("%s: unindexed filtering is allowed on retrieval", req.GetCollectionName())
		}
		if strict.GetUnindexedFilteringUpdate() {
			t.Errorf("%s: unindexed filtering is allowed on update", req.GetCollectionName())
		}
	}
}

func TestApply_CreatesPayloadIndexes_IncludingTenantIsTenant(t *testing.T) {
	m := Manifest()
	api := &fakeQdrant{info: map[string]*qdrant.CollectionInfo{}}
	for _, c := range m {
		// The collections exist, but with no payload index at all.
		info := liveInfo(c)
		info.PayloadSchema = nil
		api.info[c.Name] = info
	}

	if _, err := Apply(t.Context(), api, m, &fakeState{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	wantCalls := len(m) * len(m[0].Indexes)
	if len(api.fieldIndexes) != wantCalls {
		t.Fatalf("CreateFieldIndex called %d time(s), want %d (%d collections x %d indexes)",
			len(api.fieldIndexes), wantCalls, len(m), len(m[0].Indexes))
	}

	kinds := map[string]qdrant.FieldType{}
	for _, idx := range m[0].Indexes {
		kinds[idx.Field] = idx.Kind
	}

	tenantCalls := 0
	for _, req := range api.fieldIndexes {
		if want := kinds[req.GetFieldName()]; req.GetFieldType() != want {
			t.Errorf("%s/%s: field type = %v, want %v",
				req.GetCollectionName(), req.GetFieldName(), req.GetFieldType(), want)
		}
		isTenant := req.GetFieldIndexParams().GetKeywordIndexParams().GetIsTenant()
		if want := req.GetFieldName() == "tenant_id"; isTenant != want {
			t.Errorf("%s/%s: is_tenant = %v, want %v",
				req.GetCollectionName(), req.GetFieldName(), isTenant, want)
		}
		if isTenant {
			tenantCalls++
		}
	}
	if tenantCalls != len(m) {
		t.Errorf("%d index(es) created with is_tenant, want %d — one per collection", tenantCalls, len(m))
	}
}

func TestApply_SecondRun_NoWrites(t *testing.T) {
	m := Manifest()
	api, state := upToDate(m)

	report, err := Apply(t.Context(), api, m, state)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if len(api.created) != 0 {
		t.Errorf("CreateCollection called %d time(s) on an already-correct Qdrant, want 0", len(api.created))
	}
	if len(api.fieldIndexes) != 0 {
		t.Errorf("CreateFieldIndex called %d time(s) on an already-correct Qdrant, want 0", len(api.fieldIndexes))
	}
	if state.writes != 0 {
		t.Errorf("applied state written %d time(s) on a no-op run, want 0", state.writes)
	}

	if len(report.Collections) != len(m) {
		t.Fatalf("report covers %d collection(s), want %d", len(report.Collections), len(m))
	}
	for _, cr := range report.Collections {
		if !cr.UpToDate() {
			t.Errorf("%s: report says %q, want already up to date", cr.Name, cr)
		}
	}
}

func TestApply_DimensionMismatch_ReturnsExplicitError(t *testing.T) {
	m := Manifest()
	api, state := upToDate(m)
	drifted := m[0]
	drifted.DenseDim = 768
	api.info[m[0].Name] = liveInfo(drifted)

	_, err := Apply(t.Context(), api, m, state)
	assertDrift(t, err, m[0].Name, "768", "1024")

	// Drift is fatal, never auto-healed: rewriting a collection whose vectors were produced by a
	// different geometry would silently destroy every point already in it.
	if len(api.created) != 0 {
		t.Errorf("CreateCollection called %d time(s) on a drifted collection, want 0", len(api.created))
	}
	if state.writes != 0 {
		t.Errorf("applied state written %d time(s) on a failed apply, want 0", state.writes)
	}
}

func TestApply_DistanceMismatch_ReturnsExplicitError(t *testing.T) {
	m := Manifest()
	api, state := upToDate(m)
	drifted := m[0]
	drifted.Distance = qdrant.Distance_Dot
	api.info[m[0].Name] = liveInfo(drifted)

	_, err := Apply(t.Context(), api, m, state)
	assertDrift(t, err, m[0].Name, qdrant.Distance_Dot.String(), qdrant.Distance_Cosine.String())
}

func TestApply_MissingNamedVector_ReturnsExplicitError(t *testing.T) {
	m := Manifest()
	api, state := upToDate(m)
	drifted := m[0]
	drifted.SparseVectorName = "something-else"
	api.info[m[0].Name] = liveInfo(drifted)

	_, err := Apply(t.Context(), api, m, state)
	assertDrift(t, err, m[0].Name, "", m[0].SparseVectorName)
}

// TestApply_ModelRevisionMismatch_ReturnsExplicitError covers the half of drift Qdrant cannot see:
// an empty collection has no point to compare against, and Qdrant stores no "which model wrote
// this" attribute, so the only oracle is the record this repo commits alongside the manifest.
func TestApply_ModelRevisionMismatch_ReturnsExplicitError(t *testing.T) {
	m := Manifest()
	api, state := upToDate(m)
	state.state = state.state.with(m[0].Name, "bge-m3@a-previous-revision")

	_, err := Apply(t.Context(), api, m, state)
	assertDrift(t, err, m[0].Name, "bge-m3@a-previous-revision", m[0].ModelRevision)

	if len(api.created) != 0 || len(api.fieldIndexes) != 0 {
		t.Errorf("model-revision drift still wrote to Qdrant: %d create(s), %d index(es)",
			len(api.created), len(api.fieldIndexes))
	}
	if state.writes != 0 {
		t.Errorf("applied state written %d time(s) on a failed apply, want 0", state.writes)
	}
}

func TestApply_FirstRun_WritesAppliedState(t *testing.T) {
	m := Manifest()
	state := &fakeState{}

	if _, err := Apply(t.Context(), &fakeQdrant{}, m, state); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if state.writes != 1 {
		t.Fatalf("applied state written %d time(s) on a first apply, want 1", state.writes)
	}
	if len(state.state.Collections) != len(m) {
		t.Fatalf("applied state holds %d record(s), want %d", len(state.state.Collections), len(m))
	}
	for _, c := range m {
		rec, ok := state.state.find(c.Name)
		if !ok {
			t.Errorf("no applied-state record for %s", c.Name)
			continue
		}
		if rec.EmbeddingModel != c.ModelRevision {
			t.Errorf("%s: recorded revision %q, want %q", c.Name, rec.EmbeddingModel, c.ModelRevision)
		}
		if rec.AppliedAt.IsZero() {
			t.Errorf("%s: recorded applied_at is the zero time", c.Name)
		}
	}
}

// TestApply_EmptyState_IsNotDrift is the flip side of the revision check: a fresh checkout that has
// never applied anything has no record, and treating "no record" as a mismatch would make every
// first run fail.
func TestApply_EmptyState_IsNotDrift(t *testing.T) {
	m := Manifest()
	api, _ := upToDate(m)

	report, err := Apply(t.Context(), api, m, &fakeState{})
	if err != nil {
		t.Fatalf("Apply with an empty applied-state store: %v", err)
	}
	if len(report.Collections) != len(m) {
		t.Errorf("report covers %d collection(s), want %d", len(report.Collections), len(m))
	}
}

func assertDrift(t *testing.T, err error, collection, actual, expected string) {
	t.Helper()
	if err == nil {
		t.Fatal("Apply returned a nil error, want schema drift")
	}
	if !errors.Is(err, ErrSchemaDrift) {
		t.Errorf("error %v does not match ErrSchemaDrift", err)
	}
	msg := err.Error()
	for _, want := range []string{collection, actual, expected} {
		if want == "" {
			continue
		}
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not name %q", msg, want)
		}
	}
	// The operator has to learn from the message itself that this is not a fix-in-place situation.
	if !strings.Contains(msg, "three collections") {
		t.Errorf("error %q does not state the reindex-all-three rule", msg)
	}
}

// TestAppliedState_With_DoesNotMutateTheReceiver guards the property Apply relies on to leave the
// loaded state untouched until every collection has succeeded.
func TestAppliedState_With_DoesNotMutateTheReceiver(t *testing.T) {
	loaded := AppliedState{}.with("interno", "rev-a")
	updated := loaded.with("interno", "rev-b")

	rec, _ := loaded.find("interno")
	if rec.EmbeddingModel != "rev-a" {
		t.Errorf("with() changed the receiver: %q, want %q", rec.EmbeddingModel, "rev-a")
	}
	rec, _ = updated.find("interno")
	if rec.EmbeddingModel != "rev-b" {
		t.Errorf("with() returned %q, want %q", rec.EmbeddingModel, "rev-b")
	}
}

// TestAppliedState_With_KeepsRecordsSorted keeps the committed applied_state.json diff readable:
// map iteration order must never reach the file.
func TestAppliedState_With_KeepsRecordsSorted(t *testing.T) {
	s := AppliedState{}.with("interno", "r").with("base_paga", "r").with("clientes", "r")

	names := make([]string, len(s.Collections))
	for i, c := range s.Collections {
		names[i] = c.ManagedCollection
	}
	if !slices.IsSorted(names) {
		t.Errorf("applied-state records are %v, want them sorted by collection name", names)
	}
}
