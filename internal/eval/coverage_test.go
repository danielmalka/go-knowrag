package eval

import (
	"strings"
	"testing"
)

// testTable is the shape of the real weighted table with neutral names and small numbers: a
// high-value group with a floor and a ceiling, a medium one, and a low one that is waived.
func testTable() CoverageTable {
	return CoverageTable{
		MinTotal: 10,
		MaxTotal: 20,
		Groups: []CoverageGroup{
			{Name: "high", Areas: []string{"alfa", "beta"}, Min: 4, Max: 6},
			{Name: "medium", Areas: []string{"gama"}, Min: 2, Max: 3},
			{Name: "low", Areas: []string{"delta"}},
		},
	}
}

// questionsFor builds n questions in each named area. The text is unique per question because
// EntryIdentity and git -S both key on it (provenance.go).
func questionsFor(counts map[string]int) []GoldenQuestion {
	var out []GoldenQuestion
	for _, area := range []string{"alfa", "beta", "gama", "delta", "epsilon"} {
		for i := range counts[area] {
			out = append(out, GoldenQuestion{
				Question: area + " question " + string(rune('a'+i)),
				UID:      uidA,
				Area:     area,
				Author:   "owner",
				Date:     "2026-08-11",
			})
		}
	}
	return out
}

// satisfying is a question set the table accepts: 4+4+2 = 10, the minimum total, with delta waived.
func satisfying() map[string]int {
	return map[string]int{"alfa": 4, "beta": 4, "gama": 2}
}

func TestValidateCoverage_SatisfiedTablePasses(t *testing.T) {
	if err := ValidateCoverage(questionsFor(satisfying()), testTable()); err != nil {
		t.Fatalf("a satisfying golden set was rejected: %v", err)
	}

	// The waived group with entries in it is still fine — a minimum of zero is not a maximum of zero.
	counts := satisfying()
	counts["delta"] = 3
	if err := ValidateCoverage(questionsFor(counts), testTable()); err != nil {
		t.Errorf("entries in a group with no minimum were rejected: %v", err)
	}
}

func TestValidateCoverage_HighValueAreaBelowMinimum_ReturnsError(t *testing.T) {
	counts := satisfying()
	counts["alfa"] = 3

	err := ValidateCoverage(questionsFor(counts), testTable())
	if err == nil {
		t.Fatal("an area below its minimum was accepted")
	}
	for _, want := range []string{`"alfa"`, "3", "minimum of 4"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error %q does not name %q — an author cannot act on it", err, want)
		}
	}
}

func TestValidateCoverage_ReportsEveryViolation(t *testing.T) {
	cases := map[string]struct {
		counts map[string]int
		want   []string
	}{
		"area above its maximum": {
			counts: map[string]int{"alfa": 7, "beta": 4, "gama": 2},
			want:   []string{`"alfa"`, "7", "maximum of 6"},
		},
		"medium area below its minimum": {
			counts: map[string]int{"alfa": 4, "beta": 4, "gama": 1},
			want:   []string{`"gama"`, "minimum of 2"},
		},
		"total below the minimum": {
			counts: map[string]int{"alfa": 4, "beta": 4, "gama": 0},
			want:   []string{"total is 8", "minimum of 10"},
		},
		"total above the maximum": {
			counts: map[string]int{"alfa": 6, "beta": 6, "gama": 3, "delta": 6},
			want:   []string{"total is 21", "maximum of 20"},
		},
		"area in no group": {
			counts: map[string]int{"alfa": 4, "beta": 4, "gama": 2, "epsilon": 2},
			want:   []string{`"epsilon"`, "no group", "misspelled"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidateCoverage(questionsFor(tc.counts), testTable())
			if err == nil {
				t.Fatalf("%s was accepted", name)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error %q does not mention %q", err, want)
				}
			}
		})
	}
}

// TestValidateCoverage_EmptyTableIsRefused is the vacuous-check guard. A table with no groups makes
// every loop below it run zero times and the function return nil — a validator that looked at
// nothing reporting that everything is fine, which is the shape of "orphans: not scanned" rendered
// as "no orphans found".
func TestValidateCoverage_EmptyTableIsRefused(t *testing.T) {
	questions := questionsFor(satisfying())

	if err := ValidateCoverage(questions, CoverageTable{}); err == nil {
		t.Fatal("an empty coverage table accepted a golden set, so the check passes anything at all")
	}
	if err := ValidateCoverage(questions, CoverageTable{MinTotal: 1, MaxTotal: 100}); err == nil {
		t.Error("a table with totals but no groups accepted a golden set with no per-area coverage")
	}
	if err := ValidateCoverage(questions, CoverageTable{Groups: testTable().Groups}); err == nil {
		t.Error("a table with groups but no total bounds was accepted; a one-question golden set " +
			"would satisfy it")
	}

	// The case the no-groups guard is the *only* check that catches, found by planting a defect on
	// it and watching nothing go red: with a maximum but no minimum, no groups and no questions, the
	// total check passes (0 ≤ 100), the per-group loop runs zero times and no area is unknown. Every
	// other case above is also caught by the totals guard or by the unknown-area check, so without
	// this one the guard could be deleted and the suite would stay green.
	if err := ValidateCoverage(nil, CoverageTable{MaxTotal: 100}); err == nil {
		t.Error("a table with no groups accepted an empty golden set, so ValidateCoverage returned " +
			"\"fine\" from a check that looked at nothing")
	}
}
