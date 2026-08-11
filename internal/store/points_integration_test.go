//go:build integration

package store

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/qdrant/go-client/qdrant"

	"github.com/danielmalka/go-knowrag/internal/chunk"
	"github.com/danielmalka/go-knowrag/internal/embed"
	"github.com/danielmalka/go-knowrag/internal/ingest"
	"github.com/danielmalka/go-knowrag/internal/schema"
	"github.com/danielmalka/go-knowrag/internal/vault"
)

// This file is S06a's proof against a real Qdrant, and it lives in internal/store rather than in
// internal/ingest for one reason: startQdrant (apply_integration_test.go) is the container helper
// S05 already owns, it is unexported, and duplicating ninety lines of `docker run` into a second
// package to satisfy a file-placement preference would be a second thing to keep in sync. The
// container is ephemeral and local — never the deployed instance.
//
// It runs the full pipeline (ingest.RunBatch → store.Client → Qdrant) rather than the three CRUD
// methods on their own, because the properties worth a container are the ones a fake cannot fake:
// that strict mode accepts the filters, that a range condition on chunk_index really is legal, that
// the payload survives the round trip with its Go types intact, and that idempotency holds against
// a server whose scroll ordering nobody controls.

// TestPoints_Integration_IngestIdempotencyAndPrune is the whole story end to end.
func TestPoints_Integration_IngestIdempotencyAndPrune(t *testing.T) {
	api := startQdrant(t)
	provision(t, api)

	collection := schema.Manifest()[0].Name
	client, err := NewClient(api, collection)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	const tenantA, tenantB = "tenant-a", "tenant-b"
	deps := integrationDeps(client, tenantA)

	note := integrationNote(t, "research/curadoria/nota.md", 8)

	// (a) First run: the note is written in full.
	report, err := ingest.RunBatch(t.Context(), deps, []vault.Note{note})
	if err != nil {
		t.Fatalf("first RunBatch: %v", err)
	}
	if got := report.Count(ingest.StatePruned); got != 1 {
		t.Fatalf("first run: %d note(s) completed, want 1 (%s)", got, report)
	}
	assertIndices(t, client, tenantA, note.UID, rangeTo(8))

	// The payload really survived the round trip, with the types the predicate compares on.
	records, err := client.ScrollByUID(t.Context(), tenantA, note.UID)
	if err != nil {
		t.Fatalf("ScrollByUID: %v", err)
	}
	assertPayloadShape(t, records[0], tenantA, note)

	// (b) Second run over an unchanged note: zero writes, proven at the wire.
	counted := &countingPointAPI{PointAPI: api}
	countedClient, err := NewClient(counted, collection)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	spy := &countingEmbedder{}
	second := integrationDeps(countedClient, tenantA)
	second.Embedder = spy

	report, err = ingest.RunBatch(t.Context(), second, []vault.Note{note})
	if err != nil {
		t.Fatalf("second RunBatch: %v", err)
	}
	if got := report.Count(ingest.StateSkipped); got != 1 {
		t.Fatalf("second run: %d note(s) skipped, want 1 (%s)", got, report)
	}
	if counted.upserts != 0 || counted.deletes != 0 {
		t.Errorf("the second run wrote to Qdrant: %d upsert(s), %d delete(s)", counted.upserts, counted.deletes)
	}
	if spy.calls != 0 {
		t.Errorf("the second run made %d embedder call(s) over an unchanged note", spy.calls)
	}

	// (c) A neighbouring tenant holds the same uid in the same collection — legal, since tenant_id
	// is part of the point ID — and must survive tenant A's prune untouched.
	neighbour := integrationDeps(client, tenantB)
	if _, err := ingest.RunBatch(t.Context(), neighbour, []vault.Note{note}); err != nil {
		t.Fatalf("seeding tenant B: %v", err)
	}
	assertIndices(t, client, tenantB, note.UID, rangeTo(8))

	// (d) The note shrinks to 5 chunks: the tail is pruned and nothing else is.
	shrunk := integrationNote(t, "research/curadoria/nota.md", 5)
	report, err = ingest.RunBatch(t.Context(), deps, []vault.Note{shrunk})
	if err != nil {
		t.Fatalf("shrink RunBatch: %v", err)
	}
	if got := report.Count(ingest.StatePruned); got != 1 {
		t.Fatalf("shrink run: %d note(s) completed, want 1 (%s)", got, report)
	}
	assertIndices(t, client, tenantA, note.UID, rangeTo(5))
	assertIndices(t, client, tenantB, note.UID, rangeTo(8))

	// (e) And the shrunk note is now idempotent in its own right.
	report, err = ingest.RunBatch(t.Context(), deps, []vault.Note{shrunk})
	if err != nil {
		t.Fatalf("post-shrink RunBatch: %v", err)
	}
	if got := report.Count(ingest.StateSkipped); got != 1 {
		t.Fatalf("post-shrink run: %d note(s) skipped, want 1 (%s)", got, report)
	}
}

// countingPointAPI counts the writes that reach the server, which is how "the second run wrote
// nothing" is proven against a live Qdrant rather than inferred from a report.
type countingPointAPI struct {
	PointAPI
	upserts int
	deletes int
}

func (c *countingPointAPI) Upsert(ctx context.Context, r *qdrant.UpsertPoints) (*qdrant.UpdateResult, error) {
	c.upserts++
	return c.PointAPI.Upsert(ctx, r)
}

func (c *countingPointAPI) Delete(ctx context.Context, r *qdrant.DeletePoints) (*qdrant.UpdateResult, error) {
	c.deletes++
	return c.PointAPI.Delete(ctx, r)
}

// countingEmbedder wraps the deterministic fake and counts calls.
type countingEmbedder struct {
	embed.FakeEmbedder
	calls int
}

func (c *countingEmbedder) EmbedDocuments(ctx context.Context, texts []string) ([]embed.Embedding, error) {
	c.calls++
	return c.FakeEmbedder.EmbedDocuments(ctx, texts)
}

func provision(t *testing.T, api *qdrant.Client) {
	t.Helper()
	state := &schema.FileAppliedStateStore{Path: filepath.Join(t.TempDir(), "applied_state.json")}
	if _, err := Apply(t.Context(), api, schema.Manifest(), state); err != nil {
		t.Fatalf("provisioning the collections: %v", err)
	}
}

func integrationDeps(store ingest.Store, tenantID string) ingest.Deps {
	return ingest.Deps{
		TenantID: tenantID,
		Store:    store,
		Embedder: embed.FakeEmbedder{},
		Handshake: embed.BackendHandshakeInfo{
			ModelRevision:     schema.BGEM3Revision,
			TokenizerRevision: schema.BGEM3Revision,
			Dim:               embed.DenseDim,
			Normalization:     embed.ExpectedNormalization,
			Pooling:           embed.ExpectedPooling,
			Precision:         embed.ExpectedPrecision,
			SparseParams:      map[string]string{"indices": "token_id", "values": "lexical_weight"},
		},
		Chunk:  chunk.Config{FloorTokens: 1, CeilingTokens: 1000},
		Tokens: chunk.FakeTokenCounter{},
	}
}

// integrationNote builds a note with n `##` sections, therefore n chunks under the config above.
func integrationNote(t *testing.T, path string, n int) vault.Note {
	t.Helper()
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "## Section %d\n\nbody of section %d\n\n", i, i)
	}
	created := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	return vault.Note{
		Vault:      "pessoal",
		Path:       path,
		UID:        uuid.NewSHA1(uuid.MustParse(schema.NamespaceKnowrag), []byte("integration:"+path)),
		Type:       schema.NoteTypeConcept(),
		Status:     schema.StatusStable(),
		Visibility: schema.VisibilityInternal(),
		Tags:       []string{"golang", "architecture"},
		Created:    created,
		Title:      "Uma nota",
		Lang:       "pt",
		Area:       "research",
		Sub:        "curadoria",
		Updated:    created.Add(48 * time.Hour),
		Body:       b.String(),
	}
}

func assertIndices(t *testing.T, client *Client, tenantID string, uid uuid.UUID, want []int) {
	t.Helper()
	records, err := client.ScrollByUID(t.Context(), tenantID, uid)
	if err != nil {
		t.Fatalf("ScrollByUID(%s): %v", tenantID, err)
	}
	got := make([]int, len(records))
	for i, r := range records {
		got[i] = r.ChunkIndex
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("%s holds chunk_index %v, want %v", tenantID, got, want)
	}
}

// assertPayloadShape checks the fields whose *type* matters on the wire — an integer chunk_index
// (the prune's range filter is refused on a keyword), a list of tags (S07 filters on one of them),
// a boolean oversize — plus that tenant_id and point_hash really made it.
func assertPayloadShape(t *testing.T, rec ingest.PointRecord, tenantID string, note vault.Note) {
	t.Helper()
	if rec.PointHash == "" {
		t.Error("the point came back with no point_hash")
	}
	if got, ok := rec.Fields["tenant_id"].(string); !ok || got != tenantID {
		t.Errorf("tenant_id read back as %v", rec.Fields["tenant_id"])
	}
	if _, ok := rec.Fields["chunk_index"].(int); !ok {
		t.Errorf("chunk_index read back as %T, want an integer", rec.Fields["chunk_index"])
	}
	if _, ok := rec.Fields["oversize"].(bool); !ok {
		t.Errorf("oversize read back as %T, want a bool", rec.Fields["oversize"])
	}
	tags, ok := rec.Fields["tags"].([]string)
	if !ok || !slices.Equal(tags, note.Tags) {
		t.Errorf("tags read back as %v, want %v", rec.Fields["tags"], note.Tags)
	}
	if _, ok := rec.Fields["point_hash"]; ok {
		t.Error("point_hash is inside Fields; condition 4 must not be able to compare it")
	}
}

func rangeTo(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}
