package lock

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const (
	testEndpoint   = "127.0.0.1:6334"
	testCollection = "knowrag_interno"
	testTenant     = "tenant-a"
)

// scopedCache points os.UserCacheDir at a directory this test owns. XDG_CACHE_HOME is the hook
// os.UserCacheDir documents on Linux, so redirecting it exercises the same path derivation
// production runs — nothing here is a test-only branch through New.
func scopedCache(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
}

func newLock(t *testing.T, endpoint, collection, tenant string) *FileLock {
	t.Helper()
	l, err := New(t.Context(), endpoint, collection, tenant)
	if err != nil {
		t.Fatalf("New(%q, %q, %q): %v", endpoint, collection, tenant, err)
	}
	return l
}

func TestFileLock_TryAcquire_SucceedsWhenFree(t *testing.T) {
	scopedCache(t)
	l := newLock(t, testEndpoint, testCollection, testTenant)

	if err := l.TryAcquire(); err != nil {
		t.Fatalf("TryAcquire on a free key: %v", err)
	}
	t.Cleanup(func() { _ = l.Release() })

	if _, err := os.Stat(l.Path()); err != nil {
		t.Errorf("stat %s after acquiring: %v", l.Path(), err)
	}
}

func TestFileLock_TryAcquire_FailsFastWhenHeldBySameProcess(t *testing.T) {
	scopedCache(t)
	held := newLock(t, testEndpoint, testCollection, testTenant)
	if err := held.TryAcquire(); err != nil {
		t.Fatalf("TryAcquire on a free key: %v", err)
	}
	t.Cleanup(func() { _ = held.Release() })

	// The second attempt runs in a goroutine against a deadline, because "returns ErrHeld" and
	// "returns at all" are two different failures: an implementation without LOCK_NB would sit here
	// until the first lock is released, and a test that simply called TryAcquire would hang the
	// suite instead of reporting it.
	second := newLock(t, testEndpoint, testCollection, testTenant)
	done := make(chan error, 1)
	go func() { done <- second.TryAcquire() }()

	select {
	case err := <-done:
		if !errors.Is(err, ErrHeld) {
			_ = second.Release()
			t.Fatalf("TryAcquire while held = %v, want ErrHeld", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("TryAcquire is still waiting after 2s: the lock blocks instead of failing fast")
	}
}

func TestFileLock_Release_FreesLockForNextAcquire(t *testing.T) {
	scopedCache(t)
	first := newLock(t, testEndpoint, testCollection, testTenant)
	if err := first.TryAcquire(); err != nil {
		t.Fatalf("TryAcquire on a free key: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	second := newLock(t, testEndpoint, testCollection, testTenant)
	if err := second.TryAcquire(); err != nil {
		t.Fatalf("TryAcquire after Release: %v", err)
	}
	t.Cleanup(func() { _ = second.Release() })
}

// TestFileLock_Release_ReportsBothFailures covers the case where releasing fails twice. Reporting
// only the first of them is a silent loss: the two halves are different faults, and an operator
// reading a stuck lock wants both.
func TestFileLock_Release_ReportsBothFailures(t *testing.T) {
	scopedCache(t)
	l := newLock(t, testEndpoint, testCollection, testTenant)
	if err := l.TryAcquire(); err != nil {
		t.Fatalf("TryAcquire on a free key: %v", err)
	}

	// Closing the descriptor behind Release's back is what makes both halves fail at once: the unlock
	// hits a descriptor that is no longer open, and so does the close.
	if err := l.file.Close(); err != nil {
		t.Fatalf("closing the lock file out from under Release: %v", err)
	}

	err := l.Release()
	if err == nil {
		t.Fatal("Release on a closed descriptor reported success")
	}
	if !errors.Is(err, unix.EBADF) {
		t.Errorf("Release = %v, which does not carry the failed unlock (%v)", err, unix.EBADF)
	}
	if !errors.Is(err, os.ErrClosed) {
		t.Errorf("Release = %v, which does not carry the failed close (%v)", err, os.ErrClosed)
	}
}

func TestFileLock_DifferentScopes_DoNotBlockEachOther(t *testing.T) {
	scopedCache(t)
	base := newLock(t, testEndpoint, testCollection, testTenant)
	if err := base.TryAcquire(); err != nil {
		t.Fatalf("TryAcquire on a free key: %v", err)
	}
	t.Cleanup(func() { _ = base.Release() })

	for _, tc := range []struct{ name, collection, tenant string }{
		{"different collection", "knowrag_base_paga", testTenant},
		{"different tenant", testCollection, "tenant-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other := newLock(t, testEndpoint, tc.collection, tc.tenant)
			if other.Path() == base.Path() {
				t.Fatalf("scope %s/%s shares a lock file with %s/%s", tc.collection, tc.tenant, testCollection, testTenant)
			}
			if err := other.TryAcquire(); err != nil {
				t.Fatalf("TryAcquire on an independent scope: %v", err)
			}
			t.Cleanup(func() { _ = other.Release() })
		})
	}
}

// TestFileLock_ScopeComponents_DoNotCollideAcrossTheirBoundary is about the encoding of the key
// rather than the lock. collection and tenant are operator free text and nothing rejects a "|" in
// either, so a key that joins them with one cannot say where the boundary was.
func TestFileLock_ScopeComponents_DoNotCollideAcrossTheirBoundary(t *testing.T) {
	scopedCache(t)
	// Two different scopes that a "|"-joined key spells identically. The collision refuses a run that
	// nobody is competing with — the safe direction to be wrong in, and still wrong.
	first := newLock(t, testEndpoint, "a|b", "c")
	second := newLock(t, testEndpoint, "a", "b|c")
	if first.Path() == second.Path() {
		t.Errorf("collection %q + tenant %q and collection %q + tenant %q share the lock file %s",
			"a|b", "c", "a", "b|c", first.Path())
	}
}

// TestCheckLockFS_RefusesWhereFlockDoesNotExclude passes the filesystem in rather than mounting one:
// making a 9p or NFS mount needs privilege this suite does not have, and a test that skipped without
// one would leave the check unexercised on every machine that runs it.
func TestCheckLockFS_RefusesWhereFlockDoesNotExclude(t *testing.T) {
	const dir = "/does/not/need/to/exist/locks"
	for _, tc := range []struct{ name, wantNamed string }{
		{"9p", "9p"},
		{"NFS", "NFS"},
		{"FUSE", "FUSE"},
		{"ext4", ""},
		{"tmpfs", ""}, // what t.TempDir() is on this host, so the suite's own case has to pass
	} {
		magic := map[string]int64{
			"9p": unix.V9FS_MAGIC, "NFS": unix.NFS_SUPER_MAGIC, "FUSE": unix.FUSE_SUPER_MAGIC,
			"ext4": unix.EXT4_SUPER_MAGIC, "tmpfs": unix.TMPFS_MAGIC,
		}[tc.name]

		t.Run(tc.name, func(t *testing.T) {
			err := checkLockFS(dir, magic)
			if tc.wantNamed == "" {
				if err != nil {
					t.Errorf("checkLockFS on %s = %v, want it accepted", tc.name, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkLockFS on %s returned no error: TryAcquire would report success there "+
					"while excluding nothing", tc.name)
			}
			// The operator has to be able to act on this without reading the source: which directory,
			// which filesystem, and the knob that moves it.
			for _, want := range []string{dir, tc.wantNamed, "XDG_CACHE_HOME"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("checkLockFS on %s = %v, which never mentions %q", tc.name, err, want)
				}
			}
		})
	}
}

func TestFileLock_ConcurrentAcquire_ExactlyOneWinner(t *testing.T) {
	scopedCache(t)
	const n = 16

	// The locks are built before the race starts so that only TryAcquire is contended, and each
	// goroutine gets its own FileLock: flock is held by the open file description, so two
	// descriptors on one file in one process really do exclude each other.
	locks := make([]*FileLock, n)
	for i := range locks {
		locks[i] = newLock(t, testEndpoint, testCollection, testTenant)
	}

	results := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range locks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i] = locks[i].TryAcquire()
		}()
	}
	close(start)
	wg.Wait()

	winners := 0
	for i, err := range results {
		switch {
		case err == nil:
			winners++
			t.Cleanup(func() { _ = locks[i].Release() })
		case errors.Is(err, ErrHeld):
		default:
			t.Errorf("goroutine %d: TryAcquire = %v, want nil or ErrHeld", i, err)
		}
	}
	if winners != 1 {
		t.Errorf("%d of %d goroutines acquired the lock, want exactly 1", winners, n)
	}
}

func TestFileLock_EndpointNormalisation_OneServerOneLockFile(t *testing.T) {
	scopedCache(t)
	path := func(endpoint string) string {
		t.Helper()
		return newLock(t, endpoint, testCollection, testTenant).Path()
	}

	literal := path("127.0.0.1:6334")

	t.Run("spellings of one endpoint agree", func(t *testing.T) {
		// The whitespace entries are not decoration. config assigns the environment value verbatim,
		// and a .env authored on a Windows drive arrives with a trailing \r — which SplitHostPort
		// returns as part of the port, so an endpoint that is byte-for-byte this server would get a
		// lock file of its own.
		for _, endpoint := range []string{
			"http://127.0.0.1:6334", "grpc://127.0.0.1:6334", "127.0.0.1",
			"127.0.0.1:6334\r", "127.0.0.1:6334 ", "\t127.0.0.1:6334\n", "127.0.0.1:06334",
		} {
			if got := path(endpoint); got != literal {
				t.Errorf("%q maps to %s, want the same file as 127.0.0.1:6334 (%s)", endpoint, got, literal)
			}
		}
	})

	t.Run("a name resolving to the literal agrees with it", func(t *testing.T) {
		addrs, err := net.DefaultResolver.LookupNetIP(t.Context(), "ip", "localhost")
		if err != nil {
			t.Skipf("this sandbox does not resolve localhost (%v), so the name-to-address case cannot be exercised here", err)
		}
		// Unmapped before comparing: a resolver is free to answer 127.0.0.1 as ::ffff:127.0.0.1,
		// and this sandbox does. That is the same address, and the production path unmaps it too.
		for i := range addrs {
			addrs[i] = addrs[i].Unmap()
		}
		if !slices.Contains(addrs, netip.MustParseAddr("127.0.0.1")) {
			t.Skipf("localhost resolves to %v, which does not include 127.0.0.1: the two spellings are not the same server here", addrs)
		}
		for _, endpoint := range []string{"localhost:6334", "LOCALHOST:6334"} {
			if got := path(endpoint); got != literal {
				t.Errorf("%q maps to %s, want the same file as 127.0.0.1:6334 (%s)", endpoint, got, literal)
			}
		}
	})

	t.Run("the two loopback families are one server", func(t *testing.T) {
		// Not cosmetic, and the one case here that completes end to end: internal/store's grpcHost
		// dials either spelling, so a dual-stack Qdrant left with two keys gets two runs that both
		// connect and both write. The localhost half of the triple is the subtest above.
		for _, endpoint := range []string{"[::1]:6334", "::1", "[::ffff:127.0.0.1]:6334"} {
			if got := path(endpoint); got != literal {
				t.Errorf("%q maps to %s, want the same file as 127.0.0.1:6334 (%s)", endpoint, got, literal)
			}
		}
	})

	t.Run("a different endpoint gets a different file", func(t *testing.T) {
		for _, endpoint := range []string{"127.0.0.2:6334", "127.0.0.1:6335"} {
			if got := path(endpoint); got == literal {
				t.Errorf("%q shares a lock file with 127.0.0.1:6334", endpoint)
			}
		}
	})

	t.Run("an endpoint that does not resolve is an error, not a fallback", func(t *testing.T) {
		if _, err := New(t.Context(), "nowhere.invalid:6334", testCollection, testTenant); err == nil {
			t.Error("New on an unresolvable endpoint returned no error: the raw string is being used as the key")
		}
	})

	t.Run("an endpoint with a malformed port is an error, not a second lock file", func(t *testing.T) {
		// Each of these is this same server written wrongly, and there is no repair that is obviously
		// the one the operator meant. Accepting them is worse than refusing them: the run would take a
		// lock nobody else can collide with and believe it was excluded from nothing.
		for _, endpoint := range []string{
			"127.0.0.1:6334/collections", "http://127.0.0.1:6334/collections",
			"127.0.0.1:6334?x=1", "127.0.0.1: 6334", "127.0.0.1:qdrant", "127.0.0.1:0", "127.0.0.1:70000",
		} {
			l, err := New(t.Context(), endpoint, testCollection, testTenant)
			if err == nil {
				t.Errorf("New(%q) returned no error and mapped to %s: a malformed endpoint is being hashed as written", endpoint, l.Path())
			}
		}
	})
}
