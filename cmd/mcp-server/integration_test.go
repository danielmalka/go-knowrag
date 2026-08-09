//go:build integration

package main

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danielmalka/go-knowrag/internal/chunk"
	"github.com/danielmalka/go-knowrag/internal/embed"
	"github.com/danielmalka/go-knowrag/internal/ingest"
	"github.com/danielmalka/go-knowrag/internal/retrieval"
	"github.com/danielmalka/go-knowrag/internal/schema"
	"github.com/danielmalka/go-knowrag/internal/store"
	"github.com/danielmalka/go-knowrag/internal/vault"
)

// qdrantAPI is every store-side interface the setup below needs, named locally so this package
// never has to spell the Qdrant client's type. Embedding the three store interfaces rather than
// listing methods keeps it honest: if store adds a method to one of them, this compiles or fails
// for the same reason store's own callers do.
type qdrantAPI interface {
	store.QdrantAPI
	store.PointAPI
	store.QueryAPI
	Close() error
}

// Pinned to the deployed patch (PRD-contrato §2.3b), same as internal/store's suite and
// docker-compose.yml: a test that proved something about a different Qdrant would prove it about a
// server nobody runs.
const (
	qdrantImage        = "qdrant/qdrant:v1.19.0"
	integrationAPIKey  = "integration-test-key"
	integrationTenant  = "malka"
	integrationAddress = "127.0.0.1:6334"
)

// TestSearchKnowledge_Integration_PrivateNoteNeverReturnedForConfiguredScope is S08 T3 step 3.
//
// S07 already proves the visibility rule inside internal/retrieval. This proves it survives the
// path that actually ships: schema decode → handler → real Searcher → real Qdrant → formatResults →
// MCP response. The only fake left is the embedder, which decides *ranking*, not *filtering* — and
// with two points in the collection and top_k above two, ranking cannot be what hides the private
// chunk. Only the filter can.
func TestSearchKnowledge_Integration_PrivateNoteNeverReturnedForConfiguredScope(t *testing.T) {
	api := startQdrant(t)

	const collection = "interno"
	points, err := store.NewClient(api, collection)
	if err != nil {
		t.Fatalf("store.NewClient: %v", err)
	}

	publicNote := integrationNote(t, "research/publica.md", schema.VisibilityInternal(), "MARKER_PUBLIC_CHUNK")
	privateNote := integrationNote(t, "research/privada.md", schema.VisibilityPrivate(), "MARKER_PRIVATE_CHUNK")

	deps := ingest.Deps{
		TenantID: integrationTenant,
		Store:    points,
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
	if _, err := ingest.RunBatch(t.Context(), deps, []vault.Note{publicNote, privateNote}); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	// Non-vacuity: the private chunk really is in the collection. Without this the test would pass
	// just as happily against a seeding step that silently wrote nothing.
	seeded, err := points.ScrollByUID(t.Context(), integrationTenant, privateNote.UID)
	if err != nil {
		t.Fatalf("reading back the private note: %v", err)
	}
	if len(seeded) == 0 {
		t.Fatal("the private note was never indexed, so hiding it proves nothing")
	}

	querier, err := store.NewQuerier(api)
	if err != nil {
		t.Fatalf("store.NewQuerier: %v", err)
	}
	searcher := retrieval.NewSearcher(embed.FakeEmbedder{}, querier, retrieval.DefaultConfig())

	cfg := Config{
		Collection:       collection,
		TenantID:         integrationTenant,
		QdrantEndpoint:   integrationAddress,
		QdrantAPIKey:     integrationAPIKey,
		EmbedderEndpoint: "unused: the fake embedder is wired directly",
	}
	cs := connect(t, cfg, searcher)

	res := callRaw(t, cs, `{"query":"MARKER","top_k":20}`)
	if res.IsError {
		t.Fatalf("search_knowledge failed: %s", resultText(t, res))
	}
	body := resultText(t, res)

	if strings.Contains(body, "MARKER_PRIVATE_CHUNK") {
		t.Errorf("a visibility:private chunk came back through the MCP call path:\n%s", body)
	}
	if !strings.Contains(body, "MARKER_PUBLIC_CHUNK") {
		t.Errorf("the non-private chunk did not come back either, so the assertion above is "+
			"passing because nothing was returned at all:\n%s", body)
	}
}

// integrationNote builds a one-chunk note whose body carries a distinctive marker.
func integrationNote(t *testing.T, path string, visibility schema.Visibility, marker string) vault.Note {
	t.Helper()
	created := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	return vault.Note{
		Vault:      schema.VaultMalkaLife(),
		Path:       path,
		UID:        uuid.NewSHA1(uuid.MustParse(schema.NamespaceKnowrag), []byte("s08-integration:"+path)),
		Type:       schema.NoteTypeConcept(),
		Status:     schema.StatusStable(),
		Visibility: visibility,
		Tags:       []string{"golang"},
		Created:    created,
		Updated:    created.Add(48 * time.Hour),
		Title:      "Uma nota",
		Lang:       "pt",
		Area:       schema.AreaResearch(),
		Sub:        "curadoria",
		Body:       fmt.Sprintf("## Section\n\n%s body about MARKER content\n", marker),
	}
}

// startQdrant runs an ephemeral Qdrant and returns a provisioned client.
//
// It duplicates internal/store's helper rather than importing it because that one is unexported and
// lives in a _test.go file; there is no way to share it short of exporting a container helper from
// production code, which is worse than forty lines repeated in one other suite.
//
// It goes through store.NewQdrantClient rather than the Qdrant SDK on purpose: this package must
// not name the Qdrant client, not even in a test file, so that "cmd/mcp-server talks to Qdrant only
// through internal/retrieval and internal/store" stays true by inspection and not only by the
// architecture test, which skips _test.go files. The cost is the fixed port that
// store.NewQdrantClient pins — hence the pre-flight check below rather than an ephemeral port.
func startQdrant(t *testing.T) qdrantAPI {
	t.Helper()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not on PATH")
	}
	if conn, err := net.DialTimeout("tcp", integrationAddress, 500*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Skipf("%s is already in use — stop the docker-compose Qdrant to run this test", integrationAddress)
	}

	container := runDocker(t, "run", "-d", "--rm",
		"-e", "QDRANT__SERVICE__API_KEY="+integrationAPIKey,
		"-p", integrationAddress+":6334",
		qdrantImage)
	t.Cleanup(func() { runDocker(t, "rm", "-f", container) })

	client, err := store.NewQdrantClient(store.Config{Endpoint: integrationAddress, APIKey: integrationAPIKey})
	if err != nil {
		t.Fatalf("store.NewQdrantClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// The container is up before Qdrant answers, so provisioning doubles as the readiness probe:
	// it is the first call that has to succeed anyway.
	state := &schema.FileAppliedStateStore{Path: filepath.Join(t.TempDir(), "applied_state.json")}
	deadline := time.Now().Add(90 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		_, err = store.Apply(ctx, client, schema.Manifest(), state)
		cancel()
		if err == nil {
			return client
		}
		if time.Now().After(deadline) {
			t.Fatalf("Qdrant never became ready: %v", err)
		}
		time.Sleep(time.Second)
	}
}

func runDocker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}
