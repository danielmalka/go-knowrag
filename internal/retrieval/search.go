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

// FilterMatchesAnything reports whether the filter this Query builds matches at least one point.
//
// It answers the one question an empty result cannot: whether the search came back empty because
// the index holds nothing on the subject, or because the filter it carried matches nothing at all.
// The two answers are identical on the wire and mean opposite things.
//
// It reuses buildFilter, which is what keeps the half that must not drift from drifting: the tenant
// condition and the archived/private MustNot guards are built by the same code the search's filter
// is, so this can never report a filter alive under a scope or a visibility rule the search never
// ran under. A second, hand-assembled filter would agree with the search only until someone edited
// one of them.
//
// The facets are the caller's, and callers are expected to narrow. This asks about whatever the
// Query it is handed still names: pass the search's own Query and it probes the search's whole
// filter; pass one that names only the facet the message is about and it probes that one. That is
// not a sloppier version of the search — a caller whose message blames `area` probes `area` alone,
// because area+type can be dead between them while the area is full, and the honest message then is
// not the one about a dead area (see cmd/mcp-server's explainEmptyArea).
//
// The consequence of the guards is that false means "nothing *visible* matches": an area whose
// notes are all archived or all private is perfectly well indexed and still answers false, so a
// caller wording a message from this may claim only that much.
//
// Validate is deliberately not called. Text, TopK and Offset rank and bound an answer, and this
// asks for no answer: one point is enough to prove the filter is alive, and no embedding is
// computed at all. The two fields the filter cannot be built without are checked instead, and they
// are the two Validate checks first.
func (s *Searcher) FilterMatchesAnything(ctx context.Context, q Query) (bool, error) {
	if s == nil || s.executor == nil {
		return false, errors.New("retrieval: Searcher has no executor")
	}
	switch {
	case q.Collection == "":
		return false, ErrEmptyCollection
	case q.TenantID == "":
		return false, ErrEmptyTenant
	}

	points, err := s.executor.ExecuteQuery(ctx, SearchRequest{
		Collection: q.Collection,
		// No prefetch leg and no fusion, so internal/store leaves the query itself unset and Qdrant
		// reads the filter alone — the Query API's own filtered read, not a quirk: in the pinned
		// client (go-client v1.19.0, internal/proto/points.proto:1114) a missing query is documented
		// to return points ordered by ID. That is what makes this cost a fraction of the search it
		// follows.
		Filter: buildFilter(q),
		Limit:  1,
		// One field because nothing here reads the point: an empty projection would mean the whole
		// payload, and the payload is where the chunk text is.
		PayloadFields: []string{fieldUID},
	})
	if err != nil {
		return false, fmt.Errorf("retrieval: probing the filter on %s: %w", q.Collection, err)
	}
	return len(points) > 0, nil
}
