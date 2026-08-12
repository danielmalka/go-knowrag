package eval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
)

// ErrNoBaseline is "there is no baseline yet", which is an ordinary state on the first run and must
// stay distinguishable from "the baseline file is corrupt". Both used to arrive at a caller as an
// error from LoadBaseline, and the first is not a failure at all.
var ErrNoBaseline = errors.New("eval: no baseline recorded yet")

// Delta is one report measured against the one before it.
type Delta struct {
	GlobalDelta  float64            `json:"global_delta"`
	PerAreaDelta map[string]float64 `json:"per_area_delta"`

	// Regressed is true iff global recall dropped. It is not "dropped by more than some tolerance":
	// a threshold here would be a second, invisible gate on top of --min-recall.
	Regressed bool `json:"regressed"`
}

// SaveBaseline writes a report as the new baseline, creating the directory if needed.
//
// Written to a temporary file beside the target and renamed into place. What that buys is narrower
// than "corruption is impossible", and the narrow version is the true one: os.WriteFile opens with
// O_TRUNC, so an in-place write never leaves the tail of a longer old baseline behind, and a
// half-written JSON document fails LoadBaseline's Decode rather than loading as a number. What an
// in-place write does destroy is the baseline that was already there — truncate succeeds, the
// process dies (full disk, killed run, container stopped), and the good baseline is gone along with
// the bad one. Renaming leaves the old file untouched until a complete new one exists to replace
// it, so the worst case is "the save did not happen" instead of "the comparison point is gone".
//
// Rename within one directory is atomic on every filesystem this ships on. It is not provable by a
// test in this package: reaching the failure needs a process killed between the truncate and the
// write, and there is no seam here to stage that — the defect-plant round records it as such rather
// than pretending a test covers it.
//
// The temporary name is derived from the target rather than randomised: two `cli eval` runs saving
// the same baseline at the same moment would collide, and that is already a scenario nothing here
// makes safe. Same directory, deliberately — a rename across filesystems is a copy, and a copy is
// not atomic.
func SaveBaseline(path string, r Report) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("eval: creating the directory for %s: %w", path, err)
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("eval: encoding the baseline: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("eval: writing the baseline to %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		// The half-written file is removed rather than left beside the good one, where the next
		// reader would have to guess which is which.
		_ = os.Remove(tmp)
		return fmt.Errorf("eval: replacing the baseline at %s: %w", path, err)
	}
	return nil
}

// LoadBaseline reads a saved baseline.
func LoadBaseline(path string) (Report, error) {
	data, err := os.ReadFile(path) // #nosec G304 — the path is the operator's own argument.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Report{}, fmt.Errorf("%w at %s: %w", ErrNoBaseline, path, err)
		}
		return Report{}, fmt.Errorf("eval: reading the baseline at %s: %w", path, err)
	}

	var r Report
	// Strict, so a baseline written by a future version with a field this one does not know about is
	// an error rather than a silently narrower comparison.
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if derr := dec.Decode(&r); derr != nil {
		return Report{}, fmt.Errorf("eval: the baseline at %s is corrupt: %w", path, derr)
	}
	// Decode stops at the end of the first JSON value and reports no error for whatever follows it.
	// Anything following is a corrupt file — an interrupted write that left the tail of an older,
	// longer baseline behind, or two documents concatenated — and it must not load as a valid
	// baseline with something extra. Requiring EOF is what makes that visible.
	if derr := dec.Decode(new(json.RawMessage)); !errors.Is(derr, io.EOF) {
		return Report{}, fmt.Errorf("eval: the baseline at %s is corrupt: it carries more than one "+
			"JSON document, which is what an interrupted write leaves behind; the first one decoded "+
			"cleanly and must not be trusted as the baseline (trailing content: %v)", path, derr)
	}
	return r, nil
}

// CompareToBaseline is current minus previous: positive is an improvement.
//
// Areas present in only one of the two still appear, with the missing side read as zero — an area
// that appeared or vanished between runs is exactly what a reader needs shown, and dropping it
// would make the delta table silently narrower than the report it describes.
func CompareToBaseline(previous, current Report) Delta {
	d := Delta{
		GlobalDelta:  current.Global.Recall() - previous.Global.Recall(),
		PerAreaDelta: map[string]float64{},
		Regressed:    current.Global.Recall() < previous.Global.Recall(),
	}

	areas := slices.Sorted(maps.Keys(previous.PerArea))
	for _, area := range slices.Sorted(maps.Keys(current.PerArea)) {
		if !slices.Contains(areas, area) {
			areas = append(areas, area)
		}
	}
	for _, area := range areas {
		d.PerAreaDelta[area] = current.PerArea[area].Recall() - previous.PerArea[area].Recall()
	}
	return d
}
