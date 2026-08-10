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
// a query embed measures 50–92 ms (ADR-001 §6.2's p99 of 71 ms, re-measured 2026-08-10), so almost
// any timeout would do. But scripts/embedder-service/server.py serialises every request behind one
// threading.Lock, and the ingestion profile below in this same binary's sibling command runs
// BatchSize 32 with MaxConcurrent 2 — so a query issued during an ingestion does not share the GPU,
// it *queues* behind up to two 32-chunk batches. Measured on the live service: a 32-chunk batch
// takes 1,13–1,24 s, a query behind one batch returns in ~1,1 s, and a query behind two returns in
// ~2,3 s. Four seconds leaves ~1,7× headroom over that worst case; two seconds did not, and would
// have reported KNOWLEDGE BASE UNAVAILABLE for a service that was up, healthy and busy.
//
// MaxRetries is 2 rather than 3 for the same reason, and it is the trade that lets the budget fit:
// against a serialised queue a longer single attempt beats more short ones, because every retry
// rejoins the same queue. Two attempts of 4 s ride out a longer stall than three of 2 s and cost
// 1,5 s less wall clock.
//
// ponytail: calibrated around a server that cannot prioritise a query over a batch. The real fix is
// in server.py's single lock, not in these numbers — see the debt candidate raised 2026-08-10. If
// the ingestion profile's MaxConcurrent ever rises, re-measure: the queue in front of a query grows
// with it and this margin does not.
func embedProfile(endpoint string) embed.Profile {
	return embed.Profile{
		Endpoint:      endpoint,
		Timeout:       4 * time.Second,
		BatchSize:     1,
		MaxConcurrent: 1,
		MaxRetries:    2,
	}
}
