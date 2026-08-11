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
	d = withPrefetch(ctx, d)
	if err := d.Validate(); err != nil {
		return Report{}, err
	}
	if err := checkDuplicateUIDs(notes); err != nil {
		return Report{}, err
	}

	report := Report{Results: make([]NoteResult, 0, len(notes))}
	for _, n := range notes {
		report.Results = append(report.Results, ProcessNote(ctx, d, n))
	}
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
	for i := range scans {
		for j := i + 1; j < len(scans); j++ {
			if err := vault.CheckCrossVaultDuplicateUIDs(scans[i], scans[j]); err != nil {
				return Report{}, fmt.Errorf("vaults %s and %s: %w", scans[i].Vault, scans[j].Vault, err)
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
	return RunBatch(ctx, d, notes)
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
