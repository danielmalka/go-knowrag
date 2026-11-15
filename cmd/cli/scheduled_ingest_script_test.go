package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The scheduled ingest is a shell wrapper, and what it must do is easy to get subtly wrong and hard to
// see: it runs unattended, so it has to prune without a prompt, report how much it pruned in the log, and
// never turn a failure into a success. These tests run the real script with a fake `knowrag` that
// records its arguments, so they need no Qdrant, no GPU and no vault.

const scheduledIngestScript = "../../scripts/embedder-service/scheduled-ingest.sh"

type scheduledRun struct {
	out    string
	exit   int
	args   string // what the fake knowrag was called with; "" when it never ran
	called bool
}

// runScheduledIngest runs the wrapper with a fake ensure script and a fake knowrag. fakeKnowrag is the
// body of the latter; `$@` is already recorded before it runs.
func runScheduledIngest(t *testing.T, ensureBody, fakeKnowrag string) scheduledRun {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the wrapper is a POSIX shell script")
	}
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil { // #nosec G306 -- an executable test double in a temp dir
			t.Fatal(err)
		}
		return path
	}
	ensure := write("ensure", ensureBody)
	bin := write("knowrag", `echo "$@" > "`+argsFile+`"`+"\n"+fakeKnowrag)

	cmd := exec.Command("sh", scheduledIngestScript) // #nosec G204 -- a fixed script path and test doubles built just above
	cmd.Env = append(os.Environ(), "KNOWRAG_BIN="+bin, "KNOWRAG_ENSURE="+ensure)
	raw, err := cmd.CombinedOutput()
	run := scheduledRun{out: string(raw)}
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("running the wrapper: %v", err)
		}
		run.exit = exit.ExitCode()
	}
	if b, err := os.ReadFile(argsFile); err == nil { // #nosec G304 -- a file this test created
		run.args, run.called = strings.TrimSpace(string(b)), true
	}
	return run
}

func TestScheduledIngest_PrunesWithoutAPromptAndLogsWhatItPruned(t *testing.T) {
	r := runScheduledIngest(t, "exit 0",
		`echo "3 note(s): skipped=3"; echo "orphans: 2 note(s) deleted from the vault, 7 point(s), all removed"`)

	if r.exit != 0 {
		t.Fatalf("exit = %d, want 0:\n%s", r.exit, r.out)
	}
	if r.args != "ingest --vault both --prune --yes" {
		t.Errorf("knowrag was called with %q, want the scheduled ingest to prune and authorize it with --yes", r.args)
	}
	want := "scheduled-ingest: exit=0; orphans: 2 note(s) deleted from the vault, 7 point(s), all removed"
	if !strings.Contains(r.out, want) {
		t.Errorf("the log has no line carrying the pruned count; want %q in:\n%s", want, r.out)
	}
	if !strings.Contains(r.out, "3 note(s): skipped=3") {
		t.Errorf("the CLI's own report was swallowed instead of passed through:\n%s", r.out)
	}
}

func TestScheduledIngest_NoOrphansIsLoggedToo(t *testing.T) {
	r := runScheduledIngest(t, "exit 0", `echo "orphans: none"`)
	if !strings.Contains(r.out, "scheduled-ingest: exit=0; orphans: none") {
		t.Errorf("a run with nothing to prune says nothing about it:\n%s", r.out)
	}
}

// A report without the line (an older CLI, a crash before the report) must not look like "none".
func TestScheduledIngest_MissingOrphansLineIsSaidOutLoud(t *testing.T) {
	r := runScheduledIngest(t, "exit 0", `echo "something else"`)
	if !strings.Contains(r.out, "no orphans line in the report") || strings.Contains(r.out, "orphans: none") {
		t.Errorf("the absence of the orphans line was not flagged:\n%s", r.out)
	}
}

// A refusal from the CLI (an empty vault, an unreadable index) is a failure, and the wrapper must carry
// the exit status through: a timer reads the exit code, not the prose.
func TestScheduledIngest_PassesTheFailureStatusThrough(t *testing.T) {
	r := runScheduledIngest(t, "exit 0",
		`echo "refusing to prune: the scan of vault \"dono\" found no notes"; exit 3`)
	if r.exit != 3 {
		t.Fatalf("exit = %d, want the CLI's 3:\n%s", r.exit, r.out)
	}
	if !strings.Contains(r.out, "scheduled-ingest: exit=3") || !strings.Contains(r.out, "refusing to prune") {
		t.Errorf("the failure and its reason are not in the log:\n%s", r.out)
	}
}

// If the embedder cannot be made ready, nothing is ingested, and above all nothing is pruned.
func TestScheduledIngest_EnsureFailureNeverReachesTheIngest(t *testing.T) {
	r := runScheduledIngest(t, "echo no GPU >&2; exit 1", `echo should not run`)
	if r.exit == 0 {
		t.Fatalf("a failed ensure step exited 0:\n%s", r.out)
	}
	if r.called {
		t.Errorf("knowrag ran (%q) although the embedder was not ready", r.args)
	}
}

// The two limits on the unattended prune are the CLI's; the wrapper only lets the operator move them from
// the same env file as the rest, and passes nothing when they are unset.
func TestScheduledIngest_CeilingKnobsArePassedOnlyWhenSet(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the wrapper is a POSIX shell script")
	}
	r := runScheduledIngest(t, "exit 0", `echo "orphans: none"`)
	if strings.Contains(r.args, "--prune-max") {
		t.Errorf("an unset knob reached the CLI: %q", r.args)
	}
	t.Setenv("KNOWRAG_PRUNE_MAX_NOTES", "10")
	t.Setenv("KNOWRAG_PRUNE_MAX_PERCENT", "50")
	r = runScheduledIngest(t, "exit 0", `echo "orphans: none"`)
	if want := "ingest --vault both --prune --yes --prune-max-notes 10 --prune-max-percent 50"; r.args != want {
		t.Errorf("args = %q, want %q", r.args, want)
	}
}
