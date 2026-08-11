package ingest

import (
	"context"
	"errors"
	"fmt"
)

var (
	// ErrPruneNotConfirmed is the refusal of an unconfirmed destructive run. It is a sentinel so the
	// CLI can map it to a usage exit code rather than a backend one — nothing is broken, the operator
	// just did not authorize the deletion.
	ErrPruneNotConfirmed = errors.New("ingest: prune was not confirmed")

	// ErrPruneSubset is the refusal of pruning a run that only looked at part of the corpus.
	ErrPruneSubset = errors.New("ingest: prune refused on a filtered run")
)

// PruneOptions carries the two authorizations a prune requires, and they are parameters rather than
// checks left to the caller on purpose: Prune is the one function in this package that destroys
// data, so the conditions that make destruction legitimate are enforced where the destruction
// happens. A second caller — a future daemon, a script, a test that wires the package directly —
// gets the same refusals without having to remember them. cmd/cli checks the same two things one
// layer earlier so the operator hears about it before the vault scan, not after.
type PruneOptions struct {
	// Confirmed is the operator's explicit yes (--yes, or an answered prompt on a terminal).
	Confirmed bool
	// Filtered reports that the run processed a subset of the corpus (--only). A subset run knows
	// which notes it looked at and nothing about the rest, so every uid outside the filter looks
	// deleted while being merely unvisited. There is no safe reading of that state — the refusal is
	// the answer, not a warning.
	//
	// cmd/cli sets it from --only (cmd/cli/ingest_modes.go, pruneOrphans) and refuses the combination
	// one layer earlier, so this refusal cannot fire for that caller. It is not dead code for it: it
	// is what still refuses when the validation is bypassed, wrong, or absent — a second entry point,
	// a future daemon, a test wiring the package directly.
	Filtered bool
}

// Prune deletes every point of every orphan, scoped by tenant and uid.
//
// The scope is the one Store already enforces on every method: tenant_id + uid, never bare uid,
// because tenant_id is part of the point ID and two tenants may legitimately share a uid (ADR-006
// §2). `vault` needs no filter *here* — the vault scoping already happened, in ScanOrphans, which is
// what decided this uid belongs to a vault the run actually scanned. Adding a vault condition to the
// delete would re-check the same fact against the same payload; leaving the scoping out of
// ScanOrphans and relying on this filter instead would be the bug, since an out-of-scope uid would
// already have been reported to the operator as deleted.
//
// It stops at the first failure rather than continuing. A delete that fails is a live Qdrant
// refusing or a link that dropped, and the notes after it would fail the same way; returning the
// count that did land keeps the report truthful about what was removed.
func Prune(ctx context.Context, s Store, tenantID string, orphans []Orphan, opts PruneOptions) (int, error) {
	if opts.Filtered {
		return 0, fmt.Errorf("%w: --only restricted this run to part of the corpus, so a uid missing "+
			"from it was not necessarily deleted — it may simply not match the filter; run the prune "+
			"without --only", ErrPruneSubset)
	}
	if !opts.Confirmed {
		return 0, fmt.Errorf("%w: pass --yes to authorize deleting the %d orphan(s) listed above",
			ErrPruneNotConfirmed, len(orphans))
	}

	pruned := 0
	for _, o := range orphans {
		// fromChunkIndex 0 because the whole note is gone: there is no surviving chunk to keep, which
		// is exactly what distinguishes this from the per-note tail prune in ProcessNote.
		if err := s.DeleteByFilter(ctx, tenantID, o.UID, 0); err != nil {
			return pruned, fmt.Errorf("pruning orphan %s (%s/%s): %w", o.UID, o.Vault, o.Path, err)
		}
		pruned += o.Points
	}
	return pruned, nil
}
