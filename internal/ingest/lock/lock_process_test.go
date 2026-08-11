package lock

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	// holderEnv makes the helper below run as the lock holder instead of skipping.
	holderEnv = "KNOWRAG_LOCK_TEST_HOLDER"
	// holderReady is what the child prints once it holds the lock, so the parent never has to guess
	// by sleeping.
	holderReady = "KNOWRAG_LOCK_ACQUIRED"
)

// TestHelperProcess_HoldsLock is not a test of anything. It is the body of the child process the
// kill test spawns: it takes the lock, announces it, and waits to be killed.
//
// It has to be a real process. A goroutine cannot stand in, because goroutines share one file
// descriptor table — killing one releases nothing, and the property under test is that the kernel
// drops the flock when the *process* dies.
func TestHelperProcess_HoldsLock(t *testing.T) {
	if os.Getenv(holderEnv) == "" {
		t.Skip("helper for TestFileLock_KillHolder_FreesLockWithNoManualCleanup; runs only as the spawned child")
	}
	l, err := New(t.Context(), testEndpoint, testCollection, testTenant)
	if err != nil {
		t.Fatalf("New in the child: %v", err)
	}
	if err := l.TryAcquire(); err != nil {
		t.Fatalf("TryAcquire in the child: %v", err)
	}
	fmt.Println(holderReady)
	// Long enough that the parent's SIGKILL always lands first; the child is never expected to
	// return from here.
	time.Sleep(2 * time.Minute)
}

func TestFileLock_KillHolder_FreesLockWithNoManualCleanup(t *testing.T) {
	scopedCache(t) // t.Setenv puts XDG_CACHE_HOME in the environment the child inherits below.

	// #nosec G204 G702 -- the binary is this test binary and the only flag is a literal; re-execing
	// os.Args[0] is the standard way to get a second process out of a Go test.
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestHelperProcess_HoldsLock$")
	cmd.Env = append(os.Environ(), holderEnv+"=1")
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the holder process: %v", err)
	}

	// scanErr is written by the goroutine and read after <-drained, which is what orders the two.
	ready, drained := make(chan struct{}), make(chan struct{})
	var scanErr error
	go func() {
		defer close(drained)
		seen := false
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if !seen && strings.Contains(scanner.Text(), holderReady) {
				seen = true
				close(ready)
			}
		}
		// Kept rather than dropped: a pipe that broke is why the child's line never arrived, and
		// without it the only symptom is the readiness timeout below — which reads as "the lock is
		// slow to acquire" and sends whoever is debugging at the wrong file.
		scanErr = scanner.Err()
	}()

	select {
	case <-ready:
	case <-drained:
		// The child's output ended without the line. Sitting out the budget below would report a
		// timeout for something already decided, and scanErr says whether the pipe or the child failed.
		_ = cmd.Process.Kill()
		t.Fatalf("the holder process's output ended before it reported acquiring the lock (read error: %v)", scanErr)
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the holder process never reported acquiring the lock")
	}

	l, err := New(t.Context(), testEndpoint, testCollection, testTenant)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := l.TryAcquire(); !errors.Is(err, ErrHeld) {
		t.Fatalf("TryAcquire while another process holds the lock = %v, want ErrHeld", err)
	}

	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("killing the holder process: %v", err)
	}
	<-drained
	if scanErr != nil {
		t.Errorf("reading the holder process's output: %v", scanErr)
	}
	_ = cmd.Wait() // reaping the child; its exit status is the kill we just sent

	// From here on: no rm, no unlink, no cleanup of any kind. The kill is the whole recovery.
	if err := l.TryAcquire(); err != nil {
		t.Fatalf("TryAcquire after the holder was killed = %v, want success with no manual cleanup", err)
	}
	t.Cleanup(func() { _ = l.Release() })

	if _, err := os.Stat(l.Path()); err != nil {
		t.Errorf("stat %s: %v — the lock file is expected to survive; its presence was never the lock", l.Path(), err)
	}
}
