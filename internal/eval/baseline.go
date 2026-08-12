package eval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
func SaveBaseline(path string, r Report) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("eval: creating the directory for %s: %w", path, err)
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("eval: encoding the baseline: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("eval: writing the baseline to %s: %w", path, err)
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
