package main

import (
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/danielmalka/go-knowrag/internal/ingest"
)

// The two limits on a prune that nobody is watching (--yes). Both exist because the on-disk check in
// splitStillOnDisk only protects a note whose file is still where the index says it was: point the vault
// root at another vault, or let a sync leave half of one behind, and every missing uid is a "provably
// absent" note.
//
//   - the CEILING refuses a prune that would delete more than a handful of notes in one run, however
//     the vault got that way;
//   - the IDENTITY refuses a prune when the vault root is not the one the last successful run used.
//
// A manual `knowrag ingest --prune` in a terminal is the way past either: it shows the reasons and asks.

// Defaults of the ceiling. A vault of N indexed notes may lose min(maxNotes, ceil(percent of N)) notes in
// one prune, and never fewer than pruneFloor (so a tiny vault can still drop one or two notes):
//
//   - 20 notes bounds a big vault. Deleting twenty notes between two runs six hours apart is a clean-up;
//     an unmounted or half-synced vault removes hundreds.
//   - 5% bounds a small one. In a 100-note vault, 5 deleted is routine and 10 is a sync gone wrong.
const (
	defaultPruneMaxNotes   = 20
	defaultPruneMaxPercent = 5.0
	pruneFloor             = 3
)

// ceilingPolicy is the two numbers behind the ceiling.
type ceilingPolicy struct {
	MaxNotes   int
	MaxPercent float64
}

func defaultCeiling() ceilingPolicy {
	return ceilingPolicy{MaxNotes: defaultPruneMaxNotes, MaxPercent: defaultPruneMaxPercent}
}

func (p ceilingPolicy) validate() error {
	if p.MaxNotes < 1 {
		return fmt.Errorf("--prune-max-notes = %d, must be at least 1", p.MaxNotes)
	}
	if p.MaxPercent <= 0 || p.MaxPercent > 100 {
		return fmt.Errorf("--prune-max-percent = %v, must be above 0 and at most 100", p.MaxPercent)
	}
	return nil
}

// limit is how many notes one prune may delete from a vault that holds `indexed` notes.
func (p ceilingPolicy) limit(indexed int) int {
	byPercent := int(math.Ceil(p.MaxPercent / 100 * float64(indexed)))
	return max(min(p.MaxNotes, byPercent), min(pruneFloor, p.MaxNotes))
}

// pruneScope is what a prune needs to know about the run beyond the orphan list. nil (tests of the layers
// below) skips every check in it.
type pruneScope struct {
	// NoteCounts is how many notes the scan returned per scanned vault.
	NoteCounts map[string]int
	Ceiling    ceilingPolicy
	// Roots holds the identity verdict per vault.
	Roots map[string]rootVerdict
	// Confirm, when non-nil, is asked with the reasons a scheduled prune would be refused for. It is the
	// manual path: a terminal and a person. nil means nobody can be asked.
	Confirm func(reasons []string) bool
	// accepted is set by pruneOrphans when a person confirmed past the reasons, which also accepts the
	// current vault roots as the recorded ones.
	accepted bool
}

func (s *pruneScope) noteCounts() map[string]int {
	if s == nil {
		return nil
	}
	return s.NoteCounts
}

// reasons lists, per vault with orphans, why this prune is outside the limits of an unattended one.
func (s *pruneScope) reasons(orphans []ingest.Orphan) []string {
	if s == nil {
		return nil
	}
	perVault := map[string]int{}
	var order []string
	for _, o := range orphans {
		if perVault[o.Vault] == 0 {
			order = append(order, o.Vault)
		}
		perVault[o.Vault]++
	}
	var out []string
	for _, vault := range order {
		if v, ok := s.Roots[vault]; ok && v.Reason != "" {
			out = append(out, v.Reason)
		}
		if scanned, ok := s.NoteCounts[vault]; ok {
			notes := perVault[vault]
			indexed := scanned + notes
			if limit := s.Ceiling.limit(indexed); notes > limit {
				out = append(out, fmt.Sprintf(
					"vault %q: %d of %d indexed notes (%.1f%%) would be pruned, above the ceiling of %d "+
						"(the smaller of %d notes and %v%% of the vault, at least %d)",
					vault, notes, indexed, 100*float64(notes)/float64(indexed), limit,
					s.Ceiling.MaxNotes, s.Ceiling.MaxPercent, min(pruneFloor, s.Ceiling.MaxNotes)))
			}
		}
	}
	return out
}

// gate decides whether a prune with these reasons may go ahead. No reasons: yes. Reasons and --yes, or
// nobody to ask: refused, with the reasons in the error and in the report. Reasons and a person who says
// yes: go ahead, and remember that the person accepted the vault roots.
func (s *pruneScope) gate(orphans []ingest.Orphan, yes bool, report *ingest.Report) error {
	reasons := s.reasons(orphans)
	if len(reasons) == 0 {
		return nil
	}
	if !yes && s.Confirm != nil && s.Confirm(reasons) {
		s.accepted = true
		return nil
	}
	report.PruneSkipped = strings.Join(reasons, "; ")
	return fmt.Errorf("refusing to prune: %s — nothing was removed. A scheduled prune stays inside these "+
		"limits on purpose; if the vault really is in this state, run `knowrag ingest --dry-run` to see "+
		"what would go, then `knowrag ingest --prune` by hand in a terminal and answer the question",
		strings.Join(reasons, "; "))
}

// askYesNo asks one question and takes only a bare y or Y as a yes, like askConfirmation.
func askYesNo(in io.Reader, out io.Writer, question string) bool {
	_, _ = fmt.Fprint(out, question+" [y/N]: ")
	var answer string
	_, _ = fmt.Fscanln(in, &answer)
	return answer == "y" || answer == "Y"
}
