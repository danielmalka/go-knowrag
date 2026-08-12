package eval

import (
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
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
