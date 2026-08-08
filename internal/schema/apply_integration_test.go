//go:build integration

package schema

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qdrant/go-client/qdrant"
)

// qdrantImage is pinned to the exact patch the deployed server runs (PRD-contrato §2.3b) and
// matches docker-compose.yml. A test that proved the schema applies to some other Qdrant would
// prove it about a server nobody runs.
const qdrantImage = "qdrant/qdrant:v1.19.0"

// integrationAPIKey is a throwaway credential for a container that lives for the duration of one
// test and publishes only on loopback. It is here rather than in the environment so the test runs
// with no setup; nothing else in this repo ever accepts a literal key.
const integrationAPIKey = "integration-test-key"

// TestApply_Integration_FullLifecycle is the end-to-end proof of this story: every acceptance
// criterion, against a real Qdrant, in the order an operator meets them.
//
// It talks to the container through qdrant.NewClient rather than store.NewQdrantClient because the
// container publishes gRPC on an ephemeral host port, and store.NewQdrantClient deliberately pins
// 6334 (see internal/store/client.go). An ephemeral port is what keeps this test from colliding
// with a docker-compose.yml left running on 6334; the pinned-port rule itself is covered by unit
// tests in internal/store.
func TestApply_Integration_FullLifecycle(t *testing.T) {
	client := startQdrant(t)
	statePath := filepath.Join(t.TempDir(), "applied_state.json")
	state := &FileAppliedStateStore{Path: statePath}
	m := Manifest()

	// (a) A clean instance: everything gets created.
	counted := &countingAPI{QdrantAPI: client}
	report, err := Apply(t.Context(), counted, m, state)
	if err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if len(report.Collections) != len(m) {
		t.Fatalf("report covers %d collection(s), want %d", len(report.Collections), len(m))
	}

	// Exactly the three manifest collections exist — no operational fourth for locks or state.
	live, err := client.ListCollections(t.Context())
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	if len(live) != len(m) {
		t.Fatalf("Qdrant holds %d collection(s) (%v), want exactly %d", len(live), live, len(m))
	}
	for _, c := range m {
		assertLiveCollectionMatches(t, client, c)
	}

	// The applied-state record now names every collection at the manifest's revision.
	written, err := state.Load()
	if err != nil {
		t.Fatalf("loading applied state: %v", err)
	}
	for _, c := range m {
		rec, ok := written.find(c.Name)
		if !ok {
			t.Errorf("no applied-state record for %s", c.Name)
			continue
		}
		if rec.EmbeddingModel != c.ModelRevision {
			t.Errorf("%s: recorded revision %q, want %q", c.Name, rec.EmbeddingModel, c.ModelRevision)
		}
	}

	// (b) Second run: a proven no-op, counted at the wire rather than inferred from the report.
	counted = &countingAPI{QdrantAPI: client}
	report, err = Apply(t.Context(), counted, m, state)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if counted.creates != 0 || counted.fieldIndexes != 0 {
		t.Errorf("second Apply wrote to Qdrant: %d collection create(s), %d index create(s)",
			counted.creates, counted.fieldIndexes)
	}
	for _, cr := range report.Collections {
		if !cr.UpToDate() {
			t.Errorf("second Apply reports %q, want already up to date", cr)
		}
	}

	// (c) Dimension drift, against a live collection that really is 1024.
	drifted := Manifest()
	drifted[0].DenseDim = 768
	_, err = Apply(t.Context(), client, drifted, state)
	assertIntegrationDrift(t, err, drifted[0].Name, "768")

	// (d) Model-revision drift, with dimension and distance still matching — the half Qdrant
	// cannot see, caught only because the record is compared against the manifest.
	drifted = Manifest()
	drifted[0].ModelRevision = "bge-m3@a-different-revision"
	_, err = Apply(t.Context(), client, drifted, state)
	assertIntegrationDrift(t, err, drifted[0].Name, "bge-m3@a-different-revision")

	// (e) Strict mode: a filter on a field with no payload index is refused, not scanned.
	assertUnindexedFilterRefused(t, client, m[0].Name)

	// (f) The tenant index really carries is_tenant, on every collection.
	for _, c := range m {
		assertTenantIndex(t, client, c)
	}

	// (g) The filters S06a actually issues are accepted. Strict mode makes the manifest's index
	// list a hard contract with the stories downstream, so this runs their two filtered calls
	// verbatim against what Apply just provisioned.
	assertS06aFiltersAccepted(t, client, m[0].Name)
}

// assertS06aFiltersAccepted issues S06a's two filtered operations — ScrollByUID (T1) and
// DeleteByFilter's prune (T6) — against the live collection.
//
// It exists because strict mode turns a field missing from the manifest into an InvalidArgument at
// the call site, in a story that is not this one: without this step, dropping an index here still
// passes every S05 test and only fails once S06a is written. Both calls are made on an empty
// collection on purpose — nothing here is about the points returned, only about whether Qdrant
// accepts the filter at all.
func assertS06aFiltersAccepted(t *testing.T, client *qdrant.Client, collection string) {
	t.Helper()
	const (
		tenantID = "integration-tenant"
		uid      = "integration-uid"
	)
	scope := []*qdrant.Condition{
		qdrant.NewMatchKeyword("tenant_id", tenantID),
		qdrant.NewMatchKeyword("uid", uid),
	}

	// ScrollByUID: tenant_id + uid, never bare uid (S06a T1).
	if _, err := client.Scroll(t.Context(), &qdrant.ScrollPoints{
		CollectionName: collection,
		Filter:         &qdrant.Filter{Must: scope},
		Limit:          qdrant.PtrOf(uint32(1)),
	}); err != nil {
		t.Errorf("S06a ScrollByUID filter (tenant_id + uid) was refused by the live collection: %v", err)
	}

	// DeleteByFilter: the same scope plus chunk_index >= N, which prunes the tail of a note that
	// got shorter. The range condition is why chunk_index is indexed as an integer — a keyword
	// index would be rejected here even though the field is indexed.
	if _, err := client.Delete(t.Context(), &qdrant.DeletePoints{
		CollectionName: collection,
		Points: qdrant.NewPointsSelectorFilter(&qdrant.Filter{
			Must: append(slices.Clone(scope), qdrant.NewRange("chunk_index", &qdrant.Range{
				Gte: qdrant.PtrOf(float64(3)),
			})),
		}),
		Wait: qdrant.PtrOf(true),
	}); err != nil {
		t.Errorf("S06a DeleteByFilter filter (tenant_id + uid + chunk_index >= 3) was refused by the live collection: %v", err)
	}
}

// countingAPI passes every call through to the real client and counts the writes, which is how
// "the second run performs no writes" is proven against a live server rather than a fake.
type countingAPI struct {
	QdrantAPI
	creates      int
	fieldIndexes int
}

func (c *countingAPI) CreateCollection(ctx context.Context, request *qdrant.CreateCollection) error {
	c.creates++
	return c.QdrantAPI.CreateCollection(ctx, request)
}

func (c *countingAPI) CreateFieldIndex(ctx context.Context, request *qdrant.CreateFieldIndexCollection) (*qdrant.UpdateResult, error) {
	c.fieldIndexes++
	return c.QdrantAPI.CreateFieldIndex(ctx, request)
}

func assertLiveCollectionMatches(t *testing.T, client *qdrant.Client, c CollectionManifest) {
	t.Helper()
	info, err := client.GetCollectionInfo(t.Context(), c.Name)
	if err != nil {
		t.Fatalf("GetCollectionInfo(%s): %v", c.Name, err)
	}
	params := info.GetConfig().GetParams()

	dense, ok := params.GetVectorsConfig().GetParamsMap().GetMap()[c.DenseVectorName]
	if !ok {
		t.Fatalf("%s: no named dense vector %q", c.Name, c.DenseVectorName)
	}
	if dense.GetSize() != c.DenseDim {
		t.Errorf("%s: dense dimension = %d, want %d", c.Name, dense.GetSize(), c.DenseDim)
	}
	if dense.GetDistance() != c.Distance {
		t.Errorf("%s: distance = %v, want %v", c.Name, dense.GetDistance(), c.Distance)
	}
	if _, ok := params.GetSparseVectorsConfig().GetMap()[c.SparseVectorName]; !ok {
		t.Errorf("%s: no named sparse vector %q", c.Name, c.SparseVectorName)
	}
	if !info.GetConfig().GetStrictModeConfig().GetEnabled() {
		t.Errorf("%s: strict mode is not enabled on the live collection", c.Name)
	}
	if got, want := len(info.GetPayloadSchema()), len(c.Indexes); got != want {
		t.Errorf("%s: %d payload index(es), want %d", c.Name, got, want)
	}
}

func assertTenantIndex(t *testing.T, client *qdrant.Client, c CollectionManifest) {
	t.Helper()
	info, err := client.GetCollectionInfo(t.Context(), c.Name)
	if err != nil {
		t.Fatalf("GetCollectionInfo(%s): %v", c.Name, err)
	}
	for _, idx := range c.Indexes {
		live, ok := info.GetPayloadSchema()[idx.Field]
		if !ok {
			t.Errorf("%s: field %q is not indexed", c.Name, idx.Field)
			continue
		}
		if want := payloadSchemaTypes[idx.Kind]; live.GetDataType() != want {
			t.Errorf("%s: index %q data type = %v, want %v", c.Name, idx.Field, live.GetDataType(), want)
		}
		if idx.Kind != qdrant.FieldType_FieldTypeKeyword {
			continue
		}
		if got := live.GetParams().GetKeywordIndexParams().GetIsTenant(); got != idx.IsTenant {
			t.Errorf("%s: index %q is_tenant = %v, want %v", c.Name, idx.Field, got, idx.IsTenant)
		}
	}
}

// assertUnindexedFilterRefused issues the query strict mode exists to stop. The field is chosen to
// be absent from the manifest's Indexes on purpose: filtering on it is exactly the mistake that,
// without strict mode, answers correctly after a full scan and looks like a slow day.
func assertUnindexedFilterRefused(t *testing.T, client *qdrant.Client, collection string) {
	t.Helper()
	_, err := client.Query(t.Context(), &qdrant.QueryPoints{
		CollectionName: collection,
		Filter: &qdrant.Filter{
			Must: []*qdrant.Condition{qdrant.NewMatchKeyword("a_field_nobody_indexed", "x")},
		},
		Limit: qdrant.PtrOf(uint64(1)),
	})
	if err == nil {
		t.Fatalf("%s: a filter on an unindexed field was accepted — strict mode is not in force", collection)
	}
	t.Logf("strict mode refused the unindexed filter: %v", err)
}

func assertIntegrationDrift(t *testing.T, err error, collection, actual string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: Apply returned no error, want schema drift", collection)
	}
	if !errors.Is(err, ErrSchemaDrift) {
		t.Fatalf("%s: error %v does not match ErrSchemaDrift", collection, err)
	}
	for _, want := range []string{collection, actual, "three collections"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("drift error %q does not mention %q", err, want)
		}
	}
}

// startQdrant runs the pinned Qdrant image and returns a client for it, cleaning the container up
// when the test ends.
//
// Plain `docker run` rather than testcontainers-go: the whole need is start, read a port, stop, and
// a container runner would add a large dependency tree to a project that re-pins every module by
// hand. The trade is that a killed test process leaves the container behind — bounded by --rm plus
// the cleanup below, and visible as a stray `qdrant/qdrant` in `docker ps`.
func startQdrant(t *testing.T) *qdrant.Client {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not on PATH")
	}

	// Port 0 asks the daemon for a free host port, so a docker-compose.yml already bound to 6334
	// does not make this test unrunnable.
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
	// `docker port` prints one line per address family; the first is enough, and the port is
	// whatever follows the last colon.
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

// waitReady polls until the server answers, because the container is up before Qdrant is.
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

// runDocker shells out to the docker CLI. Its timeout is its own rather than the test's context:
// the container has to be removed during t.Cleanup, which runs after the test context is already
// cancelled, and a cleanup that inherits a cancelled context leaks the container it meant to
// remove.
func runDocker(t *testing.T, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// #nosec G204 -- every argument here is a literal or a container ID this function produced;
	// nothing reaches it from outside the test.
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out)), nil
}
