package vault

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// gitLogFormat prefixes every commit header with a NUL byte so header lines are distinguishable
// from file names without guessing. A path can contain a tab; it cannot contain a NUL.
const gitLogFormat = "--format=%x00%H%x09%aI"

// notARepoMessage is the fragment git prints when root lies outside any repository. Matching the
// message is the only way to tell that case apart from a repository that exists and is broken:
// both exit 128, and git offers no distinguishing code. The match is made deterministic by forcing
// LC_ALL=C on the command below — git translates its fatal messages, and a translated one would
// otherwise turn "this vault is not a repository" into a failed ingestion on a localized host.
const notARepoMessage = "not a git repository"

// runGitLog is a package var so a test can count invocations without spawning git. Batching is the
// requirement being tested, not an optimization: the corpus is 731 files, and one `git log` per
// file is 731 process spawns.
var runGitLog = func(root string) ([]byte, error) {
	// core.quotepath=false keeps non-ASCII file names as raw UTF-8 instead of octal escapes. The
	// corpus is largely Portuguese, so with the default the map keys would not match the paths the
	// walk produced and nearly every accented note would silently fall through to mtime.
	//
	// --relative reports paths relative to the working directory (root, thanks to -C) instead of
	// to the repository root. When the two coincide — both real vaults have .git/ at their root —
	// it changes nothing. When they do not, a vault nested inside a larger repository, it is the
	// difference between keys the walk can match and keys it never will, which would look like
	// "git knows nothing about this vault" and fall the whole corpus back to mtime in silence.
	// #nosec G204 -- the binary and every flag are fixed literals; the only variable is the vault
	// root, which is operator configuration passed through -C, not input crossing a trust boundary.
	cmd := exec.Command("git", "-C", root, "-c", "core.quotepath=false",
		"log", "--name-only", "--relative", gitLogFormat)
	// LC_ALL=C fixes the language of git's fatal messages, which is what makes the classification
	// in gitUpdatedMap hold on any host. It does not touch the file names: those are raw bytes from
	// the index, kept unescaped by core.quotepath=false above.
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	return cmd.Output()
}

// gitUpdatedMap derives `updated` for every committed file in one `git log` call, mapping paths
// relative to root to the date of the most recent commit that lists them.
//
// A root outside any git repository yields (nil, nil): every file then falls back to mtime and the
// scan continues. That degradation is deliberate — an ingestion that dies because the vault was
// copied out of its repository would be a worse outcome than a slightly different timestamp on a
// field that is display-only and outside point_hash.
//
// Every other failure — no git binary on the host, permission denied, a broken repository — is
// returned instead of being folded into that same silent answer. Both real vaults *are* git
// repositories, so the benign case cannot happen there; collapsing the two, as this function did
// until it was corrected, made a real misconfiguration indistinguishable from it, with the whole
// corpus quietly falling back to mtime and no signal anywhere. The caller (ScanVault) fails the
// scan on it, which is the only way the operator finds out.
//
// Precision, stated as it is rather than better than it is: `git log --name-only` does not list
// files for merge commits (that needs -m), so a path whose latest change arrived through a merge
// keeps the date of the last non-merge commit that touched it. Measured on MalkaLife: 12 merges in
// 360 commits, one note about 90 minutes older than reality. The command deliberately stays as it
// is — `updated` is the one §2.4 payload field kept out of point_hash precisely because it is
// allowed this kind of drift, and -m multiplies the output of a call NFR-5 wants cheap.
func gitUpdatedMap(root string) (map[string]time.Time, error) {
	out, err := runGitLog(root)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if bytes.Contains(exitErr.Stderr, []byte(notARepoMessage)) {
				return nil, nil
			}
			return nil, fmt.Errorf("git log in %s: %w: %s",
				root, err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("git log in %s: %w", root, err)
	}

	updated := make(map[string]time.Time)
	var commitDate time.Time
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "\x00") {
			// Header: \x00<hash>\t<RFC 3339 author date>.
			_, date, ok := strings.Cut(line, "\t")
			if !ok {
				commitDate = time.Time{}
				continue
			}
			if commitDate, err = time.Parse(time.RFC3339, date); err != nil {
				commitDate = time.Time{}
			}
			continue
		}
		if line == "" || commitDate.IsZero() {
			continue
		}
		// git log walks newest-first, so the first date seen for a path is the latest one this
		// command reports — see the doc comment for what "latest" excludes (merge commits).
		if _, seen := updated[line]; !seen {
			updated[line] = commitDate
		}
	}
	return updated, nil
}

// resolveUpdated implements the map-then-mtime fallback. A file git has never seen (untracked, or
// the whole vault copied out of its repository) uses its mtime; a file that has neither is left at
// the zero time rather than failing the scan.
func resolveUpdated(root, relPath string, gitUpdated map[string]time.Time) time.Time {
	if t, ok := gitUpdated[relPath]; ok {
		return t
	}
	// #nosec G703 -- relPath comes from walkVault's own output under root, not from a caller.
	if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(relPath))); err == nil {
		return info.ModTime()
	}
	return time.Time{}
}
