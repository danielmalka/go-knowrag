package ingest

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/danielmalka/go-knowrag/internal/vault"
)

// TestScanOrphans_ReturnsTheUIDWithNoNoteOnDisk is the base case: two uids in the index, one of them
// still on disk, and the answer is the other one with the size of what a prune would delete.
func TestScanOrphans_ReturnsTheUIDWithNoNoteOnDisk(t *testing.T) {
	kept, gone := uuidFromPath(t, "kept.md"), uuidFromPath(t, "deleted.md")
	snapshot := map[uuid.UUID][]PointRecord{
		kept: orphanRecords("pessoal", "kept.md", 2),
		gone: orphanRecords("pessoal", "deleted.md", 3),
	}

	got := ScanOrphans(snapshot, []vault.Note{testNote(t, "kept.md", 1)}, []string{"pessoal"})

	want := []Orphan{{UID: gone, Vault: "pessoal", Path: "deleted.md", Points: 3}}
	if !slices.Equal(got, want) {
		t.Errorf("ScanOrphans = %v, want %v", got, want)
	}
}

// TestScanOrphans_ExcludesAVaultTheRunNeverScanned is the test this file exists for.
//
// A run over one vault has no evidence about the other: it did not walk its disk, so every uid there
// is missing from the note set for the same reason a deleted one is, and nothing in the snapshot
// tells the two apart. Reporting them would be the same mistake as pruning them, one step earlier —
// and since the report is what an operator reads before typing --prune --yes, one step earlier is
// where it does its damage.
//
// Both uids here are absent from disk, which is what makes the scope the only thing that can
// separate them: drop the vault filter and this returns two orphans instead of one.
func TestScanOrphans_ExcludesAVaultTheRunNeverScanned(t *testing.T) {
	scanned, unscanned := uuidFromPath(t, "mine.md"), uuidFromPath(t, "theirs.md")
	snapshot := map[uuid.UUID][]PointRecord{
		scanned:   orphanRecords("pessoal", "mine.md", 1),
		unscanned: orphanRecords("trabalho", "theirs.md", 4),
	}

	got := ScanOrphans(snapshot, nil, []string{"pessoal"})

	want := []Orphan{{UID: scanned, Vault: "pessoal", Path: "mine.md", Points: 1}}
	if !slices.Equal(got, want) {
		t.Errorf("ScanOrphans over vault %q = %v, want %v — a point from a vault this run never "+
			"opened is not evidence that its note was deleted", "pessoal", got, want)
	}
}

// TestScanOrphans_ExcludesAPointWithNoReadableVault covers the payload the scan cannot attribute.
//
// A point with no `vault`, or with something that is not a string under that key, cannot be placed
// inside or outside the run's scope. Defaulting it in would delete a point on the strength of a
// payload nobody could read.
//
// The empty name in the scope list is what makes this test discriminate, and it is worth explaining
// because it looks like a typo. A failed type assertion yields "", so with any ordinary scope the
// vault filter of the previous test already excludes these points and this one would stay green with
// the readability check deleted — a test passing for a reason that has nothing to do with its name.
// With "" in scope, the filter lets "" through and only the readability check stands between an
// unreadable payload and a report that says its note was deleted.
func TestScanOrphans_ExcludesAPointWithNoReadableVault(t *testing.T) {
	missing, wrongType := uuidFromPath(t, "no-vault.md"), uuidFromPath(t, "vault-is-a-number.md")
	snapshot := map[uuid.UUID][]PointRecord{
		missing:   {{ChunkIndex: 0, Fields: map[string]any{fieldPath: "no-vault.md"}}},
		wrongType: {{ChunkIndex: 0, Fields: map[string]any{fieldVault: 42, fieldPath: "n.md"}}},
	}

	got := ScanOrphans(snapshot, nil, []string{"pessoal", ""})
	if len(got) != 0 {
		t.Errorf("ScanOrphans over unattributable points = %v, want none — a point whose vault "+
			"nobody can read is not evidence that its note was deleted", got)
	}
}

// TestScanOrphans_OrderIsDeterministic pins the sort. The source is a map, and Go randomizes map
// iteration, so without it two runs over an identical index print the orphans in different orders —
// which makes two reports impossible to diff and the --json golden fixture impossible to write.
//
// Eight entries rather than two: with the sort removed, an unordered result has one chance in 40320
// of coming out sorted anyway, and the second call is there to catch that one.
func TestScanOrphans_OrderIsDeterministic(t *testing.T) {
	paths := []string{"h.md", "c.md", "a.md", "g.md", "b.md", "f.md", "d.md", "e.md"}
	snapshot := map[uuid.UUID][]PointRecord{}
	for _, p := range paths {
		snapshot[uuidFromPath(t, p)] = orphanRecords("pessoal", p, 1)
	}

	first := ScanOrphans(snapshot, nil, []string{"pessoal"})
	second := ScanOrphans(snapshot, nil, []string{"pessoal"})

	sorted := slices.Sorted(slices.Values(paths))
	got := make([]string, len(first))
	for i, o := range first {
		got[i] = o.Path
	}
	if !slices.Equal(got, sorted) {
		t.Errorf("ScanOrphans returned %v, want them sorted: %v", got, sorted)
	}
	if !slices.Equal(first, second) {
		t.Errorf("two calls over the same snapshot returned different lists:\n%v\n%v", first, second)
	}
}

// TestRunBatch_DeletedNote_IsReportedAsAnOrphan is the wiring half: ScanOrphans can be perfect and
// still never run. The store here answers the whole-tenant snapshot, so the batch takes the prefetch
// path (prefetch.go) and the scan has something to compare against.
//
// The orphan is seeded but never handed to RunBatch as a note, which is exactly what a note deleted
// between two runs looks like.
func TestRunBatch_DeletedNote_IsReportedAsAnOrphan(t *testing.T) {
	j := &journal{}
	fake := newFakeStore(j)
	gone := uuidFromPath(t, "deleted.md")
	fake.seed(testTenant, gone, orphanRecords("pessoal", "deleted.md", 2)...)

	d := testDeps(bulkStore{fakeStore: fake}, newSpyEmbedder(j))
	d.Vaults = []string{"pessoal"}

	report, err := RunBatch(t.Context(), d, []vault.Note{testNote(t, "kept.md", 1)})
	if err != nil {
		t.Fatalf("RunBatch: %v", err)
	}

	if !report.OrphansScanned {
		t.Fatal("OrphansScanned is false although the store answered the snapshot")
	}
	want := []Orphan{{UID: gone, Vault: "pessoal", Path: "deleted.md", Points: 2}}
	if !slices.Equal(report.Orphans, want) {
		t.Errorf("Report.Orphans = %v, want %v", report.Orphans, want)
	}
	// Reported, not removed: --prune is what deletes, and this run did not ask for it. The count is
	// per uid rather than over the whole journal, because the kept note's own tail prune is a delete
	// this run is expected to make (note.go).
	for _, call := range fake.deletes {
		if call.UID == gone {
			t.Errorf("the run deleted from orphan %s (from chunk_index %d) with nobody having "+
				"authorized a prune", call.UID, call.FromChunkIndex)
		}
	}
}

// TestRunBatch_SnapshotUnavailable_SaysNotScannedRatherThanNone is the difference between the two
// answers an empty orphan list can carry.
//
// The snapshot is an optimization allowed to fail (prefetch.go), and a run that lost it knows
// nothing about deletions. Printing "orphans: none" there tells the operator the vault is in sync
// when the truth is that nobody looked — and that sentence is the one they would act on.
//
// A real orphan is seeded, so the scan would have found something had it run: the test cannot pass
// by there being nothing to find.
func TestRunBatch_SnapshotUnavailable_SaysNotScannedRatherThanNone(t *testing.T) {
	j := &journal{}
	fake := newFakeStore(j)
	fake.seed(testTenant, uuidFromPath(t, "deleted.md"), orphanRecords("pessoal", "deleted.md", 2)...)

	store := bulkStore{fakeStore: fake, scrollTenantErr: errors.New("qdrant is unreachable")}
	d := testDeps(store, newSpyEmbedder(j))
	d.Vaults = []string{"pessoal"}

	report, err := RunBatch(t.Context(), d, []vault.Note{testNote(t, "kept.md", 1)})
	if err != nil {
		t.Fatalf("RunBatch: %v — a failed snapshot degrades the run, it does not fail it", err)
	}

	if report.OrphansScanned {
		t.Error("OrphansScanned is true although the snapshot could not be read")
	}
	if len(report.Orphans) != 0 {
		t.Errorf("Report.Orphans = %v from a snapshot that was never taken", report.Orphans)
	}

	rendered := report.String()
	if !strings.Contains(rendered, "not scanned") {
		t.Errorf("report %q does not say the orphan scan did not run", rendered)
	}
	if strings.Contains(rendered, "orphans: none") {
		t.Errorf("report %q claims there are no orphans on a run that never looked", rendered)
	}
}

// TestOrchestrate_VaultScopeComesFromTheScans covers the one input the orphan scan cannot do
// without, in the two ways it can be wrong.
//
// One scanned vault has zero notes, which is the case worth pinning on its own: deriving the scope
// from the flattened note set instead of from the scans would leave it out, and the run where every
// note was deleted — the one where the orphan list matters most — would report nothing.
//
// A third vault has points and is *not* scanned, which is what makes this discriminate. With only
// in-scope points seeded, the test passes with the vault filter deleted entirely; the outsider is
// the assertion.
//
// Deps.Vaults arrives pre-populated with a wrong scope, because "empty" is not the only way a
// caller gets it wrong and filling it in only when empty leaves the other way silent.
func TestOrchestrate_VaultScopeComesFromTheScans(t *testing.T) {
	j := &journal{}
	fake := newFakeStore(j)
	mine := uuidFromPath(t, "was-here.md")
	theirs := uuidFromPath(t, "not-my-vault.md")
	fake.seed(testTenant, mine, orphanRecords("pessoal", "was-here.md", 3)...)
	fake.seed(testTenant, theirs, orphanRecords("arquivo", "not-my-vault.md", 4)...)

	d := testDeps(bulkStore{fakeStore: fake}, newSpyEmbedder(j))
	d.Vaults = []string{"arquivo"}

	// Two scans, so the scope is a list rather than a single name, and neither of them has a note:
	// a run of a roster that was emptied on disk is exactly the state this has to survive.
	scans := []vault.ScanResult{{Vault: "pessoal"}, {Vault: "trabalho"}}
	report, err := Orchestrate(t.Context(), d, scans...)
	if err != nil {
		t.Fatalf("Orchestrate: %v", err)
	}

	if !report.OrphansScanned {
		t.Fatal("OrphansScanned is false: Orchestrate did not pass the scanned vaults down as scope")
	}
	want := []Orphan{{UID: mine, Vault: "pessoal", Path: "was-here.md", Points: 3}}
	if !slices.Equal(report.Orphans, want) {
		t.Errorf("Report.Orphans = %v, want %v — `arquivo` was never scanned by this run, so nothing "+
			"in it is evidence of a deletion, and the caller saying otherwise does not make it so",
			report.Orphans, want)
	}
}

// TestScanOrphans_PathClaimedComesFromTheSameNotesAsTheLiveUIDs is the single-derivation property.
//
// Both halves of the comparison are computed inside ScanOrphans, from the one slice it is given:
// which uids are alive, and which paths are spoken for. That is not tidiness — a caller able to pass
// them separately could pass them from different note sets, and the failure would be silent, because
// the uid half decides who is a candidate while the path half decides whether a candidate is safe to
// delete.
//
// The note here has the deleted uid's *path* and a different uid, which is what editing `uid` in the
// frontmatter produces: the old uid is a candidate, and its path is claimed by the new one.
func TestScanOrphans_PathClaimedComesFromTheSameNotesAsTheLiveUIDs(t *testing.T) {
	oldUID := uuidFromPath(t, "old-identity")
	renamed := testNote(t, "renamed.md", 1) // vault pessoal, path renamed.md, its own uid
	snapshot := map[uuid.UUID][]PointRecord{
		oldUID: orphanRecords("pessoal", "renamed.md", 3),
	}

	got := ScanOrphans(snapshot, []vault.Note{renamed}, []string{"pessoal"})

	want := []Orphan{{UID: oldUID, Vault: "pessoal", Path: "renamed.md", Points: 3, PathClaimed: true}}
	if !slices.Equal(got, want) {
		t.Errorf("ScanOrphans = %v, want %v — the live note holds that path, so the old uid has none",
			got, want)
	}
}

// TestScanOrphans_UnclaimedPathIsNotFlagged is the other side: an ordinary deleted note has nothing
// at its path, and flagging it would tell cmd/cli to skip the disk check that protects excluded
// folders.
func TestScanOrphans_UnclaimedPathIsNotFlagged(t *testing.T) {
	gone := uuidFromPath(t, "deleted.md")
	snapshot := map[uuid.UUID][]PointRecord{gone: orphanRecords("pessoal", "deleted.md", 2)}

	got := ScanOrphans(snapshot, []vault.Note{testNote(t, "elsewhere.md", 1)}, []string{"pessoal"})

	if len(got) != 1 || got[0].PathClaimed {
		t.Errorf("ScanOrphans = %v, want the candidate with PathClaimed false", got)
	}
}
