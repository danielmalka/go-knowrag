// Command mcp-server exposes the knowledge layer to MCP clients over stdio.
//
// It is a thin entrypoint over internal/retrieval and holds no search logic of its own (NFR-8).
// Its whole security posture is one sentence: the collection and tenant it serves are fixed in this
// process's configuration, so no tool argument, no handshake field and no sentence inside a
// retrieved note can move it (ADR-002). Serving a second scope is a second process.
//
// stdout belongs to the MCP protocol. Every diagnostic goes to stderr — a stray write to stdout
// corrupts the JSON-RPC stream and the client hangs with nothing to read in a log.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danielmalka/go-knowrag/internal/config"
	"github.com/danielmalka/go-knowrag/internal/embed"
	"github.com/danielmalka/go-knowrag/internal/retrieval"
	"github.com/danielmalka/go-knowrag/internal/store"
)

const serverVersion = "0.1.0"

func main() {
	if err := run(); err != nil {
		// stderr, not the logger: the logger may be the thing that failed to come up, and this
		// message has to reach the operator either way.
		fmt.Fprintln(os.Stderr, "mcp-server:", err)
		os.Exit(1)
	}
}

func run() error {
	logger := config.NewLogger(os.Getenv("LOG_LEVEL"))
	slog.SetDefault(logger)

	cfg, err := LoadConfig()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Everything expensive is built here, once, and captured by the tool handler for the process's
	// whole life: the embedding client and the Qdrant connection must not be rebuilt per call
	// (S08 T7 — "no cold start per call").
	qdrantClient, err := store.NewQdrantClient(store.Config{
		Endpoint: cfg.QdrantEndpoint,
		APIKey:   cfg.QdrantAPIKey,
	})
	if err != nil {
		return err
	}
	defer func() { _ = qdrantClient.Close() }()

	// The searcher gets a Querier, not a Client: Client is the collection-bound point-CRUD type the
	// ingestion path uses, and a search names its collection in the request it built (S07).
	querier, err := store.NewQuerier(qdrantClient)
	if err != nil {
		return err
	}

	transport, err := embed.NewHTTPTransport(cfg.EmbedderEndpoint)
	if err != nil {
		return err
	}
	embedder, err := embed.NewServiceEmbedder(embedProfile(cfg.EmbedderEndpoint), transport)
	if err != nil {
		return err
	}

	searcher := retrieval.NewSearcher(embedder, querier, retrieval.DefaultConfig())

	logger.Info("mcp-server starting", "config", cfg, "version", serverVersion)
	return newServer(cfg, searcher).Run(ctx, &mcp.StdioTransport{})
}

// embedProfile is how this process talks to the embedding service. A query is a single short text,
// so the batching knobs the ingestion path needs are irrelevant here; the timeout is short because
// a search that takes a minute has already failed as far as the agent asking is concerned.
//
// Timeout is per attempt, so the whole embedding leg is bounded by 2×4 s plus 0,25 s of backoff =
// 8,25 s. That total is the number that matters, and it has to stay below searchDeadline: a search
// deadline that could fire while this leg was still retrying would silently shorten MaxRetries,
// turning a blip the retry absorbs into a reported outage and dropping the last transport failure
// from the error.
//
// The per-attempt number is not derived from a healthy idle service, and that is the point. Idle,
// a query embed measures ~50 ms (ADR-001 §6.2 measured a p99 of 71 ms on its own run; the number
// here is the median of the 2026-08-10 run tabulated below, and the two are different occasions,
// not one range). Almost any timeout would clear that. What this one has to survive is a query
// issued while an ingestion is running, because the embedding service holds one resident model and
// serialises inference: the query waits out whatever is executing on the GPU when it arrives.
//
// That wait used to grow with the ingestion profile's MaxConcurrent — the service admitted requests
// first-come-first-served, so a query queued behind every waiting batch as well as the running one.
// It no longer does: scripts/embedder-service/server.py reads the `kind` this client already sends
// and admits a query ahead of queued batches (D-27, paid 2026-08-10), which bounds the wait at the
// batch in flight — plus, if the query loses a microseconds-wide race to register, one more, once.
// Never one per queued batch, which is the difference that matters here: the bound is a constant,
// not a multiple of MaxConcurrent. The argument for why it cannot be worse lives in that file's
// gpu(), where the lock that makes it true is.
//
// Measured on the live service, 2026-08-10, one batch = 32 chunks of real Portuguese prose from
// this repo's markdown, 485–901 tokens each (median 651, 21330 tokens per request), which is the
// band cmd/cli/ingest.go's chunker produces; query = 16 tokens. Medians, n=5 (n=10 for the query
// alone), before and after on the same payload with a service restart between:
//
//	query alone, idle          0,05 s     0,04 s
//	one 32-chunk batch alone   1,28 s     1,19 s
//	query behind 1 batch       1,23 s     1,18 s
//	query behind 2 batches     2,39 s     1,22 s
//	query behind 4 batches     4,42 s     1,24 s
//
// The payload line is not decoration: the same scenario measured with 32 short strings puts a batch
// at ~0,3 s and every row shrinks with it, so a number quoted without it cannot be compared to
// anything. What does not depend on the payload is the shape — before, the wait is depth × batch;
// after, it is one batch.
//
// Four seconds is ~3,2× that bound and stays. Re-measuring it against the new floor would trade
// real headroom for nothing: the failure it prevents is reporting KNOWLEDGE BASE UNAVAILABLE for a
// service that is up, healthy and busy, and that error is expensive in a way 3 s of a timeout that
// almost never fires is not.
//
// MaxRetries is 2 rather than 3 for the same reason, and it is the trade that lets the budget fit:
// a longer single attempt beats more short ones, because a retry rejoins the same serialised
// service. Two attempts of 4 s ride out a longer stall than three of 2 s and cost 1,5 s less wall
// clock.
func embedProfile(endpoint string) embed.Profile {
	return embed.Profile{
		Endpoint:      endpoint,
		Timeout:       4 * time.Second,
		BatchSize:     1,
		MaxConcurrent: 1,
		MaxRetries:    2,
	}
}
