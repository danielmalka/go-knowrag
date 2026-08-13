package vault

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeTree materializes a map of slash-separated relative paths to contents under a temp dir.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return root
}

// trashNote is a real, well-formed note, not a poison stub: the property under test is that a note
// the owner deleted does not come back through search, and a malformed stub would be excluded by
// the parser even if the walk let it through.
const trashNote = `---
uid: 0198a7f2-4b31-7c42-9e15-3d8a92c47b09
type: concept
status: stable
created: 2026-08-07
tags: [deleted]
---

# Deleted on purpose
`

func TestWalkVault_SkipsDotDirsAndNonMarkdown(t *testing.T) {
	root := writeTree(t, map[string]string{
		".obsidian/config":       "{}",
		".git/HEAD":              "ref: refs/heads/main\n",
		".trash/deleted-note.md": trashNote,
		"notes/a.md":             "a",
		"notes/image.png":        "png",
		"notes/sub/b.md":         "b",
		"notes/.hidden/c.md":     "c",
	})

	got, _, err := walkVault(root, Exclusions{})
	if err != nil {
		t.Fatalf("walkVault returned an error: %v", err)
	}

	want := []string{"notes/a.md", "notes/sub/b.md"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("walkVault() = %v, want %v", got, want)
	}
}

// TestWalkVault_NamedExclusionList_SkipsSilently proves the named list is data, not a compiled-in
// constant: the same tree, two configurations, two results, inside one test binary.
func TestWalkVault_NamedExclusionList_SkipsSilently(t *testing.T) {
	root := writeTree(t, map[string]string{
		"notes/a.md":      "a",
		"PowerAI/spec.md": "spec",
	})

	excluded, _, err := walkVault(root, Exclusions{Folders: []string{"PowerAI"}})
	if err != nil {
		t.Fatalf("walkVault with exclusion returned an error: %v", err)
	}
	if want := []string{"notes/a.md"}; !reflect.DeepEqual(excluded, want) {
		t.Errorf("with exclusion: got %v, want %v", excluded, want)
	}

	included, _, err := walkVault(root, Exclusions{})
	if err != nil {
		t.Fatalf("walkVault without exclusion returned an error: %v", err)
	}
	if want := []string{"PowerAI/spec.md", "notes/a.md"}; !reflect.DeepEqual(included, want) {
		t.Errorf("without exclusion: got %v, want %v", included, want)
	}
}

// TestWalkVault_NamedExclusionIsCaseInsensitiveAndFirstLevelOnly pins both halves of the *bare
// name* rule, which nested entries did not change. Case-insensitive because the real folder is
// `PowerAI` while the contract writes `powerai`. First level only because a bare name names an
// area, not an arbitrary subtree — a `resources/` nested inside `research/` is a different thing
// from the vault's top-level `resources/`, and reaching it takes the slashed form asserted in
// TestWalkVault_NestedExclusionSkipsOneSubtree.
func TestWalkVault_NamedExclusionIsCaseInsensitiveAndFirstLevelOnly(t *testing.T) {
	root := writeTree(t, map[string]string{
		"resources/top.md":                    "top",
		"research/ingles/resources/nested.md": "nested",
	})

	got, _, err := walkVault(root, Exclusions{Folders: []string{"RESOURCES"}})
	if err != nil {
		t.Fatalf("walkVault returned an error: %v", err)
	}
	want := []string{"research/ingles/resources/nested.md"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("walkVault() = %v, want %v", got, want)
	}
}

// nestedTree is the shape D-40 is about: one folder, two levels down, holding an artifact whose
// `.md` is an accident, inside an area that also holds real notes. Its neighbours exist to prove
// what the exclusion must NOT reach — including `research/14`, whose name is a string prefix of
// `research/14-internal-work`.
func nestedTree() map[string]string {
	return map[string]string{
		"research/14-internal-work/landing/PACKET.md":  "briefing, never a note",
		"research/14-internal-work/landing/index.html": "<html></html>",
		"research/14-internal-work/real.md":            "a real note beside it",
		"research/14/keep.md":                          "prefix neighbour, must survive",
		"research/note.md":                             "a real note in the area",
		"landing/top.md":                               "same leaf name at first level",
	}
}

// TestWalkVault_NestedExclusionSkipsOneSubtree is the whole point of D-40: excluding the offending
// folder must not cost the area that contains it.
func TestWalkVault_NestedExclusionSkipsOneSubtree(t *testing.T) {
	root := writeTree(t, nestedTree())

	got, _, err := walkVault(root, Exclusions{Folders: []string{"research/14-internal-work/landing"}})
	if err != nil {
		t.Fatalf("walkVault returned an error: %v", err)
	}
	want := []string{
		"landing/top.md",
		"research/14/keep.md",
		"research/14-internal-work/real.md",
		"research/note.md",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("walkVault() = %v, want %v", got, want)
	}
}

// TestWalkVault_NestedExclusionMatchesSegmentsNotPrefix is the trap a naive strings.HasPrefix
// falls into: `research/14` is a real sibling folder AND a character-for-character prefix of
// `research/14-internal-work`. Excluding one must leave the other whole, in both directions.
func TestWalkVault_NestedExclusionMatchesSegmentsNotPrefix(t *testing.T) {
	root := writeTree(t, nestedTree())

	for _, tc := range []struct {
		entry string
		want  []string
	}{
		{
			// The short sibling: excluding it must not swallow `14-internal-work`.
			entry: "research/14",
			want: []string{
				"landing/top.md",
				"research/14-internal-work/landing/PACKET.md",
				"research/14-internal-work/real.md",
				"research/note.md",
			},
		},
		{
			// The long one: excluding it must not swallow `research/14`.
			entry: "research/14-internal-work",
			want: []string{
				"landing/top.md",
				"research/14/keep.md",
				"research/note.md",
			},
		},
		{
			// A leading segment on its own is not a subtree exclusion, and a trailing one is not a
			// parent exclusion: only the whole path matches.
			entry: "14-internal-work",
			want: []string{
				"landing/top.md",
				"research/14/keep.md",
				"research/14-internal-work/landing/PACKET.md",
				"research/14-internal-work/real.md",
				"research/note.md",
			},
		},
	} {
		t.Run(tc.entry, func(t *testing.T) {
			got, _, err := walkVault(root, Exclusions{Folders: []string{tc.entry}})
			if err != nil {
				t.Fatalf("walkVault returned an error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("walkVault(exclude %q) = %v, want %v", tc.entry, got, tc.want)
			}
		})
	}
}

// TestWalkVault_NestedExclusionSpelling pins the two ways an operator can write the same entry and
// still mean it. Case, because the vaults sit on a case-insensitive Windows filesystem while the
// contract writes lowercase; the backslash, because a path pasted out of Explorer arrives with
// them and internal/config/config.go passes the string through untouched apart from commas and
// spaces. Each spelling must produce exactly what the canonical one produces.
func TestWalkVault_NestedExclusionSpelling(t *testing.T) {
	root := writeTree(t, nestedTree())

	canonical, _, err := walkVault(root, Exclusions{Folders: []string{"research/14-internal-work/landing"}})
	if err != nil {
		t.Fatalf("walkVault (canonical): %v", err)
	}

	for _, entry := range []string{
		`Research/14-Internal-Work/LANDING`,
		`research\14-internal-work\landing`,
		`/research/14-internal-work/landing/`,
		`Research\14-Internal-Work\Landing\`,
	} {
		got, _, err := walkVault(root, Exclusions{Folders: []string{entry}})
		if err != nil {
			t.Fatalf("walkVault(%q): %v", entry, err)
		}
		if !reflect.DeepEqual(got, canonical) {
			t.Errorf("walkVault(exclude %q) = %v, want %v (same as the canonical spelling)",
				entry, got, canonical)
		}
	}
}

// TestScanVault_NonNoteInExcludedSubtreeDoesNotAbort is the occurrence D-40 was written from,
// end to end: a `.md` with no frontmatter at all, two levels down, that broke every ingestion for
// two days. Excluded, the scan must succeed AND still return the real notes of the same area —
// a scan that skipped the whole area would also pass an "err == nil" assertion.
func TestScanVault_NonNoteInExcludedSubtreeDoesNotAbort(t *testing.T) {
	root := writeTree(t, map[string]string{
		"research/14-internal-work/landing/PACKET.md": "# Session briefing\n\nno frontmatter here\n",
		"research/keeper.md":                          trashNote,
	})

	ex := Exclusions{Folders: []string{"research/14-internal-work/landing"}}
	result, err := ScanVault(root, lifeVault(), lifeAreas(), ex)
	if err != nil {
		t.Fatalf("ScanVault with the offending folder excluded: %v", err)
	}
	if len(result.Notes) != 1 || result.Notes[0].Path != "research/keeper.md" {
		t.Fatalf("got notes %+v, want only research/keeper.md", result.Notes)
	}

	// Without the exclusion the same tree must still fail, or the test above proves nothing about
	// the exclusion — it would pass just as well if the parser had stopped rejecting the file.
	if _, err := ScanVault(root, lifeVault(), lifeAreas(), Exclusions{}); err == nil {
		t.Fatal("ScanVault accepted a `.md` with no frontmatter block; want a refusal")
	}
}

// TestFrontmatterError_NoBlockNamesTheWayOut guards the operator-facing half of D-40. The message
// for "no block at all" must offer the exclusion, and must NOT go back to the bare sentence that
// left an owner with a broken ingestion and nothing to act on — the absent wording is what keeps
// the old text from creeping back.
func TestFrontmatterError_NoBlockNamesTheWayOut(t *testing.T) {
	_, err := parseFrontmatter("research/14-internal-work/landing/PACKET.md",
		[]byte("# Session briefing\n\nno frontmatter here\n"))
	if err == nil {
		t.Fatal("parseFrontmatter accepted a file with no frontmatter block")
	}
	msg := err.Error()
	for _, want := range []string{"probably not a note", "exclude_folders", "area/sub/folder"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not contain %q", msg, want)
		}
	}
	// Frontmatter that is present and merely wrong is a real note with a defect, and must not be
	// told to exclude its folder.
	_, badErr := parseFrontmatter("research/note.md", []byte("---\nuid: nope\n---\nbody\n"))
	if badErr == nil {
		t.Fatal("parseFrontmatter accepted an invalid uid")
	}
	if strings.Contains(badErr.Error(), "exclude_folders") {
		t.Errorf("an invalid-frontmatter message offers the exclusion way out: %q", badErr.Error())
	}
}

func TestWalkVault_RootFileExclusion(t *testing.T) {
	files := map[string]string{
		"AGENTS.md":  "agents",
		"CLAUDE.md":  "claude",
		"stray.md":   "stray",
		"notes/a.md": "a",
	}
	root := writeTree(t, files)

	got, _, err := walkVault(root, Exclusions{RootFiles: []string{"agents.md", "claude.md"}})
	if err != nil {
		t.Fatalf("walkVault returned an error: %v", err)
	}
	// stray.md survives the walk on purpose: an unlisted root file is not silently skipped, it is
	// handed to derivation so it fails there as an explicit misfiling error.
	want := []string{"notes/a.md", "stray.md"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("walkVault() = %v, want %v", got, want)
	}
}

// TestWalkVault_RefusesSymlinks pins the security property the walk's own comment used to claim
// for free. filepath.WalkDir does not *follow* links, but it does report a link to a file as an
// ordinary non-directory entry, so before this check a `*.md` link passed the extension test and
// the caller's os.ReadFile resolved it — reading a file outside the vault root. A link to a
// directory is the mirror failure: WalkDir will not descend it, so notes under it vanish from the
// index with no signal. Both are refused, and both are asserted here.
func TestWalkVault_RefusesSymlinks(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.md")
	if err := os.WriteFile(secret, []byte("TOP SECRET, outside vault root\n"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	hidden := filepath.Join(outside, "tree")
	if err := os.MkdirAll(hidden, 0o750); err != nil {
		t.Fatalf("mkdir outside tree: %v", err)
	}

	root := writeTree(t, map[string]string{"research/real.md": "real"})
	if err := os.Symlink(secret, filepath.Join(root, "research", "leaked.md")); err != nil {
		t.Skipf("symlinks unavailable on this filesystem: %v", err)
	}
	if err := os.Symlink(hidden, filepath.Join(root, "linked-dir")); err != nil {
		t.Fatalf("symlink dir: %v", err)
	}

	got, violations, err := walkVault(root, Exclusions{})
	if err != nil {
		t.Fatalf("walkVault: %v", err)
	}

	want := []string{filepath.ToSlash(filepath.Join("research", "real.md"))}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("walkVault returned %v, want %v — a symlink must never reach the caller", got, want)
	}
	if len(violations) != 2 {
		t.Fatalf("got %d violations, want 2 (one file link, one dir link): %v", len(violations), violations)
	}
	for _, v := range violations {
		var se *SymlinkError
		if !errors.As(v, &se) {
			t.Errorf("violation %v is not a *SymlinkError", v)
			continue
		}
		if se.Target == "" {
			t.Errorf("%s: SymlinkError.Target is empty; the operator needs to see where it points", se.Path)
		}
	}
}

// TestScanVault_SymlinkContentIsNeverRead is the end-to-end half: the walk refusing the entry is
// only useful if ScanVault also stops, rather than reporting the violation and reading the file
// anyway. The link here points at a *valid* note, so nothing but the symlink rule can reject it.
func TestScanVault_SymlinkContentIsNeverRead(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.md")
	if err := os.WriteFile(secret, []byte(trashNote), 0o600); err != nil {
		t.Fatalf("write outside note: %v", err)
	}

	root := writeTree(t, map[string]string{"mocs/real.md": trashNote})
	if err := os.Symlink(secret, filepath.Join(root, "mocs", "leaked.md")); err != nil {
		t.Skipf("symlinks unavailable on this filesystem: %v", err)
	}

	result, err := ScanVault(root, lifeVault(), lifeAreas(), Exclusions{})
	if err == nil {
		t.Fatal("ScanVault succeeded on a vault containing a symlink; want a refusal")
	}
	var se *SymlinkError
	if !errors.As(err, &se) {
		t.Fatalf("error does not unwrap to *SymlinkError: %v", err)
	}
	if len(result.Notes) != 0 {
		t.Errorf("got %d notes alongside the error; ScanVault must return the zero result", len(result.Notes))
	}
	for _, n := range result.Notes {
		if strings.Contains(n.Path, "leaked") {
			t.Errorf("symlinked path %s reached the result", n.Path)
		}
	}
}
