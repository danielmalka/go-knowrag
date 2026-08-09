package vault

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// initGitRepo creates a repository with one commit per (file, date) pair, in the order given.
// It skips the test when git is unavailable rather than failing: git is a dependency of the code
// under test, not of the build.
func initGitRepo(t *testing.T, commits []struct{ path, date string }) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available on this host")
	}

	root := t.TempDir()
	run := func(env []string, args ...string) {
		t.Helper()
		// #nosec G204 -- test-local arguments and a t.TempDir() root
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	run(nil, "init", "--quiet")
	run(nil, "config", "user.email", "test@example.invalid")
	run(nil, "config", "user.name", "Test")

	for i, c := range commits {
		path := filepath.Join(root, filepath.FromSlash(c.path))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		// The revision counter keeps a second commit to the same path from being an empty commit.
		content := fmt.Sprintf("content of %s, revision %d\n", c.path, i)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		run(nil, "add", c.path)
		run([]string{"GIT_AUTHOR_DATE=" + c.date, "GIT_COMMITTER_DATE=" + c.date},
			"commit", "--quiet", "-m", "add "+c.path)
	}
	return root
}

func TestGitUpdatedMap_OneInvocationPerVault(t *testing.T) {
	root := initGitRepo(t, []struct{ path, date string }{
		{"a.md", "2026-01-02T03:04:05+00:00"},
		{"notes/b.md", "2026-02-03T04:05:06+00:00"},
		// A second commit touching a.md: the map must keep the newest, not the first commit.
		{"a.md", "2026-03-04T05:06:07+00:00"},
	})

	calls := 0
	real := runGitLog
	runGitLog = func(root string) ([]byte, error) {
		calls++
		return real(root)
	}
	t.Cleanup(func() { runGitLog = real })

	got, err := gitUpdatedMap(root)
	if err != nil {
		t.Fatalf("gitUpdatedMap: %v", err)
	}
	if calls != 1 {
		t.Errorf("git was invoked %d times, want exactly 1 per vault", calls)
	}

	want := map[string]string{
		"a.md":       "2026-03-04T05:06:07Z",
		"notes/b.md": "2026-02-03T04:05:06Z",
	}
	if len(got) != len(want) {
		t.Fatalf("map has %d entries (%v), want %d", len(got), got, len(want))
	}
	for path, wantDate := range want {
		if got[path].UTC().Format(time.RFC3339) != wantDate {
			t.Errorf("%s = %v, want %s", path, got[path], wantDate)
		}
	}
}

// TestGitUpdatedMap_NonAsciiPaths guards the core.quotepath setting. The corpus is largely
// Portuguese; with git's default the map keys would come back octal-escaped, match nothing the
// walk produced, and every accented note would silently fall through to mtime.
func TestGitUpdatedMap_NonAsciiPaths(t *testing.T) {
	root := initGitRepo(t, []struct{ path, date string }{
		{"research/ingles/plano-de-estudos-inglês.md", "2026-05-06T07:08:09+00:00"},
	})

	got, err := gitUpdatedMap(root)
	if err != nil {
		t.Fatalf("gitUpdatedMap: %v", err)
	}
	if _, ok := got["research/ingles/plano-de-estudos-inglês.md"]; !ok {
		t.Errorf("non-ASCII path missing from the map; got keys %v", got)
	}
}

// TestGitUpdatedMap_NotAGitRepo: a vault copied out of its repository must not fail the scan.
//
// It runs the real git binary rather than injecting an error, which is what makes the message-based
// classification in gitUpdatedMap honest: this test fails the day git's wording drifts away from
// notARepoMessage, before a real vault does.
func TestGitUpdatedMap_NotAGitRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available on this host")
	}
	got, err := gitUpdatedMap(t.TempDir())
	if err != nil {
		t.Fatalf("gitUpdatedMap on a non-repo returned an error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("gitUpdatedMap on a non-repo = %v, want empty", got)
	}
}

// TestGitUpdatedMap_RealFailureIsReportedNotDegraded: git missing from PATH, a permission error or
// a broken repository must not come back looking like "this vault is not a repository". Both real
// vaults are git repositories, so there the benign case cannot occur — folding the two together, as
// this function did until it was corrected, meant a real misconfiguration silently dropped the whole
// corpus to mtime with no signal anywhere.
func TestGitUpdatedMap_RealFailureIsReportedNotDegraded(t *testing.T) {
	t.Run("git binary missing", func(t *testing.T) {
		real := runGitLog
		runGitLog = func(string) ([]byte, error) {
			return nil, &exec.Error{Name: "git", Err: exec.ErrNotFound}
		}
		t.Cleanup(func() { runGitLog = real })

		if _, err := gitUpdatedMap(t.TempDir()); err == nil {
			t.Error("a missing git binary degraded to mtime in silence")
		}
	})

	// A `.git` that is a file holding garbage: git finds it, refuses it, and exits 128 with a
	// message that is not the not-a-repository one. Real git, real message — the same reason the
	// non-repo test above spawns the binary.
	t.Run("broken repository", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git not available on this host")
		}
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, ".git"), []byte("garbage\n"), 0o600); err != nil {
			t.Fatalf("write .git: %v", err)
		}

		_, err := gitUpdatedMap(root)
		if err == nil {
			t.Fatal("a broken repository degraded to mtime in silence")
		}
		// The operator has to be able to act on it, which means git's own words, not "exit 128".
		if !strings.Contains(err.Error(), "gitfile") {
			t.Errorf("error does not carry git's message: %v", err)
		}
	})
}

func TestResolveUpdated_UntrackedFileUsesMtime(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "untracked.md")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	mtime := time.Date(2026, 4, 5, 6, 7, 8, 0, time.UTC)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	gitUpdated := map[string]time.Time{"other.md": time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}
	if got := resolveUpdated(root, "untracked.md", gitUpdated); !got.Equal(mtime) {
		t.Errorf("resolveUpdated = %v, want the file's mtime %v", got, mtime)
	}
}

func TestResolveUpdated_GitWinsOverMtime(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	committed := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

	got := resolveUpdated(root, "a.md", map[string]time.Time{"a.md": committed})
	if !got.Equal(committed) {
		t.Errorf("resolveUpdated = %v, want the git date %v", got, committed)
	}
}

func TestResolveUpdated_MissingFileAndNoGitEntry(t *testing.T) {
	if got := resolveUpdated(t.TempDir(), "gone.md", nil); !got.IsZero() {
		t.Errorf("resolveUpdated = %v, want the zero time", got)
	}
}
