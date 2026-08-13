package goldenset

import (
	"errors"
	"fmt"
	"slices"
	"strings"
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

// AreaStatus is one area's standing against the table: what it holds and what the table asks of it.
//
// It is a report and a work queue at once. ValidateCoverage answers "is this set legal", which is
// the question a gate asks; this answers "where is it short", which is the question an author asks,
// and the two must not be different derivations of the same counts.
type AreaStatus struct {
	Area  string
	Group string
	Have  int
	Min   int
	Max   int
}

// Needs is how many more questions this area has to gain before it satisfies its minimum. Zero
// means the minimum is met, or that the group declares none.
func (a AreaStatus) Needs() int {
	if a.Min > a.Have {
		return a.Min - a.Have
	}
	return 0
}

// Full reports whether the table forbids another question in this area. A Max of zero is "no
// maximum" (see CoverageGroup), so it is never full.
func (a AreaStatus) Full() bool { return a.Max > 0 && a.Have >= a.Max }

// CoverageStatus reports every area the table declares, neediest first.
//
// The order is the whole point, and it is a single order rather than one for display and one for
// selection: an author who is shown "research is 5 short" and then handed a note from a full area
// has been given two answers to one question. Areas that are full sort last, so a caller that walks
// this list and stops at the first area it can use never has to re-derive which areas are eligible.
//
// Within equal need, fewer questions first, then the area name — so two runs over the same file
// produce the same order and the same first pick. Areas the table does not list do not appear:
// a question in one is a violation ValidateCoverage reports, not a shortfall to fill.
func CoverageStatus(questions []GoldenQuestion, table CoverageTable) []AreaStatus {
	counts := map[string]int{}
	for _, q := range questions {
		counts[q.Area]++
	}

	var out []AreaStatus
	for _, g := range table.Groups {
		for _, area := range g.Areas {
			out = append(out, AreaStatus{
				Area: area, Group: g.Name, Have: counts[area], Min: g.Min, Max: g.Max,
			})
		}
	}

	slices.SortFunc(out, func(a, b AreaStatus) int {
		if a.Full() != b.Full() {
			if a.Full() {
				return 1
			}
			return -1
		}
		if d := b.Needs() - a.Needs(); d != 0 {
			return d
		}
		if d := a.Have - b.Have; d != 0 {
			return d
		}
		return strings.Compare(a.Area, b.Area)
	})
	return out
}

// Validate refuses a table that constrains nothing, independently of any question set.
//
// It is separate from ValidateCoverage because two callers need it and only one of them has a
// finished question set to check. AppendQuestion (authoring.go) is the other: it is the function that
// writes, so it is where a table that would aim a whole authoring session at nothing has to be
// refused — a guard only the caller applies is a guard the next caller does not.
//
// Both refusals below have the same shape. A table with no groups would let every per-area check pass
// over an empty loop and return nil, which reads as "coverage is fine" from a validator that looked at
// nothing — the same shape as an ingestion report rendering "orphans not scanned" as "no orphans
// found". A table that bounds no total is satisfied by one question.
func (t CoverageTable) Validate() error {
	if len(t.Groups) == 0 {
		return errors.New("goldenset: the coverage table declares no groups, so this check would pass " +
			"any golden set at all — declare the table in the golden-set file under `coverage:`")
	}
	if t.MinTotal <= 0 && t.MaxTotal <= 0 {
		return errors.New("goldenset: the coverage table bounds no total, so a one-question golden set " +
			"would satisfy it — set `min_total` and `max_total`")
	}
	return nil
}

// ValidateCoverage checks a question set against its table, reporting every violation at once.
//
// Warn-only at run time, strict at authoring time: `cli eval --golden` renders what this returns as
// a warning and keeps going, because a temporarily out-of-range golden set should not stop somebody
// from measuring recall (S10 open question 4, decided). Authoring is where it is fatal.
func ValidateCoverage(questions []GoldenQuestion, table CoverageTable) error {
	// Returned alone rather than joined with the rest: a table that constrains nothing makes every
	// violation below meaningless, and reporting them together would bury the one that matters.
	if err := table.Validate(); err != nil {
		return err
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
