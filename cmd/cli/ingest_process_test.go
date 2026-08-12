//go:build integration

package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// The two tests here spawn real processes, and that is the requirement rather than a preference:
// flock is held by the open file description, and every goroutine in one process shares one
// descriptor table, so two goroutines "contending" would exercise nothing the kernel does for two
// invocations of the binary.
//
// They are behind the integration tag because they compile the CLI and start processes, not because
// they need equipment: neither test touches a Qdrant. That is deliberate. What a refused run must
// prove is that it did *no work* — and the sharpest evidence of that is its embedder never being
// contacted, since nothing reaches the write path without embedding first. A live Qdrant would add
// a "zero points" assertion that cannot fail while that one holds, at the cost of a container, a
// provisioning wait, and a test that skips on any host without docker.
//
// The winner is parked, not raced. Its embedder stalls forever on /tokenize, so it holds the lock
// from a known point until the test kills it, and the loser's outcome never depends on how long a
// fixture takes to ingest.

const (
	// The collections and tenants below are the lock scopes these tests contend on. Nothing is ever
	// written to a collection by either name — QDRANT_ENDPOINT points at deadQdrant — so each only
	// has to be a name, not a provisioned collection.
	processCollection      = "knowrag_lock_test"
	processOtherCollection = "knowrag_lock_test_other"
	processTenantA         = "tenant-a"
	processTenantB         = "tenant-b"

	// startupBudget bounds how long a child gets to reach its embedder. It is generous because it
	// covers a cold binary start on a busy machine, and it is not what the tests measure.
	startupBudget = 60 * time.Second

	// refusalBudget is how long the second process gets to give up. It is the assertion, not a
	// convenience: the first process is still stalled when it fires, so anything that waits for the
	// lock instead of failing fast blows through it.
	refusalBudget = 20 * time.Second
)

// TestIngestCLI_TwoProcesses_SecondFailsFast is the acceptance criterion of D-31: two invocations
// against one endpoint + collection + tenant, and the second is refused quickly without doing
// anything.
func TestIngestCLI_TwoProcesses_SecondFailsFast(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache) // the parent reads the same lock directory the children write
	bin := buildCLI(t)
	vaultPath := processVault(t)

	stalled, reached := stallingEmbedder(t)
	winner := startIngest(t, bin, ingestEnv{
		cache: cache, embedder: stalled, vaultPath: vaultPath,
		collection: processCollection, tenant: processTenantA,
	})
	waitReached(t, reached, "the first process never got as far as its embedder")

	// The loser gets an embedder of its own that fails any request and records that it was asked.
	// That recording is the "wrote nothing" assertion: a run that got past the lock would handshake
	// before it embedded and embed before it wrote, so an untouched embedder is a run that never
	// reached the write path at all.
	refused, contacted := refusingEmbedder(t)
	loser := startIngest(t, bin, ingestEnv{
		cache: cache, embedder: refused, vaultPath: vaultPath,
		collection: processCollection, tenant: processTenantA,
	})

	code, stdout, stderr := waitExit(t, loser, refusalBudget)
	if code != exitLockHeld {
		t.Errorf("the second process exited %d, want %d (the lock-held code)\nstdout: %s\nstderr: %s",
			code, exitLockHeld, stdout, stderr)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("another ingestion is already running")) {
		t.Errorf("stderr %q does not tell the operator what happened", stderr)
	}
	if contacted.Load() {
		t.Error("the refused process contacted its embedder: it did work before giving up")
	}
	if stdout.Len() != 0 {
		t.Errorf("the refused process reported %q; a run that never started has nothing to report", stdout)
	}
	// The whole point of a bounded wait: the loser gave up while the winner was still holding the
	// lock, rather than because the winner finished and let it through.
	if err := winner.Process.Signal(syscall.Signal(0)); err != nil {
		t.Errorf("the first process was no longer running (%v), so the second may simply have waited "+
			"its turn — this test proves nothing in that state", err)
	}
}

// TestIngestCLI_DifferentScopes_EachProceeds is the cross-process half of "different scopes do not
// block each other": same host, same endpoint, and then one run per axis the key is built from —
// a second tenant and a second collection — with none of the three waiting on another.
//
// Each process reaching its embedder is what "proceeded" means here — an embedder call happens only
// after the lock was taken, so all of them being there at once is three locks held at once.
func TestIngestCLI_DifferentScopes_EachProceeds(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	bin := buildCLI(t)
	vaultPath := processVault(t)

	// Every entry differs from the one before it in exactly one field, so a lock key that dropped
	// either collection or tenant would refuse a run here rather than merely narrowing the scope.
	running := map[string]*exec.Cmd{}
	for _, scope := range []struct{ name, collection, tenant string }{
		{"the baseline scope", processCollection, processTenantA},
		{"a second tenant", processCollection, processTenantB},
		{"a second collection", processOtherCollection, processTenantA},
	} {
		embedder, reached := stallingEmbedder(t)
		proc := startIngest(t, bin, ingestEnv{
			cache: cache, embedder: embedder, vaultPath: vaultPath,
			collection: scope.collection, tenant: scope.tenant,
		})
		waitReached(t, reached, "the process on "+scope.name+" ("+scope.collection+"/"+scope.tenant+
			") never got as far as its embedder: it was blocked by a run holding a different scope's lock")
		running[scope.name] = proc
	}

	for name, proc := range running {
		if err := proc.Process.Signal(syscall.Signal(0)); err != nil {
			t.Errorf("the process on %s is gone (%v): it was refused rather than proceeding", name, err)
		}
	}
}

// TestIngestCLI_PruneWithoutYes_ExitsUsageWithoutDoingAnything is the non-interactive half of the
// prune gate, proven where it actually matters: in a process, against the exit code main returns.
//
// The exit code is the whole point. A scheduler that reads 1 cannot tell "the command line was
// wrong" from "the ingestion broke", and the two want opposite responses — fix the invocation, or
// retry. exitUsage says which, and nothing reachable in-process asserts it, because os.Exit lives in
// main.
//
// The embedder is the "did nothing" witness: a run that got past this refusal would handshake before
// it embedded and embed before it wrote, so an untouched embedder is a run that never started.
func TestIngestCLI_PruneWithoutYes_ExitsUsageWithoutDoingAnything(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	bin := buildCLI(t)

	refused, contacted := refusingEmbedder(t)
	proc := startIngest(t, bin, ingestEnv{
		cache: cache, embedder: refused, vaultPath: processVault(t),
		collection: processCollection, tenant: processTenantA,
		args: []string{"--prune"},
	})

	code, stdout, stderr := waitExit(t, proc, refusalBudget)
	if code != exitUsage {
		t.Errorf("`ingest --prune` on a piped stdin exited %d, want %d (the usage code)\nstdout: %s\nstderr: %s",
			code, exitUsage, stdout, stderr)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("--yes")) {
		t.Errorf("stderr %q does not tell the operator how to authorize the prune", stderr)
	}
	if contacted.Load() {
		t.Error("the refused process contacted its embedder: it did work before giving up")
	}
	// The prompt is the failure mode this refusal exists to prevent, and it would land here.
	if stdout.Len() != 0 {
		t.Errorf("the refused process wrote %q to stdout; it must not ask a question nobody can answer", stdout)
	}
}

// TestIngestCLI_SIGINT_ExitsWithDedicatedCode is the signal path in a real process, which is the
// only place it exists: signal.Notify, the grace timer and os.Exit are all things no in-process call
// reaches.
//
// The child stalls inside its embedder, so it is interrupted with a note genuinely in flight rather
// than between notes. The grace period is short so the test measures the timer instead of waiting
// one out, and the exit code has to be the dedicated constant — a generic 1 would tell a scheduler
// the ingestion is broken when somebody pressed Ctrl-C.
func TestIngestCLI_SIGINT_ExitsWithDedicatedCode(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	bin := buildCLI(t)

	stalled, reached := stallingEmbedder(t)
	proc := startIngest(t, bin, ingestEnv{
		cache: cache, embedder: stalled, vaultPath: processVault(t),
		collection: processCollection, tenant: processTenantA,
		args: []string{"--grace-period", "1s"},
	})
	waitReached(t, reached, "the process never got as far as its embedder")

	if err := proc.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("sending SIGINT: %v", err)
	}

	code, stdout, stderr := waitExit(t, proc, refusalBudget)
	if code != exitInterrupted {
		t.Errorf("the interrupted process exited %d, want %d\nstdout: %s\nstderr: %s",
			code, exitInterrupted, stdout, stderr)
	}
	// The partial report is what the operator reads to know where the run got to, and ADR-006 makes
	// it safe to resume from. A run that exited without printing one leaves them with nothing.
	if !bytes.Contains(stdout.Bytes(), []byte("note(s)")) {
		t.Errorf("stdout %q carries no partial report", stdout)
	}
}

// TestIngestCLI_SecondSIGINT_DoesNotWaitOutTheGracePeriod proves the second signal is not ignored.
//
// The grace period is set far above the budget this test allows, so a process that honoured only the
// first signal cannot finish in time: passing means the second signal cut it short. The stalling
// embedder guarantees there is something in flight for the grace period to apply to — without it,
// the run would end at the note boundary and the test would pass for the wrong reason.
func TestIngestCLI_SecondSIGINT_DoesNotWaitOutTheGracePeriod(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	bin := buildCLI(t)

	stalled, reached := stallingEmbedder(t)
	proc := startIngest(t, bin, ingestEnv{
		cache: cache, embedder: stalled, vaultPath: processVault(t),
		collection: processCollection, tenant: processTenantA,
		args: []string{"--grace-period", "10m"},
	})
	waitReached(t, reached, "the process never got as far as its embedder")

	// The second signal is sent only once the child has acknowledged the first, and that ordering is
	// the scenario rather than a convenience. SIGINT is a standard signal, and standard signals are
	// not queued: if one is already pending, the kernel discards the next, so two sent in the same
	// instant arrive as one and no handler on earth can tell them apart. A human pressing Ctrl-C
	// twice always clears that window; a test that fires both in a tight loop does not, and would be
	// measuring POSIX rather than this program.
	//
	// So this wait is not flakiness being papered over, and removing it does not make the test
	// stricter — it makes it assert something no program can satisfy. The limitation is the
	// operating system's: no handler, here or anywhere, can distinguish two coalesced signals.
	if err := proc.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("sending the first SIGINT: %v", err)
	}
	stderr := proc.Stderr.(*syncBuffer)
	waitForOutput(t, stderr, "starting no more", startupBudget)
	if err := proc.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("sending the second SIGINT: %v", err)
	}

	code, stdout, stderr := waitExit(t, proc, refusalBudget)
	if code != exitInterrupted {
		t.Errorf("the twice-interrupted process exited %d, want %d\nstdout: %s\nstderr: %s",
			code, exitInterrupted, stdout, stderr)
	}
	// Proof it was the second signal and not the grace period: the grace period is 10 minutes and
	// this test waited 20 seconds.
	if !bytes.Contains(stderr.Bytes(), []byte("interrupted twice")) {
		t.Errorf("stderr %q does not show the second signal being acted on", stderr)
	}
}

// syncBuffer is a bytes.Buffer a test can read while the child is still writing into it. The plain
// one cannot: os/exec writes from its own goroutine, so `go test -race` calls that a data race, and
// waiting for a line the child has printed is exactly what the double-SIGINT test needs.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Bytes() []byte { return []byte(b.String()) }
func (b *syncBuffer) Len() int      { return len(b.String()) }

// waitForOutput blocks until the child has written want, or the budget expires.
func waitForOutput(t *testing.T, b *syncBuffer, want string, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if strings.Contains(b.String(), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the process never wrote %q within %s; it has: %s", want, budget, b.String())
}

// buildCLI compiles the command under test once, into a directory the test owns. The binary is what
// these tests are about — the exit code lives in main, and no in-process call reaches it.
func buildCLI(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "knowrag")
	out, err := exec.CommandContext(t.Context(), "go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("building the CLI: %v\n%s", err, out)
	}
	return bin
}

// processVault is a vault with one note in it: enough that the run has something to chunk, which is
// what carries it into the embedder and therefore past the lock.
func processVault(t *testing.T) string {
	t.Helper()
	return writeVault(t, map[string]string{
		"00-inbox/uma.md": note("0198a7f2-4b31-7c42-9e15-3d8a92c47b07", "Uma"),
	})
}

// ingestEnv is what one child process is pointed at.
type ingestEnv struct {
	cache      string
	embedder   string
	vaultPath  string
	collection string
	tenant     string
	// args are appended to `ingest --vault fixture --tenant <tenant>`.
	args []string
}

// startIngest launches `knowrag ingest` and returns it running.
//
// The environment is built from nothing rather than inherited on purpose: a host that runs this
// project for real has KNOWRAG_VAULT_* variables for its own vaults set, and a child declaring
// KNOWRAG_VAULTS=fixture while inheriting those would be refused at load time for orphan variables
// — a failure with nothing to do with what is under test here.
func startIngest(t *testing.T, bin string, env ingestEnv) *exec.Cmd {
	t.Helper()

	args := append([]string{"ingest", "--vault", "fixture", "--tenant", env.tenant}, env.args...)
	// #nosec G204 -- bin is the binary this test just built, and every argument is a literal or a
	// path this test created.
	cmd := exec.Command(bin, args...)
	// An empty reader rather than the default: os/exec gives a child with no Stdin an open
	// /dev/null, which is a character device and therefore reads as a terminal to the --prune
	// confirmation (cmd/cli/ingest.go, isTerminal). A reader makes os/exec hand over a pipe, which is
	// what a cron job actually has.
	cmd.Stdin = strings.NewReader("")
	cmd.Env = []string{
		"HOME=" + os.Getenv("HOME"),
		"PATH=" + os.Getenv("PATH"),
		"XDG_CACHE_HOME=" + env.cache,
		"QDRANT_ENDPOINT=" + deadQdrant,
		"KNOWRAG_ADMIN_QDRANT_API_KEY=not-a-real-key",
		"DEFAULT_COLLECTION=" + env.collection,
		"EMBEDDER_ENDPOINT=" + env.embedder,
		"KNOWRAG_VAULTS=fixture",
		"KNOWRAG_VAULT_FIXTURE_PATH=" + env.vaultPath,
		"KNOWRAG_VAULT_FIXTURE_AREAS=00-inbox",
	}
	cmd.Stdout, cmd.Stderr = &syncBuffer{}, &syncBuffer{}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting `knowrag ingest --tenant %s`: %v", env.tenant, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

// waitExit waits for a process to end, but only for so long: a run that sits waiting for the lock
// instead of refusing is the defect, and a test that waited for it would report a timeout of its
// own rather than the failure.
func waitExit(t *testing.T, cmd *exec.Cmd, budget time.Duration) (int, *syncBuffer, *syncBuffer) {
	t.Helper()
	stdout, stderr := cmd.Stdout.(*syncBuffer), cmd.Stderr.(*syncBuffer)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var exit *exec.ExitError
		switch {
		case err == nil:
			return 0, stdout, stderr
		case errors.As(err, &exit):
			return exit.ExitCode(), stdout, stderr
		default:
			t.Fatalf("waiting for the process: %v", err)
		}
	case <-time.After(budget):
		t.Fatalf("the process was still running after %s: it is waiting for the lock instead of "+
			"refusing", budget)
	}
	return 0, stdout, stderr // unreachable; t.Fatalf above does not return
}

// waitReached blocks until a child hits its embedder, which it can only do with the lock in hand.
//
// The parent deliberately does not poll the lock to detect this. An attempt that succeeded would
// take the lock away from the very process it is waiting for, and the test would then be about a
// race it created itself.
func waitReached(t *testing.T, reached <-chan struct{}, complaint string) {
	t.Helper()
	select {
	case <-reached:
	case <-time.After(startupBudget):
		t.Fatalf("%s within %s", complaint, startupBudget)
	}
}

// stallingEmbedder answers the handshake and then never answers /tokenize, so the process that
// reaches it stops there holding the ingestion lock until the test kills it. The returned channel
// closes on the first /tokenize.
func stallingEmbedder(t *testing.T) (string, <-chan struct{}) {
	t.Helper()
	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/handshake" {
			writeHandshake(t, w)
			return
		}
		once.Do(func() { close(reached) })
		// Either the test is finished with this process, or the process is already dead and its
		// connection with it. Waiting on both means nothing here outlives the test.
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	// Registered before the release, so cleanup runs in the order that terminates: LIFO means the
	// release is closed first and srv.Close then has no in-flight handler to wait for.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	return srv.URL, reached
}

// refusingEmbedder fails every request and remembers that one arrived. The process pointed at it is
// expected never to ask it anything.
func refusingEmbedder(t *testing.T) (string, *atomic.Bool) {
	t.Helper()
	var contacted atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		contacted.Store(true)
		http.Error(w, "this run was supposed to be refused before it did any work", http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &contacted
}
