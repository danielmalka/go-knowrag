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

	// Notes is how many notes the run touched, and Reprocessed how many of those it did work for —
	// anything that is not a skip. NFR-5 gates a run that changes *nothing*, so a run that embedded
	// and wrote is not a slow no-op, it is a different measurement wearing this one's label.
	//
	// This exists because the harness reported FAILED on a run with 43 reprocessed notes, against a
	// gate that assumes zero. The number was right and answered a question nobody asked.
	Notes       int
	Reprocessed int
}

// NoOp reports whether the run measured the thing NFR-5 names. A run that reprocessed anything did
// work the requirement excludes, so Pass says nothing about it either way.
func (r IngestReport) NoOp() bool { return r.Reprocessed == 0 }

// EvaluateIngest builds the report and verdict.
//
// Pass is gated on total, the one duration measured by wrapping the entire run end to end — never
// on Phases.Accounted(). Unaccounted (total minus the sum of the three timed phases) is reported
// rather than folded silently into whichever phase ran last, so a cost this harness is not yet
// timing shows up as a visible gap instead of vanishing the way D-22's per-note round trips did
// before anyone measured the complete command (docs/debitos-tecnicos.md D-22).
func EvaluateIngest(total time.Duration, phases IngestPhases, gate time.Duration, notes, reprocessed int) IngestReport {
	return IngestReport{
		Total:       total,
		Phases:      phases,
		Unaccounted: total - phases.Accounted(),
		Gate:        gate,
		// Pass stays a plain answer about the clock. Whether that answer is *about NFR-5* is NoOp's
		// question, and String refuses to render a verdict when it is not — rather than folding the
		// two into one boolean, where "passed" would come to mean two different things.
		Pass:        total <= gate,
		Notes:       notes,
		Reprocessed: reprocessed,
	}
}

func (r IngestReport) String() string {
	verdict := "FAILED"
	if r.Pass {
		verdict = "passed"
	}
	if !r.NoOp() {
		verdict = fmt.Sprintf("NOT MEASURED — %d of %d note(s) were reprocessed, so this was not a "+
			"no-op run and the clock above does not answer NFR-5. Run it again: the second run over "+
			"an unchanged vault is the one this requirement is about", r.Reprocessed, r.Notes)
	}
	return fmt.Sprintf(
		"NFR-5 incremental reingestion, no-op run (PRD.md NFR-5: <= %s)\n"+
			"  %s\n"+
			"  unaccounted %s\n"+
			"  total       %s\n"+
			"  notes       %d, of which %d reprocessed\n"+
			"verdict: %s",
		r.Gate, r.Phases, r.Unaccounted, r.Total, r.Notes, r.Reprocessed, verdict,
	)
}
