//go:build integration

package store

import (
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/danielmalka/go-knowrag/internal/embed"
	"github.com/danielmalka/go-knowrag/internal/ingest"
	"github.com/danielmalka/go-knowrag/internal/retrieval"
	"github.com/danielmalka/go-knowrag/internal/schema"
)

// PR 3 of the embedding migration against a real Qdrant: the dense-only profile's collections are
// created WITHOUT a sparse vector, a dense-only point is accepted, and the single dense query (no
// prefetch, no RRF) finds it through the tenant filter. The default profile's lifecycle test next door
// keeps proving the hybrid path.
func TestDenseOnlyProfile_Integration_CreateWriteQuery(t *testing.T) {
	api := startQdrant(t)
	profile, ok := schema.ProfileByID("embeddinggemma-2")
	if !ok {
		t.Fatal("embeddinggemma-2 not registered")
	}
	manifest := schema.ManifestFor(profile)
	state := &schema.FileAppliedStateStore{Path: filepath.Join(t.TempDir(), "applied_state.json")}

	if _, err := Apply(t.Context(), api, manifest, state); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, c := range manifest {
		assertLiveCollectionMatches(t, api, c)
	}
	// Idempotent and drift-free on the second run.
	if _, err := Apply(t.Context(), api, manifest, state); err != nil {
		t.Fatalf("second Apply: %v", err)
	}

	collection := manifest[0].Name
	client, err := NewClient(api, collection)
	if err != nil {
		t.Fatal(err)
	}
	fake, err := embed.NewFakeEmbedderFor(profile)
	if err != nil {
		t.Fatal(err)
	}
	uid := uuid.NewSHA1(uuid.MustParse(schema.NamespaceKnowrag), []byte("dense-only:note"))
	text := "um texto de teste sobre arquitetura"
	vec, err := fake.EmbedDocuments(t.Context(), []string{text})
	if err != nil {
		t.Fatal(err)
	}
	point := ingest.Point{
		ChunkIndex: 0, PointHash: "h",
		Fields: map[string]any{
			"tenant_id": "tenant-a", "uid": uid.String(), "chunk_index": 0, "text": text, "path": "n.md",
			"title": "Nota", "status": "stable", "visibility": "internal", "area": "research",
			"type": "concept", "vault": "pessoal", "tags": []string{"golang"}, "headings": []string{"H"},
		},
		Vector: vec[0],
	}
	if out, err := client.UpsertPoints(t.Context(), "tenant-a", uid, []ingest.Point{point}); err != nil || out != ingest.UpsertConfirmed {
		t.Fatalf("UpsertPoints = %v, %v", out, err)
	}

	querier, err := NewQuerier(api)
	if err != nil {
		t.Fatal(err)
	}
	cfg := retrieval.DefaultConfig()
	cfg.Profile = profile
	searcher := retrieval.NewSearcher(fake, querier, cfg)

	q := retrieval.Query{Collection: collection, TenantID: "tenant-a", Text: text, TopK: 3}
	hits, err := searcher.Search(t.Context(), q)
	if err != nil {
		t.Fatalf("dense-only Search: %v", err)
	}
	if len(hits) != 1 || hits[0].UID != uid.String() {
		t.Fatalf("hits = %+v, want the one point written", hits)
	}
	// the tenant filter still isolates
	q.TenantID = "tenant-b"
	if hits, err := searcher.Search(t.Context(), q); err != nil || len(hits) != 0 {
		t.Fatalf("another tenant saw %d hit(s) (%v)", len(hits), err)
	}
}
