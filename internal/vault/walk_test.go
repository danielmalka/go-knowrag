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

// TestWalkVault_NamedExclusionIsCaseInsensitiveAndFirstLevelOnly pins both halves of the named
// rule. Case-insensitive because the real folder is `PowerAI` while the contract writes `powerai`.
// First level only because the list names areas, not arbitrary subtrees — a `resources/` nested
// inside `research/` is a different thing from the vault's top-level `resources/`.
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
