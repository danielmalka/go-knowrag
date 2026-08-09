package retrieval

import (
	"context"
	"errors"
	"fmt"

	"github.com/danielmalka/go-knowrag/internal/embed"
)

// DefaultPrefetchMultiplier is provisional and knowingly uncalibrated.
//
// The floor is fixed (top_k + offset, PRD-contrato §2.3b); how far above it a prefetch leg should
// reach is a recall question, and answering it needs the golden set that only exists in S10. Four
// is the value this build ships until S10 measures one — S10 revisits the multiplier, not only the
// fusion mode (S07 open question 1).
const DefaultPrefetchMultiplier = 4

// Config is what a Searcher needs beyond its two dependencies.
type Config struct {
	PrefetchMultiplier int
}

// DefaultConfig is the provisional calibration above. It is a function rather than a var so no
// caller can rewrite the default for the whole process.
func DefaultConfig() Config { return Config{PrefetchMultiplier: DefaultPrefetchMultiplier} }

// queryExecutor executes an already-built request. It is deliberately the narrowest possible
// dependency: it cannot be handed a tenant, a filter or a collection separately from the request
// this package built, so there is nothing for an executor to reinterpret.
//
// *store.Querier satisfies it with no adapter type. The interface is declared here, on the consumer
// side, which is what keeps this package free of any import of internal/store.
type queryExecutor interface {
	ExecuteQuery(ctx context.Context, req SearchRequest) ([]ScoredPoint, error)
}

// Searcher is the package's entry point. Both dependencies are injected so the whole flow is
// testable without a Qdrant or a GPU.
type Searcher struct {
	embedder embed.Embedder
	executor queryExecutor
	cfg      Config
}

func NewSearcher(embedder embed.Embedder, executor queryExecutor, cfg Config) *Searcher {
	return &Searcher{embedder: embedder, executor: executor, cfg: cfg}
}

// Search embeds the query, builds the hybrid request and formats what comes back.
//
// Validate runs first and unconditionally: it is the only gate, and everything after it assumes a
// Query that cleared it. An invalid Query therefore costs no embedding call and no round trip.
func (s *Searcher) Search(ctx context.Context, q Query) ([]Result, error) {
	if s == nil || s.embedder == nil || s.executor == nil {
		return nil, errors.New("retrieval: Searcher has no embedder or no executor")
	}
	if err := q.Validate(s.cfg.PrefetchMultiplier); err != nil {
		return nil, err
	}

	emb, err := s.embedder.EmbedQuery(ctx, q.Text)
	if err != nil {
		return nil, fmt.Errorf("retrieval: embedding the query: %w", err)
	}

	req := buildQueryRequest(q, emb, calibratedPrefetchLimit(q, s.cfg.PrefetchMultiplier))
	points, err := s.executor.ExecuteQuery(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("retrieval: executing the query on %s: %w", q.Collection, err)
	}
	return formatResults(points)
}
