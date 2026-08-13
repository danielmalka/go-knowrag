package measure

import (
	"testing"
	"time"
)

func TestEvaluateIngest_FastRunPasses(t *testing.T) {
	phases := IngestPhases{LockAcquire: ms(5), VaultScan: 2 * time.Second, Orchestrate: 3 * time.Second}
	total := phases.Accounted() + ms(50)

	r := EvaluateIngest(total, phases, 60*time.Second)
	if !r.Pass {
		t.Fatalf("EvaluateIngest = %+v, want Pass=true", r)
	}
	if r.Unaccounted != ms(50) {
		t.Errorf("Unaccounted = %v, want %v", r.Unaccounted, ms(50))
	}
}

// TestEvaluateIngest_OrchestratePhaseAloneFailsTheGate is the mandatory plant: grow one leg
// (Orchestrate, the phase D-22 named — docs/debitos-tecnicos.md) until the total blows the 60s NFR-5
// budget, and confirm the verdict flips to failed. Lock and scan stay small on purpose, the same
// shape as CLAUDE.md's postmortem: a verdict that only looked at the fast legs would report a pass.
func TestEvaluateIngest_OrchestratePhaseAloneFailsTheGate(t *testing.T) {
	const gate = 60 * time.Second
	phases := IngestPhases{LockAcquire: ms(5), VaultScan: 13 * time.Second, Orchestrate: 65 * time.Second}
	total := phases.Accounted()

	lockPlusScan := phases.LockAcquire + phases.VaultScan
	if lockPlusScan >= gate {
		t.Fatalf("test setup: lock+scan alone was supposed to look small (got %v)", lockPlusScan)
	}

	r := EvaluateIngest(total, phases, gate)
	if r.Pass {
		t.Fatalf("EvaluateIngest = %+v, want Pass=false — Orchestrate alone (%v) exceeds the %v gate",
			r, phases.Orchestrate, gate)
	}
}

// TestEvaluateIngest_UnaccountedGapIsVisibleNotHidden proves the decomposition cannot silently
// absorb a cost nobody is timing yet: a total measured well above the sum of the three named phases
// shows up as Unaccounted, not folded into Orchestrate or dropped.
func TestEvaluateIngest_UnaccountedGapIsVisibleNotHidden(t *testing.T) {
	phases := IngestPhases{LockAcquire: ms(1), VaultScan: ms(1), Orchestrate: ms(1)}
	total := 10 * time.Second // far more than the phases sum to

	r := EvaluateIngest(total, phases, 60*time.Second)
	if r.Unaccounted < 9*time.Second {
		t.Errorf("Unaccounted = %v, want it to surface the ~10s gap between total and the timed phases", r.Unaccounted)
	}
}

func TestIngestPhases_Accounted(t *testing.T) {
	p := IngestPhases{LockAcquire: ms(1), VaultScan: ms(2), Orchestrate: ms(3)}
	if got, want := p.Accounted(), ms(6); got != want {
		t.Errorf("Accounted() = %v, want %v", got, want)
	}
}
