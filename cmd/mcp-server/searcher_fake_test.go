package main

import (
	"context"
	"sync"

	"github.com/danielmalka/go-knowrag/internal/retrieval"
)

// fakeSearcher is the deterministic Searcher the handler tests drive.
//
// It records every retrieval.Query it was handed, which is how the escalation tests prove a
// negative: not "the response looked right", but "the scope that reached the search layer was the
// configured one and nothing else".
type fakeSearcher struct {
	mu      sync.Mutex
	queries []retrieval.Query

	results []retrieval.Result
	// errs is consumed one per call; a nil entry (or running past the end) means success.
	errs []error
}

func (f *fakeSearcher) Search(_ context.Context, q retrieval.Query) ([]retrieval.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	n := len(f.queries)
	f.queries = append(f.queries, q)

	if n < len(f.errs) && f.errs[n] != nil {
		return nil, f.errs[n]
	}
	return f.results, nil
}

func (f *fakeSearcher) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queries)
}

func (f *fakeSearcher) lastQuery() retrieval.Query {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queries) == 0 {
		return retrieval.Query{}
	}
	return f.queries[len(f.queries)-1]
}

// testConfig is the instance scope every handler test runs against — this build's real one.
func testConfig() Config {
	return Config{
		Collection:       "interno",
		TenantID:         "malka",
		QdrantEndpoint:   "qdrant.internal:6334",
		QdrantAPIKey:     "runtime-read-key",
		EmbedderEndpoint: "http://embedder.internal:8080",
	}
}
