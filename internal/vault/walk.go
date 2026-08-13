package vault

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// Exclusions is one vault's exclusion configuration, per PRD-contrato §2.4b. Both lists are data
// passed in by the caller (S01's config loader), never package constants: re-including PowerAI is
// meant to be one config line, not a code change.
//
// Matching is case-insensitive because the real folders are mixed-case on a case-insensitive
// Windows filesystem while the contract writes them lowercase.
type Exclusions struct {
	// Folders are folder paths skipped in silence, each compared against the whole path relative to
	// the vault root. A bare name stays first-level-only — the list names areas, so a `resources/`
	// nested deep inside `research/` is NOT excluded by an entry for "resources". An entry that
	// carries slashes names one nested subtree and excludes exactly it: `area/sub/folder`
	// skips that folder and everything under it, and nothing else.
	//
	// The nested form exists because a `.md` that was never a note — a design-session briefing,
	// sibling to an `index.html`, whose extension is an accident — lands inside an area that holds
	// real notes. Without it the only way to skip that one folder was to exclude the whole area
	// (D-40). Comparison is whole-path, so it is by path segment and never by string prefix:
	// `area/sub` does not exclude `area/sub-other`.
	Folders []string
	// RootFiles are `.md` file names at the vault root skipped in silence — vault infrastructure
	// (agent instructions, a project index) that exists and will keep existing, so rejecting it
	// would fail every ingestion. A root `.md` outside this list is still an error.
	RootFiles []string
}

// walkVault returns every indexable markdown path under root, relative to root, slash-separated
// and deterministically ordered, applying PRD-contrato §2.4b's exclusion rules *during* the walk.
//
// The order is normative, not an optimization: both vaults are git repositories, so `.git/` is a
// first-level folder outside the area map. Excluding after deriving would fail every ingestion
// before it read a note.
//
// The dot rule is deliberately a rule and not a list of three names. Obsidian moves a deleted note
// into `.trash/` inside the vault as an intact `.md`; indexing it resurrects in search exactly the
// notes the owner deleted, including those deleted for being sensitive. A closed list passes every
// other case and loses that one the day a plugin renames its trash folder.
//
// It returns three values because two kinds of failure are not the same kind. violations holds
// per-entry contract breaches (today: symlinks) and is collected rather than returned on the first
// one, so a caller reports every offender in a single pass — the same reason ScanErrors exists. err
// is a genuine filesystem failure, which aborts the walk.
func walkVault(root string, ex Exclusions) (paths []string, violations []error, err error) {
	folders := folderSet(ex.Folders)
	rootFiles := lowerSet(ex.RootFiles)

	var out []string
	// #nosec G703 -- root is the operator-configured vault path (env var / config file), the one
	// trusted input this package takes. WalkDir does not follow symlinks, and the symlink check
	// below refuses the entries it does report, so nothing the walk hands back can resolve outside
	// the tree the operator pointed it at.
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)

		// Checked before every other rule, including the .md extension and both exclusion lists: a
		// link is refused for where it points, which has nothing to do with what it is named. Note
		// that d.IsDir() is false for a link to a directory too — WalkDir reports the link itself,
		// never the target — so this branch is the only one that ever sees one.
		if d.Type()&fs.ModeSymlink != 0 {
			target, linkErr := os.Readlink(path)
			if linkErr != nil {
				target = ""
			}
			violations = append(violations, &SymlinkError{Path: rel, Target: target})
			return nil
		}

		if d.IsDir() {
			if rel == "." {
				return nil
			}
			if strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			// rel, not d.Name(): the whole path is what the entry is compared against, which is what
			// keeps a bare name first-level-only and a slashed entry pinned to one subtree.
			if _, excluded := folders[foldName(rel)]; excluded {
				return fs.SkipDir
			}
			return nil
		}

		if !strings.EqualFold(filepath.Ext(rel), ".md") {
			return nil
		}
		if name, first := isFirstLevel(rel); first {
			if _, excluded := rootFiles[foldName(name)]; excluded {
				return nil
			}
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return out, violations, nil
}

// isFirstLevel reports whether rel names an entry directly under the vault root, and returns that
// entry's name. The root-file exclusion list applies at the first level only, so this is the single
// place that decides what "first level" means for it. Folders no longer go through it: their rule
// is whole-path equality, which makes a bare name first-level-only on its own.
func isFirstLevel(rel string) (string, bool) {
	if strings.Contains(rel, "/") {
		return "", false
	}
	return rel, true
}

// folderSet turns the configured folder exclusions into the lowercase, slash-separated keys
// walkVault compares a relative directory path against.
//
// Backslashes fold into slashes, and empty segments — leading, trailing, doubled — are dropped, so
// `\Arcanto\14-internal-work\` and `area/sub/folder` are one entry. Both spellings reach
// here verbatim: internal/config/config.go only splits the setting on commas and trims spaces, so
// an operator who pasted a path out of Windows Explorer would otherwise have configured something
// that matches nothing while looking configured.
func folderSet(entries []string) map[string]struct{} {
	set := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		segments := strings.FieldsFunc(strings.ReplaceAll(e, `\`, "/"), func(r rune) bool {
			return r == '/'
		})
		if len(segments) == 0 {
			continue
		}
		set[foldName(strings.Join(segments, "/"))] = struct{}{}
	}
	return set
}

// foldName is the one spelling every comparison in this file goes through, on both sides: the
// configured entry and the name the filesystem reports.
//
// strings.ToLower alone was not enough, and the gap is invisible until it bites. It case-folds and
// does not normalise: "é" written as one rune (NFC) and as "e" plus a combining accent (NFD) stay
// different strings through it. Apple's filesystems hand back NFD; a value typed into a config file
// or pasted from a browser is usually NFC. An accented folder would then be configured as excluded
// and not excluded, with no error — the same silence that let D-40 break ingestion for two days,
// only inverted: the non-note file comes back into the index instead of aborting the run.
//
// Both vaults are ASCII today, so this buys nothing right now. It is here because the cost of
// finding out is a silent wrong index, and NFC of ASCII is the same ASCII.
func foldName(s string) string { return strings.ToLower(norm.NFC.String(s)) }

func lowerSet(names []string) map[string]struct{} {
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[foldName(n)] = struct{}{}
	}
	return set
}
