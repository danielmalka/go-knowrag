package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// The two scripts under test, relative to this package — the same way verifyDeployScript points at
// scripts/verify-deploy.sh.
const (
	recoveryDrillScript = "../../scripts/recovery-drill.sh"
	pruneDrillScript    = "../../scripts/prune-drill.sh"
)

// fakeSSH stands in for the ssh that reaches the machine running Qdrant. It records every remote
// command it is handed — that log, and not the script's own output, is what the tests assert on:
// what the script *printed* it would do is not evidence of what it did.
//
// It answers the three reads scripts/recovery-drill.sh performs (the container id, the volume name,
// the space) from environment variables, so each test decides what the machine looks like.
const fakeSSH = `#!/usr/bin/env bash
printf '%s\n' "$2" >>"$FAKE_SSH_LOG"
if [ "${FAKE_SSH_FAIL:-0}" = 1 ]; then exit 255; fi
case "$2" in
  *"compose ps -q"*)
    # Counted, because the drill discovers twice — once in the preflight and once inside the
    # destruction — and FAKE_CID_SECOND is how a test makes the container disappear between them.
    c=$(cat "$FAKE_PS_COUNT" 2>/dev/null || printf 0)
    c=$((c + 1)); printf '%s' "$c" >"$FAKE_PS_COUNT"
    if [ "$c" -ge 2 ] && [ -n "${FAKE_CID_SECOND+set}" ]; then printf '%s\n' "$FAKE_CID_SECOND"
    else printf '%s\n' "${FAKE_CID-cid-0001}"; fi ;;
  *"docker inspect"*) printf '%s\n' "${FAKE_VOLUME-qdrant_storage}" ;;
  # ${VAR-default}, never ${VAR:-default}: a test that sets FAKE_DU_KB="" is saying the machine
  # answered with nothing, and the colon form would hand it the default instead — which is how the
  # first version of this fake reported a passing disk check over a size that was never read.
  *"du -sk"*)         printf '%s\t/qdrant/storage\n' "${FAKE_DU_KB-1024}" ;;
  *"df -Pk"*)
    printf 'Filesystem 1024-blocks Used Available Capacity Mounted on\n'
    printf '/dev/vda1 200000 1024 %s 1%% /\n' "${FAKE_DF_KB-100000}" ;;
  *"volume rm"*)      if [ "${FAKE_VOLUME_RM_FAIL:-0}" = 1 ]; then exit 1; fi ;;
  *"compose up -d"*)  if [ "${FAKE_UP_FAIL:-0}" = 1 ]; then exit 1; fi ;;
esac
exit 0
`

// fakeKnowrag stands in for the CLI. `stats` answers FAKE_STATS_BEFORE the first time and
// FAKE_STATS_AFTER every time after that, which is what lets a test say "the index came back
// smaller" without anything real being destroyed.
//
// FAKE_STATS_SEQUENCE=1 adds a third answer, back at FAKE_STATS_BEFORE: scripts/prune-drill.sh
// counts three times — before, after the prune, after the note is restored — and its full cycle
// only closes if the third read is back where the first was.
const fakeKnowrag = `#!/usr/bin/env bash
printf '%s\n' "$*" >>"$FAKE_CLI_LOG"
case "$1" in
  stats)
    n=$(cat "$FAKE_STATS_COUNT" 2>/dev/null || printf 0)
    n=$((n + 1))
    printf '%s' "$n" >"$FAKE_STATS_COUNT"
    if [ "$n" = 1 ]; then printf '%s\n' "$FAKE_STATS_BEFORE"
    elif [ "$n" = 2 ]; then printf '%s\n' "$FAKE_STATS_AFTER"
    elif [ "${FAKE_STATS_SEQUENCE:-0}" = 1 ]; then printf '%s\n' "$FAKE_STATS_BEFORE"
    else printf '%s\n' "$FAKE_STATS_AFTER"; fi
    exit 0 ;;
  schema)
    if [ "${FAKE_SCHEMA_FAIL:-0}" = 1 ]; then exit 1; fi
    printf 'interno: ok\n'; exit 0 ;;
  ingest)
    if [ "${2:-}" = "--dry-run" ]; then
      if [ "${FAKE_DRYRUN_FAIL:-0}" = 1 ]; then printf 'embedder handshake failed\n' >&2; exit 1; fi
      printf '730 note(s): skipped=730\norphans: none\n'; exit 0
    fi
    case "$*" in
      *--prune*) printf '%s\n' "${FAKE_PRUNE_REPORT-}"; exit "${FAKE_PRUNE_CODE:-0}" ;;
    esac
    if [ "${FAKE_INGEST_FAIL:-0}" = 1 ]; then exit 1; fi
    printf '%s\n' "${FAKE_INGEST_REPORT-730 note(s): upserted=730}"; exit 0 ;;
esac
exit 0
`

// drillRun is one execution of a script against the fakes: what it exited with, everything it
// printed, and — the part the dangerous assertions read — what actually reached ssh and the CLI.
type drillRun struct {
	code   int
	output string
	sshLog string
	cliLog string
	dir    string
}

// destroyed reports whether this run reached the destructive step. It looks for the delete itself
// rather than for a message about it, because every test that matters here is asking whether the
// index survived, not whether the script said the right thing.
func (r drillRun) destroyed() bool {
	return strings.Contains(r.sshLog, "docker volume rm") || strings.Contains(r.sshLog, "compose down")
}

func writeFake(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil { // #nosec G306 -- a fake executable in t.TempDir()
		t.Fatalf("writing fake %s: %v", name, err)
	}
}

// openPTY returns a pseudo-terminal slave to hand a subprocess as stdin.
//
// It exists because `[ -t 0 ]` is a real isatty() call and there is no way to satisfy it with a
// pipe or with /dev/null — the character-device proxy cmd/cli/ingest_modes.go's isTerminal settles
// for is a Go-side approximation, and bash does not make the same one. Both files stay open until
// the child has exited: closing the master first sends the slave EIO.
//
// x/sys/unix rather than a new dependency: it is already a direct require of this module
// (internal/ingest/lock/lock.go).
func openPTY(t *testing.T) *os.File {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("opening /dev/ptmx: %v", err)
	}
	t.Cleanup(func() { _ = master.Close() })
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatalf("unlocking the pty: %v", err)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatalf("reading the pty number: %v", err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("opening the pty slave: %v", err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	return slave
}

// runDrill executes one of the two scripts with the fakes in front of ssh and knowrag.
//
// tty decides what stdin is: a real pseudo-terminal, or the closed pipe a scheduler would give it.
// Nothing here ever answers a prompt, and that is deliberate — neither script may ever wait for
// input, so a run that hung would fail this suite by timing out rather than by being answered.
func runDrill(t *testing.T, script string, args []string, env map[string]string, tty bool) drillRun {
	t.Helper()

	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o750); err != nil {
		t.Fatalf("creating the fake bin dir: %v", err)
	}
	writeFake(t, bin, "ssh", fakeSSH)
	writeFake(t, bin, "knowrag", fakeKnowrag)

	full := map[string]string{
		"PATH":                      bin + ":" + os.Getenv("PATH"),
		"HOME":                      dir,
		"FAKE_SSH_LOG":              filepath.Join(dir, "ssh.log"),
		"FAKE_CLI_LOG":              filepath.Join(dir, "cli.log"),
		"FAKE_STATS_COUNT":          filepath.Join(dir, "stats.count"),
		"FAKE_PS_COUNT":             filepath.Join(dir, "ps.count"),
		"FAKE_STATS_BEFORE":         "interno      points: 4210     uids: 730",
		"FAKE_STATS_AFTER":          "interno      points: 4210     uids: 730",
		"KNOWRAG_DRILL_SSH":         "drill-fixture",
		"KNOWRAG_DRILL_COMPOSE_DIR": "/srv/qdrant",
		"KNOWRAG_DRILL_STATE_DIR":   filepath.Join(dir, "state"),
		"KNOWRAG_DRILL_UP_TIMEOUT":  "0",
	}
	for k, v := range env {
		full[k] = v
	}
	envv := make([]string, 0, len(full))
	for k, v := range full {
		envv = append(envv, k+"="+v)
	}

	// Absolute, because cmd.Dir below moves the process into the temp directory: anything the script
	// writes to a relative path lands there rather than in the repository, and the two script paths
	// are relative to this package.
	abs, err := filepath.Abs(script)
	if err != nil {
		t.Fatalf("resolving %s: %v", script, err)
	}
	cmd := exec.Command("bash", append([]string{abs}, args...)...) // #nosec G204 -- fixed scripts, test-generated arguments
	cmd.Env = envv
	cmd.Dir = dir
	if tty {
		cmd.Stdin = openPTY(t)
	}
	out, err := cmd.CombinedOutput()

	run := drillRun{output: string(out), dir: dir}
	run.sshLog = readIfPresent(filepath.Join(dir, "ssh.log"))
	run.cliLog = readIfPresent(filepath.Join(dir, "cli.log"))
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("running %s: %v\noutput:\n%s", script, err, out)
		}
		run.code = exitErr.ExitCode()
	}
	return run
}

func readIfPresent(path string) string {
	b, err := os.ReadFile(path) // #nosec G304 -- a path this test just built under t.TempDir()
	if err != nil {
		return ""
	}
	return string(b)
}

// TestRecoveryDrill_PreflightFailureDoesNotDestroy is the test this whole script exists for.
//
// Every case here is authorized to destroy — --yes is passed and stdin is a real terminal — so the
// preflight is the only thing standing between the run and an index that is gone with no backup.
// The assertion is on the ssh log rather than on the message: what has to be true is that no
// `docker compose down` and no `docker volume rm` ever reached the machine.
func TestRecoveryDrill_PreflightFailureDoesNotDestroy(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"the machine is unreachable", map[string]string{"FAKE_SSH_FAIL": "1"},
			"cannot reach the Qdrant machine over ssh"},
		{"the container is not running", map[string]string{"FAKE_CID": ""},
			"could not read the qdrant container"},
		{"the volume has no name to delete", map[string]string{"FAKE_VOLUME": ""},
			"could not read the qdrant container"},
		// The one that only the preflight catches: ssh works, the volume is right there, and the
		// thing that cannot rebuild the index is the embedder. Removing the preflight call site
		// leaves every other guard in this script satisfied.
		{"the embedder cannot rebuild the index", map[string]string{"FAKE_DRYRUN_FAIL": "1"},
			"'knowrag ingest --dry-run' failed"},
		{"the rebuild would not fit on disk", map[string]string{"FAKE_DU_KB": "90000", "FAKE_DF_KB": "1000"},
			"would not fit"},
		// Only one of the two numbers is missing, which is the shape that gets waved through: the two
		// concatenated are still an integer, and the comparison that follows would be `[ n -lt "" ]` —
		// an error bash reports and every `if` reads as false.
		{"the volume's size did not come back", map[string]string{"FAKE_DU_KB": ""},
			"could not read the volume's size"},
		{"the free space did not come back", map[string]string{"FAKE_DF_KB": ""},
			"could not read the volume's size"},
		{"KNOWRAG_DRILL_SSH is unset", map[string]string{"KNOWRAG_DRILL_SSH": ""},
			"KNOWRAG_DRILL_SSH is unset"},
		{"KNOWRAG_DRILL_COMPOSE_DIR is unset", map[string]string{"KNOWRAG_DRILL_COMPOSE_DIR": ""},
			"KNOWRAG_DRILL_COMPOSE_DIR is unset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := runDrill(t, recoveryDrillScript, []string{"--yes"}, tc.env, true)
			if run.code == 0 {
				t.Fatalf("the drill exited 0 with a failing preflight; output:\n%s", run.output)
			}
			if run.destroyed() {
				t.Fatalf("THE PREFLIGHT FAILED AND THE INDEX WAS DESTROYED ANYWAY.\nssh log:\n%s", run.sshLog)
			}
			if !strings.Contains(run.output, tc.want) {
				t.Errorf("output did not name the failing check %q; got:\n%s", tc.want, run.output)
			}
		})
	}
}

// TestRecoveryDrill_RefusesWithoutAuthorization covers the gate in front of the destruction, which
// is stricter than --prune's (cmd/cli/ingest_modes.go, confirmPrune: --yes OR an answered prompt).
// Here both are needed, and neither case may prompt: runDrill never answers anything, so a script
// that decided to ask would hang and fail this test by timing out.
func TestRecoveryDrill_RefusesWithoutAuthorization(t *testing.T) {
	t.Run("no --yes, even with a human at the terminal", func(t *testing.T) {
		run := runDrill(t, recoveryDrillScript, nil, nil, true)
		if run.code == 0 || run.destroyed() {
			t.Fatalf("destroyed without --yes (code %d); ssh log:\n%s", run.code, run.sshLog)
		}
		if !strings.Contains(run.output, "--yes was not passed") {
			t.Errorf("output did not say why it refused; got:\n%s", run.output)
		}
	})

	t.Run("--yes but nobody is watching", func(t *testing.T) {
		run := runDrill(t, recoveryDrillScript, []string{"--yes"}, nil, false)
		if run.code == 0 || run.destroyed() {
			t.Fatalf("destroyed from a non-terminal stdin (code %d); ssh log:\n%s", run.code, run.sshLog)
		}
		if !strings.Contains(run.output, "stdin is not a terminal") {
			t.Errorf("output did not say why it refused; got:\n%s", run.output)
		}
	})
}

// TestRecoveryDrill_RemovesTheVolumeTheContainerReports is the divergence guard.
//
// The repository's deploy/docker-compose.yml declares `qdrant-storage`; the file deployed on the
// VPS declares `qdrant_storage`. The container here reports the second, and the drill must follow
// it — a script that trusted either file would, against the machine using the other name, remove a
// volume nobody is using and bring Qdrant back on a fresh empty one.
//
// The `down -v` assertion is the other half: that flag removes what the compose file names, which
// is the value this test is proving must not be trusted.
func TestRecoveryDrill_RemovesTheVolumeTheContainerReports(t *testing.T) {
	run := runDrill(t, recoveryDrillScript, []string{"--yes"},
		map[string]string{"FAKE_VOLUME": "some-other-name_storage"}, true)
	if run.code != 0 {
		t.Fatalf("the drill failed; output:\n%s", run.output)
	}
	if !strings.Contains(run.sshLog, "docker volume rm some-other-name_storage") {
		t.Errorf("the drill did not remove the volume the container reported; ssh log:\n%s", run.sshLog)
	}
	if strings.Contains(run.sshLog, "qdrant-storage") || strings.Contains(run.sshLog, "qdrant_storage") {
		t.Errorf("the drill named a volume from a compose file instead of the container's; ssh log:\n%s", run.sshLog)
	}
	if strings.Contains(run.sshLog, "down -v") {
		t.Errorf("the drill used 'compose down -v', which removes the volume the compose file names; ssh log:\n%s", run.sshLog)
	}
}

// TestRecoveryDrill_RediscoversBeforeDeleting covers the second discover(), the one inside
// destroy_and_rebuild rather than in the preflight — the container goes away after the preflight
// approved it, which is the whole window between "this is the volume" and "delete that volume".
//
// Without this the guard is unprovable: a plant that deleted it left every test green, because
// nothing else in the suite lets the machine change its answer mid-run.
func TestRecoveryDrill_RediscoversBeforeDeleting(t *testing.T) {
	run := runDrill(t, recoveryDrillScript, []string{"--yes"},
		map[string]string{"FAKE_CID_SECOND": ""}, true)
	if run.code == 0 {
		t.Fatalf("the drill succeeded although the container vanished before the delete; output:\n%s", run.output)
	}
	if strings.Contains(run.sshLog, "docker volume rm") {
		t.Fatalf("it deleted a volume it could no longer confirm the name of; ssh log:\n%s", run.sshLog)
	}
	if !strings.Contains(run.output, "refusing to delete a volume whose name this script had to guess") {
		t.Errorf("output did not name the refusal; got:\n%s", run.output)
	}
}

// TestRecoveryDrill_VerdictOnTheCounts. A rebuild that finished without an error is not a recovery:
// the index can come back smaller and read exactly like success from every other angle.
func TestRecoveryDrill_VerdictOnTheCounts(t *testing.T) {
	const before = "interno      points: 4210     uids: 730"

	for _, tc := range []struct {
		name, after, want string
		pass              bool
	}{
		{"the same index came back", before, "RECOVERED", true},
		{"fewer points", "interno      points: 4109     uids: 730",
			"had 4210 point(s) and came back with 4109", false},
		{"fewer uids", "interno      points: 4210     uids: 729",
			"had 730 uid(s) and came back with 729", false},
		// More is a failure too. It does not mean content was lost, it means something else wrote
		// into this tenant while the drill was running — so the two numbers are no longer about the
		// same thing, and neither of them is evidence about the rebuild.
		{"more points than before", "interno      points: 4300     uids: 730",
			"had 4210 point(s) and came back with 4300", false},
		{"the collection is gone", "outra        points: 10       uids: 2",
			"was there before and is absent now", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := runDrill(t, recoveryDrillScript, []string{"--yes"}, map[string]string{
				"FAKE_STATS_BEFORE": before,
				"FAKE_STATS_AFTER":  tc.after,
			}, true)
			if tc.pass != (run.code == 0) {
				t.Fatalf("exit code %d, want pass=%v; output:\n%s", run.code, tc.pass, run.output)
			}
			if !strings.Contains(run.output, tc.want) {
				t.Errorf("output did not contain %q; got:\n%s", tc.want, run.output)
			}
		})
	}
}

// TestRecoveryDrill_RefusesAnEmptyIndex closes the way this drill could certify itself: zero points
// before and zero after compare equal, so an empty index passes the verdict without anything having
// been recovered.
func TestRecoveryDrill_RefusesAnEmptyIndex(t *testing.T) {
	run := runDrill(t, recoveryDrillScript, []string{"--yes"}, map[string]string{
		"FAKE_STATS_BEFORE": "interno      points: 0        uids: 0",
		"FAKE_STATS_AFTER":  "interno      points: 0        uids: 0",
	}, true)
	if run.code == 0 {
		t.Fatalf("the drill reported success over an index that held nothing; output:\n%s", run.output)
	}
	if run.destroyed() {
		t.Errorf("it destroyed an index it had already decided was not worth drilling; ssh log:\n%s", run.sshLog)
	}
}

// TestRecoveryDrill_WritesTheBeforeCountToDisk. The "before" is the only number the verdict can be
// wrong against, and a terminal that closes mid-drill must not take the only copy of it.
func TestRecoveryDrill_WritesTheBeforeCountToDisk(t *testing.T) {
	run := runDrill(t, recoveryDrillScript, []string{"--yes"}, nil, true)
	if run.code != 0 {
		t.Fatalf("the drill failed; output:\n%s", run.output)
	}
	files, err := filepath.Glob(filepath.Join(run.dir, "state", "*-before.txt"))
	if err != nil || len(files) != 1 {
		t.Fatalf("expected exactly one before-file in the state dir, got %v (err %v)", files, err)
	}
	if !strings.Contains(readIfPresent(files[0]), "points: 4210") {
		t.Errorf("the before-file does not hold the counts; got:\n%s", readIfPresent(files[0]))
	}
}

// TestRecoveryDrill_SaysWhatItDoesNotMeasure, on a run that SUCCEEDS — which is the case where the
// disclaimer is easiest to lose and most needed, the same property scripts/verify-deploy.sh has.
// The subjects are asserted individually because dropping one of them is the regression: a gap
// nobody states reads as a gap that is covered.
func TestRecoveryDrill_SaysWhatItDoesNotMeasure(t *testing.T) {
	run := runDrill(t, recoveryDrillScript, []string{"--yes"}, nil, true)
	if run.code != 0 {
		t.Fatalf("the drill failed; output:\n%s", run.output)
	}
	for _, subject := range []string{
		"how long search was unavailable",
		"partial recovery",
		"a failure in the middle of the reingestion",
	} {
		if !strings.Contains(run.output, subject) {
			t.Errorf("a successful run did not say it does not measure %q; got:\n%s", subject, run.output)
		}
	}
}
