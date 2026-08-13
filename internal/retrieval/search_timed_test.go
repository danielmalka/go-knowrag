package retrieval

import (
	"context"
	"testing"
	"time"

	"github.com/danielmalka/go-knowrag/internal/embed"
)

// delayEmbedder and delayExecutor sleep a fixed amount before answering, which is what lets a test
// assert on the shape of Timing rather than on its exact values — real durations are never
// deterministic enough to assert "equals", only "at least this long, in this leg and no other".
type delayEmbedder struct {
	embed.FakeEmbedder
	delay time.Duration
}

func (d delayEmbedder) EmbedQuery(ctx context.Context, text string) (embed.Embedding, error) {
	time.Sleep(d.delay)
	return d.FakeEmbedder.EmbedQuery(ctx, text)
}

type delayExecutor struct {
	delay  time.Duration
	points []ScoredPoint
}

func (d delayExecutor) ExecuteQuery(_ context.Context, _ SearchRequest) ([]ScoredPoint, error) {
	time.Sleep(d.delay)
	return d.points, nil
}

func validTimedQuery() Query {
	return Query{Collection: "interno", TenantID: "malka", Text: "renewal terms", TopK: 5}
}

// TestSearchTimed_LegsAttributeToTheRightStage is the load-bearing assertion: a slow embedder must
// show up as Embed, not as Overhead or Qdrant, or the decomposition would point an operator at the
// wrong stage exactly the way D-22 (docs/debitos-tecnicos.md) went unnoticed until someone measured
// the real command instead of a part of it.
func TestSearchTimed_LegsAttributeToTheRightStage(t *testing.T) {
	const embedDelay = 40 * time.Millisecond
	const qdrantDelay = 25 * time.Millisecond
	s := NewSearcher(delayEmbedder{delay: embedDelay}, delayExecutor{delay: qdrantDelay}, DefaultConfig())

	_, timing, err := s.SearchTimed(context.Background(), validTimedQuery())
	if err != nil {
		t.Fatalf("SearchTimed: %v", err)
	}

	if timing.Embed < embedDelay {
		t.Errorf("Embed = %v, want at least the %v the embedder slept", timing.Embed, embedDelay)
	}
	if timing.Qdrant < qdrantDelay {
		t.Errorf("Qdrant = %v, want at least the %v the executor slept", timing.Qdrant, qdrantDelay)
	}
	// Embed+Qdrant+Overhead must reproduce Total exactly, by construction (Overhead is defined as
	// Total-Embed-Qdrant in search_timed.go). internal/measure/search_test.go plants a slow leg and
	// shows a verdict gated on this sum catches it, which a verdict gated on Embed+Qdrant alone would
	// not — the exact bug shape CLAUDE.md's postmortem describes.
	if got, want := timing.Embed+timing.Qdrant+timing.Overhead, timing.Total; got != want {
		t.Errorf("Embed+Qdrant+Overhead = %v, want it to equal Total = %v", got, want)
	}
}

func TestSearchTimed_UnwiredSearcher(t *testing.T) {
	var s *Searcher
	_, _, err := s.SearchTimed(context.Background(), validTimedQuery())
	if err == nil {
		t.Fatal("expected an error from a nil Searcher, got nil")
	}
}

func TestSearchTimed_InvalidQuery_NoEmbedNoExecute(t *testing.T) {
	e, x := &spyEmbedder{}, &spyExecutor{}
	s := NewSearcher(e, x, DefaultConfig())
	q := validTimedQuery()
	q.TenantID = ""

	_, timing, err := s.SearchTimed(context.Background(), q)
	if err != ErrEmptyTenant {
		t.Fatalf("err = %v, want ErrEmptyTenant", err)
	}
	if e.calls != 0 || x.calls != 0 {
		t.Fatalf("an invalid query reached the embedder (%d calls) or executor (%d calls)", e.calls, x.calls)
	}
	if timing != (Timing{}) {
		t.Fatalf("timing = %+v, want the zero value on a rejected query", timing)
	}
}
