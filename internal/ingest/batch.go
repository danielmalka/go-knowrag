package ingest

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/danielmalka/go-knowrag/internal/vault"
)

// RunBatch processes every note and collects the outcomes, whatever they are.
//
// Two failure modes are treated in opposite ways on purpose. A duplicate uid aborts the whole batch
// before a single write, because the point ID is uuid5(tenant_id + uid + chunk_index): two notes
// with one uid produce one set of point IDs, and processing them would silently overwrite the first
// note with the second — corrupting data rather than failing. Everything else is isolated to its
// note, because a batch of 730 notes that stops at the first bad frontmatter is a batch nobody can
// finish.
func RunBatch(ctx context.Context, d Deps, notes []vault.Note) (Report, error) {
	// One snapshot for the whole batch instead of one round trip per note. See withPrefetch: it
	// changes only how current state is read, and degrades to the per-note path if the store cannot
	// produce a snapshot.
	d, snapshot, snapshotTaken := withPrefetch(ctx, d)
	if err := d.Validate(); err != nil {
		return Report{}, err
	}
	if err := checkDuplicateUIDs(notes); err != nil {
		return Report{}, err
	}

	report := Report{Results: make([]NoteResult, 0, len(notes))}

	// Before the loop, not after, so both sides of the comparison come from the same moment: the
	// snapshot is the index as this run found it, and the note set is the disk as the scan found it.
	// Running it afterwards would subtract a start-of-run disk from an index this run had already
	// rewritten, and the answer to that subtraction describes no point in time at all.
	//
	// An earlier version of this comment justified the ordering with the upsert results instead, and
	// was false for a reason worth keeping: `live` is built from the scanned notes, never from what
	// the run did to them, so no outcome of the loop can move a uid in or out of it. The ordering is
	// right; the reason once given for it was not.
	switch {
	case d.Only != "":
		report.OrphanScanSkipped = fmt.Sprintf(
			"this run was restricted to %q, so the notes it did not visit are indistinguishable "+
				"from notes that were deleted", d.Only)
	case !snapshotTaken:
		report.OrphanScanSkipped = "the index snapshot could not be read"
	case len(d.Vaults) == 0:
		report.OrphanScanSkipped = "the caller declared no vault scope"
	default:
		report.Orphans = ScanOrphans(snapshot, notes, d.Vaults)
		report.OrphansScanned = true
	}

	for _, n := range notes {
		// Checked between notes, never inside one. A note is the unit that converges: ADR-006's
		// insert-then-prune leaves the index consistent at every note boundary, and stopping there
		// means the run ends on one instead of between the write and the prune. What bounds a note
		// already in flight is ctx, which the caller cancels when the grace period expires or a
		// second signal arrives (cmd/cli/ingest.go) — this channel only stops new work.
		if interrupted(d.Interrupt) {
			report.Interrupted = true
			return report, nil
		}
		report.Results = append(report.Results, ProcessNote(ctx, d, n))
	}

	// Checked again after the loop, and the reason is the case the loop check cannot see: an
	// interrupt that arrives while the *last* note is running never reaches another iteration, so a
	// run stopped during its final note would report itself as having completed normally — and exit
	// as a failure rather than as an interruption, because that note was cut short and failed.
	report.Interrupted = report.Interrupted || interrupted(d.Interrupt)
	return report, nil
}

// Orchestrate is the entry point that owns the whole-roster sequence: scan results in, one report
// out. It takes however many vaults the roster names — two was never a property of this function,
// only of the enum that used to supply the list (D-26).
//
// The cross-vault duplicate check has to live here and nowhere else. vault.ScanVault sees one vault
// per call and detects duplicates only inside it (S02 T6), while the point ID does not include
// `vault` — so a uid repeated across two vaults collides in Qdrant exactly like a uid repeated
// inside one of them. This is the only place that holds both scan results at once, so this is where
// the check runs: before the notes are flattened, and before any note from either vault reaches
// RunBatch.
func Orchestrate(ctx context.Context, d Deps, scans ...vault.ScanResult) (Report, error) {
	// Ranged over a slice expression rather than indexed. The pair loop is the same one; what the
	// rewrite removes is the explicit index, which gosec's G602 repeatedly misread as unbounded
	// whenever a test called this with a single scan.
	for i, a := range scans {
		for _, b := range scans[i+1:] {
			if err := vault.CheckCrossVaultDuplicateUIDs(a, b); err != nil {
				return Report{}, fmt.Errorf("vaults %s and %s: %w", a.Vault, b.Vault, err)
			}
		}
	}

	// Sized up front because the total is already known. Honest about the payoff: at this corpus
	// the growth this avoids is a handful of reallocations of struct headers inside a run that
	// takes tens of seconds, and profiling did not find it. It is here because the size is free to
	// compute, not because anything measured said to do it.
	total := 0
	for _, s := range scans {
		total += len(s.Notes)
	}
	notes := make([]vault.Note, 0, total)
	for _, s := range scans {
		notes = append(notes, s.Notes...)
	}

	// After flattening, so one pattern can name a vault and a path in the same expression, and
	// before the scope is derived: the filter narrows what is processed, never what was scanned.
	if d.Only != "" {
		filtered, err := FilterByGlob(notes, d.Only)
		if err != nil {
			return Report{}, err
		}
		notes = filtered
	}

	// The scans are the authority on scope, so this is where it is set — not derived from the notes
	// downstream. A vault whose every note was deleted has zero notes and is still in scope; deriving
	// the list from the note set would drop it silently, which is precisely the run where the orphan
	// list matters most.
	//
	// Overwritten rather than filled in when empty. Whatever a caller put in Deps.Vaults is a claim
	// about what was scanned, and the scans are the scan — a caller that disagrees is wrong, and the
	// way it is wrong authorizes deleting a vault this run never opened. Taking the authoritative
	// value costs nothing and leaves no version of that mistake reachable.
	d.Vaults = make([]string, 0, len(scans))
	for _, s := range scans {
		d.Vaults = append(d.Vaults, s.Vault)
	}
	return RunBatch(ctx, d, notes)
}

// interrupted reports whether the operator asked the run to stop. A nil channel — the default for
// every caller that does not wire signals — blocks forever in a select, so the default case makes
// "nobody asked" the answer rather than a panic or a stall.
func interrupted(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// checkDuplicateUIDs re-verifies within the note list at the ingest boundary.
//
// It duplicates a check ScanVault already performs, and that is deliberate: RunBatch accepts a note
// list from any source — a partial reingest, a test fixture, a future CLI flag — not only from the
// standard scan, and this is the last point before the write where the collision is still cheap.
func checkDuplicateUIDs(notes []vault.Note) error {
	seen := make(map[uuid.UUID]string, len(notes))
	for _, n := range notes {
		if first, dup := seen[n.UID]; dup {
			return &vault.DuplicateUIDError{UID: n.UID, PathA: first, PathB: n.Path}
		}
		seen[n.UID] = n.Path
	}
	return nil
}
