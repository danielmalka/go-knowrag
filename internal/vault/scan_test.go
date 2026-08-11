package vault

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/schema"
)

// Fixture exclusion configuration, mirroring the real per-vault lists of PRD-contrato §2.4b.
var (
	fixtureLifeExclusions = Exclusions{
		Folders:   []string{"PowerAI", "resources", "templates"},
		RootFiles: []string{"AGENTS.md", "CLAUDE.md", "CLEITON.md", "GEMINI.md"},
	}
	fixtureWayExclusions = Exclusions{
		RootFiles: []string{"CLAUDE.md", "de-para-projetos.md"},
	}
)

// The fixture vaults' names and area sets — installation configuration (D-26), which the tests
// supply the same way an operator does: `vault` and `area` are no longer schema enums, just names a
// deployment's roster carries.
func lifeVault() string { return "pessoal" }
func wayVault() string  { return "trabalho" }

func lifeAreas() []string {
	return []string{"00-inbox", "mocs", "personal", "research"}
}

func wayAreas() []string {
	return []string{"00-inbox", "alfa", "beta", "gama", "infra", "delta"}
}

func fixtureRoot(name string) string {
	return filepath.Join("testdata", "fixture-vault", name)
}

// copyVault copies a fixture vault into a temp dir, passing every file's bytes through transform
// (nil for a verbatim copy). Copying is what lets a test add a `.git/` — which cannot be committed
// under testdata — or generate a CRLF twin without committing a second fixture.
func copyVault(t *testing.T, src string, transform func([]byte) []byte) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		// #nosec G304 G122 -- both paths are derived from a committed testdata tree and a
		// t.TempDir(); no symlinks, no untrusted input.
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if transform != nil {
			b = transform(b)
		}
		// #nosec G703 -- target is filepath.Join(t.TempDir(), <testdata-relative path>)
		return os.WriteFile(target, b, 0o600)
	})
	if err != nil {
		t.Fatalf("copying fixture vault: %v", err)
	}
	return dst
}

func writeFileIn(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	// #nosec G703 -- rel is a test-local literal under a t.TempDir() root
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func scanFixture(t *testing.T, name string, areas []string, ex Exclusions) ScanResult {
	t.Helper()
	result, err := ScanVault(fixtureRoot(name), name, areas, ex)
	if err != nil {
		t.Fatalf("ScanVault(%s): %v", name, err)
	}
	return result
}

func TestScanVault_FixtureProducesCorrectNotes(t *testing.T) {
	life := scanFixture(t, lifeVault(), lifeAreas(), fixtureLifeExclusions)
	way := scanFixture(t, wayVault(), wayAreas(), fixtureWayExclusions)

	byPath := map[string]Note{}
	types := map[string]bool{}
	areas := map[string]bool{}
	for _, r := range []ScanResult{life, way} {
		for _, n := range r.Notes {
			byPath[r.Vault+"/"+n.Path] = n
			types[n.Type.String()] = true
			areas[r.Vault+"/"+n.Area] = true
			if n.Updated.IsZero() {
				t.Errorf("%s: Updated is the zero time", n.Path)
			}
		}
	}

	if len(byPath) < 14 {
		t.Errorf("fixture produced %d notes, want at least 14", len(byPath))
	}
	if len(types) != 15 {
		t.Errorf("fixture covers %d of the 15 `type` values: %v", len(types), types)
	}
	if len(areas) != 10 {
		t.Errorf("fixture covers %d of the 10 vault/`area` pairs: %v", len(areas), areas)
	}

	// Sampled field-by-field assertions across both vaults, including a note with a `sub` and one
	// without.
	tests := []struct {
		key                                     string
		uid, typ, status, visibility, area, sub string
		tags                                    []string
	}{
		{
			key: "pessoal/research/golang/concurrency.md",
			uid: "0198a7f2-4b31-7c42-9e15-3d8a92c47b04",
			typ: "concept", status: "stable", visibility: "internal",
			area: "research", sub: "golang", tags: []string{"golang", "architecture"},
		},
		{
			key: "pessoal/mocs/central.md",
			uid: "0198a7f2-4b31-7c42-9e15-3d8a92c47b02",
			typ: "moc", status: "stable", visibility: "internal",
			area: "mocs", sub: "", tags: []string{"moc", "index"},
		},
		{
			key: "trabalho/infra/deploy/runbook.md",
			uid: "0198a7f2-4b31-7c42-9e15-3d8a92c47b16",
			typ: "agent", status: "stable", visibility: "internal",
			area: "infra", sub: "deploy", tags: []string{"infra", "deploy"},
		},
		{
			key: "trabalho/00-inbox/captura.md",
			uid: "0198a7f2-4b31-7c42-9e15-3d8a92c47b10",
			typ: "index", status: "draft", visibility: "internal",
			area: "00-inbox", sub: "", tags: []string{"inbox", "index"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.key, func(t *testing.T) {
			n, ok := byPath[tc.key]
			if !ok {
				t.Fatalf("note %s missing from the scan", tc.key)
			}
			if n.UID.String() != tc.uid {
				t.Errorf("uid = %s, want %s", n.UID, tc.uid)
			}
			if n.Type.String() != tc.typ || n.Status.String() != tc.status ||
				n.Visibility.String() != tc.visibility {
				t.Errorf("type/status/visibility = %s/%s/%s, want %s/%s/%s",
					n.Type, n.Status, n.Visibility, tc.typ, tc.status, tc.visibility)
			}
			if n.Area != tc.area || n.Sub != tc.sub {
				t.Errorf("area/sub = %s/%s, want %s/%s", n.Area, n.Sub, tc.area, tc.sub)
			}
			if strings.Join(n.Tags, ",") != strings.Join(tc.tags, ",") {
				t.Errorf("tags = %v, want %v", n.Tags, tc.tags)
			}
			if n.Created.Format("2006-01-02") != "2026-08-07" {
				t.Errorf("created = %v", n.Created)
			}
		})
	}
}

// TestScanVault_ExcludedFoldersNeverReachDerivation pins the regression that PRD-contrato §2.4b's
// ordering exists to prevent: before exclusion ran first, this fixture failed on `.git/` with an
// *UnknownAreaError before reading a single note.
func TestScanVault_ExcludedFoldersNeverReachDerivation(t *testing.T) {
	root := copyVault(t, fixtureRoot(lifeVault()), nil)
	writeFileIn(t, root, ".git/HEAD", "ref: refs/heads/main\n")

	result, err := ScanVault(root, lifeVault(), lifeAreas(), fixtureLifeExclusions)
	if err != nil {
		t.Fatalf("scan failed on a vault with .git/, .obsidian/ and .trash/: %v", err)
	}

	for _, n := range result.Notes {
		for _, forbidden := range []string{".git/", ".obsidian/", ".trash/", "PowerAI/", "AGENTS.md"} {
			if strings.HasPrefix(n.Path, forbidden) || n.Path == forbidden {
				t.Errorf("excluded path %q reached ScanResult.Notes", n.Path)
			}
		}
	}
	// The .trash/ note is well-formed and would parse cleanly; only the dot rule keeps it out. A
	// note the owner deleted must not come back through search.
	if strings.Contains(strings.Join(notePaths(result), " "), ".trash") {
		t.Error(".trash/ note present in the scan result")
	}
}

func notePaths(r ScanResult) []string {
	paths := make([]string, len(r.Notes))
	for i, n := range r.Notes {
		paths[i] = n.Path
	}
	return paths
}

// TestScanVault_UnknownFolderStillFails: exclusion narrows what reaches derivation, it does not
// soften what derivation does then.
func TestScanVault_UnknownFolderStillFails(t *testing.T) {
	root := copyVault(t, fixtureRoot(lifeVault()), nil)
	writeFileIn(t, root, "misfiled/note.md", validNote)

	_, err := ScanVault(root, lifeVault(), lifeAreas(), fixtureLifeExclusions)
	if err == nil {
		t.Fatal("scan succeeded with an unknown first-level folder")
	}
	if !hasUnknownArea(err, "misfiled") {
		t.Errorf("got %v, want an *UnknownAreaError naming %q", err, "misfiled")
	}
}

// TestScanVault_UnknownRootFileFails: a root `.md` outside the configured list is an error, not a
// silent skip — "excluded is a declared decision, unknown is an error" (PRD-contrato §2.4b).
func TestScanVault_UnknownRootFileFails(t *testing.T) {
	root := copyVault(t, fixtureRoot(lifeVault()), nil)
	writeFileIn(t, root, "stray.md", validNote)

	_, err := ScanVault(root, lifeVault(), lifeAreas(), fixtureLifeExclusions)
	if err == nil {
		t.Fatal("scan succeeded with an unlisted root .md file")
	}
	if !hasUnknownArea(err, "") {
		t.Errorf("got %v, want an *UnknownAreaError with an empty Folder", err)
	}
}

func hasUnknownArea(err error, folder string) bool {
	var scanErrs *ScanErrors
	if !errors.As(err, &scanErrs) {
		return false
	}
	for _, e := range scanErrs.Errs {
		var areaErr *UnknownAreaError
		if errors.As(e, &areaErr) && areaErr.Folder == folder {
			return true
		}
	}
	return false
}

// TestScanVault_NamedExclusion_SkipsSilently: one fixture, two configurations. Note the second
// half — with the list empty, `PowerAI/` is not "filed under area powerai", it is an error: the
// corrected §2.4b map has no `powerai` entry, so exclusion is the only thing keeping the folder
// out of an ingestion failure.
func TestScanVault_NamedExclusion_SkipsSilently(t *testing.T) {
	root := fixtureRoot(lifeVault())

	result, err := ScanVault(root, lifeVault(), lifeAreas(), fixtureLifeExclusions)
	if err != nil {
		t.Fatalf("scan with PowerAI excluded returned an error: %v", err)
	}
	for _, path := range notePaths(result) {
		if strings.HasPrefix(path, "PowerAI/") {
			t.Errorf("excluded folder produced note %q", path)
		}
	}

	withoutList := Exclusions{RootFiles: fixtureLifeExclusions.RootFiles}
	if _, err := ScanVault(root, lifeVault(), lifeAreas(), withoutList); !hasUnknownArea(err, "PowerAI") {
		t.Errorf("without the exclusion list, got %v; want an *UnknownAreaError naming PowerAI", err)
	}
}

// TestScanVault_CRLFVaultMatchesLFVault proves T2's normalization survives the whole orchestration.
// The CRLF twin is generated here rather than committed, so the two vaults cannot drift apart.
func TestScanVault_CRLFVaultMatchesLFVault(t *testing.T) {
	src := fixtureRoot(lifeVault())
	crlfRoot := copyVault(t, src, func(b []byte) []byte {
		return bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n"))
	})
	lfRoot := copyVault(t, src, nil)

	lf, err := ScanVault(lfRoot, lifeVault(), lifeAreas(), fixtureLifeExclusions)
	if err != nil {
		t.Fatalf("scanning the LF vault: %v", err)
	}
	crlf, err := ScanVault(crlfRoot, lifeVault(), lifeAreas(), fixtureLifeExclusions)
	if err != nil {
		t.Fatalf("scanning the CRLF vault: %v", err)
	}
	if len(lf.Notes) != len(crlf.Notes) || len(lf.Notes) == 0 {
		t.Fatalf("note counts differ or are empty: LF=%d CRLF=%d", len(lf.Notes), len(crlf.Notes))
	}

	for i, want := range lf.Notes {
		got := crlf.Notes[i]
		if got.Path != want.Path {
			t.Fatalf("note %d: path %q vs %q", i, got.Path, want.Path)
		}
		if got.Body != want.Body {
			t.Errorf("%s: Body differs:\n  LF   = %q\n  CRLF = %q", want.Path, want.Body, got.Body)
		}
		a := schema.SerializeFields(NoteMetadataFields(want))
		b := schema.SerializeFields(NoteMetadataFields(got))
		if a != b {
			t.Errorf("%s: serialized metadata differs:\n  LF   = %q\n  CRLF = %q", want.Path, a, b)
		}
	}
}

func TestScanVault_DuplicateUID_NamesBothPaths(t *testing.T) {
	root := copyVault(t, fixtureRoot(lifeVault()), nil)
	// A verbatim copy of an existing note under a second path: same uid, two places.
	original := filepath.Join(root, "research", "golang", "concurrency.md")
	b, err := os.ReadFile(original) // #nosec G304 -- test fixture path
	if err != nil {
		t.Fatalf("read fixture note: %v", err)
	}
	writeFileIn(t, root, "mocs/copia.md", string(b))

	result, err := ScanVault(root, lifeVault(), lifeAreas(), fixtureLifeExclusions)
	if err == nil {
		t.Fatal("scan succeeded with a duplicate uid")
	}
	if len(result.Notes) != 0 {
		t.Errorf("scan returned %d notes alongside the error; it must be fail-closed", len(result.Notes))
	}

	var dup *DuplicateUIDError
	if !errors.As(err, &dup) {
		t.Fatalf("got %v, want a *DuplicateUIDError", err)
	}
	if dup.UID.String() != "0198a7f2-4b31-7c42-9e15-3d8a92c47b04" {
		t.Errorf("UID = %s", dup.UID)
	}
	paths := dup.PathA + " " + dup.PathB
	for _, want := range []string{"mocs/copia.md", "research/golang/concurrency.md"} {
		if !strings.Contains(paths, want) {
			t.Errorf("error names %q, missing %q", paths, want)
		}
	}
}

// TestScanVault_ThreeNotesShareUID: three collisions must still name two concrete paths, not
// degrade into a vague "duplicates found".
func TestScanVault_ThreeNotesShareUID(t *testing.T) {
	root := copyVault(t, fixtureRoot(lifeVault()), nil)
	b, err := os.ReadFile(filepath.Join(root, "research", "golang", "concurrency.md")) // #nosec G304
	if err != nil {
		t.Fatalf("read fixture note: %v", err)
	}
	writeFileIn(t, root, "mocs/copia-a.md", string(b))
	writeFileIn(t, root, "mocs/copia-b.md", string(b))

	_, err = ScanVault(root, lifeVault(), lifeAreas(), fixtureLifeExclusions)

	var dup *DuplicateUIDError
	if !errors.As(err, &dup) {
		t.Fatalf("got %v, want a *DuplicateUIDError", err)
	}
	if dup.PathA == "" || dup.PathB == "" || dup.PathA == dup.PathB {
		t.Errorf("error names PathA=%q PathB=%q, want two distinct concrete paths", dup.PathA, dup.PathB)
	}
}

func TestCheckCrossVaultDuplicateUIDs(t *testing.T) {
	life := scanFixture(t, lifeVault(), lifeAreas(), fixtureLifeExclusions)
	way := scanFixture(t, wayVault(), wayAreas(), fixtureWayExclusions)

	if err := CheckCrossVaultDuplicateUIDs(life, way); err != nil {
		t.Errorf("fixtures share no uid, but got %v", err)
	}

	// Point IDs do not include `vault`, so a uid repeated across the two vaults collides in Qdrant
	// exactly like a repeat inside one.
	collided := way
	collided.Notes = append(append([]Note{}, way.Notes...), Note{
		Path: "alfa/colisao.md",
		UID:  life.Notes[0].UID,
	})

	err := CheckCrossVaultDuplicateUIDs(life, collided)
	var dup *DuplicateUIDError
	if !errors.As(err, &dup) {
		t.Fatalf("got %v, want a *DuplicateUIDError", err)
	}
	if dup.PathA != life.Notes[0].Path || dup.PathB != "alfa/colisao.md" {
		t.Errorf("error = %+v, want one path from each ScanResult", dup)
	}
}

// TestScanVault_ArchivedNotesAreIncludedNotFiltered: PRD-contrato §2.4 indexes `archived` and
// filters it at query time (S07). Dropping it here would leave it unindexed forever, with nothing
// downstream able to notice.
func TestScanVault_ArchivedNotesAreIncludedNotFiltered(t *testing.T) {
	result := scanFixture(t, lifeVault(), lifeAreas(), fixtureLifeExclusions)

	var found *Note
	for i, n := range result.Notes {
		if n.Path == "research/curadoria/arquivada.md" {
			found = &result.Notes[i]
		}
	}
	if found == nil {
		t.Fatalf("archived note dropped from the scan; got %v", notePaths(result))
	}
	if found.Status != schema.StatusArchived() {
		t.Errorf("Status = %s, want %s", found.Status, schema.StatusArchived())
	}
}

// TestScanVault_EveryStatusIsScannedAndPreserved is T7's behavioural half, and the one that
// actually holds: S02 filters nothing by status, and every registered value comes back carrying the
// value it went in with. PRD-contrato §2.4 indexes `archived` and filters it at query time (S07);
// a status dropped here is a set of notes unindexed forever, with nothing downstream able to notice.
//
// The cases are generated from schema.AllStatuses(), so a value added to the enum is covered the
// day it is added rather than the day someone remembers this test.
func TestScanVault_EveryStatusIsScannedAndPreserved(t *testing.T) {
	root := copyVault(t, fixtureRoot(lifeVault()), nil)

	want := map[string]schema.Status{}
	for i, st := range schema.AllStatuses() {
		rel := fmt.Sprintf("mocs/status-%d.md", i)
		note := strings.Replace(validNote, "status: draft", "status: "+st.String(), 1)
		// A distinct uid per note: a repeat is fatal by design, and would fail this test for a
		// reason that has nothing to do with status.
		note = strings.Replace(note, "uid: 0198a7f2-4b31-7c42-9e15-3d8a92c47b6a",
			fmt.Sprintf("uid: 0198a7f2-4b31-7c42-9e15-3d8a92c47b%02x", 0xd0+i), 1)
		writeFileIn(t, root, rel, note)
		want[rel] = st
	}

	result, err := ScanVault(root, lifeVault(), lifeAreas(), fixtureLifeExclusions)
	if err != nil {
		t.Fatalf("ScanVault: %v", err)
	}

	got := map[string]schema.Status{}
	for _, n := range result.Notes {
		if _, planted := want[n.Path]; planted {
			got[n.Path] = n.Status
		}
	}
	for path, st := range want {
		switch actual, present := got[path]; {
		case !present:
			t.Errorf("note with status %s was dropped by the scan (%s)", st, path)
		case actual != st:
			t.Errorf("%s: Status = %s, want %s", path, actual, st)
		}
	}
}

// TestNoStatusBasedFiltering is the structural half, and covers the one thing the behavioural test
// above cannot: a filter added later behind a config flag still passes a scan of today's fixture.
//
// It looks for the enum accessor, not for a quoted "archived". The previous version searched only
// the quoted literal — and this package's own convention is to never write that literal, so
// `if n.Status == schema.StatusArchived() { continue }`, the form anyone would actually write, went
// straight through the check. Both forms are searched now; the quoted one still matters because a
// filter could compare against `n.Status.String()`.
func TestNoStatusBasedFiltering(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name) // #nosec G304 -- iterating this package's own directory
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		// The quoted literal and the accessor call. Neither appears in prose in a doc comment, so
		// neither form trips on documentation the way a bare `archived` would.
		for _, form := range []string{
			`"` + schema.StatusArchived().String() + `"`,
			"StatusArchived()",
		} {
			if bytes.Contains(src, []byte(form)) {
				t.Errorf("%s mentions %s; internal/vault must not filter by status", name, form)
			}
		}
	}
}

// TestScanVault_EmptyNoteIsSkippedNotFatal documents S02's decision on the one 0-byte note in the
// real corpus: recorded as skipped, not silently dropped and not fatal.
func TestScanVault_EmptyNoteIsSkippedNotFatal(t *testing.T) {
	root := copyVault(t, fixtureRoot(lifeVault()), nil)
	writeFileIn(t, root, "research/curadoria/vazia.md", "")

	result, err := ScanVault(root, lifeVault(), lifeAreas(), fixtureLifeExclusions)
	if err != nil {
		t.Fatalf("an empty note failed the scan: %v", err)
	}
	if len(result.Skipped) != 1 ||
		result.Skipped[0].Path != "research/curadoria/vazia.md" ||
		result.Skipped[0].Reason != reasonEmptyFile {
		t.Errorf("Skipped = %+v, want exactly the empty note", result.Skipped)
	}
	for _, path := range notePaths(result) {
		if path == "research/curadoria/vazia.md" {
			t.Error("empty note produced a Note")
		}
	}
}

// TestScanVault_MalformedNoteSurfacesWithItsPath: collect-all, fail-closed. A batch over hundreds
// of files must report every bad note in one run, and must not hand back a partial list.
func TestScanVault_MalformedNoteSurfacesWithItsPath(t *testing.T) {
	root := copyVault(t, fixtureRoot(lifeVault()), nil)
	writeFileIn(t, root, "mocs/sem-uid.md", strings.Replace(validNote,
		"uid: 0198a7f2-4b31-7c42-9e15-3d8a92c47b6a\n", "", 1))
	writeFileIn(t, root, "personal/sem-tags.md", strings.Replace(validNote,
		"tags: [golang, architecture]\n", "", 1))

	result, err := ScanVault(root, lifeVault(), lifeAreas(), fixtureLifeExclusions)
	if len(result.Notes) != 0 {
		t.Errorf("returned %d notes alongside an error; scan must be fail-closed", len(result.Notes))
	}

	var scanErrs *ScanErrors
	if !errors.As(err, &scanErrs) {
		t.Fatalf("got %v, want a *ScanErrors", err)
	}
	if len(scanErrs.Errs) != 2 {
		t.Errorf("reported %d failures, want both malformed notes in one run: %v",
			len(scanErrs.Errs), scanErrs.Errs)
	}
	for _, want := range []string{"mocs/sem-uid.md", "personal/sem-tags.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error text does not name %q:\n%s", want, err)
		}
	}
}

// TestScanVault_GitUpdatedMapCalledOncePerScan: 731 files must not mean 731 process spawns.
func TestScanVault_GitUpdatedMapCalledOncePerScan(t *testing.T) {
	calls := 0
	real := runGitLog
	runGitLog = func(root string) ([]byte, error) {
		calls++
		return real(root)
	}
	t.Cleanup(func() { runGitLog = real })

	scanFixture(t, lifeVault(), lifeAreas(), fixtureLifeExclusions)

	if calls != 1 {
		t.Errorf("git log was invoked %d times for one ScanVault call, want exactly 1", calls)
	}
}
