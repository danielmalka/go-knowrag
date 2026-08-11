package ingest

import (
	"fmt"
	"strings"
)

// Report is what a run did, note by note. It holds the results rather than only the counts because
// a failed note is useless without its path and its reason — "3 failed" sends an operator back to
// the logs to find out which three.
type Report struct {
	Results []NoteResult

	// Orphans are the uids that still hold points and no longer have a note on disk, within the
	// run's vault scope. They are listed on every run, pruned only on an authorized one.
	Orphans []Orphan
	// OrphansScanned distinguishes "no orphans" from "nobody looked", which an empty Orphans slice
	// cannot. The scan needs the whole-tenant snapshot, and that snapshot is an optimization allowed
	// to fail (prefetch.go); a run that lost it must not print a clean orphan report.
	OrphansScanned bool
	// OnDisk are candidates the scan did not return that this run could not confirm deleted. Three
	// things land here, and only the first is a sighting: the file is present (excluded by
	// configuration, or emptied to zero bytes — vault.ScanVault sends those to Skipped, and
	// SkippedNote carries no uid, so they vanish from the live set exactly like a deletion); the file
	// could not be read at all (permission, I/O, a mount that dropped); or the vault has no root in
	// this run and there was nothing to check against.
	//
	// They are reported and never pruned, because none of the three is evidence of a deletion. Only a
	// caller that can read the vault roots can tell them apart, so this is filled by cmd/cli, not by
	// RunBatch — cmd/cli/ingest_modes.go, splitStillOnDisk, which is also where the list of causes
	// will grow next and where this sentence goes stale if it does.
	OnDisk []Orphan
	// OrphanScanSkipped is why the scan did not run, empty when it did. It exists because "nobody
	// looked" has more than one cause and they are not equally alarming: a snapshot that failed is a
	// broken link, while a --only run simply cannot ask the question. Both must print, and printing
	// the wrong reason is its own way of misleading.
	OrphanScanSkipped string
	// PruneSkipped is why an authorized prune did not run, empty when it did or was never asked for.
	// A run that was told to delete and then did not has to say so: silence there reads as "there was
	// nothing to delete", which is the one meaning it never has.
	PruneSkipped string
	// PointsPruned is how many points the orphan prune removed. It is filled by the caller that ran
	// Prune, not by RunBatch: the batch reports what it found, deleting is a separate, authorized
	// step (prune.go).
	//
	// It is the number of points those uids *held in the snapshot*, not a count the server confirmed
	// removing: Store.DeleteByFilter returns no count (state.go), so there is nothing to report from
	// the other side. It answers "how much was indexed for the notes we deleted", which is the
	// question an operator reconciling a run actually asks.
	PointsPruned int
	// Interrupted reports that the run stopped early because the operator asked it to. The notes it
	// never reached are simply absent from Results — a partial report, which ADR-006 makes safe to
	// resume from: the next run converges on whatever this one left.
	Interrupted bool
	// Mode is what the operator asked for ("incremental", "full", "dry-run"), carried so the JSON
	// report is self-describing — a stored report that does not say which mode produced it cannot be
	// compared to another one.
	Mode string
}

// PointsWritten is how many points this run actually put in the index: the chunks of every note
// whose upsert was confirmed.
//
// A note that stopped at StateEmbedded contributes nothing, which is the point — it embedded, and
// the write is exactly what did not happen. StateFailed contributes nothing either, even though a
// failed note may have landed a partial write: the count would be a guess, and a guess in the
// operator's summary is worse than a conservative number the next run corrects.
func (r Report) PointsWritten() int {
	n := 0
	for _, res := range r.Results {
		if res.State == StateUpsertConfirmed || res.State == StatePruned {
			n += res.Chunks
		}
	}
	return n
}

// Count returns how many notes ended in state.
func (r Report) Count(state NoteState) int {
	n := 0
	for _, res := range r.Results {
		if res.State == state {
			n++
		}
	}
	return n
}

// Counts returns one entry per state observed in the run. States that did not occur are absent
// rather than present with a zero, so the map reads as "what happened" instead of "what could
// have".
func (r Report) Counts() map[NoteState]int {
	out := map[NoteState]int{}
	for _, res := range r.Results {
		out[res.State]++
	}
	return out
}

// Failed reports whether any note ended StateFailed. This is what decides the process exit code:
// PRD-stories-pipeline §3 S06a requires a run with any failed note to exit non-zero, so a partial
// ingestion cannot be mistaken for a clean one by a cron job reading only the status.
func (r Report) Failed() bool { return r.Count(StateFailed) > 0 }

// ExitCode is Failed() as a process exit code.
func (r Report) ExitCode() int {
	if r.Failed() {
		return 1
	}
	return 0
}

// Failures returns the failed results, in run order, for the end-of-run summary.
func (r Report) Failures() []NoteResult {
	var out []NoteResult
	for _, res := range r.Results {
		if res.State == StateFailed {
			out = append(out, res)
		}
	}
	return out
}

// String renders the per-state counts in a fixed order — the order of the state machine, not map
// order — followed by one line per failure.
func (r Report) String() string {
	counts := r.Counts()
	parts := make([]string, 0, 5)
	for _, s := range []NoteState{
		StateSkipped, StateEmbedded, StateUpsertConfirmed, StatePruned, StateFailed,
	} {
		if n, ok := counts[s]; ok {
			parts = append(parts, fmt.Sprintf("%s=%d", s, n))
		}
	}

	summary := fmt.Sprintf("%d note(s): %s", len(r.Results), strings.Join(parts, " "))
	if r.Interrupted {
		summary += " — interrupted, the remaining notes were not started; the next run converges"
	}
	lines := []string{summary}
	lines = append(lines, r.orphanLines()...)
	for _, f := range r.Failures() {
		lines = append(lines, fmt.Sprintf("  - %s: %v", f.Path, f.Err))
	}
	return strings.Join(lines, "\n")
}

// orphanLines renders the deleted-note section, and it renders *something* in all three cases —
// scanned and empty, scanned and non-empty, not scanned — because the one output that must never
// occur is silence that an operator reads as "no orphans".
func (r Report) orphanLines() []string {
	if !r.OrphansScanned {
		why := r.OrphanScanSkipped
		if why == "" {
			why = "the index snapshot could not be read"
		}
		return []string{"orphans: not scanned — " + why + ", so this run cannot say whether any " +
			"note was deleted"}
	}
	if len(r.Orphans) == 0 && len(r.OnDisk) == 0 {
		return []string{"orphans: none"}
	}
	if len(r.Orphans) == 0 {
		return r.onDiskLines()
	}

	found := 0
	for _, o := range r.Orphans {
		found += o.Points
	}

	// Found and removed are two numbers, never one verb over the whole list. Prune stops at the
	// first delete that fails and returns what it removed before stopping (prune.go), so a run can
	// legitimately end with some of these gone and the rest still answering searches — and a single
	// "pruned" covering every line would name the survivors as deleted. Printing both lets the
	// mismatch show itself; the per-note lines below say what was found, and claim nothing about
	// what happened to it.
	outcome := fmt.Sprintf("%d point(s) still indexed; run --prune --yes to remove", found)
	switch {
	case r.PointsPruned >= found:
		outcome = fmt.Sprintf("%d point(s), all removed", found)
	case r.PointsPruned > 0:
		outcome = fmt.Sprintf("%d point(s), of which %d removed before the prune stopped; the "+
			"remaining %d are still indexed", found, r.PointsPruned, found-r.PointsPruned)
	}

	if r.PruneSkipped != "" {
		outcome = fmt.Sprintf("%d point(s) still indexed; the prune was skipped — %s",
			found, r.PruneSkipped)
	}
	lines := []string{fmt.Sprintf("orphans: %d note(s) deleted from the vault, %s",
		len(r.Orphans), outcome)}
	for _, o := range r.Orphans {
		// The claimed case gets a reason on the line, because it is the one that looks wrong from
		// outside: the operator can open that path and find a file sitting there, and without this
		// the report reads as a prune of a note that plainly exists.
		reason := ""
		if o.PathClaimed {
			reason = " — its path now belongs to another note, so this uid has none"
		}
		lines = append(lines, fmt.Sprintf("  - %s/%s (%d point(s), uid %s)%s",
			o.Vault, o.Path, o.Points, o.UID, reason))
	}
	return append(lines, r.onDiskLines()...)
}

// onDiskLines names the candidates that are indexed but were not scanned today, and the wording
// avoids the word "deleted" on purpose: their files are on disk, and calling them deleted is the
// sentence that would talk an operator into pruning them.
func (r Report) onDiskLines() []string {
	if len(r.OnDisk) == 0 {
		return nil
	}
	// "not confirmed deleted" rather than "still on disk", because the list holds two different
	// facts and only one of them is an observation: a file the check found, and a file the check
	// could not read at all (a permission or I/O error, which cmd/cli's splitStillOnDisk keeps here
	// rather than pruning on). Naming only the first would make the second read as a sighting, and
	// this line already had that bug once — it used to offer "excluded by configuration, or emptied
	// to zero bytes" as the complete set of causes, written before the unreadable case existed.
	lines := []string{fmt.Sprintf("indexed but not confirmed deleted: %d note(s) — the file is still "+
		"there (excluded by configuration, or emptied to zero bytes), or it could not be read to "+
		"tell. They are left alone; if you meant to drop them from the index, remove the files",
		len(r.OnDisk))}
	for _, o := range r.OnDisk {
		lines = append(lines, fmt.Sprintf("  - %s/%s (%d point(s), uid %s)",
			o.Vault, o.Path, o.Points, o.UID))
	}
	return lines
}
