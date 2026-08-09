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
// ponytail: not configurable. Promote to settings when a deployment needs different numbers.
func embedProfile(endpoint string) embed.Profile {
	return embed.Profile{
		Endpoint:      endpoint,
		Timeout:       15 * time.Second,
		BatchSize:     1,
		MaxConcurrent: 1,
		MaxRetries:    3,
	}
}
