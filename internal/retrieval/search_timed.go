package retrieval

import (
	"context"
	"fmt"
	"time"
)

// Timing is one query's NFR-1b decomposition: PRD.md's NFR-1b row requires "embedding da query /
// Qdrant / overhead" recorded separately, because a budget that only checks the total lets one leg
// eat the other's slack in silence.
//
// Overhead is not a fourth measurement — it is Total minus Embed minus Qdrant, computed once, here,
// so a leg added to Search later without updating this file shows up as a growing Overhead instead
// of vanishing from the sum. Total is measured independently by wrapping the whole call, not derived
// by adding the legs, so Embed+Qdrant+Overhead reproduces Total by construction rather than by
// convention a caller could get wrong.
type Timing struct {
	Embed    time.Duration
	Qdrant   time.Duration
	Overhead time.Duration
	Total    time.Duration
}

// SearchTimed is Search with its NFR-1b decomposition attached. It runs the identical steps Search
// does — same validation, same embed call, same executor call, same formatting — so a caller cannot
// observe a different query shape depending on which of the two it calls; only the timers are new.
func (s *Searcher) SearchTimed(ctx context.Context, q Query) ([]Result, Timing, error) {
	if s == nil || s.embedder == nil || s.executor == nil {
		return nil, Timing{}, fmt.Errorf("retrieval: Searcher has no embedder or no executor")
	}
	start := time.Now()
	if err := q.Validate(s.cfg.PrefetchMultiplier); err != nil {
		return nil, Timing{}, err
	}

	t0 := time.Now()
	emb, err := s.embedder.EmbedQuery(ctx, q.Text)
	embedDur := time.Since(t0)
	if err != nil {
		return nil, Timing{}, fmt.Errorf("retrieval: embedding the query: %w", err)
	}

	req := buildQueryRequest(q, emb, calibratedPrefetchLimit(q, s.cfg.PrefetchMultiplier))
	t1 := time.Now()
	points, err := s.executor.ExecuteQuery(ctx, req)
	qdrantDur := time.Since(t1)
	if err != nil {
		return nil, Timing{}, fmt.Errorf("retrieval: executing the query on %s: %w", q.Collection, err)
	}

	results, err := formatResults(points)
	total := time.Since(start)
	if err != nil {
		return nil, Timing{}, err
	}
	return results, Timing{
		Embed:    embedDur,
		Qdrant:   qdrantDur,
		Overhead: total - embedDur - qdrantDur,
		Total:    total,
	}, nil
}
