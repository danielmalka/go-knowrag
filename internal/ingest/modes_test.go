package ingest

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/danielmalka/go-knowrag/internal/vault"
)

// TestRunBatch_DryRun_TouchesNothingAndReportsWhatItWould is the hermetic half of "--dry-run writes
// nothing", and it is stronger than the before/after point count the integration test asserts: a
// count is equal again after a delete followed by an identical rewrite, while an empty write log
// cannot be.
//
// The note is seeded as stale, not absent, so it is genuinely work: an unchanged corpus would report
// `skipped` and prove nothing about the path that decides to write.
func TestRunBatch_DryRun_TouchesNothingAndReportsWhatItWould(t *testing.T) {
	j := &journal{}
	fake := newFakeStore(j)
	stale := testNote(t, "stale.md", 2)
	fake.seed(testTenant, stale.UID, PointRecord{ChunkIndex: 0, PointHash: "no-longer-what-disk-says"})

	d := testDeps(fake, newSpyEmbedder(j))
	d.DryRun = true

	report, err := RunBatch(t.Context(), d, []vault.Note{stale})
	if err != nil {
		t.Fatalf("RunBatch: %v", err)
	}

	if got := report.Count(StatePruned); got != 1 {
		t.Errorf("%d note(s) reported as would-be-written, want 1 — a dry run has to say what the "+
			"real run would achieve, and %v is what it said", got, report.Counts())
	}
	// The whole promise, against the call log. Reading is expected; writing is not.
	for _, call := range []string{"embed", "upsert", "delete"} {
		if n := j.count(call); n != 0 {
			t.Errorf("a dry run issued %d %s call(s)", n, call)
		}
	}
	if got := fake.indices(testTenant, stale.UID); !slices.Equal(got, []int{0}) {
		t.Errorf("the store now holds chunk_index %v; it held [0] before the dry run", got)
	}
}

// TestRunBatch_Full_RewritesAnIntegralNote covers the flag's only reason to exist: the note is
// already integral, so an incremental run skips it and a full run must not.
func TestRunBatch_Full_RewritesAnIntegralNote(t *testing.T) {
	j := &journal{}
	fake := newFakeStore(j)
	note := testNote(t, "unchanged.md", 2)
	fake.seed(testTenant, note.UID, expectedFor(t, note)...)

	d := testDeps(fake, newSpyEmbedder(j))
	d.Full = true

	report, err := RunBatch(t.Context(), d, []vault.Note{note})
	if err != nil {
		t.Fatalf("RunBatch: %v", err)
	}

	if got := report.Count(StateSkipped); got != 0 {
		t.Errorf("%d note(s) skipped on a --full run; the integrity short-circuit is what --full "+
			"exists to ignore", got)
	}
	if got := report.Count(StatePruned); got != 1 {
		t.Errorf("counts = %v, want the note rewritten", report.Counts())
	}
	if n := j.count("upsert"); n != 1 {
		t.Errorf("%d upsert(s) on a --full run over one note, want 1", n)
	}
}

// TestRunBatch_Full_StillScansForOrphans pins the interaction that is easy to lose: --full changes
// what happens per note, and must not cost the corpus-wide comparison. The snapshot the orphan scan
// subtracts from is the same prefetch the integrity check uses, so a --full run that skipped the
// prefetch as pointless would silently stop reporting deleted notes.
func TestRunBatch_Full_StillScansForOrphans(t *testing.T) {
	j := &journal{}
	fake := newFakeStore(j)
	gone := uuidFromPath(t, "deleted.md")
	fake.seed(testTenant, gone, orphanRecords("pessoal", "deleted.md", 2)...)

	d := testDeps(bulkStore{fakeStore: fake}, newSpyEmbedder(j))
	d.Full = true
	d.Vaults = []string{"pessoal"}

	report, err := RunBatch(t.Context(), d, []vault.Note{testNote(t, "kept.md", 1)})
	if err != nil {
		t.Fatalf("RunBatch: %v", err)
	}
	if !report.OrphansScanned {
		t.Fatal("a --full run did not scan for orphans")
	}
	if len(report.Orphans) != 1 || report.Orphans[0].UID != gone {
		t.Errorf("Report.Orphans = %v, want the deleted note", report.Orphans)
	}
}

// TestOrchestrate_Only_NarrowsTheRunAndSkipsTheOrphanScan is the pair of effects --only has, and the
// second is the one that would otherwise be forgotten.
//
// A filtered run's live set is a subset of the corpus, so `snapshot \ live` names every unvisited
// note as deleted. Reporting that list is not a lesser version of the answer, it is a wrong one — and
// it is what an operator would read before deciding to prune. So the scan does not run, and the
// report says which restriction stopped it.
func TestOrchestrate_Only_NarrowsTheRunAndSkipsTheOrphanScan(t *testing.T) {
	j := &journal{}
	fake := newFakeStore(j)
	fake.seed(testTenant, uuidFromPath(t, "gone.md"), orphanRecords("pessoal", "gone.md", 2)...)

	d := testDeps(bulkStore{fakeStore: fake}, newSpyEmbedder(j))
	d.Only = "pessoal/areas/**"

	inside := testNote(t, "areas/inside.md", 1)
	outside := testNote(t, "outra/outside.md", 1)
	// Spread from a slice rather than passed as one literal argument: a single variadic argument
	// lets gosec infer len(scans)==1 and flag the bounded duplicate-uid loop in batch.go as an
	// out-of-range index, which is a false positive in untouched code.
	scans := []vault.ScanResult{{Vault: "pessoal", Notes: []vault.Note{inside, outside}}}
	report, err := Orchestrate(t.Context(), d, scans...)
	if err != nil {
		t.Fatalf("Orchestrate: %v", err)
	}

	if len(report.Results) != 1 || report.Results[0].Path != inside.Path {
		t.Errorf("the run processed %v, want only %s", report.Results, inside.Path)
	}
	if report.OrphansScanned {
		t.Error("a --only run scanned for orphans; every note it did not visit would be in that list")
	}
	if !strings.Contains(report.OrphanScanSkipped, d.Only) {
		t.Errorf("the skip reason %q does not name the restriction that caused it",
			report.OrphanScanSkipped)
	}
	// The reason has to reach the operator, not just the struct.
	if rendered := report.String(); !strings.Contains(rendered, "not scanned") ||
		!strings.Contains(rendered, d.Only) {
		t.Errorf("report %q does not explain why the orphan scan did not run", rendered)
	}
}

// TestOrchestrate_Only_BadPattern_FailsTheRun keeps a pattern this build cannot honour from being
// reported as a corpus with one note in it.
func TestOrchestrate_Only_BadPattern_FailsTheRun(t *testing.T) {
	j := &journal{}
	d := testDeps(newFakeStore(j), newSpyEmbedder(j))
	d.Only = "pessoal/**/deep.md"

	scans := []vault.ScanResult{{Vault: "pessoal", Notes: []vault.Note{testNote(t, "areas/one.md", 1)}}}
	_, err := Orchestrate(t.Context(), d, scans...)
	if err == nil {
		t.Fatal("Orchestrate with an unusable --only pattern returned no error")
	}
	if n := j.count("upsert"); n != 0 {
		t.Errorf("%d upsert(s) before the pattern was refused", n)
	}
}

// TestRunBatch_Interrupted_StopsScheduling is the scheduling half, and only that: the channel
// arrives closed, so no note is started.
//
// It used to also claim "prunes nothing unconfirmed" and loop over fake.deletes to prove it — a
// tautology, because with zero notes processed there are no deletes to look at. The claim needed a
// note actually in flight with an unconfirmed upsert, which is the test below.
func TestRunBatch_Interrupted_StopsScheduling(t *testing.T) {
	j := &journal{}
	fake := newFakeStore(j)
	notes := []vault.Note{testNote(t, "one.md", 2), testNote(t, "two.md", 2)}

	stop := make(chan struct{})
	close(stop)
	d := testDeps(fake, newSpyEmbedder(j))
	d.Interrupt = stop

	report, err := RunBatch(t.Context(), d, notes)
	if err != nil {
		t.Fatalf("RunBatch: %v — an interrupted run is a partial result, not a failure", err)
	}
	if !report.Interrupted {
		t.Error("Report.Interrupted is false on a run that stopped early")
	}
	if len(report.Results) != 0 {
		t.Errorf("%d note(s) were processed after the interrupt", len(report.Results))
	}
	// The partial report is what the next run converges from, so it has to say it is partial.
	if !strings.Contains(report.String(), "interrupted") {
		t.Errorf("report %q does not tell the operator the run stopped early", report.String())
	}
}

// TestRunBatch_InterruptedMidNote_NeverPrunesAnUnconfirmedUpsert is the claim the test above used to
// make without exercising it.
//
// A note is genuinely in flight and its upsert comes back **ambiguous** — the shape a cancelled
// context produces against a real store, where the write may or may not have landed. Pruning on that
// is how a note loses points it still needs (ADR-006 §3), so the assertion is per uid against the
// store's call log: whatever else happened, no DeleteByFilter carries the uid of a note that never
// reached StateUpsertConfirmed.
func TestRunBatch_InterruptedMidNote_NeverPrunesAnUnconfirmedUpsert(t *testing.T) {
	j := &journal{}
	fake := newFakeStore(j)
	stop := make(chan struct{})

	ctx, cancel := context.WithCancel(t.Context())
	fake.upsertHook = func(_ int, points []Point) (int, UpsertOutcome, error) {
		// The operator's Ctrl-C lands exactly in the upsert→prune window, and the store cannot say
		// whether the write completed.
		close(stop)
		cancel()
		return len(points), UpsertAmbiguous, context.Canceled
	}

	d := testDeps(fake, newSpyEmbedder(j))
	d.Interrupt = stop
	d.UpsertAttempts = 3

	notes := []vault.Note{testNote(t, "in-flight.md", 2), testNote(t, "never-started.md", 2)}
	report, err := RunBatch(ctx, d, notes)
	if err != nil {
		t.Fatalf("RunBatch: %v", err)
	}

	confirmed := map[uuid.UUID]bool{}
	for _, res := range report.Results {
		confirmed[res.UID] = res.State == StateUpsertConfirmed || res.State == StatePruned
	}
	if confirmed[notes[0].UID] {
		t.Fatalf("the in-flight note ended %v, want a non-confirmed state — the fixture is not "+
			"exercising the window it exists for", report.Results[0].State)
	}
	for _, call := range fake.deletes {
		if !confirmed[call.UID] {
			t.Errorf("DeleteByFilter was called for %s, whose upsert was never confirmed; pruning on "+
				"top of an unconfirmed write is how a note loses points it still needs", call.UID)
		}
	}
	if !report.Interrupted {
		t.Error("Report.Interrupted is false although the run was interrupted mid-note")
	}
}

// TestRunBatch_Interrupted_MidBatch_KeepsWhatItFinished covers the ordinary case: the interrupt
// arrives while the run is under way, and the notes already done stay done.
func TestRunBatch_Interrupted_MidBatch_KeepsWhatItFinished(t *testing.T) {
	j := &journal{}
	fake := newFakeStore(j)
	first, second := testNote(t, "first.md", 2), testNote(t, "second.md", 2)

	stop := make(chan struct{})
	// Closed by the store the moment the first note's write lands, which is the closest a serial
	// batch gets to "Ctrl-C during note one".
	fake.upsertHook = func(call int, points []Point) (int, UpsertOutcome, error) {
		if call == 1 {
			close(stop)
		}
		return len(points), UpsertConfirmed, nil
	}
	d := testDeps(fake, newSpyEmbedder(j))
	d.Interrupt = stop

	report, err := RunBatch(t.Context(), d, []vault.Note{first, second})
	if err != nil {
		t.Fatalf("RunBatch: %v", err)
	}

	if len(report.Results) != 1 || report.Results[0].UID != first.UID {
		t.Fatalf("results = %v, want only the note that was already running", report.Results)
	}
	// The note in flight finished its whole round rather than being dropped between the confirmed
	// upsert and the prune — the window ADR-006 §3 exists to protect.
	if got := report.Results[0].State; got != StatePruned {
		t.Errorf("the in-flight note ended %s, want %s: the interrupt must not cut a note in half",
			got, StatePruned)
	}
	if got := fake.indices(testTenant, second.UID); len(got) != 0 {
		t.Errorf("the second note was written (chunk_index %v) after the interrupt", got)
	}
}

// TestRunBatch_InterruptedDuringTheLastNote_StillSaysSo covers the case the loop check cannot see.
//
// The interrupt arrives while the only note is running, so no second iteration ever reaches the top
// of the loop. Without the check after the loop the run reports itself as having completed
// normally — and because the note it was cutting short failed, the process exits as a generic
// failure instead of as an interruption. A real SIGINT test caught this; nothing hermetic did.
func TestRunBatch_InterruptedDuringTheLastNote_StillSaysSo(t *testing.T) {
	j := &journal{}
	fake := newFakeStore(j)
	stop := make(chan struct{})
	fake.upsertHook = func(_ int, points []Point) (int, UpsertOutcome, error) {
		close(stop) // the signal lands mid-note, and there is no note after it
		return len(points), UpsertConfirmed, nil
	}

	d := testDeps(fake, newSpyEmbedder(j))
	d.Interrupt = stop

	report, err := RunBatch(t.Context(), d, []vault.Note{testNote(t, "only.md", 2)})
	if err != nil {
		t.Fatalf("RunBatch: %v", err)
	}
	if !report.Interrupted {
		t.Error("Report.Interrupted is false although the run was interrupted during its last note")
	}
	if len(report.Results) != 1 {
		t.Errorf("%d result(s), want the note that was already running to be kept", len(report.Results))
	}
}
