package store

import (
	"testing"

	"github.com/qdrant/go-client/qdrant"

	"github.com/danielmalka/go-knowrag/internal/embed"
	"github.com/danielmalka/go-knowrag/internal/ingest"
	"github.com/danielmalka/go-knowrag/internal/schema"
)

// PR 3 of the embedding migration, store side: a dense-only profile's collection has one named dense
// vector and NO sparse vector — created without it, checked for drift without it, written without it.

func eg2Manifest(t *testing.T) []schema.CollectionManifest {
	t.Helper()
	p, ok := schema.ProfileByID("embeddinggemma-2")
	if !ok {
		t.Fatal("embeddinggemma-2 not registered")
	}
	return schema.ManifestFor(p)
}

func TestApply_DenseOnlyProfile_CreatesCollectionsWithoutASparseVector(t *testing.T) {
	m := eg2Manifest(t)
	api, state := &fakeQdrant{}, &fakeState{}
	if _, err := Apply(t.Context(), api, m, state); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(api.created) != len(m) {
		t.Fatalf("%d collection(s) created, want %d", len(api.created), len(m))
	}
	for i, req := range api.created {
		if req.GetCollectionName() != m[i].Name {
			t.Errorf("created[%d] = %q, want %q", i, req.GetCollectionName(), m[i].Name)
		}
		dense := denseParams(t, req, m[i].DenseVectorName)
		if dense.GetSize() != 768 || dense.GetDistance() != qdrant.Distance_Cosine {
			t.Errorf("%s: dense %d/%v, want 768/Cosine", m[i].Name, dense.GetSize(), dense.GetDistance())
		}
		if req.GetSparseVectorsConfig() != nil {
			t.Errorf("%s: a dense-only collection was created with a sparse vectors config: %v",
				m[i].Name, req.GetSparseVectorsConfig())
		}
	}
}

func TestApply_DenseOnlyProfile_UpToDateIsNotDrift(t *testing.T) {
	m := eg2Manifest(t)
	api, state := upToDate(m)
	if _, err := Apply(t.Context(), api, m, state); err != nil {
		t.Fatalf("a matching dense-only collection was reported as drift: %v", err)
	}
	if len(api.created) != 0 {
		t.Errorf("an existing collection was created again: %d", len(api.created))
	}
}

// A live collection that carries a sparse vector under a dense-only name is another profile's
// collection (or a hand-made one): that is drift, not a harmless extra.
func TestApply_DenseOnlyProfile_LiveSparseVectorIsDrift(t *testing.T) {
	m := eg2Manifest(t)
	api, state := upToDate(m)
	hybrid := m[0]
	hybrid.SparseVectorName = schema.SparseVectorName
	api.info[m[0].Name] = liveInfo(hybrid)

	_, err := Apply(t.Context(), api, m, state)
	assertDrift(t, err, m[0].Name, schema.SparseVectorName, "none")
}

// The default profile still creates and checks its sparse vector exactly as before.
func TestApply_DefaultProfile_StillCreatesTheSparseVector(t *testing.T) {
	req := createCollectionRequest(schema.Manifest()[0])
	if _, ok := req.GetSparseVectorsConfig().GetMap()[schema.SparseVectorName]; !ok {
		t.Fatal("the default profile's collection lost its sparse vector")
	}
}

func TestClient_UpsertPoints_DenseOnlyEmbeddingWritesOnlyTheDenseVector(t *testing.T) {
	api := &fakePointAPI{}
	c := newTestClient(t, api)
	p := testPoint(1)
	p.Vector = embed.Embedding{Dense: make([]float32, 768)}

	outcome, err := c.UpsertPoints(t.Context(), testTenant, testUID(t), []ingest.Point{p})
	if err != nil || outcome != ingest.UpsertConfirmed {
		t.Fatalf("UpsertPoints = %v, %v", outcome, err)
	}
	vectors := api.upserts[0].GetPoints()[0].GetVectors().GetVectors().GetVectors()
	if _, ok := vectors[schema.DenseVectorName]; !ok {
		t.Error("the dense vector is missing")
	}
	if _, ok := vectors[schema.SparseVectorName]; ok {
		t.Error("a dense-only point carries a sparse vector the collection has no config for")
	}
	if len(vectors) != 1 {
		t.Errorf("%d named vectors, want 1", len(vectors))
	}
}
