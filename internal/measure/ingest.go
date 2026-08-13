package measure

import (
	"fmt"
	"time"
)

// IngestPhases is the wall-clock decomposition of one `cli ingest` run, timed around the same three
// boundaries cmd/measure-ingest/main.go calls in the same order cmd/cli/ingest.go's runIngest and
// ingestScans do: acquiring the local lock, scanning the vault(s), and the embed/write/prune
// orchestration (ingest.Orchestrate, which is also where internal/ingest/prefetch.go's snapshot
// lives — see docs/debitos-tecnicos.md D-22 for what that snapshot replaced). If cmd/cli/ingest.go's
// phase order changes, this comment and cmd/measure-ingest/main.go both need to change with it.
type IngestPhases struct {
	LockAcquire time.Duration
	VaultScan   time.Duration
	Orchestrate time.Duration
}

// Accounted sums the phases.
func (p IngestPhases) Accounted() time.Duration {
	return p.LockAcquire + p.VaultScan + p.Orchestrate
}

func (p IngestPhases) String() string {
	return fmt.Sprintf("lock_acquire=%s vault_scan=%s orchestrate=%s", p.LockAcquire, p.VaultScan, p.Orchestrate)
}

// IngestReport is NFR-5 measured over one real, complete `cli ingest` invocation: PRD.md's NFR-5 row
// gates a no-op incremental reingestion — "measured no comando completo do operador" — at <= 60s.
type IngestReport struct {
	Total       time.Duration
	Phases      IngestPhases
	Unaccounted time.Duration
	Gate        time.Duration
	Pass        bool
}

// EvaluateIngest builds the report and verdict.
//
// Pass is gated on total, the one duration measured by wrapping the entire run end to end — never
// on Phases.Accounted(). Unaccounted (total minus the sum of the three timed phases) is reported
// rather than folded silently into whichever phase ran last, so a cost this harness is not yet
// timing shows up as a visible gap instead of vanishing the way D-22's per-note round trips did
// before anyone measured the complete command (docs/debitos-tecnicos.md D-22).
func EvaluateIngest(total time.Duration, phases IngestPhases, gate time.Duration) IngestReport {
	return IngestReport{
		Total:       total,
		Phases:      phases,
		Unaccounted: total - phases.Accounted(),
		Gate:        gate,
		Pass:        total <= gate,
	}
}

func (r IngestReport) String() string {
	verdict := "FAILED"
	if r.Pass {
		verdict = "passed"
	}
	return fmt.Sprintf(
		"NFR-5 incremental reingestion, no-op run (PRD.md NFR-5: <= %s)\n"+
			"  %s\n"+
			"  unaccounted %s\n"+
			"  total       %s\n"+
			"verdict: %s",
		r.Gate, r.Phases, r.Unaccounted, r.Total, verdict,
	)
}
