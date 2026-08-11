package ingest

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// goldenReport is a run with every field of the JSON shape populated, including one note in each
// state the machine can end in. A field left at its zero value would be a field the golden fixture
// cannot detect a rename of.
func goldenReport() Report {
	return Report{
		Mode: "incremental",
		Results: []NoteResult{
			{Path: "areas/kept.md", UID: uuid.MustParse("0198a7f2-4b31-7c42-9e15-3d8a92c47b01"),
				State: StateSkipped, Chunks: 2},
			{Path: "areas/rewritten.md", UID: uuid.MustParse("0198a7f2-4b31-7c42-9e15-3d8a92c47b02"),
				State: StatePruned, Chunks: 4},
			{Path: "areas/written.md", UID: uuid.MustParse("0198a7f2-4b31-7c42-9e15-3d8a92c47b03"),
				State: StateUpsertConfirmed, Chunks: 1},
			{Path: "areas/stopped.md", UID: uuid.MustParse("0198a7f2-4b31-7c42-9e15-3d8a92c47b04"),
				State: StateEmbedded, Chunks: 3},
			{Path: "areas/broken.md", UID: uuid.MustParse("0198a7f2-4b31-7c42-9e15-3d8a92c47b05"),
				State: StateFailed, Err: errors.New("chunking: frontmatter has no title")},
		},
		OrphansScanned: true,
		Orphans: []Orphan{
			{UID: uuid.MustParse("0198a7f2-4b31-7c42-9e15-3d8a92c47b06"),
				Vault: "pessoal", Path: "areas/deleted.md", Points: 2},
			{UID: uuid.MustParse("0198a7f2-4b31-7c42-9e15-3d8a92c47b07"),
				Vault: "pessoal", Path: "areas/gone.md", Points: 3},
		},
		OnDisk: []Orphan{
			{UID: uuid.MustParse("0198a7f2-4b31-7c42-9e15-3d8a92c47b08"),
				Vault: "pessoal", Path: "areas/excluded.md", Points: 1},
		},
		PointsPruned: 5,
	}
}

// TestReport_JSON_MatchesGolden is the schema contract of `--json`, and it is a golden fixture
// rather than a set of field assertions on purpose: the operator's consumer is a script reading
// keys, so a rename is a breaking change even when every value is still correct. A field-by-field
// test would keep passing through exactly that rename.
//
// It compares the indented form so a failure prints a diff a human can read, but what is under test
// is json.Marshal's output — Indent reorders nothing.
func TestReport_JSON_MatchesGolden(t *testing.T) {
	encoded, err := goldenReport().JSON()
	if err != nil {
		t.Fatalf("Report.JSON(): %v", err)
	}

	var pretty bytes.Buffer
	if err := json.Indent(&pretty, encoded, "", "  "); err != nil {
		t.Fatalf("indenting the report: %v", err)
	}

	path := filepath.Join("testdata", "report_golden.json")
	want, err := os.ReadFile(path) // #nosec G304 -- a literal path inside the package
	if err != nil {
		t.Fatalf("reading the golden fixture: %v", err)
	}

	if got := pretty.String(); got != strings.TrimRight(string(want), "\n") {
		t.Errorf("the --json shape changed. Any consumer reading these keys breaks with it; if the "+
			"change is intended, update %s deliberately.\n\ngot:\n%s\n\nwant:\n%s", path, got, want)
	}
}

// TestReport_JSON_EmptyListsAreArraysNotNull covers the clean run, which is the one every consumer
// sees most often. A nil Go slice marshals to `null`, and a script iterating `.orphans` would then
// have to special-case the case where nothing is wrong.
func TestReport_JSON_EmptyListsAreArraysNotNull(t *testing.T) {
	encoded, err := Report{Mode: "incremental", OrphansScanned: true}.JSON()
	if err != nil {
		t.Fatalf("Report.JSON(): %v", err)
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("the report does not parse as JSON: %v\n%s", err, encoded)
	}
	for _, key := range []string{"orphans", "indexed_not_scanned", "errors"} {
		if got := string(decoded[key]); got != "[]" {
			t.Errorf("%q is %s on a clean run, want []", key, got)
		}
	}
}

// TestReport_String_OrphanSection covers the three answers the human report has to tell apart, and
// the third is the reason the section is rendered even when it is empty: an operator reading no
// orphan line at all concludes there are none, and on a run whose snapshot failed that conclusion is
// wrong and unprompted.
//
// The listed case carries PointsPruned == 0 deliberately — the orphans have to appear in the report
// of a run that did not prune, because that report is what the operator reads before deciding to.
func TestReport_String_OrphanSection(t *testing.T) {
	orphan := Orphan{UID: uuid.MustParse("0198a7f2-4b31-7c42-9e15-3d8a92c47b06"),
		Vault: "pessoal", Path: "areas/deleted.md", Points: 2}

	tests := map[string]struct {
		report     Report
		want       []string
		wantAbsent []string
	}{
		"scanned and clean": {
			report: Report{OrphansScanned: true},
			want:   []string{"orphans: none"},
		},
		"listed but not pruned": {
			report: Report{OrphansScanned: true, Orphans: []Orphan{orphan}},
			want: []string{"1 note(s) deleted from the vault", "--prune", "pessoal/areas/deleted.md",
				orphan.UID.String(), "2 point(s) still indexed"},
			wantAbsent: []string{"removed"},
		},
		"pruned in full": {
			report: Report{OrphansScanned: true, Orphans: []Orphan{orphan}, PointsPruned: 2},
			want:   []string{"all removed", "pessoal/areas/deleted.md"},
		},
		// The one the aggregate verb got wrong: Prune stops at the first delete that fails and
		// returns what it removed before stopping (prune.go), so this run left the second note in the
		// index. A single "pruned" over the whole list would name it as deleted while a search can
		// still return it — the report asserting a state the index does not have.
		"pruned partway and then stopped": {
			report: Report{
				OrphansScanned: true,
				Orphans:        []Orphan{orphan, {UID: orphan.UID, Vault: "pessoal", Path: "areas/other.md", Points: 3}},
				PointsPruned:   2,
			},
			want:       []string{"of which 2 removed", "remaining 3 are still indexed", "areas/other.md"},
			wantAbsent: []string{"all removed"},
		},
		// A note the scan did not return whose file is still on disk: reported, never called deleted,
		// and never counted into the prune list. The wording is the assertion — "deleted from the
		// vault" over a file sitting right there is what talks an operator into removing it.
		"indexed but not confirmed deleted": {
			report: Report{OrphansScanned: true, OnDisk: []Orphan{orphan}},
			// "could not be read" is asserted alongside "still there" because the list holds both
			// facts and the second one used to be missing from the sentence — a file the check could
			// not read would have been described as a file the check saw.
			want: []string{"indexed but not confirmed deleted", "still there", "could not be read",
				"pessoal/areas/deleted.md"},
			wantAbsent: []string{"orphans: none", "deleted from the vault"},
		},
		"never scanned": {
			report:     Report{},
			want:       []string{"not scanned"},
			wantAbsent: []string{"orphans: none"},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := tc.report.String()
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("report %q does not contain %q", got, want)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("report %q contains %q, which does not describe this run", got, absent)
				}
			}
		})
	}
}

// TestReport_PointsWritten_CountsOnlyConfirmedWrites pins the conservative half of the count: a note
// that embedded and stopped wrote nothing, and a failed note may have landed a partial write nobody
// can size. Counting either would put a guess in the operator's summary.
func TestReport_PointsWritten_CountsOnlyConfirmedWrites(t *testing.T) {
	// 4 from the pruned note, 1 from the confirmed one; the skipped, embedded and failed notes carry
	// chunk counts that must not be added.
	if got := goldenReport().PointsWritten(); got != 5 {
		t.Errorf("PointsWritten() = %d, want 5", got)
	}
}
