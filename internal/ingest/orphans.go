package ingest

import (
	"cmp"
	"slices"

	"github.com/google/uuid"

	"github.com/danielmalka/go-knowrag/internal/vault"
)

// Orphan is a uid that still has points in the index and no longer has a note on disk.
//
// It is the deleted-note case, and it is not the same thing as the stale tail ProcessNote prunes:
// that one belongs to a note that still exists and got shorter, and the per-note prune of ADR-006
// §2 removes it inside the same round. A deleted note has no round of its own — nothing iterates
// over it, because iteration is driven by what the scan found on disk — so without a corpus-wide
// comparison its points are never visited again by anything. They keep answering searches with text
// that is not in the vault anymore, and nothing in the system reports it (PRD risk R15, ADR-006 §7).
type Orphan struct {
	UID uuid.UUID
	// Vault and Path come from the stored payload, not from disk — there is no disk left to read
	// them from. They exist so the report names something the operator recognizes: a bare uuid does
	// not answer "which note was this?", and that is the question anyone reading an orphan list has.
	Vault string
	Path  string
	// Points is how many points the uid still holds, which is the size of what --prune would delete.
	Points int
	// PathClaimed reports that a note the scan *did* return now lives at this path.
	//
	// It is the difference between "the file is gone" and "the file belongs to somebody else now",
	// and only the second is invisible from disk: editing a note's `uid` in the frontmatter leaves
	// the file exactly where it was under a new identity, so the old uid stops being claimed by
	// anything while its path still stats fine (D-34). A caller that answered "does the file exist"
	// would preserve those points forever, with no command that ever removes them.
	PathClaimed bool
}

// ScanOrphans compares the points already in the index against the notes the scan found on disk and
// returns what is in the first and not in the second.
//
// **It costs no network call.** The snapshot it reads is the one withPrefetch already takes at the
// start of every batch (prefetch.go) to answer the per-note integrity check — every uid of the
// tenant, already in memory. S06b T2 specifies a dedicated uid-only scroll for this, justified by
// affordability; that scroll would be a second full pass over the same data for an answer the first
// one already contains. The affordability requirement is met by construction rather than by a
// cheaper query.
//
// vaults is the run's scope and it is not optional. A run over one vault must not report the other
// vault's points as orphans: it never scanned that vault, so it has no evidence any of those notes
// were deleted — it only knows it did not look. Reporting them would be the same error as pruning
// them, one step earlier. A uid whose stored `vault` is outside the scope is skipped, and so is one
// whose payload does not carry a readable `vault` at all: an unattributable point is not evidence of
// a deletion either.
func ScanOrphans(
	snapshot map[uuid.UUID][]PointRecord,
	live []vault.Note,
	vaults []string,
) []Orphan {
	inScope := make(map[string]struct{}, len(vaults))
	for _, v := range vaults {
		inScope[v] = struct{}{}
	}
	// Both sets are derived here, from the one slice, and that is the whole reason this takes notes
	// rather than the two sets prepared. They answer two halves of one question — which uids are
	// alive, and which paths are spoken for — and a caller that could pass them separately could
	// pass them from different note sets. There would be no symptom: the uid half decides who is a
	// candidate, the path half decides whether a candidate is safe to delete, and a filter applied to
	// one and not the other silently authorizes deleting live notes.
	liveUIDs, livePaths := liveSets(live)

	var out []Orphan
	for uid, records := range snapshot {
		if _, alive := liveUIDs[uid]; alive || len(records) == 0 {
			continue
		}
		v, ok := records[0].Fields[fieldVault].(string)
		if !ok {
			continue
		}
		if _, ok := inScope[v]; !ok {
			continue
		}
		path, _ := records[0].Fields[fieldPath].(string)
		_, claimed := livePaths[v+"/"+path]
		out = append(out, Orphan{
			UID: uid, Vault: v, Path: path, Points: len(records), PathClaimed: claimed,
		})
	}

	// Sorted because the source is a map: an unordered orphan list would make the report differ
	// between two runs over an identical index, and the --json golden fixture would be unwritable.
	slices.SortFunc(out, func(a, b Orphan) int {
		if c := cmp.Compare(a.Vault, b.Vault); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		return cmp.Compare(a.UID.String(), b.UID.String())
	})
	return out
}

// liveSets is what the scan found on disk, in the two shapes the comparison needs: the uids, which
// say who is still alive, and the vault-qualified paths, which say which locations are spoken for.
//
// One function returning both, from one loop, because they must always describe the same notes.
func liveSets(notes []vault.Note) (map[uuid.UUID]struct{}, map[string]struct{}) {
	uids := make(map[uuid.UUID]struct{}, len(notes))
	paths := make(map[string]struct{}, len(notes))
	for _, n := range notes {
		uids[n.UID] = struct{}{}
		paths[n.Vault+"/"+n.Path] = struct{}{}
	}
	return uids, paths
}
