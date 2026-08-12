package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc/status"

	"github.com/danielmalka/go-knowrag/internal/retrieval"
)

// hangCap is how long the hanging fake waits for a cancellation that should have come. It only
// matters when the code under test is broken; a working deadline fires long before it.
const hangCap = 2 * time.Second

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

	// hangs makes every call block until the caller's context runs out, which is a Qdrant that
	// accepts the connection and then never answers. Nothing but a real deadline unblocks it.
	hangs bool

	// filterAlive is what FilterMatchesAnything answers, and probeErr overrides it. The zero value
	// is the drift case on purpose: a fake holding no points is a filter that matches nothing, so a
	// test that wants the honest-empty case has to say so.
	filterAlive bool
	probeErr    error
	probes      []retrieval.Query

	// filterAliveFn answers from the probed Query instead of filterAlive, which is the only way to
	// model an index where the answer depends on which facets the probe kept: an area full of notes
	// and empty of one type inside it.
	filterAliveFn func(retrieval.Query) bool

	// probeHangs is hangs for the probe, and separate from it because the case that matters has a
	// search that answered and a probe that never does. Only the caller's deadline unblocks it.
	probeHangs bool
}

func (f *fakeSearcher) Search(ctx context.Context, q retrieval.Query) ([]retrieval.Result, error) {
	f.mu.Lock()
	n := len(f.queries)
	f.queries = append(f.queries, q)
	hangs := f.hangs
	f.mu.Unlock()

	if hangs {
		select {
		case <-ctx.Done():
			// status.FromContextError is the conversion the gRPC client makes internally, and the
			// wrap is internal/store's. Verified against a real dial to a listener that accepts and
			// never speaks: the expired context comes back as `rpc error: code = DeadlineExceeded
			// desc = context deadline exceeded while waiting for connections to become ready`, and
			// notably *not* as anything errors.Is(ctx.DeadlineExceeded) would match.
			return nil, fmt.Errorf("querying %s: %w", q.Collection, status.FromContextError(ctx.Err()).Err())
		case <-time.After(hangCap):
			// Nothing cancelled the call. Returning instead of blocking forever is what lets a
			// missing deadline show up as a failed assertion rather than as a wedged package: the
			// MCP session's Close waits for the in-flight handler, so a fake that truly never
			// returns hangs the test's own cleanup and takes the whole `go test` timeout with it.
			return nil, errors.New("the searcher was never cancelled: nothing bounds how long a search may take")
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if n < len(f.errs) && f.errs[n] != nil {
		return nil, f.errs[n]
	}
	return f.results, nil
}

func (f *fakeSearcher) FilterMatchesAnything(ctx context.Context, q retrieval.Query) (bool, error) {
	f.mu.Lock()
	f.probes = append(f.probes, q)
	hangs, err, aliveFn, alive := f.probeHangs, f.probeErr, f.filterAliveFn, f.filterAlive
	f.mu.Unlock()

	if hangs {
		select {
		case <-ctx.Done():
			return false, fmt.Errorf("probing %s: %w", q.Collection, status.FromContextError(ctx.Err()).Err())
		case <-time.After(hangCap):
			// Same reasoning as Search's: returning rather than blocking forever is what turns a
			// probe nobody bounds into a failed assertion instead of a wedged test binary.
			return false, errors.New("the probe was never cancelled: nothing bounds how long it may take")
		}
	}
	if err != nil {
		return false, err
	}
	if aliveFn != nil {
		return aliveFn(q), nil
	}
	return alive, nil
}

func (f *fakeSearcher) probeCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.probes)
}

func (f *fakeSearcher) lastProbe() retrieval.Query {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.probes) == 0 {
		return retrieval.Query{}
	}
	return f.probes[len(f.probes)-1]
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
		TenantID:         "tenant-a",
		QdrantEndpoint:   "qdrant.internal:6334",
		QdrantAPIKey:     "runtime-read-key",
		EmbedderEndpoint: "http://embedder.internal:8080",
		Areas:            []string{"infra", "research"},
	}
}
