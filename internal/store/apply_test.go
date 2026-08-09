package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/qdrant/go-client/qdrant"

	"github.com/danielmalka/go-knowrag/internal/schema"
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
	state  schema.AppliedState
	writes int
}

func (f *fakeState) Load() (schema.AppliedState, error) { return f.state, nil }

func (f *fakeState) Save(s schema.AppliedState) error {
	f.state = s
	f.writes++
	return nil
}

// liveInfo builds the CollectionInfo a Qdrant that already matches c would report.
func liveInfo(c schema.CollectionManifest) *qdrant.CollectionInfo {
	payload := map[string]*qdrant.PayloadSchemaInfo{}
	for _, idx := range c.Indexes {
		// The reported type follows the manifest entry's own kind: a fake that answered "keyword"
		// for every field would let the integer index pass a drift check it would fail live.
		live := &qdrant.PayloadSchemaInfo{DataType: payloadSchemaTypes[idx.Kind]}
		if idx.Kind == schema.FieldTypeKeyword {
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
					c.DenseVectorName: {Size: c.DenseDim, Distance: qdrantDistance(c.Distance)},
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
func upToDate(m []schema.CollectionManifest) (*fakeQdrant, *fakeState) {
	api := &fakeQdrant{info: map[string]*qdrant.CollectionInfo{}}
	state := schema.AppliedState{}
	for _, c := range m {
		api.info[c.Name] = liveInfo(c)
		state = state.With(c.Name, c.ModelRevision)
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
	m := schema.Manifest()
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
		if wantDistance := qdrantDistance(want.Distance); dense.GetDistance() != wantDistance {
			t.Errorf("%s: dense distance = %v, want %v", want.Name, dense.GetDistance(), wantDistance)
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
	custom := schema.Manifest()[:1]
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
	if _, err := Apply(t.Context(), api, schema.Manifest(), &fakeState{}); err != nil {
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

// TestEnumTranslation_MatchesTheWireEnum spells the translation table out a second time, in
// literals, for the same reason manifest_test.go spells out the collection names: every other test
// in this file routes both sides of its comparison through qdrantDistance/qdrantFieldType, so a
// wrong entry there would make them agree with each other and with nothing else. This is the one
// place that states what the mapping must be rather than reading it.
func TestEnumTranslation_MatchesTheWireEnum(t *testing.T) {
	for kind, want := range map[schema.FieldType]qdrant.FieldType{
		schema.FieldTypeKeyword: qdrant.FieldType_FieldTypeKeyword,
		schema.FieldTypeInteger: qdrant.FieldType_FieldTypeInteger,
	} {
		if got := qdrantFieldType(kind); got != want {
			t.Errorf("qdrantFieldType(%v) = %v, want %v", kind, got, want)
		}
	}
	if got := qdrantDistance(schema.DistanceCosine); got != qdrant.Distance_Cosine {
		t.Errorf("qdrantDistance(%v) = %v, want %v", schema.DistanceCosine, got, qdrant.Distance_Cosine)
	}
	// The unset value is not a metric and must not translate into one: it was Qdrant's
	// UnknownDistance before schema.Distance existed, and a silent fallback to cosine here would
	// provision a collection nobody asked for.
	if got := qdrantDistance(0); got != qdrant.Distance_UnknownDistance {
		t.Errorf("qdrantDistance(unset) = %v, want %v", got, qdrant.Distance_UnknownDistance)
	}
}

func TestApply_CreatesPayloadIndexes_IncludingTenantIsTenant(t *testing.T) {
	m := schema.Manifest()
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

	kinds := map[string]schema.FieldType{}
	for _, idx := range m[0].Indexes {
		kinds[idx.Field] = idx.Kind
	}

	tenantCalls := 0
	for _, req := range api.fieldIndexes {
		if want := qdrantFieldType(kinds[req.GetFieldName()]); req.GetFieldType() != want {
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
	m := schema.Manifest()
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
	m := schema.Manifest()
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
	m := schema.Manifest()
	api, state := upToDate(m)
	// The drifted metric is set on the Qdrant side rather than by mutating the manifest, because
	// schema.Distance declares only the one metric this project provisions — a manifest asking for
	// Dot is not a state this codebase can reach, while a live collection reporting it is exactly
	// what this check exists for.
	info := liveInfo(m[0])
	info.GetConfig().GetParams().GetVectorsConfig().GetParamsMap().GetMap()[m[0].DenseVectorName].
		Distance = qdrant.Distance_Dot
	api.info[m[0].Name] = info

	_, err := Apply(t.Context(), api, m, state)
	assertDrift(t, err, m[0].Name, qdrant.Distance_Dot.String(), qdrant.Distance_Cosine.String())
}

func TestApply_MissingNamedVector_ReturnsExplicitError(t *testing.T) {
	m := schema.Manifest()
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
	m := schema.Manifest()
	api, state := upToDate(m)
	state.state = state.state.With(m[0].Name, "bge-m3@a-previous-revision")

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
	m := schema.Manifest()
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
		rec, ok := state.state.Find(c.Name)
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
	m := schema.Manifest()
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
