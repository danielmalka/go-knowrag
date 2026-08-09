//go:build integration

// The tests in this file run the whole Search() path against a real Qdrant, provisioned by the real
// S05 schema, in an ephemeral container. They are tagged `integration` for the same reason S01 and
// S05 split the suite: `make test` has to run with no daemon, and `make test-integration` is where
// the things that need one live.
//
// The container is ephemeral on purpose and never the operator's server. What is being proven here
// is that the *filter* holds, which needs fixtures nobody else may see — two tenants, an archived
// chunk, a private chunk in each of the three collections. Seeding those into a live corpus is the
// one thing an isolation test must not do.
//
// The package is retrieval_test, not retrieval: internal/store imports internal/retrieval (it
// transcribes the request type), so a test inside package retrieval that imported store would be an
// import cycle. Talking to this package from outside is also closer to what a caller does.
package retrieval_test

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/qdrant/go-client/qdrant"

	"github.com/danielmalka/go-knowrag/internal/embed"
	"github.com/danielmalka/go-knowrag/internal/retrieval"
	"github.com/danielmalka/go-knowrag/internal/schema"
	"github.com/danielmalka/go-knowrag/internal/store"
)

// qdrantImage and integrationAPIKey mirror internal/store/apply_integration_test.go, which cannot be
// imported: its helpers live in package store's test binary. The image is pinned to the patch the
// deployed server runs (PRD-contrato §2.3b) and matches docker-compose.yml.
const (
	qdrantImage       = "qdrant/qdrant:v1.19.0"
	integrationAPIKey = "integration-test-key"
)

// seed is one fixture chunk. Everything a filter in this story looks at is a field here, so a case
// can vary exactly one of them.
type seed struct {
	tenant     string
	uid        uuid.UUID
	chunkIndex int
	text       string
	status     schema.Status
	visibility schema.Visibility
	path       string
	area       schema.Area
	vault      schema.Vault
	noteType   schema.NoteType
	tags       []string
}

func newSeed(tenant, text string) seed {
	return seed{
		tenant:     tenant,
		uid:        uuid.New(),
		text:       text,
		status:     schema.StatusStable(),
		visibility: schema.VisibilityInternal(),
		path:       "notes/" + strings.ReplaceAll(text, " ", "-") + ".md",
		area:       schema.AreaInfra(),
		vault:      schema.VaultMalkaWay(),
		noteType:   schema.NoteTypeConcept(),
	}
}

// TestSearch_Integration_TenantIsolation is S07 T9: the whole flow, not the filter shape, over
// enough points that a leaked neighbour would be visible.
func TestSearch_Integration_TenantIsolation(t *testing.T) {
	client := startQdrant(t)
	applySchema(t, client)
	searcher := newSearcher(t, client)

	const collection = "interno"
	var seeds []seed
	for i := range 12 {
		seeds = append(seeds, newSeed("tenant-a", fmt.Sprintf("cron scheduling notes number %d", i)))
		seeds = append(seeds, newSeed("tenant-b", fmt.Sprintf("cron scheduling notes number %d", i)))
	}
	seedPoints(t, client, collection, seeds)

	tenantB := map[string]bool{}
	for _, s := range seeds {
		if s.tenant == "tenant-b" {
			tenantB[s.uid.String()] = true
		}
	}

	results, err := searcher.Search(t.Context(), retrieval.Query{
		Collection: collection,
		TenantID:   "tenant-a",
		Text:       "cron scheduling",
		TopK:       20,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("tenant A's search returned nothing; the fixture never reached the collection")
	}
	for _, r := range results {
		if tenantB[r.UID] {
			t.Errorf("tenant A's search returned tenant B's point %s (%s)", r.UID, r.Path)
		}
	}
	t.Logf("tenant A saw %d of the 24 seeded points, none of them tenant B's", len(results))

	// The mirror image, so the test cannot pass because the collection is simply empty for B.
	results, err = searcher.Search(t.Context(), retrieval.Query{
		Collection: collection,
		TenantID:   "tenant-b",
		Text:       "cron scheduling",
		TopK:       20,
	})
	if err != nil {
		t.Fatalf("Search as tenant B: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("tenant B's search returned nothing; the isolation proof above is vacuous")
	}
	for _, r := range results {
		if !tenantB[r.UID] {
			t.Errorf("tenant B's search returned a point that is not tenant B's: %s (%s)", r.UID, r.Path)
		}
	}

	// A tenant nobody seeded sees nothing at all, which is what "the filter runs on the server"
	// looks like from the outside.
	results, err = searcher.Search(t.Context(), retrieval.Query{
		Collection: collection,
		TenantID:   "tenant-that-does-not-exist",
		Text:       "cron scheduling",
		TopK:       20,
	})
	if err != nil {
		t.Fatalf("Search as an unknown tenant: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("an unseeded tenant saw %d point(s)", len(results))
	}
}

// TestSearch_Integration_ArchivedExcludedByDefault is S07 T10 (a). PRD-contrato §2.4 indexes
// archived notes and filters them at query time, so the archived chunk really is in the collection
// — the default query simply must not return it.
func TestSearch_Integration_ArchivedExcludedByDefault(t *testing.T) {
	client := startQdrant(t)
	applySchema(t, client)
	searcher := newSearcher(t, client)

	const collection = "interno"
	live := newSeed("tenant-a", "cron scheduling that is current")
	archived := newSeed("tenant-a", "cron scheduling that was retired")
	archived.status = schema.StatusArchived()
	seedPoints(t, client, collection, []seed{live, archived})

	base := retrieval.Query{Collection: collection, TenantID: "tenant-a", Text: "cron scheduling", TopK: 10}

	got := searchUIDs(t, searcher, base)
	if !slices.Contains(got, live.uid.String()) {
		t.Errorf("the non-archived chunk is missing from the default search: %v", got)
	}
	if slices.Contains(got, archived.uid.String()) {
		t.Errorf("the archived chunk came back from a default search: %v", got)
	}

	withArchived := base
	withArchived.IncludeArchived = true
	got = searchUIDs(t, searcher, withArchived)
	if !slices.Contains(got, archived.uid.String()) {
		t.Errorf("IncludeArchived=true did not return the archived chunk: %v", got)
	}
}

// TestSearch_Integration_PrivateNeverReturnedByDefault_AnyCollection is S07 T10 (b). The fixture is
// seeded into all three collections because the rule that used to be keyed on the collection name
// gave a private note outside `interno` no protection at all.
func TestSearch_Integration_PrivateNeverReturnedByDefault_AnyCollection(t *testing.T) {
	client := startQdrant(t)
	applySchema(t, client)
	searcher := newSearcher(t, client)

	for _, collection := range collectionNames() {
		t.Run(collection, func(t *testing.T) {
			public := newSeed("tenant-a", "cron scheduling in the open")
			private := newSeed("tenant-a", "cron scheduling behind a curtain")
			private.visibility = schema.VisibilityPrivate()
			seedPoints(t, client, collection, []seed{public, private})

			got := searchUIDs(t, searcher, retrieval.Query{
				Collection: collection, TenantID: "tenant-a", Text: "cron scheduling", TopK: 10,
			})
			if !slices.Contains(got, public.uid.String()) {
				t.Errorf("%s: the non-private chunk is missing, so this case proves nothing: %v", collection, got)
			}
			if slices.Contains(got, private.uid.String()) {
				t.Errorf("%s: a visibility:private chunk came back from a default search: %v", collection, got)
			}
		})
	}
}

// TestSearch_Integration_PrivateReturnedOnlyWithPrivilegedFlag is S07 T10 (c): the privileged path
// has to be reachable, not merely blocked by default. A rule nobody can get past is
// indistinguishable from a rule that drops the data.
func TestSearch_Integration_PrivateReturnedOnlyWithPrivilegedFlag(t *testing.T) {
	client := startQdrant(t)
	applySchema(t, client)
	searcher := newSearcher(t, client)

	for _, collection := range collectionNames() {
		t.Run(collection, func(t *testing.T) {
			private := newSeed("tenant-a", "cron scheduling behind a curtain")
			private.visibility = schema.VisibilityPrivate()
			seedPoints(t, client, collection, []seed{private})

			got := searchUIDs(t, searcher, retrieval.Query{
				Collection: collection, TenantID: "tenant-a", Text: "cron scheduling", TopK: 10,
				IncludePrivate: true,
			})
			if !slices.Contains(got, private.uid.String()) {
				t.Errorf("%s: IncludePrivate=true did not return the private chunk: %v", collection, got)
			}

			// The privileged flag widens visibility and nothing else: another tenant's private
			// chunk stays out of reach.
			other := searchUIDs(t, searcher, retrieval.Query{
				Collection: collection, TenantID: "tenant-b", Text: "cron scheduling", TopK: 10,
				IncludePrivate: true,
			})
			if slices.Contains(other, private.uid.String()) {
				t.Errorf("%s: IncludePrivate=true crossed the tenant boundary: %v", collection, other)
			}
		})
	}
}

func collectionNames() []string {
	var out []string
	for _, c := range schema.Manifest() {
		out = append(out, c.Name)
	}
	return out
}

func searchUIDs(t *testing.T, s *retrieval.Searcher, q retrieval.Query) []string {
	t.Helper()
	results, err := s.Search(t.Context(), q)
	if err != nil {
		t.Fatalf("Search(%s/%s): %v", q.Collection, q.TenantID, err)
	}
	out := make([]string, 0, len(results))
	for _, r := range results {
		if !r.Untrusted {
			t.Errorf("result %s is not marked as untrusted content", r.UID)
		}
		out = append(out, r.UID)
	}
	return out
}

// newSearcher wires the real *store.Querier as the executor — the point of T7's interface shape —
// with the offline fake embedder, because what is under test is the filter, not the model.
func newSearcher(t *testing.T, client *qdrant.Client) *retrieval.Searcher {
	t.Helper()
	querier, err := store.NewQuerier(client)
	if err != nil {
		t.Fatalf("store.NewQuerier: %v", err)
	}
	return retrieval.NewSearcher(embed.FakeEmbedder{}, querier, retrieval.DefaultConfig())
}

func applySchema(t *testing.T, client *qdrant.Client) {
	t.Helper()
	state := &schema.FileAppliedStateStore{Path: filepath.Join(t.TempDir(), "applied_state.json")}
	if _, err := store.Apply(t.Context(), client, schema.Manifest(), state); err != nil {
		t.Fatalf("applying the schema: %v", err)
	}
}

// seedPoints writes fixtures with the raw client rather than through internal/store. This is test
// setup, not production code: internal/store's write path is S06a's story and carries its own
// contract (point_hash, the ingest.Point shape) that would only get in the way here.
func seedPoints(t *testing.T, client *qdrant.Client, collection string, seeds []seed) {
	t.Helper()

	points := make([]*qdrant.PointStruct, len(seeds))
	for i, s := range seeds {
		emb, err := embed.FakeEmbedder{}.EmbedQuery(t.Context(), s.text)
		if err != nil {
			t.Fatalf("embedding the fixture: %v", err)
		}
		payload, err := qdrant.TryValueMap(map[string]any{
			"tenant_id":   s.tenant,
			"uid":         s.uid.String(),
			"chunk_index": s.chunkIndex,
			"text":        s.text,
			"path":        s.path,
			"headings":    []any{"Infra", "Crons"},
			"status":      s.status.String(),
			"visibility":  s.visibility.String(),
			"area":        s.area.String(),
			"vault":       s.vault.String(),
			"type":        s.noteType.String(),
			"tags":        []any{"cron"},
		})
		if err != nil {
			t.Fatalf("building the fixture payload: %v", err)
		}
		points[i] = &qdrant.PointStruct{
			Id: qdrant.NewIDUUID(schema.PointID(s.tenant, s.uid, s.chunkIndex).String()),
			Vectors: qdrant.NewVectorsMap(map[string]*qdrant.Vector{
				schema.DenseVectorName:  qdrant.NewVectorDense(emb.Dense),
				schema.SparseVectorName: qdrant.NewVectorSparse(emb.Sparse.Indices, emb.Sparse.Values),
			}),
			Payload: payload,
		}
	}

	if _, err := client.Upsert(t.Context(), &qdrant.UpsertPoints{
		CollectionName: collection,
		Points:         points,
		Wait:           qdrant.PtrOf(true),
	}); err != nil {
		t.Fatalf("seeding %s: %v", collection, err)
	}
}

// startQdrant runs the pinned image and returns a client for it. Copied from
// internal/store/apply_integration_test.go for the reason stated at the top of this file — its
// helpers are not importable — and kept deliberately identical so the two suites test the same
// server. Plain `docker run` rather than a container library: the whole need is start, read a port,
// stop.
func startQdrant(t *testing.T) *qdrant.Client {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not on PATH")
	}

	out, err := runDocker(t, "run", "-d", "--rm",
		"-e", "QDRANT__SERVICE__API_KEY="+integrationAPIKey,
		"-p", "127.0.0.1:0:6334",
		qdrantImage)
	if err != nil {
		t.Fatalf("starting %s: %v", qdrantImage, err)
	}
	container := out
	t.Cleanup(func() {
		if _, err := runDocker(t, "rm", "-f", container); err != nil {
			t.Errorf("removing container %s: %v", container, err)
		}
	})

	published, err := runDocker(t, "port", container, "6334/tcp")
	if err != nil {
		t.Fatalf("reading published port: %v", err)
	}
	first := strings.TrimSpace(strings.Split(published, "\n")[0])
	port, err := strconv.Atoi(first[strings.LastIndex(first, ":")+1:])
	if err != nil {
		t.Fatalf("parsing published port from %q: %v", published, err)
	}

	client, err := qdrant.NewClient(&qdrant.Config{
		Host:                   "127.0.0.1",
		Port:                   port,
		APIKey:                 integrationAPIKey,
		PoolSize:               1,
		SkipCompatibilityCheck: true,
	})
	if err != nil {
		t.Fatalf("qdrant.NewClient: %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("closing client: %v", err)
		}
	})

	waitReady(t, client)
	return client
}

func waitReady(t *testing.T, client *qdrant.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	for {
		if _, err := client.HealthCheck(ctx); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("Qdrant did not become healthy within 60s")
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// runDocker shells out to the docker CLI with its own timeout: cleanup runs after the test context
// is cancelled, and a cleanup inheriting a cancelled context leaks the container it meant to remove.
func runDocker(t *testing.T, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// #nosec G204 -- every argument is a literal or a container ID this function produced.
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out)), nil
}
