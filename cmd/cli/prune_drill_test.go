package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"syscall"
	"testing"
	"time"

	"github.com/danielmalka/go-knowrag/internal/ingest"
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
		// The window the exit status cannot close: a run that was interrupted after printing a report
		// that otherwise passes every guard here — our note, alone, with its point count. The only
		// thing separating it from a legitimate report is the phrase internal/ingest/report.go's
		// String appends when Report.Interrupted is set, so that phrase is what gets read.
		{"the ingestion was interrupted but printed a complete-looking report",
			"730 note(s): skipped=729 — interrupted, the remaining notes were not started; " +
				"the next run converges\n" +
				"orphans: 1 note(s) deleted from the vault, 6 point(s) still indexed; run --prune --yes to remove\n" +
				"  - pessoal/areas/nota.md (6 point(s), uid 7c9e6679-7425-40de-944b-e07fc1f90ae7)",
			"describes a scan that never finished"},
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

// TestPruneDrill_RestoresTheNoteOnASignal delivers a REAL signal to the running drill, to the whole
// process group, which is what a terminal Ctrl-C does. Every other failure path in this file is
// simulated by an environment variable that forces an exit code; this one is the only place the
// restore is proven against the event it was written for.
//
// The signal lands while the note is out of the vault AND the ingestion is already running: the
// fake ingest creates FAKE_INGEST_STARTED immediately before it sleeps (fakeKnowrag,
// drill_fakes_test.go), and this test waits for that file rather than for the note's absence.
//
// WHY IT WAITS FOR THAT AND NOT FOR THE STASH, which is what it did until 2026-08-13 and what made
// it fail about once in every thirteen runs. The note disappears the moment `mv` renames it, and at
// that moment the drill's process group holds nothing but the drill's own shell — the ingestion has
// not been forked yet. A group signal delivered there kills nobody, so no child ever returns early,
// and bash cannot run a trap until the command it is waiting on comes back. Measured: those runs
// stop at 30.02 s, the exact length of the fake's sleep, with `exit status 1` from the drill's own
// handler and no `--prune` in the CLI log. The script is doing the only thing a shell can do; the
// test was signalling a process group whose membership was still being built.
//
// So the two windows the old test hit by luck are covered on purpose instead, and deterministically:
// TestPruneDrill_ASignalDuringAnOrdinaryCommandStopsTheDrill for a signal taken during a foreground
// command that succeeds, TestPruneDrill_ASignalBetweenCommandsStopsTheDrill for one pending at a
// command boundary. What is left here is the case neither can fake — a real signal to a real process
// group with a real child dying of it — and once FAKE_INGEST_STARTED exists there is always such a
// child, so the drill stops in milliseconds or something is wrong.
//
// It is launched with Setpgid because os/exec would otherwise put the script in the test binary's
// own process group, and signalling that group kills the test. That is also why this is a Go test
// and not a shell one: a script backgrounded from a shell with `&` enters with SIGINT set to
// SIG_IGN, and bash cannot trap a signal inherited as ignored — an experiment run that way reports
// an INT trap that never fires and a script that sails past its dead child, both artifacts of the
// launcher rather than facts about this drill.
func TestPruneDrill_RestoresTheNoteOnASignal(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			if err := os.MkdirAll(bin, 0o750); err != nil {
				t.Fatalf("creating the fake bin dir: %v", err)
			}
			writeFake(t, bin, "ssh", fakeSSH)
			writeFake(t, bin, "knowrag", fakeKnowrag)

			vaultRoot := filepath.Join(dir, "vault")
			if err := os.MkdirAll(filepath.Join(vaultRoot, "areas"), 0o750); err != nil {
				t.Fatalf("creating the fixture vault: %v", err)
			}
			note := filepath.Join(vaultRoot, "areas", "nota.md")
			if err := os.WriteFile(note, []byte("---\ntitle: nota\n---\n\ncorpo\n"), 0o600); err != nil {
				t.Fatalf("writing the fixture note: %v", err)
			}

			state := filepath.Join(dir, "state")
			env := map[string]string{
				"PATH":                       bin + ":" + os.Getenv("PATH"),
				"HOME":                       dir,
				"FAKE_SSH_LOG":               filepath.Join(dir, "ssh.log"),
				"FAKE_CLI_LOG":               filepath.Join(dir, "cli.log"),
				"FAKE_STATS_COUNT":           filepath.Join(dir, "stats.count"),
				"FAKE_PS_COUNT":              filepath.Join(dir, "ps.count"),
				"FAKE_STATS_BEFORE":          "interno      points: 4210     uids: 730",
				"FAKE_STATS_AFTER":           "interno      points: 4204     uids: 729",
				"FAKE_INGEST_REPORT":         oneOrphanReport,
				"FAKE_PRUNE_REPORT":          prunedReport,
				"FAKE_INGEST_SLEEP":          "30",
				"FAKE_INGEST_STARTED":        filepath.Join(dir, "ingest.started"),
				"KNOWRAG_VAULT_PESSOAL_PATH": vaultRoot,
				"KNOWRAG_DRILL_STATE_DIR":    state,
			}
			envv := make([]string, 0, len(env))
			for k, v := range env {
				envv = append(envv, k+"="+v)
			}

			abs, err2 := filepath.Abs(pruneDrillScript)
			if err2 != nil {
				t.Fatalf("resolving %s: %v", pruneDrillScript, err2)
			}
			readScriptForCacheKey(t, abs)

			cmd := exec.Command("bash", abs, "--yes", "pessoal", "areas/nota.md") // #nosec G204 -- fixed script, fixture arguments
			cmd.Env = envv
			cmd.Dir = dir
			cmd.Stdin = openPTY(t)
			// An *os.File, deliberately, and not a strings.Builder. Given anything that is not a File,
			// os/exec builds an os.Pipe and a goroutine copying out of it, and Wait blocks until that
			// copy ends — which is when every holder of the write end has closed it. The fake's `sleep`
			// grandchild inherits that end, so a Wait here would be waiting on the sleep rather than on
			// the script, and this test hung for 90 s under parallel load before the file replaced the
			// builder. A file has no such reader.
			outPath := filepath.Join(dir, "drill.out")
			outFile, err := os.Create(outPath) // #nosec G304 -- a path under t.TempDir()
			if err != nil {
				t.Fatalf("creating the output file: %v", err)
			}
			defer func() { _ = outFile.Close() }()
			cmd.Stdout = outFile
			cmd.Stderr = outFile
			out := func() string { return readIfPresent(outPath) }
			// Its own process group, so the signal below reaches the script and its children the way
			// a terminal reaches a foreground job — and reaches nothing of this test's.
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := cmd.Start(); err != nil {
				t.Fatalf("starting the drill: %v", err)
			}

			// Signal only once the ingestion is running, for the reason in this test's header: before
			// that the process group is still being assembled, and a signal to it kills nobody.
			// Sleeping a fixed time here would make this a test of whatever phase happened to be
			// running.
			deadline := time.Now().Add(20 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(dir, "ingest.started")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
					t.Fatalf("the drill never reached the ingestion; output:\n%s", out())
				}
				time.Sleep(20 * time.Millisecond)
			}
			// And the note really is out of the vault at that point. The wait above is on a later
			// event than the move, so this cannot be false today — it is here so that reordering the
			// phases fails loudly instead of quietly turning this into a test of the wrong window.
			if _, err := os.Stat(note); err == nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				t.Fatalf("the ingestion started with the note still in the vault, so this would be a "+
					"test of the phases before the move; output:\n%s", out())
			}

			if err := syscall.Kill(-cmd.Process.Pid, sig); err != nil {
				t.Fatalf("signalling the process group: %v", err)
			}

			// Bounded, because the interesting failure is "the signal did not stop it" and an unbounded
			// Wait reports that as a suite that sits there for the length of the fake's sleep. A working
			// signal returns here in milliseconds.
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			var waitErr error
			select {
			case waitErr = <-done:
			case <-time.After(20 * time.Second):
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				<-done
				t.Fatalf("the drill was still running 20s after %v; output:\n%s", sig, out())
			}

			if _, err := os.Stat(note); err != nil {
				t.Fatalf("THE NOTE DID NOT COME BACK after %v: %v\noutput:\n%s", sig, err, out())
			}
			if waitErr == nil {
				t.Errorf("the drill exited 0 after being interrupted; output:\n%s", out())
			}
			// The rule internal/ingest/prune.go enforces one layer down, restated here: a run that was
			// stopped before it finished looking must never reach the deletion.
			if strings.Contains(readIfPresent(filepath.Join(dir, "cli.log")), "--prune") {
				t.Errorf("an interrupted drill went on to prune; cli log:\n%s",
					readIfPresent(filepath.Join(dir, "cli.log")))
			}
		})
	}
}

// TestPruneDrill_ASignalBetweenCommandsStopsTheDrill is the deterministic half of the signal
// coverage, and it is the case that was actually broken.
//
// A trap handler is not a stop. Bash runs a pending trap at the next command boundary and then
// RESUMES the script, so `trap restore INT TERM` — the handler with no exit — put the note back and
// carried on into the next phase with the drill still running. The handlers exit for that reason.
//
// The window is hit deterministically here rather than by wall clock: the fake signals the process
// group leader and returns 0, so bash is holding a pending signal at a command boundary with a
// perfectly successful child. TestPruneDrill_RestoresTheNoteOnASignal covers the other window — a
// signal landing while the child runs — and it catches this defect only about one run in twelve,
// measured, which is why both exist.
func TestPruneDrill_ASignalBetweenCommandsStopsTheDrill(t *testing.T) {
	for name, sig := range map[string]string{"TERM": "TERM", "INT": "INT"} {
		t.Run(name, func(t *testing.T) {
			env, noteState := pruneFixture(t, map[string]string{
				"FAKE_SIGNAL_LEADER":  sig,
				"FAKE_STATS_SEQUENCE": "1",
			})
			run := runPruneDrill(t, []string{"--yes", "pessoal", "areas/nota.md"}, env, true)

			if strings.Contains(run.output, "FAKE COULD NOT SIGNAL THE LEADER") {
				t.Fatalf("the fake never delivered the signal, so this test proved nothing:\n%s", run.output)
			}
			if run.code == 0 {
				t.Fatalf("the drill ran to completion through a %s; output:\n%s", sig, run.output)
			}
			if strings.Contains(run.cliLog, "--prune") {
				t.Errorf("a signalled drill went on to prune; cli log:\n%s", run.cliLog)
			}
			if noteState() != "present" {
				t.Errorf("the note did not come back")
			}
		})
	}
}

// TestPruneDrill_ASignalDuringAnOrdinaryCommandStopsTheDrill is the deterministic cover for the
// window that made TestPruneDrill_RestoresTheNoteOnASignal fail about once in every thirteen runs,
// and it is the case that was actually broken.
//
// A BARE SIGINT DOES NOT STOP A BASH SCRIPT. While bash waits on an ordinary foreground command it
// only records an arriving SIGINT, and re-raises it afterwards ONLY IF that command was itself
// killed by SIGINT; a command that exits 0 takes the interrupt with it and the script walks on. The
// rule is bash's own (jobs.c, wait_sigint_handler), and scripts/prune-drill.sh is nothing but such
// commands. Measured on 2026-08-13: a Ctrl-C landing on the drill's `mv` — which is exactly where
// the sibling test aims, since it signals the instant the note leaves the vault — was swallowed,
// and the run went on to call `knowrag ingest --tenant interno --prune --yes` with the operator's
// interrupt behind it. That is the deletion this whole procedure exists to keep behind a decision.
// `trap interrupted INT TERM` is what closes it.
//
// The window is reached here without a clock: the fake signals the drill's shell from inside the
// FIRST `knowrag stats` and then exits 0, which is that shape exactly. That count runs before
// anything has moved, so a drill that honoured the signal never touched the vault at all.
//
// SIGTERM shares the loop although bash does NOT discard it — measured the same day, it acts on a
// SIGTERM at once. It is here to notice the two signals drifting apart, and it is what fails if the
// trap is ever narrowed to INT alone.
func TestPruneDrill_ASignalDuringAnOrdinaryCommandStopsTheDrill(t *testing.T) {
	for _, sig := range []string{"INT", "TERM"} {
		t.Run(sig, func(t *testing.T) {
			env, noteState := pruneFixture(t, map[string]string{"FAKE_SIGNAL_LEADER_STATS": sig})
			run := runPruneDrill(t, []string{"--yes", "pessoal", "areas/nota.md"}, env, true)

			if strings.Contains(run.output, "FAKE COULD NOT SIGNAL THE LEADER") {
				t.Fatalf("the fake never delivered the signal, so this test proved nothing:\n%s", run.output)
			}
			if run.code == 0 {
				t.Fatalf("the drill ran to completion through a %s; output:\n%s", sig, run.output)
			}
			if strings.Contains(run.cliLog, "--prune") {
				t.Errorf("a signalled drill went on to prune; cli log:\n%s", run.cliLog)
			}
			// Stronger than "it did not prune": the signal landed before the vault was touched, so a
			// drill that honoured it never reached an ingestion either. Without this line a run that
			// swallowed the signal and merely failed later — on the restored counts, say — would still
			// satisfy every other assertion here.
			if strings.Contains(run.cliLog, "ingest") {
				t.Errorf("the drill went past the count it was signalled during; cli log:\n%s", run.cliLog)
			}
			if noteState() != "present" {
				t.Errorf("the note left the vault on a run signalled before the move")
			}
			if !strings.Contains(run.output, "interrupted by a signal") {
				t.Errorf("the drill did not say it was interrupted; output:\n%s", run.output)
			}
		})
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

// TestPruneDrill_InterruptedMarkerMatchesTheReportItReads closes the gap that the fixture above
// cannot: it feeds a string this file wrote, so a change to the report's wording leaves it green
// while prune-drill.sh's grep quietly stops matching. The guard would be dead and the round of
// plants would find nothing, because the plant would be in a third file neither side reads.
//
// Same shape as TestCIWorkflow_PendingSentinelMatchesTheErrorItLooksFor: nothing at run time can
// compare a shell literal with a Go value, so a test reads the script and asks the real producer.
func TestPruneDrill_InterruptedMarkerMatchesTheReportItReads(t *testing.T) {
	script, err := os.ReadFile(pruneDrillScript)
	if err != nil {
		t.Fatalf("reading %s: %v", pruneDrillScript, err)
	}
	const marker = "interrupted, the remaining notes were not started"
	if !strings.Contains(string(script), "'"+marker+"'") {
		t.Fatalf("%s no longer greps for %q — either it stopped checking, or this test is out of date",
			pruneDrillScript, marker)
	}

	// The real producer, not a fixture: whatever ingest prints for an interrupted run has to contain
	// what the script looks for.
	produced := ingest.Report{Interrupted: true}.String()
	if !strings.Contains(produced, marker) {
		t.Errorf("ingest.Report{Interrupted: true}.String() is %q,\nwhich does not contain the marker "+
			"%s greps for: %q\n\nthe drill would accept an interrupted run as a complete one",
			produced, pruneDrillScript, marker)
	}
}
