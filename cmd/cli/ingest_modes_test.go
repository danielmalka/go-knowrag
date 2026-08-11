package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/danielmalka/go-knowrag/internal/chunk"
	"github.com/danielmalka/go-knowrag/internal/config"
	"github.com/danielmalka/go-knowrag/internal/ingest"
)

// TestConfirmPrune_NonInteractiveStdin_RefusedWithoutPrompting is the refusal that has to happen
// before anything else, and "without prompting" is the half that carries the weight.
//
// The failure it prevents is not a wrong answer, it is no answer: a scheduled `ingest --prune` that
// reached a y/N prompt would sit there holding the ingestion lock until someone noticed the nightly
// run never finished. Asserting that nothing was written to the prompt writer is what distinguishes
// "refused" from "asked and got EOF" — both return an error, only one of them cannot hang.
//
// stdin is an os.Pipe rather than /dev/null on purpose: /dev/null is a character device, so it
// passes the very check under test.
func TestConfirmPrune_NonInteractiveStdin_RefusedWithoutPrompting(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })

	var prompt bytes.Buffer
	err = confirmPrune(r, &prompt, ingestOptions{prune: true})

	if !errors.Is(err, errUsage) {
		t.Fatalf("confirmPrune with --prune, no --yes and a piped stdin = %v, want errUsage", err)
	}
	for _, want := range []string{"--yes", "stdin is not a terminal"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if prompt.Len() != 0 {
		t.Errorf("confirmPrune asked %q on a stdin nobody can answer", prompt.String())
	}
}

// TestConfirmPrune_Authorized_AsksNothing covers the two ways a run is already authorized: --yes
// given, or no --prune at all. Neither may reach the prompt — the second because a run that deletes
// nothing has nothing to confirm.
func TestConfirmPrune_Authorized_AsksNothing(t *testing.T) {
	for name, opts := range map[string]ingestOptions{
		"--prune with --yes":  {prune: true, yes: true},
		"no --prune at all":   {},
		"--yes without prune": {yes: true},
	} {
		t.Run(name, func(t *testing.T) {
			var prompt bytes.Buffer
			// os.Stdin unread: an authorized run must not touch it, so handing over the real one is
			// safe and proves the early return happened.
			if err := confirmPrune(os.Stdin, &prompt, opts); err != nil {
				t.Errorf("confirmPrune(%+v) = %v, want nil", opts, err)
			}
			if prompt.Len() != 0 {
				t.Errorf("confirmPrune(%+v) asked %q for an already-authorized run", opts, prompt.String())
			}
		})
	}
}

// TestValidateModes_RefusesPruneWithDryRunOrOnly pins the two combinations left, and both are the
// same shape: a destructive flag over a run that cannot support it.
//
// --dry-run --json is deliberately in the accepted set now. It used to be refused because the dry
// run produced no Report at all; it goes through the same orchestration as every other mode, so the
// refusal would be about a limitation that no longer exists.
func TestValidateModes_RefusesPruneWithDryRunOrOnly(t *testing.T) {
	refused := map[string]struct {
		opts ingestOptions
		name string
	}{
		"--prune --dry-run": {opts: ingestOptions{dryRun: true, prune: true}, name: "--dry-run"},
		"--prune --only":    {opts: ingestOptions{only: "pessoal/**", prune: true}, name: "--only"},
	}
	for label, tc := range refused {
		t.Run(label, func(t *testing.T) {
			err := validateModes(tc.opts)
			if !errors.Is(err, errUsage) {
				t.Fatalf("validateModes(%+v) = %v, want errUsage", tc.opts, err)
			}
			for _, want := range []string{"--prune", tc.name} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %s; the operator has to know which pair to break",
						err, want)
				}
			}
		})
	}

	accepted := map[string]ingestOptions{
		"the default incremental run": {},
		"a plain dry run":             {dryRun: true},
		"a dry run reporting JSON":    {dryRun: true, json: true},
		"a full run with --json":      {full: true, json: true},
		"--only without --prune":      {only: "pessoal/**"},
		"a real prune with both":      {prune: true, json: true, yes: true},
	}
	for name, opts := range accepted {
		t.Run(name, func(t *testing.T) {
			if err := validateModes(opts); err != nil {
				t.Errorf("validateModes(%+v) = %v, want nil", opts, err)
			}
		})
	}
}

// TestIngestCmd_DryRunHelp_DescribesWhatItActuallyDoes is a test about a sentence, and the sentence
// is what an operator reads before trusting a clean dry run.
//
// Its previous version demanded the opposite — "never reads the index", "no prune candidates" — and
// stayed green while A-ii made the dry run read the index and list candidates. A test over prose
// ages with the behaviour it describes, and nobody rereads it because it is passing: it stopped
// protecting the operator and started holding the wrong help text in place. Hence the assertions
// below name both halves of today's truth, and the absent list names yesterday's.
func TestIngestCmd_DryRunHelp_DescribesWhatItActuallyDoes(t *testing.T) {
	usage := newIngestCmd(&config.Config{}).Flags().Lookup("dry-run").Usage
	for _, want := range []string{"reads the index", "writing nothing", "pruned"} {
		if !strings.Contains(usage, want) {
			t.Errorf("--dry-run usage %q does not say %q", usage, want)
		}
	}
	if strings.Contains(usage, "never reads") {
		t.Errorf("--dry-run usage %q still carries the pre-A-ii claim", usage)
	}
}

// recordingStore is an ingest.Store that records every call and does nothing else. It exists for one
// assertion — that the prune refusal happens before the store is touched — and that assertion cannot
// be written against the returned error: a prune that deleted everything and then complained returns
// an error too.
type recordingStore struct{ calls []string }

func (s *recordingStore) ScrollByUID(context.Context, string, uuid.UUID) ([]ingest.PointRecord, error) {
	s.calls = append(s.calls, "scroll")
	return nil, nil
}

func (s *recordingStore) UpsertPoints(context.Context, string, uuid.UUID, []ingest.Point) (ingest.UpsertOutcome, error) {
	s.calls = append(s.calls, "upsert")
	return ingest.UpsertConfirmed, nil
}

func (s *recordingStore) DeleteByFilter(context.Context, string, uuid.UUID, int) error {
	s.calls = append(s.calls, "delete")
	return nil
}

// TestPruneOrphans_SnapshotUnavailable_RefusesWithoutTouchingTheStore is the exit-code half of "not
// having looked never renders as having found nothing".
//
// The orphan list is deliberately non-empty while OrphansScanned is false — a combination the batch
// never produces (batch.go sets both together), because with an empty list ingest.Prune would delete
// nothing anyway and the test could not tell a working guard from a missing one.
//
// What actually kills this test is the `err == nil` regression in splitStillOnDisk: roots is nil
// here, so no root is known for the candidate's vault, and today's code keeps it — which means the
// prune list is empty by the time the OrphansScanned guard is reached. Restore the old condition and
// the candidate reaches the guard, and a deleted guard then reaches DeleteByFilter. The list being
// non-empty is what leaves that path open; it is not by itself the mechanism.
func TestPruneOrphans_SnapshotUnavailable_RefusesWithoutTouchingTheStore(t *testing.T) {
	store := &recordingStore{}
	report := ingest.Report{
		OrphansScanned: false,
		Orphans: []ingest.Orphan{{
			UID:   uuid.MustParse("0198a7f2-4b31-7c42-9e15-3d8a92c47b06"),
			Vault: "pessoal", Path: "areas/gone.md", Points: 3,
		}},
	}

	err := pruneOrphans(t.Context(), store, defaultTenantID, nil, ingestOptions{}, &report)
	if err == nil {
		t.Fatal("pruneOrphans over a report whose snapshot failed = nil; a run that asked to delete " +
			"things must not exit clean having deleted nothing it could not identify")
	}
	if len(store.calls) != 0 {
		t.Errorf("pruneOrphans called %v before refusing; the refusal has to come before the store "+
			"is touched, not after", store.calls)
	}
	if report.PointsPruned != 0 {
		t.Errorf("PointsPruned = %d on a refused prune", report.PointsPruned)
	}
}

// TestPruneOrphans_Scanned_PrunesAndCountsWhatItRemoved is the other half, and it is what keeps the
// test above from being satisfied by a pruneOrphans that refuses unconditionally.
func TestPruneOrphans_Scanned_PrunesAndCountsWhatItRemoved(t *testing.T) {
	store := &recordingStore{}
	report := ingest.Report{
		OrphansScanned: true,
		Orphans: []ingest.Orphan{{
			UID:   uuid.MustParse("0198a7f2-4b31-7c42-9e15-3d8a92c47b06"),
			Vault: "pessoal", Path: "areas/gone.md", Points: 3,
		}},
	}

	// A real vault root whose file is absent: the prune list is only reachable through the on-disk
	// check now, so a nil root map would classify this as unverifiable and skip the delete.
	roots := map[string]string{"pessoal": t.TempDir()}
	if err := pruneOrphans(t.Context(), store, defaultTenantID, roots, ingestOptions{}, &report); err != nil {
		t.Fatalf("pruneOrphans on a scanned run: %v", err)
	}
	if want := []string{"delete"}; !slices.Equal(store.calls, want) {
		t.Errorf("pruneOrphans issued %v, want %v", store.calls, want)
	}
	if report.PointsPruned != 3 {
		t.Errorf("PointsPruned = %d, want 3", report.PointsPruned)
	}
}

// TestAskConfirmation_Answers is the prompt's own contract, and the `n` case is why it exists:
// TestConfirmPrune_Authorized_AsksNothing passes against a confirmPrune that returns nil for
// everything, so without this the interactive branch has no test that a refusal is possible at all.
//
// EOF is in the table beside it because an operator who hits Ctrl-D has not said yes, and every
// other way the read can fail leaves the answer unknown — which is the same case.
func TestAskConfirmation_Answers(t *testing.T) {
	tests := map[string]struct {
		answer  string
		wantErr bool
	}{
		"y":                 {answer: "y\n"},
		"Y":                 {answer: "Y\n"},
		"n refuses":         {answer: "n\n", wantErr: true},
		"anything else":     {answer: "maybe\n", wantErr: true},
		"a bare newline":    {answer: "\n", wantErr: true},
		"EOF, nobody typed": {answer: "", wantErr: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var prompt bytes.Buffer
			err := askConfirmation(strings.NewReader(tc.answer), &prompt)

			if tc.wantErr && !errors.Is(err, errUsage) {
				t.Errorf("askConfirmation(%q) = %v, want errUsage", tc.answer, err)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("askConfirmation(%q) = %v, want nil", tc.answer, err)
			}
			if !strings.Contains(prompt.String(), "[y/N]") {
				t.Errorf("prompt %q does not ask the question", prompt.String())
			}
		})
	}
}

// TestIngestCmd_PruneWithoutYes_PromptStaysOffStdout is the wiring of the fix, end to end through
// cobra, and it is about which stream the question lands on.
//
// With --json the run report goes to stdout. A prompt printed there sits in front of the JSON and a
// consumer's parser dies on it — and no existing test could see that, because every one of them
// passes --yes and never reaches the question.
//
// os.Stdin is swapped for /dev/null, which is a character device on Linux and therefore reads as a
// terminal (isTerminal): that is what carries the run into the prompt deterministically, on a host
// whose real stdin may be anything. The read then hits EOF, so the run refuses — after asking, which
// is all this test needs to have happened.
func TestIngestCmd_PruneWithoutYes_PromptStaysOffStdout(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("opening %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { _ = devNull.Close() })
	real := os.Stdin
	os.Stdin = devNull
	t.Cleanup(func() { os.Stdin = real })

	cmd := newIngestCmd(&config.Config{})
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--prune", "--json"})

	if err := cmd.Execute(); !errors.Is(err, errUsage) {
		t.Fatalf("`ingest --prune --json` answering nothing = %v, want errUsage", err)
	}
	if !strings.Contains(errOut.String(), "[y/N]") {
		t.Errorf("stderr %q does not carry the prompt; the operator was never asked", errOut.String())
	}
	// Covers cobra's usage block as well as the prompt: both used to land here, and either one in
	// front of a JSON document is the same broken pipe to a consumer.
	if out.Len() != 0 {
		t.Errorf("stdout carries %q — with --json it must hold the report and nothing else, and "+
			"anything printed in front of it stops the JSON parsing", out.String())
	}
}

// failingWriter is a stdout that cannot be written to — a closed pipe, a full disk. Every other test
// in this package prints into a bytes.Buffer, whose Write never fails, so nothing else in the suite
// can see a report that was only half written.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// TestPrintReport_WriteFails_IsReported is the difference between exit 0 with half a JSON document
// and an exit code that says the report never landed. A consumer piping stdout into a parser reads
// a zero status as "this run is complete and here it is".
func TestPrintReport_WriteFails_IsReported(t *testing.T) {
	for name, opts := range map[string]ingestOptions{
		"--json":     {json: true},
		"human text": {},
	} {
		t.Run(name, func(t *testing.T) {
			tokens := chunk.NewCountingTokenCounter(chunk.FakeTokenCounter{})
			err := printReport(failingWriter{}, io.Discard, opts, ingest.Report{}, tokens)
			if err == nil {
				t.Error("printReport over a writer that fails = nil; a truncated report must not " +
					"pass for a finished one")
			}
		})
	}
}

// TestSplitStillOnDisk_KeepsWhatIsStillThere is the check between a config edit and a mass deletion.
//
// "The scan did not return this uid" has at least two causes that are not a deleted note: a file
// emptied to zero bytes goes to vault.ScanVault's Skipped list, and SkippedNote carries no uid; and
// a folder added to KNOWRAG_VAULT_*_EXCLUDE_FOLDERS takes every note under it out of the scan at
// once. Both leave the file on disk, and both look exactly like a deletion from the index's side.
//
// The two candidates here differ in one thing only — whether the file exists — so nothing but the
// stat can tell them apart.
func TestSplitStillOnDisk_KeepsWhatIsStillThere(t *testing.T) {
	root := writeVault(t, map[string]string{
		"00-inbox/excluded.md": note("0198a7f2-4b31-7c42-9e15-3d8a92c47b06", "Excluded"),
	})
	present := ingest.Orphan{
		UID:   uuid.MustParse("0198a7f2-4b31-7c42-9e15-3d8a92c47b06"),
		Vault: "trabalho", Path: "00-inbox/excluded.md", Points: 3,
	}
	deleted := ingest.Orphan{
		UID:   uuid.MustParse("0198a7f2-4b31-7c42-9e15-3d8a92c47b07"),
		Vault: "trabalho", Path: "00-inbox/really-gone.md", Points: 2,
	}

	report := ingest.Report{OrphansScanned: true, Orphans: []ingest.Orphan{present, deleted}}
	splitStillOnDisk(map[string]string{"trabalho": root}, &report)

	if !slices.Equal(report.Orphans, []ingest.Orphan{deleted}) {
		t.Errorf("Orphans = %v, want only %v — a note still on disk must never reach the prune list",
			report.Orphans, deleted)
	}
	if !slices.Equal(report.OnDisk, []ingest.Orphan{present}) {
		t.Errorf("OnDisk = %v, want %v", report.OnDisk, present)
	}
	// The report has to name it as what it is. "deleted from the vault" is the sentence that talks
	// an operator into removing a file that is sitting right there.
	rendered := report.String()
	// Only the section heading is asserted here. "the on-disk note is never called deleted" is not
	// checkable by searching the whole report for that phrase — this fixture also holds a real
	// orphan, which the report is supposed to call deleted. What separates the two is which section
	// each path lands in, and the slices.Equal checks above already prove that partition.
	if !strings.Contains(rendered, "not confirmed deleted") {
		t.Errorf("report %q does not say the file is still there", rendered)
	}
}

// TestSplitStillOnDisk_UnknownVault_IsKeptRatherThanPruned covers the vault this run has no root
// for. It cannot be checked, and unchecked is not evidence of deletion — the answer that cannot
// destroy anything is the right one for a state that should not be reachable.
func TestSplitStillOnDisk_UnknownVault_IsKeptRatherThanPruned(t *testing.T) {
	stranger := ingest.Orphan{
		UID:   uuid.MustParse("0198a7f2-4b31-7c42-9e15-3d8a92c47b08"),
		Vault: "pessoal", Path: "areas/whatever.md", Points: 1,
	}
	report := ingest.Report{OrphansScanned: true, Orphans: []ingest.Orphan{stranger}}

	splitStillOnDisk(map[string]string{"trabalho": t.TempDir()}, &report)

	if len(report.Orphans) != 0 {
		t.Errorf("Orphans = %v; a vault with no root in this run cannot be checked, so its "+
			"candidates must not be prunable", report.Orphans)
	}
	if !slices.Equal(report.OnDisk, []ingest.Orphan{stranger}) {
		t.Errorf("OnDisk = %v, want %v", report.OnDisk, stranger)
	}
}

// TestPruneOrphans_NoteStillOnDisk_IsNotDeleted is the same guard as TestSplitStillOnDisk, asserted
// where it destroys data rather than where it classifies.
//
// The two are not redundant: splitStillOnDisk being correct is worth nothing if the prune path can
// be wired without it, and this is the assertion that fails when it is. The store's call log is the
// evidence, because "not in Report.Orphans afterwards" is a claim about bookkeeping and this is a
// claim about Qdrant.
func TestPruneOrphans_NoteStillOnDisk_IsNotDeleted(t *testing.T) {
	root := writeVault(t, map[string]string{
		"00-inbox/excluded.md": note("0198a7f2-4b31-7c42-9e15-3d8a92c47b06", "Excluded"),
	})
	store := &recordingStore{}
	report := ingest.Report{
		OrphansScanned: true,
		Orphans: []ingest.Orphan{{
			UID:   uuid.MustParse("0198a7f2-4b31-7c42-9e15-3d8a92c47b06"),
			Vault: "trabalho", Path: "00-inbox/excluded.md", Points: 3,
		}},
	}

	err := pruneOrphans(t.Context(), store, defaultTenantID,
		map[string]string{"trabalho": root}, ingestOptions{}, &report)
	if err != nil {
		t.Fatalf("pruneOrphans: %v", err)
	}

	if len(store.calls) != 0 {
		t.Errorf("pruneOrphans called %v for a note whose file is on disk; a folder added to the "+
			"exclusion list would take the whole folder out of the index", store.calls)
	}
	if report.PointsPruned != 0 {
		t.Errorf("PointsPruned = %d, want 0", report.PointsPruned)
	}
}

// TestSplitStillOnDisk_StatFailsForAnotherReason_IsNotPruned is the difference between "this note is
// gone" and "I could not find out".
//
// Only ENOENT is evidence of a deletion. A stat can fail for permission, I/O, ENOTDIR, or a mount
// that dropped halfway — and the vaults on this machine sit behind a /mnt crossing that fails in
// exactly that shape rather than by returning ENOENT. Reading any of those as a deleted note is how
// `--prune --yes` on a schedule removes points for files that are sitting right there.
//
// The failure is provoked with a path whose parent component is a regular file, which returns
// ENOTDIR on Linux with no permission bits, no root caveat, and no injected stat seam.
func TestSplitStillOnDisk_StatFailsForAnotherReason_IsNotPruned(t *testing.T) {
	root := writeVault(t, map[string]string{
		"00-inbox/notes.md": note("0198a7f2-4b31-7c42-9e15-3d8a92c47b06", "Notes"),
	})
	unreadable := ingest.Orphan{
		UID: uuid.MustParse("0198a7f2-4b31-7c42-9e15-3d8a92c47b06"),
		// notes.md is a file, so stat of a child of it is ENOTDIR — a failure that is not an absence.
		Vault: "trabalho", Path: "00-inbox/notes.md/child.md", Points: 4,
	}
	report := ingest.Report{OrphansScanned: true, Orphans: []ingest.Orphan{unreadable}}

	splitStillOnDisk(map[string]string{"trabalho": root}, &report)

	if len(report.Orphans) != 0 {
		t.Errorf("Orphans = %v; a stat that failed for a reason other than absence is not evidence "+
			"of a deletion, and this list is what --prune deletes", report.Orphans)
	}
	if !slices.Equal(report.OnDisk, []ingest.Orphan{unreadable}) {
		t.Errorf("OnDisk = %v, want %v", report.OnDisk, unreadable)
	}
}

// TestReportMode names what a stored report says it was. --dry-run beats --full because it is the
// stronger claim: a full dry run still writes nothing, and a report labelled "full" reads as a run
// that reindexed the corpus.
func TestReportMode(t *testing.T) {
	for name, tc := range map[string]struct {
		opts ingestOptions
		want string
	}{
		"nothing set":    {want: modeIncremental},
		"--full":         {opts: ingestOptions{full: true}, want: modeFull},
		"--dry-run":      {opts: ingestOptions{dryRun: true}, want: modeDryRun},
		"both, dry wins": {opts: ingestOptions{dryRun: true, full: true}, want: modeDryRun},
	} {
		t.Run(name, func(t *testing.T) {
			if got := reportMode(tc.opts); got != tc.want {
				t.Errorf("reportMode(%+v) = %q, want %q", tc.opts, got, tc.want)
			}
		})
	}
}

// TestIngestCmd_RegistersTheModeFlags proves the A-ii flags exist as flags. Cobra builds its help
// from the registered set, so one that is missing is one the operator cannot pass — and --only in
// particular is the flag that sets ingest.PruneOptions.Filtered, the safeguard that shipped in A-i
// with no caller at all.
func TestIngestCmd_RegistersTheModeFlags(t *testing.T) {
	cmd := newIngestCmd(&config.Config{})
	for _, name := range []string{"full", "only", "grace-period"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("ingest does not register --%s", name)
		}
	}
	if got := cmd.Flags().Lookup("grace-period").DefValue; got != defaultGracePeriod.String() {
		t.Errorf("--grace-period defaults to %q, want %q", got, defaultGracePeriod)
	}
	// The refusal is only useful if the operator can read why from the flag itself.
	if usage := cmd.Flags().Lookup("only").Usage; !strings.Contains(usage, "--prune") {
		t.Errorf("--only usage %q does not mention that --prune is refused with it", usage)
	}
}

// TestPruneOrphans_FilteredRun_RefusedByThePackage gives ingest.PruneOptions.Filtered the caller it
// shipped without. cmd/cli refuses --prune with --only before this point, so the only way to reach
// the check is the way this test does — and that is the point of it: the refusal has to survive a
// validation that was bypassed, because it is the last thing between a subset run and a delete.
func TestPruneOrphans_FilteredRun_RefusedByThePackage(t *testing.T) {
	store := &recordingStore{}
	report := ingest.Report{
		OrphansScanned: true,
		Orphans: []ingest.Orphan{{
			UID:   uuid.MustParse("0198a7f2-4b31-7c42-9e15-3d8a92c47b06"),
			Vault: "trabalho", Path: "00-inbox/gone.md", Points: 3,
		}},
	}

	err := pruneOrphans(t.Context(), store, defaultTenantID,
		map[string]string{"trabalho": t.TempDir()}, ingestOptions{only: "trabalho/areas/**"}, &report)

	if !errors.Is(err, ingest.ErrPruneSubset) {
		t.Fatalf("pruneOrphans on a filtered run = %v, want ErrPruneSubset", err)
	}
	if len(store.calls) != 0 {
		t.Errorf("pruneOrphans called %v on a run that visited part of the corpus", store.calls)
	}
}

// TestRunOutcome_InterruptedBeatsFailed pins the order that decides the exit code.
//
// An interrupted run has almost always failed a note — the one it cut short — so a check that asked
// "did anything fail" first would give every Ctrl-C the generic failure code, and a scheduler would
// page somebody for it.
func TestRunOutcome_InterruptedBeatsFailed(t *testing.T) {
	failed := ingest.Report{Results: []ingest.NoteResult{{State: ingest.StateFailed, Err: errors.New("cut short")}}}
	interruptedAndFailed := failed
	interruptedAndFailed.Interrupted = true

	if err := runOutcome(interruptedAndFailed, nil); !errors.Is(err, errInterrupted) {
		t.Errorf("an interrupted run with a failed note = %v, want errInterrupted", err)
	}
	if err := runOutcome(failed, nil); errors.Is(err, errInterrupted) {
		t.Errorf("a run that merely failed = %v, want the generic failure", err)
	}
	// A prune that broke names a specific thing, and outranks both.
	pruneErr := errors.New("qdrant refused the delete")
	if err := runOutcome(interruptedAndFailed, pruneErr); !errors.Is(err, pruneErr) {
		t.Errorf("runOutcome with a prune failure = %v, want it reported", err)
	}
	if err := runOutcome(ingest.Report{}, nil); err != nil {
		t.Errorf("a clean run = %v, want nil", err)
	}
}

// TestSplitStillOnDisk_PathClaimedByALiveNote_IsPruned is D-34: the file existing is not an answer
// to whether *this uid* still has a note.
//
// Editing a note's uid in the frontmatter leaves the file exactly where it was under a new identity.
// The scan returns the new uid, so the old one has nothing claiming it and becomes a candidate —
// correctly. Then the check stats the old payload's path, finds the file that now belongs to the new
// uid, and concludes the note is still there. Nothing ever removes those points afterwards: there is
// no command that revisits them.
//
// The file is created on disk on purpose. Without the claim check this test cannot fail, because the
// stat succeeds and the candidate is preserved — which is exactly the defect.
func TestSplitStillOnDisk_PathClaimedByALiveNote_IsPruned(t *testing.T) {
	root := writeVault(t, map[string]string{
		"00-inbox/renamed.md": note("0198a7f2-4b31-7c42-9e15-3d8a92c47baa", "Renamed"),
	})
	// PathClaimed is what ScanOrphans set from the note set it also derived the live uids from
	// (internal/ingest/orphans.go); this layer reads it and never re-derives it.
	oldUID := ingest.Orphan{
		UID:   uuid.MustParse("0198a7f2-4b31-7c42-9e15-3d8a92c47b06"),
		Vault: "trabalho", Path: "00-inbox/renamed.md", Points: 3, PathClaimed: true,
	}
	report := ingest.Report{OrphansScanned: true, Orphans: []ingest.Orphan{oldUID}}

	splitStillOnDisk(map[string]string{"trabalho": root}, &report)

	if !slices.Equal(report.Orphans, []ingest.Orphan{oldUID}) {
		t.Errorf("Orphans = %v, want the old uid prunable: its path belongs to another identity now, "+
			"and nothing else in the system ever removes its points", report.Orphans)
	}
	if len(report.OnDisk) != 0 {
		t.Errorf("OnDisk = %v; the file is there but it is not this uid's file", report.OnDisk)
	}
}

// TestPruneOrphans_PathClaimedByALiveNote_IsDeleted asserts D-34 where it deletes rather than where
// it classifies, against the store's call log.
func TestPruneOrphans_PathClaimedByALiveNote_IsDeleted(t *testing.T) {
	root := writeVault(t, map[string]string{
		"00-inbox/renamed.md": note("0198a7f2-4b31-7c42-9e15-3d8a92c47baa", "Renamed"),
	})
	store := &recordingStore{}
	report := ingest.Report{
		OrphansScanned: true,
		Orphans: []ingest.Orphan{{
			UID:   uuid.MustParse("0198a7f2-4b31-7c42-9e15-3d8a92c47b06"),
			Vault: "trabalho", Path: "00-inbox/renamed.md", Points: 3, PathClaimed: true,
		}},
	}

	err := pruneOrphans(t.Context(), store, defaultTenantID,
		map[string]string{"trabalho": root}, ingestOptions{}, &report)
	if err != nil {
		t.Fatalf("pruneOrphans: %v", err)
	}

	if !slices.Equal(store.calls, []string{"delete"}) {
		t.Errorf("pruneOrphans issued %v, want [delete]: the uid was replaced in the frontmatter and "+
			"its points have no note left", store.calls)
	}
	if report.PointsPruned != 3 {
		t.Errorf("PointsPruned = %d, want 3", report.PointsPruned)
	}
}

// TestPruneOrphans_InterruptedRun_DeletesNothing is the destructive half of Ctrl-C.
//
// The per-note prune inside ProcessNote is already safe — it is unreachable without a confirmed
// upsert. This is the other destructive path, one layer up: the orphan prune runs after the batch
// returns, from a snapshot taken before a reindex that then did not finish. An operator who hit
// Ctrl-C because they realised the tenant or the vault was wrong is exactly who must not have their
// orphans deleted anyway.
//
// Asserted against the store's call log: "it returned an error" is compatible with deleting first.
func TestPruneOrphans_InterruptedRun_DeletesNothing(t *testing.T) {
	store := &recordingStore{}
	report := ingest.Report{
		OrphansScanned: true,
		Interrupted:    true,
		Orphans: []ingest.Orphan{{
			UID:   uuid.MustParse("0198a7f2-4b31-7c42-9e15-3d8a92c47b06"),
			Vault: "trabalho", Path: "00-inbox/gone.md", Points: 3,
		}},
	}

	err := pruneOrphans(t.Context(), store, defaultTenantID,
		map[string]string{"trabalho": t.TempDir()}, ingestOptions{}, &report)

	// Not an error: the operator asked the run to stop, and stopping includes this step. The exit
	// code comes from Interrupted (runOutcome), not from pretending the prune broke.
	if err != nil {
		t.Errorf("pruneOrphans on an interrupted run = %v, want nil", err)
	}
	if len(store.calls) != 0 {
		t.Errorf("pruneOrphans called %v after the operator asked the run to stop", store.calls)
	}
	if report.PointsPruned != 0 {
		t.Errorf("PointsPruned = %d on a run that was interrupted", report.PointsPruned)
	}
	// A run told to delete that did not has to say so; silence reads as "nothing to delete".
	if !strings.Contains(report.String(), "prune was skipped") {
		t.Errorf("report %q does not say the prune was skipped", report.String())
	}
}
