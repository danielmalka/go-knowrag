package main

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/danielmalka/go-knowrag/internal/clicmd"
	"github.com/danielmalka/go-knowrag/internal/config"
	"github.com/danielmalka/go-knowrag/internal/embed"
	"github.com/danielmalka/go-knowrag/internal/retrieval"
	"github.com/danielmalka/go-knowrag/internal/store"
)

// newSearchCmd builds `search` over a real Qdrant and a real embedding service.
//
// The command itself lives in internal/clicmd, and only because one test outside this package has
// to run it — see that package's doc comment. What stays here is this file: the connection, which
// is the half a hermetic test must not have.
func newSearchCmd(cfg *config.Config) *cobra.Command {
	return clicmd.NewSearchCmd(cfg, func(ctx context.Context) (clicmd.Searcher, func(), error) {
		return dialSearcher(ctx, cfg)
	})
}

// dialSearcher opens everything one search needs and returns the function that closes it again.
//
// It runs at the start of a run rather than while the command tree is assembled, so `--help` opens
// no sockets, and it asks config.Require first so a host missing three settings hears about all
// three at once instead of one per attempt.
//
// It performs no handshake of its own, and that is not an omission: embed.ServiceEmbedder confirms
// the backend it is about to trust before it embeds anything (D-33, internal/embed), so a service
// serving a different revision, pooling or precision is refused by name on the first query rather
// than answering from a vector space the index was not built in. The ingestion calls Handshake
// explicitly because it needs the returned report — it feeds point_hash — not because the check
// would otherwise be skipped.
func dialSearcher(_ context.Context, cfg *config.Config) (clicmd.Searcher, func(), error) {
	if err := cfg.Require(config.NeedQdrant | config.NeedEmbedder | config.NeedCollection); err != nil {
		return nil, nil, err
	}

	client, err := store.NewQdrantClient(store.Config{
		Endpoint: cfg.QdrantEndpoint,
		APIKey:   cfg.QdrantAPIKey,
	})
	if err != nil {
		return nil, nil, err
	}
	release := func() { _ = client.Close() }

	// A Querier, not a Client: Client is the collection-bound point-CRUD type the ingestion writes
	// through, and a search names its collection in the request internal/retrieval built (S07).
	querier, err := store.NewQuerier(client)
	if err != nil {
		release()
		return nil, nil, err
	}

	transport, err := embed.NewHTTPTransport(cfg.EmbedderEndpoint)
	if err != nil {
		release()
		return nil, nil, err
	}
	// The ingestion's profile, deliberately reused rather than duplicated with search-shaped
	// numbers. cmd/mcp-server keeps its own because it has a latency contract to protect (NFR-1, a
	// per-call p99 an agent waits on); a one-shot operator command has nobody waiting on a p99, so a
	// second set of timeouts here would be numbers with no measurement behind them. What it costs is
	// a slow failure against a wedged backend, which is the right trade for a command a human ran
	// and can interrupt.
	embedder, err := embed.NewServiceEmbedder(embedProfile(cfg.EmbedderEndpoint), transport)
	if err != nil {
		release()
		return nil, nil, err
	}

	return retrieval.NewSearcher(embedder, querier, retrieval.DefaultConfig()), release, nil
}
