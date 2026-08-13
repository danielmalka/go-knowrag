package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The reports the fake CLI hands back, in internal/ingest/report.go's exact shape — orphanLines
// writes the summary line and one indented line per orphan, and scripts/prune-drill.sh reads both.
// A change to that renderer is meant to break these fixtures: the script parses what the operator
// is shown, so the two cannot be allowed to drift apart silently.
const (
	oneOrphanReport = "730 note(s): skipped=729\n" +
		"orphans: 1 note(s) deleted from the vault, 6 point(s) still indexed; run --prune --yes to remove\n" +
		"  - pessoal/areas/nota.md (6 point(s), uid 7c9e6679-7425-40de-944b-e07fc1f90ae7)"
	prunedReport = "729 note(s): skipped=729\n" +
		"orphans: 1 note(s) deleted from the vault, 6 point(s), all removed\n" +
		"  - pessoal/areas/nota.md (6 point(s), uid 7c9e6679-7425-40de-944b-e07fc1f90ae7)"
)

// pruneFixture lays out a one-note vault and returns the environment the drill needs to find it.
// The vault root goes under the run's temp directory, so the note this drill moves aside is a
// fixture and never anything the owner wrote.
func pruneFixture(t *testing.T, env map[string]string) (map[string]string, func() string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "areas"), 0o750); err != nil {
		t.Fatalf("creating the fixture vault: %v", err)
	}
	note := filepath.Join(root, "areas", "nota.md")
	if err := os.WriteFile(note, []byte("---\ntitle: nota\n---\n\ncorpo\n"), 0o600); err != nil {
		t.Fatalf("writing the fixture note: %v", err)
	}

	full := map[string]string{
		"KNOWRAG_VAULT_PESSOAL_PATH": root,
		"FAKE_STATS_BEFORE":          "interno      points: 4210     uids: 730",
		"FAKE_STATS_AFTER":           "interno      points: 4204     uids: 729",
		"FAKE_INGEST_REPORT":         oneOrphanReport,
		"FAKE_PRUNE_REPORT":          prunedReport,
	}
	for k, v := range env {
		full[k] = v
	}
	return full, func() string {
		if _, err := os.Stat(note); err == nil {
			return "present"
		}
		return "MISSING"
	}
}

// runPruneDrill drives scripts/prune-drill.sh over the fixture vault. A run that reaches the final
// phase needs FAKE_STATS_SEQUENCE=1 in its environment, which is what makes the fake's third count
// answer the restored numbers rather than the post-prune ones (see fakeKnowrag, drill_test.go).
func runPruneDrill(t *testing.T, args []string, env map[string]string, tty bool) drillRun {
	t.Helper()
	return runDrill(t, pruneDrillScript, args, env, tty)
}

// TestPruneDrill_RefusesWithoutAuthorization. The gate sits in front of the first thing that
// changes anything at all, so a refusal has to leave the note where it was — not merely skip the
// prune. Neither case may prompt: nothing here answers input, so a script that asked would hang.
func TestPruneDrill_RefusesWithoutAuthorization(t *testing.T) {
	t.Run("no --yes", func(t *testing.T) {
		env, noteState := pruneFixture(t, nil)
		run := runPruneDrill(t, []string{"pessoal", "areas/nota.md"}, env, true)
		if run.code == 0 {
			t.Fatalf("the drill pruned without --yes; output:\n%s", run.output)
		}
		if noteState() != "present" {
			t.Errorf("the note was moved out of the vault by a run that was refused")
		}
		if strings.Contains(run.cliLog, "--prune") {
			t.Errorf("a refused run still called --prune; cli log:\n%s", run.cliLog)
		}
	})

	t.Run("--yes but nobody is watching", func(t *testing.T) {
		env, noteState := pruneFixture(t, nil)
		run := runPruneDrill(t, []string{"--yes", "pessoal", "areas/nota.md"}, env, false)
		if run.code == 0 {
			t.Fatalf("the drill pruned from a non-terminal stdin; output:\n%s", run.output)
		}
		if noteState() != "present" {
			t.Errorf("the note was moved out of the vault by a run that was refused")
		}
		if strings.Contains(run.cliLog, "--prune") {
			t.Errorf("a refused run still called --prune; cli log:\n%s", run.cliLog)
		}
	})
}

// TestPruneDrill_RefusesAReportItDidNotSetUp is the guard against pruning a state this drill did
// not create. Every case here has the note already moved aside, so the refusal happens with the
// vault modified — and the note still has to come back.
func TestPruneDrill_RefusesAReportItDidNotSetUp(t *testing.T) {
	for _, tc := range []struct {
		name, report, want string
	}{
		{"the note is not among the orphans",
			"730 note(s): skipped=730\norphans: none",
			"did not report pessoal/areas/nota.md as an orphan"},
		// The dangerous one: our note IS there, and so is another. Pruning on that state deletes a
		// note this drill never chose, and the count assertions afterwards would not separate the two.
		{"another note went missing too",
			"730 note(s): skipped=728\n" +
				"orphans: 2 note(s) deleted from the vault, 9 point(s) still indexed; run --prune --yes to remove\n" +
				"  - pessoal/areas/nota.md (6 point(s), uid 7c9e6679-7425-40de-944b-e07fc1f90ae7)\n" +
				"  - trabalho/areas/outra.md (3 point(s), uid 0d8f5a20-1c3b-4d1e-8f6a-2b9c4e7d1a55)",
			"reported orphans other than pessoal/areas/nota.md"},
		{"the index snapshot could not be read",
			"730 note(s): skipped=730\norphans: not scanned — the index snapshot could not be read, " +
				"so this run cannot say whether any note was deleted",
			"did not report pessoal/areas/nota.md as an orphan"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, noteState := pruneFixture(t, map[string]string{"FAKE_INGEST_REPORT": tc.report})
			run := runPruneDrill(t, []string{"--yes", "pessoal", "areas/nota.md"}, env, true)
			if run.code == 0 {
				t.Fatalf("the drill went ahead over a report it should have refused; output:\n%s", run.output)
			}
			if strings.Contains(run.cliLog, "--prune") {
				t.Fatalf("IT PRUNED ANYWAY; cli log:\n%s", run.cliLog)
			}
			if !strings.Contains(run.output, tc.want) {
				t.Errorf("output did not name the refusal %q; got:\n%s", tc.want, run.output)
			}
			// The trap, which is the only thing standing between a failed drill and a note that
			// silently left the vault.
			if noteState() != "present" {
				t.Errorf("the note was not put back after the drill aborted")
			}
		})
	}
}

// TestPruneDrill_CountsMustMoveByExactlyTheOrphan is the assertion that proves nothing else was
// deleted. The orphan holds 6 points and 1 uid; a total that fell by any other amount means the
// prune reached beyond the note this drill removed — which is the failure the whole procedure
// exists to detect, and the one a per-vault count would be needed for if this pair did not settle it.
func TestPruneDrill_CountsMustMoveByExactlyTheOrphan(t *testing.T) {
	for _, tc := range []struct {
		name, after, want string
		pass              bool
	}{
		{"exactly the orphan", "interno      points: 4204     uids: 729",
			"exactly 6 point(s) and 1 uid left the index", true},
		{"another vault's points went too", "interno      points: 4190     uids: 729",
			"should have cost exactly 6", false},
		{"another note's uid went too", "interno      points: 4204     uids: 728",
			"should have cost exactly one", false},
		{"nothing actually left the index", "interno      points: 4210     uids: 730",
			"should have cost exactly 6", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, noteState := pruneFixture(t, map[string]string{
				"FAKE_STATS_AFTER":    tc.after,
				"FAKE_STATS_SEQUENCE": "1",
			})
			run := runPruneDrill(t, []string{"--yes", "pessoal", "areas/nota.md"}, env, true)
			if tc.pass != (run.code == 0) {
				t.Fatalf("exit code %d, want pass=%v; output:\n%s", run.code, tc.pass, run.output)
			}
			if !strings.Contains(run.output, tc.want) {
				t.Errorf("output did not contain %q; got:\n%s", tc.want, run.output)
			}
			if noteState() != "present" {
				t.Errorf("the note was not put back")
			}
		})
	}
}

// TestPruneDrill_FullCycle drives the whole procedure, including the restore and the ingestion that
// puts the points back. It needs a third answer from `stats` — before, after the prune, after the
// restore — which the shared fake does not do, so this one test brings its own.
func TestPruneDrill_FullCycle(t *testing.T) {
	env, noteState := pruneFixture(t, map[string]string{
		// A three-answer stats: 4210/730, then 4204/729, then 4210/730 again.
		"FAKE_STATS_SEQUENCE": "1",
	})
	run := runPruneDrill(t, []string{"--yes", "pessoal", "areas/nota.md"}, env, true)
	if run.code != 0 {
		t.Fatalf("the full cycle failed; output:\n%s", run.output)
	}
	if !strings.Contains(run.output, "PRUNE CONFIRMED") {
		t.Errorf("the drill did not confirm the prune; output:\n%s", run.output)
	}
	if noteState() != "present" {
		t.Errorf("the note was not put back at the end of a successful drill")
	}
	// The order is the procedure: report first, prune second. A run that pruned before looking has
	// no evidence that the orphan it deleted was the note it removed.
	firstPrune := strings.Index(run.cliLog, "--prune")
	firstIngest := strings.Index(run.cliLog, "ingest --tenant")
	if firstIngest < 0 || firstPrune < 0 || firstIngest > firstPrune {
		t.Errorf("the prune did not come after a plain ingest; cli log:\n%s", run.cliLog)
	}
}

// TestPruneDrill_FailsWhenTheIndexDoesNotComeBack. The last phase is not bookkeeping: it is what
// turns "the note is back on disk" into "the note is back in the index". Here the restoring
// ingestion leaves the index at the post-prune counts, and a drill that walked away from that would
// hand the owner a search missing a note it deleted on purpose.
//
// Its environment omits FAKE_STATS_SEQUENCE, which is exactly what makes the third count answer the
// post-prune numbers instead of the original ones (fakeKnowrag, drill_test.go).
func TestPruneDrill_FailsWhenTheIndexDoesNotComeBack(t *testing.T) {
	env, noteState := pruneFixture(t, nil)
	run := runPruneDrill(t, []string{"--yes", "pessoal", "areas/nota.md"}, env, true)
	if run.code == 0 {
		t.Fatalf("the drill passed with the restored note still absent from the index; output:\n%s", run.output)
	}
	if !strings.Contains(run.output, "did not come back to where it started") {
		t.Errorf("output did not name the failure; got:\n%s", run.output)
	}
	if noteState() != "present" {
		t.Errorf("the note is not on disk either")
	}
}

// TestPruneDrill_SaysWhatItDoesNotProve, on a successful run, for the reason
// TestRecoveryDrill_SaysWhatItDoesNotMeasure gives.
func TestPruneDrill_SaysWhatItDoesNotProve(t *testing.T) {
	env, _ := pruneFixture(t, map[string]string{"FAKE_STATS_SEQUENCE": "1"})
	run := runPruneDrill(t, []string{"--yes", "pessoal", "areas/nota.md"}, env, true)
	if run.code != 0 {
		t.Fatalf("the drill failed; output:\n%s", run.output)
	}
	for _, subject := range []string{
		"that a note deleted for real behaves like one moved aside",
		"a prune that fails partway through",
		"anything about a vault this run did not scan",
	} {
		if !strings.Contains(run.output, subject) {
			t.Errorf("a successful run did not say it does not prove %q; got:\n%s", subject, run.output)
		}
	}
}

// TestPruneDrill_RefusesAVaultItCannotLocate. The vault root comes from the same variable the CLI
// reads (internal/config/config.go's KNOWRAG_VAULT_<NAME>_PATH); without it the script has no idea
// which file to move, and guessing is not among its options.
func TestPruneDrill_RefusesAVaultItCannotLocate(t *testing.T) {
	env, _ := pruneFixture(t, map[string]string{"KNOWRAG_VAULT_PESSOAL_PATH": ""})
	run := runPruneDrill(t, []string{"--yes", "pessoal", "areas/nota.md"}, env, true)
	if run.code == 0 {
		t.Fatalf("the drill ran against a vault it could not locate; output:\n%s", run.output)
	}
	if !strings.Contains(run.output, "KNOWRAG_VAULT_PESSOAL_PATH is unset") {
		t.Errorf("output did not name the missing variable; got:\n%s", run.output)
	}

	env2, noteState := pruneFixture(t, nil)
	run2 := runPruneDrill(t, []string{"--yes", "pessoal", "areas/ausente.md"}, env2, true)
	if run2.code == 0 {
		t.Fatalf("the drill ran against a note that is not there; output:\n%s", run2.output)
	}
	// The message, not only the exit code. Dropping the `-f` guard still fails the run — the `mv`
	// that follows has nothing to move — but it fails one phase later, after the counts were taken
	// and with a message about moving a file rather than about the argument being wrong. A plant
	// that removed the guard left an exit-code-only assertion green.
	if !strings.Contains(run2.output, "areas/ausente.md is not a file") {
		t.Errorf("output did not refuse the path as an argument; got:\n%s", run2.output)
	}
	if noteState() != "present" {
		t.Errorf("a run over a missing path disturbed the note that is there")
	}
}
