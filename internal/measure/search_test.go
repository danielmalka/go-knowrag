package measure

import (
	"testing"
	"time"

	"github.com/danielmalka/go-knowrag/internal/retrieval"
)

func fastLegs(n int) []retrieval.Timing {
	legs := make([]retrieval.Timing, n)
	for i := range legs {
		legs[i] = retrieval.Timing{Embed: ms(20), Qdrant: ms(15), Overhead: ms(5), Total: ms(40)}
	}
	return legs
}

func TestEvaluateSearch_FastRunPasses(t *testing.T) {
	r := EvaluateSearch(fastLegs(30), 3*time.Second, 5*time.Second)
	if !r.Pass {
		t.Fatalf("EvaluateSearch = %+v, want Pass=true for a run well under budget", r)
	}
	if r.N != 30 {
		t.Errorf("N = %d, want 30", r.N)
	}
}

// TestEvaluateSearch_SlowOverheadAloneFailsTheGate is the mandatory plant: grow one leg
// (Overhead) until the total blows the budget, and confirm the verdict flips to failed.
//
// It also encodes the specific regression CLAUDE.md's postmortem names: "um teste de orçamento que
// somava uma das duas pernas ficava verde enquanto o total real ultrapassava o deadline." Embed and
// Qdrant alone stay small here — a verdict built from Embed+Qdrant would report a pass — and Total
// (which retrieval.SearchTimed measures independently, not by summing) is what actually exceeds the
// gate.
func TestEvaluateSearch_SlowOverheadAloneFailsTheGate(t *testing.T) {
	const gate = 3 * time.Second
	legs := make([]retrieval.Timing, 30)
	for i := range legs {
		legs[i] = retrieval.Timing{
			Embed: ms(20), Qdrant: ms(15), Overhead: 4 * time.Second, Total: 4*time.Second + ms(35),
		}
	}

	// The buggy shortcut this plant guards against: gating on Embed+Qdrant alone.
	buggyP95 := Percentile([]time.Duration{ms(35)}, 0.95) // representative of embed+qdrant, ~35ms
	if buggyP95 >= gate {
		t.Fatalf("test setup: the two-leg sum was supposed to look small (got %v)", buggyP95)
	}

	r := EvaluateSearch(legs, gate, 5*time.Second)
	if r.Pass {
		t.Fatalf("EvaluateSearch = %+v, want Pass=false — Overhead alone (%v) exceeds the %v gate, "+
			"and a verdict that missed it would be exactly the bug this plant exists to catch",
			r, legs[0].Overhead, gate)
	}
	if r.Overhead.P95 < 4*time.Second {
		t.Errorf("Overhead.P95 = %v, want the decomposition to actually show the slow leg", r.Overhead.P95)
	}
}

// TestEvaluateSearch_SlowEmbedAloneFailsTheGate is the same plant grown on the other leg an
// operator is most likely to suspect first — the embedding call — proving the gate is not
// accidentally reading only Qdrant+Overhead either.
func TestEvaluateSearch_SlowEmbedAloneFailsTheGate(t *testing.T) {
	const p99Gate = 5 * time.Second
	legs := make([]retrieval.Timing, 30)
	for i := range legs {
		legs[i] = retrieval.Timing{
			Embed: 6 * time.Second, Qdrant: ms(15), Overhead: ms(5), Total: 6*time.Second + ms(20),
		}
	}

	r := EvaluateSearch(legs, 3*time.Second, p99Gate)
	if r.Pass {
		t.Fatalf("EvaluateSearch = %+v, want Pass=false — Embed alone (%v) exceeds the p99 gate %v",
			r, legs[0].Embed, p99Gate)
	}
}

func TestEvaluateSearch_EmptyRun(t *testing.T) {
	r := EvaluateSearch(nil, 3*time.Second, 5*time.Second)
	if r.N != 0 {
		t.Errorf("N = %d, want 0", r.N)
	}
	// Zero durations are <= any positive gate, so an empty run reads as a pass — which is why
	// cmd/measure-search must never call EvaluateSearch over zero collected queries and has to
	// refuse before printing a report. This test pins the raw arithmetic; the refusal is the
	// command's job, tested in cmd/measure-search.
	if !r.Pass {
		t.Errorf("Pass = false for an empty run, want true (the vacuous case the caller must refuse before it prints)")
	}
}
