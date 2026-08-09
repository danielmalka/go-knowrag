//go:build integration

package retrieval_test

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qdrant/go-client/qdrant"

	"github.com/danielmalka/go-knowrag/internal/embed"
	"github.com/danielmalka/go-knowrag/internal/retrieval"
	"github.com/danielmalka/go-knowrag/internal/store"
)

// This file is the other half of the integration suite, and it is deliberately a different kind of
// test from integration_test.go.
//
// integration_test.go proves the *rules* against fixtures it controls, in a container it created
// and destroys. This one proves the *shape of the query works against the real thing*: the deployed
// Qdrant holding the real vault corpus, and the real BGE-M3 service. It seeds nothing, writes
// nothing and deletes nothing — a test that mutated the owner's index to make an assertion pass
// would be a worse outcome than no test.
//
// Everything it asserts is a property, never a ranking. Pinning "this note must be the top hit"
// against a corpus that gains notes every week is a test that goes red for reasons that are not
// bugs, and gets deleted the third time it does.
//
// It runs only when the environment points at a real deployment, so `make test-integration` on a
// laptop with docker still runs the container half.
const realCorpusQuery = "como configurar cron no n8n para rodar toda segunda"

type realCorpusEnv struct {
	endpoint   string
	apiKey     string
	embedder   string
	collection string
	tenant     string
}

// realCorpus reads the deployment out of the environment and skips unless all four required values
// are present. Skipping is the correct default: this is the one test in the repo that talks to a
// server somebody else's data lives on.
func realCorpus(t *testing.T) realCorpusEnv {
	t.Helper()

	env := realCorpusEnv{
		endpoint:   os.Getenv("QDRANT_ENDPOINT"),
		apiKey:     os.Getenv("QDRANT_API_KEY"),
		embedder:   os.Getenv("EMBEDDER_ENDPOINT"),
		collection: os.Getenv("DEFAULT_COLLECTION"),
		tenant:     os.Getenv("KNOWRAG_TENANT_ID"),
	}
	if env.collection == "" {
		env.collection = "interno"
	}
	if env.tenant == "" {
		env.tenant = env.collection
	}

	// The values themselves are never logged — only which name was missing.
	for name, value := range map[string]string{
		"QDRANT_ENDPOINT":   env.endpoint,
		"QDRANT_API_KEY":    env.apiKey,
		"EMBEDDER_ENDPOINT": env.embedder,
	} {
		if value == "" {
			t.Skipf("%s is not set: this test needs the deployed Qdrant and the real embedding service", name)
		}
	}
	return env
}

func realCorpusClient(t *testing.T, env realCorpusEnv) *qdrant.Client {
	t.Helper()
	client, err := store.NewQdrantClient(store.Config{Endpoint: env.endpoint, APIKey: env.apiKey})
	if err != nil {
		t.Fatalf("connecting to the deployed Qdrant: %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("closing the client: %v", err)
		}
	})
	return client
}

func realCorpusSearcher(t *testing.T, env realCorpusEnv, client *qdrant.Client) *retrieval.Searcher {
	t.Helper()

	transport, err := embed.NewHTTPTransport(env.embedder)
	if err != nil {
		t.Fatalf("building the embedding transport: %v", err)
	}
	embedder, err := embed.NewServiceEmbedder(embed.Profile{
		Endpoint:      env.embedder,
		Timeout:       30 * time.Second,
		BatchSize:     1,
		MaxConcurrent: 1,
		MaxRetries:    2,
	}, transport)
	if err != nil {
		t.Fatalf("building the embedder: %v", err)
	}

	querier, err := store.NewQuerier(client)
	if err != nil {
		t.Fatalf("store.NewQuerier: %v", err)
	}
	return retrieval.NewSearcher(embedder, querier, retrieval.DefaultConfig())
}

// TestSearch_RealCorpus_ReturnsCoherentResults is the end-to-end proof that the §2.3b query shape —
// two ACORN-enabled prefetch legs fused with RRF, under a mandatory tenant filter — is accepted by
// the deployed server and answers sensibly over the real corpus.
func TestSearch_RealCorpus_ReturnsCoherentResults(t *testing.T) {
	env := realCorpus(t)
	client := realCorpusClient(t, env)
	searcher := realCorpusSearcher(t, env, client)

	const topK = 5
	results, err := searcher.Search(t.Context(), retrieval.Query{
		Collection: env.collection,
		TenantID:   env.tenant,
		Text:       realCorpusQuery,
		TopK:       topK,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(results) != topK {
		t.Fatalf("%d result(s), want %d — the corpus holds thousands of chunks, so a short answer "+
			"means the query shape, not the data", len(results), topK)
	}
	for i, r := range results {
		if r.UID == "" || r.Text == "" || r.Path == "" {
			t.Errorf("result %d is missing a required field: %+v", i, r)
		}
		if !r.Untrusted {
			t.Errorf("result %d (%s) is not marked as untrusted content", i, r.Path)
		}
		t.Logf("#%d score=%.4f %s [%s]", i+1, r.Score, r.Path, r.Breadcrumb)
	}

	// A topical assertion, not a ranking one: the top hit has to be about the thing that was asked
	// about. Which note wins is the corpus's business and changes as it grows.
	if !strings.Contains(strings.ToLower(results[0].Text+" "+results[0].Path), "cron") {
		t.Errorf("the top hit for %q mentions no cron anywhere in its text or path:\n%s\n%s",
			realCorpusQuery, results[0].Path, truncate(results[0].Text, 300))
	}

	assertRealCorpusScope(t, client, env, results)
}

// TestSearch_RealCorpus_TenantFilterIsServerSide asks the deployed server for a tenant that does
// not exist. An empty answer over a collection holding thousands of points is what a server-side
// filter looks like from the outside; a client-side filter would have had to fetch them first.
func TestSearch_RealCorpus_TenantFilterIsServerSide(t *testing.T) {
	env := realCorpus(t)
	searcher := realCorpusSearcher(t, env, realCorpusClient(t, env))

	results, err := searcher.Search(t.Context(), retrieval.Query{
		Collection: env.collection,
		TenantID:   "tenant-that-does-not-exist-9f3a",
		Text:       realCorpusQuery,
		TopK:       5,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("a tenant nobody indexed got %d result(s) from %s", len(results), env.collection)
	}
}

// assertRealCorpusScope reads the returned points back out of the live collection and checks the
// three payload fields Result does not carry.
//
// It is a read-back rather than an extra Result field on purpose: Result's shape is the PRD's
// (text, breadcrumb, path, score, uid, chunk_index), and widening it so a test can assert on
// tenant_id would put a field in every consumer's response to serve one assertion.
func assertRealCorpusScope(t *testing.T, client *qdrant.Client, env realCorpusEnv, results []retrieval.Result) {
	t.Helper()

	uids := make([]string, 0, len(results))
	for _, r := range results {
		if !slices.Contains(uids, r.UID) {
			uids = append(uids, r.UID)
		}
	}

	points, err := client.Scroll(t.Context(), &qdrant.ScrollPoints{
		CollectionName: env.collection,
		Filter: &qdrant.Filter{
			Must: []*qdrant.Condition{qdrant.NewMatchKeywords("uid", uids...)},
		},
		Limit:       qdrant.PtrOf(uint32(200)),
		WithPayload: qdrant.NewWithPayloadInclude("uid", "tenant_id", "status", "visibility"),
		WithVectors: qdrant.NewWithVectors(false),
	})
	if err != nil {
		t.Fatalf("reading the returned points back: %v", err)
	}
	if len(points) == 0 {
		t.Fatal("the read-back found none of the returned uids, so this check proves nothing")
	}

	for _, p := range points {
		payload := p.GetPayload()
		if got := payload["tenant_id"].GetStringValue(); got != env.tenant {
			t.Errorf("point %s came back from a %s search but carries tenant_id %q",
				p.GetId(), env.tenant, got)
		}
		if got := payload["status"].GetStringValue(); got == "archived" {
			t.Errorf("point %s (uid %s) is archived and was returned by a default search",
				p.GetId(), payload["uid"].GetStringValue())
		}
		if got := payload["visibility"].GetStringValue(); got == "private" {
			t.Errorf("point %s (uid %s) is private and was returned by a default search",
				p.GetId(), payload["uid"].GetStringValue())
		}
	}
	t.Logf("read back %d point(s) for the %d returned uid(s): all %s, none archived, none private",
		len(points), len(uids), env.tenant)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
