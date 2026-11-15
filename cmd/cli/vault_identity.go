package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// A scheduled prune trusts the vault root it is pointed at: every uid the index holds that the scan did
// not return is an orphan. Point the root at ANOTHER vault that has notes, or at a copy that a sync left
// half empty, and the uids that are not there become orphans, and `--prune --yes` deletes them with
// nobody asking. The ceiling (prune_guards.go) catches the size of that; this file catches the cause
// that is cheaper to name: the root is not the one the last successful ingestion used.
//
// The identity is kept in a small state file outside the vault (writing a marker into the owner's vault,
// which is a git repository, would be worse than the problem). It holds, per vault and per index scope
// (endpoint, collection, tenant), the canonical path of the root and, when the vault is a git repository,
// the hash of its root commit, which is stable across a move and different for a different vault.

// gitKind is what a vault root is known to be, in THREE states and not two, because "I could not tell" must
// never be read as "it is not a repository": the first is a failure to look, the second is a fact.
type gitKind string

const (
	// gitCommit is a git repository with a root commit; GitRoot holds its hash.
	gitCommit gitKind = "commit"
	// gitNone is PROVEN not to be a repository: git ran and said so, and there is no .git entry.
	gitNone gitKind = "none"
	// gitUnknown is everything else: git missing, timed out, an unreadable or empty repository, an
	// unexpected answer. It is never recorded and never trusted, in either direction.
	gitUnknown gitKind = "unknown"
)

// vaultRoot is what identifies a vault root across runs.
type vaultRoot struct {
	Path       string    `json:"path"`
	Git        gitKind   `json:"git"`
	GitRoot    string    `json:"git_root,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`
	// GitNote says why Git is gitUnknown. It is only for the message and is never written.
	GitNote string `json:"-"`
}

// rootRecords is the state file.
type rootRecords struct {
	Version int                  `json:"version"`
	Roots   map[string]vaultRoot `json:"roots"`
}

const rootRecordsVersion = 1

// rootVerdict is what the check concluded about one vault's current root.
type rootVerdict struct {
	// Reason is empty when the root is the recorded one.
	Reason string
	// Mismatch is true when a record exists and describes another vault; false for "no record yet",
	// which the next successful run fixes by recording the root.
	Mismatch bool
}

// vaultStateFile is where the records live: KNOWRAG_STATE_DIR, else $XDG_STATE_HOME, else
// ~/.local/state, always under a knowrag/ directory. It is state, not cache: losing it is safe (a prune
// is refused until the next successful run records the root again) but it should survive a cache wipe.
func vaultStateFile() (string, error) {
	dir := os.Getenv("KNOWRAG_STATE_DIR")
	if dir == "" {
		base := os.Getenv("XDG_STATE_HOME")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("locating the state directory: %w", err)
			}
			base = filepath.Join(home, ".local", "state")
		}
		dir = filepath.Join(base, "knowrag")
	}
	return filepath.Join(dir, "vault-roots.json"), nil
}

// rootKey names one vault within one index scope, so two deployments sharing a machine do not overwrite
// each other's records.
func rootKey(endpoint, collection, tenant, vault string) string {
	return strings.Join([]string{endpoint, collection, tenant, vault}, "|")
}

// identifyRoot reads the identity of the vault root as it is now.
func identifyRoot(ctx context.Context, root string) (vaultRoot, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return vaultRoot{}, fmt.Errorf("resolving vault root %s: %w", root, err)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return vaultRoot{}, fmt.Errorf("resolving vault root %s: %w", root, err)
	}
	id := vaultRoot{Path: canonical}
	id.Git, id.GitRoot, id.GitNote = gitIdentity(ctx, canonical)
	return id, nil
}

// gitResult is what one git invocation produced. err is set when git did not RUN to completion (missing
// binary, timeout, cancelled context); a non-zero exit is not an err, it is an answer.
type gitResult struct {
	stdout, stderr string
	exit           int
	err            error
}

// runGit is a variable so tests can make git absent, slow or wrong without touching the machine's git.
var runGit = func(ctx context.Context, root string, args ...string) gitResult {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...) // #nosec G204 -- root is the operator-configured vault path, passed as an argument, not through a shell
	// A fixed locale, so "not a git repository" is the same words wherever the operator's shell is.
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	res := gitResult{stdout: out.String(), stderr: errb.String()}
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		res.err = ctx.Err()
	case errors.As(err, &exit):
		res.exit = exit.ExitCode()
	case err != nil:
		res.err = err
	}
	return res
}

const gitTimeout = 10 * time.Second

// gitIdentity classifies the vault root and, for a repository, returns its root commit. It fails CLOSED:
// anything it cannot establish is gitUnknown with a note, never gitNone.
func gitIdentity(ctx context.Context, root string) (gitKind, string, string) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	inside := runGit(ctx, root, "rev-parse", "--is-inside-work-tree")
	switch {
	case inside.err != nil:
		return gitUnknown, "", fmt.Sprintf("git could not be run: %v", inside.err)
	case inside.exit == 0 && strings.TrimSpace(inside.stdout) == "true":
		// a work tree: read its root commit below
	case inside.exit == 128 && strings.Contains(inside.stderr, "not a git repository"):
		// Git says it is not one. Believe it only if there is also no .git entry: a .git that git
		// rejects is a damaged repository, not an absent one.
		if _, err := os.Lstat(filepath.Join(root, ".git")); errors.Is(err, fs.ErrNotExist) {
			return gitNone, "", ""
		}
		return gitUnknown, "", "git does not accept the .git entry of the vault root"
	default:
		return gitUnknown, "", fmt.Sprintf("git answered %q with exit %d", strings.TrimSpace(inside.stderr+inside.stdout), inside.exit)
	}

	revs := runGit(ctx, root, "rev-list", "--max-parents=0", "HEAD")
	if revs.err != nil || revs.exit != 0 {
		note := fmt.Sprintf("the root commit could not be read: exit %d %s", revs.exit, strings.TrimSpace(revs.stderr))
		if revs.err != nil {
			note = fmt.Sprintf("the root commit could not be read: %v", revs.err)
		}
		return gitUnknown, "", note
	}
	roots := strings.Fields(revs.stdout)
	if len(roots) == 0 {
		return gitUnknown, "", "the repository has no commit to identify it by"
	}
	slices.Sort(roots)
	return gitCommit, roots[0], ""
}

// differenceFrom says how the vault root as it is NOW differs from this recorded one, or "" when it is the
// same vault. It decides, in order:
//
//   - now unknown: a difference. Not being able to read the identity is not evidence of sameness, and a
//     recorded commit is never demoted to "compare the path" because git failed this time.
//   - a record that is not commit or none (an old or damaged one): a difference, until someone accepts.
//   - another kind (a repository that stopped being one, or the reverse): a difference.
//   - two repositories: the root commit decides, so the same repository in another folder is the same
//     vault and another repository at the same path is not.
//   - two proven non-repositories: only the canonical path is left to compare.
func (r vaultRoot) differenceFrom(now vaultRoot) string {
	switch {
	case now.Git == gitUnknown:
		return "the identity of the vault root could not be established now (" + now.GitNote + ")"
	case r.Git != gitCommit && r.Git != gitNone:
		return "the recorded identity of the root is incomplete; accept the current root once with --accept-vault-root"
	case r.Git != now.Git:
		return fmt.Sprintf("the root changed kind: was %s, now %s", r.describe(), now.describe())
	case r.Git == gitCommit && r.GitRoot != now.GitRoot:
		return fmt.Sprintf("another repository: was %s, now %s", r.describe(), now.describe())
	case r.Git == gitNone && r.Path != now.Path:
		return fmt.Sprintf("another folder: was %s, now %s", r.describe(), now.describe())
	}
	return ""
}

func (r vaultRoot) describe() string {
	switch r.Git {
	case gitCommit:
		return fmt.Sprintf("%s (git root %.12s)", r.Path, r.GitRoot)
	case gitNone:
		return r.Path + " (not a git repository)"
	}
	return r.Path + " (git identity unknown)"
}

// checkRoots compares each vault's current identity with the recorded one. A vault with nothing recorded
// gets a verdict that is a reason but not a mismatch.
func checkRoots(recs rootRecords, scope string, current map[string]vaultRoot) map[string]rootVerdict {
	out := make(map[string]rootVerdict, len(current))
	for vault, now := range current {
		rec, ok := recs.Roots[scope+"|"+vault]
		if !ok {
			reason := fmt.Sprintf("no root is recorded for vault %q (first run here, or the state file was lost)", vault)
			if now.Git == gitUnknown {
				reason += "; and its identity could not be established now (" + now.GitNote + ")"
			}
			out[vault] = rootVerdict{Reason: reason}
			continue
		}
		if diff := rec.differenceFrom(now); diff != "" {
			out[vault] = rootVerdict{Mismatch: true, Reason: fmt.Sprintf("the root of vault %q is not the one last ingested (%s)", vault, diff)}
		}
	}
	return out
}

// updateRecords returns the records after a successful run: a vault with no record, or whose root is the
// recorded one, is (re)recorded; a vault whose root differs is recorded only when someone accepted it. A
// root whose git identity is unknown is NEVER recorded, accepted or not: a record with no identity would
// either block every later run or, worse, be compared by path alone.
func updateRecords(recs rootRecords, scope string, current map[string]vaultRoot, verdicts map[string]rootVerdict, accepted bool, now time.Time) rootRecords {
	next := rootRecords{Version: rootRecordsVersion, Roots: map[string]vaultRoot{}}
	for k, v := range recs.Roots {
		next.Roots[k] = v
	}
	for vault, root := range current {
		if root.Git == gitUnknown || (verdicts[vault].Mismatch && !accepted) {
			continue
		}
		root.RecordedAt = now.UTC()
		next.Roots[scope+"|"+vault] = root
	}
	return next
}

// recordRoots writes the records of a finished run under a file lock, re-reading them inside it: two
// ingestions that finish together would otherwise each write the file from what they read at the start and
// the second would erase the first's record (a safe loss, since a lost record only postpones a prune, but
// a loss). The verdicts are recomputed against what is on disk now for the same reason.
func recordRoots(path, scope string, current map[string]vaultRoot, accepted bool, now time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating the state directory: %w", err)
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- the state path comes from the operator's own environment
	if err != nil {
		return fmt.Errorf("opening the state lock: %w", err)
	}
	defer func() { _ = lock.Close() }()                              // closing the descriptor also releases the flock
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil { // #nosec G115 -- a file descriptor fits an int
		return fmt.Errorf("locking the state file: %w", err)
	}
	fresh, err := loadRootRecords(path)
	if err != nil {
		return err // never overwrite what could not be read
	}
	return saveRootRecords(path, updateRecords(fresh, scope, current, checkRoots(fresh, scope, current), accepted, now))
}

// loadRootRecords reads the state file. A missing file is an empty record set; one that exists and cannot
// be read is an error, which the caller turns into "no prune": an unreadable record proves nothing.
func loadRootRecords(path string) (rootRecords, error) {
	empty := rootRecords{Version: rootRecordsVersion, Roots: map[string]vaultRoot{}}
	raw, err := os.ReadFile(path) // #nosec G304 -- the state file path comes from the operator's own environment
	if errors.Is(err, fs.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return empty, fmt.Errorf("reading %s: %w", path, err)
	}
	var recs rootRecords
	if err := json.Unmarshal(raw, &recs); err != nil {
		return empty, fmt.Errorf("decoding %s: %w", path, err)
	}
	if recs.Roots == nil {
		recs.Roots = map[string]vaultRoot{}
	}
	return recs, nil
}

// saveRootRecords writes the state file atomically (temp file in the same directory, then rename), so a
// crash cannot leave a half-written record that the next run would read as corruption.
func saveRootRecords(path string, recs rootRecords) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating the state directory: %w", err)
	}
	raw, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".vault-roots-*")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return os.Rename(tmp.Name(), path)
}
