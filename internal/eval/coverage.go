package eval

import (
	"errors"
	"fmt"
	"slices"
)

// CoverageGroup is one row of the weighted coverage table: a set of areas that share a per-area
// range. Min of zero means "no minimum" (the PRD waives the low-value group); Max of zero means
// "no maximum". Both being zero is a group that is listed but unconstrained, which is a legitimate
// way to say "these areas exist and nothing is required of them".
type CoverageGroup struct {
	Name  string   `yaml:"name" json:"name"`
	Areas []string `yaml:"areas" json:"areas"`
	Min   int      `yaml:"min" json:"min"`
	Max   int      `yaml:"max" json:"max"`
}

// CoverageTable is the whole normative table plus the total-count bounds.
//
// Its numbers are declared in the golden-set file, not here — see the GoldenSet doc comment in
// goldenset.go for why (this repository is public and the real area names are the owner's vault
// folders). Nothing in this file asserts a number; it asserts that the declared numbers hold.
type CoverageTable struct {
	MinTotal int             `yaml:"min_total" json:"min_total"`
	MaxTotal int             `yaml:"max_total" json:"max_total"`
	Groups   []CoverageGroup `yaml:"groups" json:"groups"`
}

// ValidateCoverage checks a question set against its table, reporting every violation at once.
//
// Warn-only at run time, strict at authoring time: `cli eval --golden` renders what this returns as
// a warning and keeps going, because a temporarily out-of-range golden set should not stop somebody
// from measuring recall (S10 open question 4, decided). Authoring is where it is fatal.
func ValidateCoverage(questions []GoldenQuestion, table CoverageTable) error {
	// A table with no groups would let every check below pass over an empty loop and return nil,
	// which reads as "coverage is fine" from a validator that looked at nothing — the same shape as
	// an ingestion report rendering "orphans not scanned" as "no orphans found". Refuse instead.
	if len(table.Groups) == 0 {
		return errors.New("eval: the coverage table declares no groups, so this check would pass " +
			"any golden set at all — declare the table in the golden-set file under `coverage:`")
	}
	if table.MinTotal <= 0 && table.MaxTotal <= 0 {
		return errors.New("eval: the coverage table bounds no total, so a one-question golden set " +
			"would satisfy it — set `min_total` and `max_total`")
	}

	counts := map[string]int{}
	for _, q := range questions {
		counts[q.Area]++
	}

	var errs []error
	if table.MinTotal > 0 && len(questions) < table.MinTotal {
		errs = append(errs, fmt.Errorf("total is %d question(s), below the minimum of %d",
			len(questions), table.MinTotal))
	}
	if table.MaxTotal > 0 && len(questions) > table.MaxTotal {
		errs = append(errs, fmt.Errorf("total is %d question(s), above the maximum of %d",
			len(questions), table.MaxTotal))
	}

	known := map[string]bool{}
	for _, g := range table.Groups {
		for _, area := range g.Areas {
			known[area] = true
			n := counts[area]
			if g.Min > 0 && n < g.Min {
				errs = append(errs, fmt.Errorf("area %q (group %q) has %d question(s), below the "+
					"minimum of %d", area, g.Name, n, g.Min))
			}
			if g.Max > 0 && n > g.Max {
				errs = append(errs, fmt.Errorf("area %q (group %q) has %d question(s), above the "+
					"maximum of %d", area, g.Name, n, g.Max))
			}
		}
	}

	// An area no group lists is the typo case, and it is the one violation that would otherwise be
	// invisible: `reserch` satisfies no minimum, so the area it was meant to be reads as empty and
	// this entry reads as belonging nowhere. Sorted so the message is the same on every run.
	var unknown []string
	for area := range counts {
		if !known[area] {
			unknown = append(unknown, area)
		}
	}
	slices.Sort(unknown)
	for _, area := range unknown {
		errs = append(errs, fmt.Errorf("area %q (%d question(s)) is in no group of the coverage "+
			"table, so nothing constrains it — a misspelled area lands here", area, counts[area]))
	}

	return errors.Join(errs...)
}
