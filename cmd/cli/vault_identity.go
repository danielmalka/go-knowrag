package main

import (
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

// vaultRoot is what identifies a vault root across runs.
type vaultRoot struct {
	Path       string    `json:"path"`
	GitRoot    string    `json:"git_root,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`
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
	return vaultRoot{Path: canonical, GitRoot: gitRootCommit(ctx, canonical)}, nil
}

// gitRootCommit is the hash of the vault repository's first commit, or "" when the folder is not a
// repository (or git is missing, or the repository has no commit). It is the stable id: a vault moved to
// another folder keeps it, a different vault does not share it.
func gitRootCommit(ctx context.Context, root string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", root, "rev-list", "--max-parents=0", "HEAD").Output() // #nosec G204 -- root is the operator-configured vault path, passed as an argument, not through a shell
	if err != nil {
		return ""
	}
	roots := strings.Fields(string(out))
	if len(roots) == 0 {
		return ""
	}
	slices.Sort(roots)
	return roots[0]
}

// sameVault decides whether two identities are the same vault. A shared git root commit settles it in
// favour of "same" even if the path moved (the owner relocated the vault); two different git roots settle
// it against; without a git root on both sides the canonical path is all there is.
func (r vaultRoot) sameVault(other vaultRoot) bool {
	if r.GitRoot != "" && other.GitRoot != "" {
		return r.GitRoot == other.GitRoot
	}
	return r.Path == other.Path
}

func (r vaultRoot) describe() string {
	if r.GitRoot == "" {
		return r.Path
	}
	return fmt.Sprintf("%s (git root %.12s)", r.Path, r.GitRoot)
}

// checkRoots compares each vault's current identity with the recorded one. A vault with nothing recorded
// gets a verdict that is a reason but not a mismatch.
func checkRoots(recs rootRecords, scope string, current map[string]vaultRoot) map[string]rootVerdict {
	out := make(map[string]rootVerdict, len(current))
	for vault, now := range current {
		rec, ok := recs.Roots[scope+"|"+vault]
		switch {
		case !ok:
			out[vault] = rootVerdict{Reason: fmt.Sprintf("no root is recorded for vault %q (first run here, or the state file was lost)", vault)}
		case !rec.sameVault(now):
			out[vault] = rootVerdict{Mismatch: true, Reason: fmt.Sprintf(
				"the root of vault %q is not the one last ingested (was %s, now %s)", vault, rec.describe(), now.describe())}
		}
	}
	return out
}

// updateRecords returns the records after a successful run: a vault with no record, or whose root is the
// recorded one, is (re)recorded; a vault whose root differs is recorded only when someone accepted it.
func updateRecords(recs rootRecords, scope string, current map[string]vaultRoot, verdicts map[string]rootVerdict, accepted bool, now time.Time) rootRecords {
	next := rootRecords{Version: rootRecordsVersion, Roots: map[string]vaultRoot{}}
	for k, v := range recs.Roots {
		next.Roots[k] = v
	}
	for vault, root := range current {
		if verdicts[vault].Mismatch && !accepted {
			continue
		}
		root.RecordedAt = now.UTC()
		next.Roots[scope+"|"+vault] = root
	}
	return next
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
