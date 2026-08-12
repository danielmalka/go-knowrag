package eval

import (
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func baselinePath(t *testing.T) string {
	t.Helper()
	// A nested directory that does not exist, so the round-trip also proves SaveBaseline creates it —
	// docs/eval/ does not exist on a fresh checkout either (it is gitignored).
	return filepath.Join(t.TempDir(), "eval", "baseline.json")
}

func TestSaveAndLoadBaseline_RoundTripsWithoutLoss(t *testing.T) {
	original := Aggregate(append(hitPattern("alfa", 3, 1), hitPattern("beta", 2, 2)...))
	original.K, original.Mode = 5, "hybrid"
	original.GoldenSetCommit = "abc1234"
	original.Stale = []StaleEntry{{Identity: "deadbeef", Question: "a late one", Commit: "999", Reason: "after the baseline"}}
	original.CoverageWarning = "beta is below its minimum"
	path := baselinePath(t)

	if err := SaveBaseline(path, original); err != nil {
		t.Fatalf("SaveBaseline: %v", err)
	}
	loaded, err := LoadBaseline(path)
	if err != nil {
		t.Fatalf("LoadBaseline: %v", err)
	}

	if !reflect.DeepEqual(original, loaded) {
		t.Errorf("the baseline did not survive the round trip:\n saved: %+v\nloaded: %+v", original, loaded)
	}
	// Spelled out separately because DeepEqual on two equally-empty reports would agree: the failed
	// questions carry the expected/actual pair a later reader needs, and a Failed list flattened to
	// counts would still DeepEqual if both sides lost it.
	if len(loaded.Failed) != 3 || len(loaded.Failed[0].TopK) == 0 {
		t.Errorf("the failed questions lost their actual top-K: %+v", loaded.Failed)
	}
}

// TestLoadBaseline_MissingIsNotCorrupt is T9's last done-when. "There is no baseline yet" is the
// ordinary state of a first run; "the baseline file is broken" is a defect. A caller that cannot
// tell them apart either reports a defect on every fresh checkout or swallows a real one.
func TestLoadBaseline_MissingIsNotCorrupt(t *testing.T) {
	missing := baselinePath(t)

	_, err := LoadBaseline(missing)
	if !errors.Is(err, ErrNoBaseline) {
		t.Fatalf("LoadBaseline on a missing file returned %v, want ErrNoBaseline", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Error("the error does not unwrap to fs.ErrNotExist")
	}

	corrupt := filepath.Join(t.TempDir(), "baseline.json")
	if werr := os.WriteFile(corrupt, []byte(`{"global": "not an object"`), 0o600); werr != nil {
		t.Fatalf("writing the corrupt fixture: %v", werr)
	}
	cerr := LoadBaselineErr(t, corrupt)
	if errors.Is(cerr, ErrNoBaseline) {
		t.Errorf("a corrupt baseline reports as a missing one: %v", cerr)
	}
	if !strings.Contains(cerr.Error(), "corrupt") {
		t.Errorf("the error %q does not say the file is corrupt", cerr)
	}
}

// TestLoadBaseline_UnknownFieldIsRefused keeps a baseline written by a newer version from loading
// as a silently narrower one — the same strict-decoding rule internal/embed/config.go applies to
// its YAML, for the same reason.
func TestLoadBaseline_UnknownFieldIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baseline.json")
	if err := os.WriteFile(path, []byte(`{"global":{"hits":4,"total":5},"future_field":1}`), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}

	if _, err := LoadBaseline(path); err == nil {
		t.Fatal("a baseline carrying an unknown field loaded, so a field this version cannot read " +
			"is silently dropped from the comparison")
	}
}

func LoadBaselineErr(t *testing.T, path string) error {
	t.Helper()
	_, err := LoadBaseline(path)
	if err == nil {
		t.Fatalf("LoadBaseline(%s) succeeded, want an error", path)
	}
	return err
}

func TestCompareToBaseline_FlagsRegression(t *testing.T) {
	// 0.85 and 0.78 as the task document names them: 17/20 against 39/50.
	previous := Report{Global: RecallStat{Hits: 17, Total: 20}, PerArea: map[string]RecallStat{
		"alfa": {Hits: 9, Total: 10}, "beta": {Hits: 8, Total: 10},
	}}
	current := Report{Global: RecallStat{Hits: 39, Total: 50}, PerArea: map[string]RecallStat{
		"alfa": {Hits: 20, Total: 25}, "beta": {Hits: 19, Total: 25},
	}}

	d := CompareToBaseline(previous, current)

	if !d.Regressed {
		t.Errorf("recall fell from %.2f to %.2f and Regressed is false",
			previous.Global.Recall(), current.Global.Recall())
	}
	if want := 0.78 - 0.85; math.Abs(d.GlobalDelta-want) > tolerance {
		t.Errorf("GlobalDelta = %v, want %v", d.GlobalDelta, want)
	}
	if want := 0.80 - 0.90; math.Abs(d.PerAreaDelta["alfa"]-want) > tolerance {
		t.Errorf("alfa delta = %v, want %v", d.PerAreaDelta["alfa"], want)
	}
	if want := 0.76 - 0.80; math.Abs(d.PerAreaDelta["beta"]-want) > tolerance {
		t.Errorf("beta delta = %v, want %v", d.PerAreaDelta["beta"], want)
	}
}

func TestCompareToBaseline_ImprovementAndTie(t *testing.T) {
	base := Report{Global: RecallStat{Hits: 8, Total: 10}, PerArea: map[string]RecallStat{"alfa": {Hits: 8, Total: 10}}}

	better := CompareToBaseline(base, Report{Global: RecallStat{Hits: 9, Total: 10}})
	if better.Regressed {
		t.Error("a run that improved is reported as a regression")
	}
	if better.GlobalDelta <= 0 {
		t.Errorf("GlobalDelta = %v for an improvement", better.GlobalDelta)
	}

	// A tie is not a regression. Making it one would turn every unchanged run red.
	same := CompareToBaseline(base, base)
	if same.Regressed || same.GlobalDelta != 0 {
		t.Errorf("an identical run reports Regressed=%t delta=%v", same.Regressed, same.GlobalDelta)
	}
}

// TestCompareToBaseline_AreaThatAppearedOrVanishedIsStillShown is what keeps the delta table from
// being silently narrower than the report it describes. An area that stopped being covered is
// exactly the change a reader needs to see, and dropping it would hide the reason global recall moved.
func TestCompareToBaseline_AreaThatAppearedOrVanishedIsStillShown(t *testing.T) {
	previous := Report{PerArea: map[string]RecallStat{"alfa": {Hits: 8, Total: 10}, "gone": {Hits: 5, Total: 5}}}
	current := Report{PerArea: map[string]RecallStat{"alfa": {Hits: 9, Total: 10}, "new": {Hits: 4, Total: 5}}}

	d := CompareToBaseline(previous, current)

	for _, area := range []string{"alfa", "gone", "new"} {
		if _, ok := d.PerAreaDelta[area]; !ok {
			t.Errorf("area %q is missing from the delta table", area)
		}
	}
	if want := -1.0; math.Abs(d.PerAreaDelta["gone"]-want) > tolerance {
		t.Errorf("the vanished area's delta = %v, want %v", d.PerAreaDelta["gone"], want)
	}
	if want := 0.8; math.Abs(d.PerAreaDelta["new"]-want) > tolerance {
		t.Errorf("the new area's delta = %v, want %v", d.PerAreaDelta["new"], want)
	}
}

// TestSaveBaseline_LeavesNoTemporaryFileBehind is the visible half of the atomic write: the
// directory holds the baseline and nothing else, so nobody has to guess which of two files is the
// real one.
func TestSaveBaseline_LeavesNoTemporaryFileBehind(t *testing.T) {
	path := baselinePath(t)
	if err := SaveBaseline(path, Aggregate(hitPattern("alfa", 3, 1))); err != nil {
		t.Fatalf("SaveBaseline: %v", err)
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("reading the baseline directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the directory holds %v, want only %s", names, filepath.Base(path))
	}
}

// TestSaveBaseline_FailedRenameLeavesNoWreckage covers the only failure of the temp-and-rename path
// that a test can reach without killing the process mid-write: the rename itself failing.
//
// What temp-and-rename actually buys is narrower than it first looks, and worth stating exactly.
// os.WriteFile opens with O_TRUNC, so an in-place write never leaves the *tail* of a longer old
// baseline behind, and a truncated JSON document fails Decode anyway. What it buys is that an
// interrupted write does not destroy the baseline that was already there: the old file is untouched
// until a complete new one exists to replace it. Reaching that needs a process killed between the
// truncate and the write, which no unit test here can stage — see the report accompanying this
// change. This covers the reachable half.
func TestSaveBaseline_FailedRenameLeavesNoWreckage(t *testing.T) {
	// A directory where the baseline should go. Writing the temporary file beside it succeeds; the
	// rename onto a directory does not.
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.json")
	if err := os.Mkdir(path, 0o750); err != nil {
		t.Fatalf("staging the fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "occupant"), []byte("x"), 0o600); err != nil {
		t.Fatalf("staging the fixture: %v", err)
	}

	if err := SaveBaseline(path, Aggregate(hitPattern("alfa", 3, 1))); err == nil {
		t.Fatal("SaveBaseline reported success writing over a directory")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the directory: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("a failed save left %s behind, where the next reader has to guess which of the "+
				"two files is the baseline", e.Name())
		}
	}
}

// TestLoadBaseline_TrailingContentIsCorrupt is the reader's half. json.Decoder stops at the end of
// the first value and says nothing about the rest, so without an EOF check a file holding a valid
// baseline followed by the tail of an older one loads clean — and the delta then compares against a
// number that was never the whole measurement.
func TestLoadBaseline_TrailingContentIsCorrupt(t *testing.T) {
	good := baselinePath(t)
	if err := SaveBaseline(good, Aggregate(hitPattern("alfa", 3, 1))); err != nil {
		t.Fatalf("SaveBaseline: %v", err)
	}
	valid, err := os.ReadFile(good) // #nosec G304 -- a path under t.TempDir()
	if err != nil {
		t.Fatalf("reading the baseline: %v", err)
	}

	cases := map[string][]byte{
		"a second document":        append(slices.Clone(valid), valid...),
		"the tail of an older one": append(slices.Clone(valid), []byte(`,"hits":9}`)...),
		"trailing junk":            append(slices.Clone(valid), []byte("\x00\x00garbage")...),
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "baseline.json")
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatalf("writing the fixture: %v", err)
			}

			_, err := LoadBaseline(path)
			if err == nil {
				t.Fatal("a file with content after the baseline loaded as a valid baseline")
			}
			if errors.Is(err, ErrNoBaseline) {
				t.Errorf("a corrupt baseline reports as a missing one: %v", err)
			}
			if !strings.Contains(err.Error(), "corrupt") {
				t.Errorf("the error %q does not say the file is corrupt", err)
			}
		})
	}

	// The absent half: whitespace after the document is not corruption, and SaveBaseline writes a
	// trailing newline itself. An EOF check that rejected it would fail on this package's own output.
	padded := filepath.Join(t.TempDir(), "baseline.json")
	// #nosec G703 -- padded is filepath.Join(t.TempDir(), <literal>)
	if err := os.WriteFile(padded, append(slices.Clone(valid), []byte("\n\n  \n")...), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}
	if _, err := LoadBaseline(padded); err != nil {
		t.Errorf("trailing whitespace was read as corruption: %v", err)
	}
}
