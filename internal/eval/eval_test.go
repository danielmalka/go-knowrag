package eval

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestModes_RefuseExplicitlyUntilTheHarnessExists is the rule S06b's report established and this
// package inherits: not having looked is never allowed to render as having found nothing. A mode
// with no harness must not answer with a zero-valued Outcome and a nil error, because the zero
// value of Passed is false today and would become a silent "the gate ran and failed" the moment
// somebody changed the field's meaning — and, worse, the caller's own check could invert it.
func TestModes_RefuseExplicitlyUntilTheHarnessExists(t *testing.T) {
	tests := map[string]struct {
		run func(context.Context, Options) (Outcome, error)
		// story is the one this refusal has to name. An operator reading it must be able to find
		// out whether the gate is missing or broken without opening the source.
		story string
		flag  string
	}{
		"golden":    {run: RunGolden, story: "S10", flag: "--golden"},
		"isolation": {run: RunIsolation, story: "S11", flag: "--isolation"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			outcome, err := tc.run(context.Background(), Options{Collection: "interno", TenantID: "malka"})
			if err == nil {
				t.Fatalf("the %s mode returned %+v and no error, which reads as a gate that ran", name, outcome)
			}
			if !errors.Is(err, ErrNotImplemented) {
				t.Errorf("the refusal does not carry ErrNotImplemented, so no caller can recognise "+
					"it without matching prose: %v", err)
			}
			if outcome.Passed {
				t.Error("the refusal came back with Passed set, which is the one thing it must never say")
			}
			for _, want := range []string{tc.story, tc.flag, "Nothing was measured"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not mention %q", err, want)
				}
			}
		})
	}
}

// TestModes_RefusalsAreDistinguishable covers the half a shared sentinel hides: both modes answer
// with the same error value, so the message is the only thing that says which gate is missing, and
// a reader who ran --isolation must not be sent to read S10's story.
func TestModes_RefusalsAreDistinguishable(t *testing.T) {
	golden, gerr := RunGolden(context.Background(), Options{})
	isolation, ierr := RunIsolation(context.Background(), Options{})

	if gerr.Error() == ierr.Error() {
		t.Errorf("both modes refuse with the same sentence, so neither says which gate is missing: %v", gerr)
	}
	if golden != isolation {
		t.Errorf("the two refusals returned different Outcomes (%+v vs %+v); both must be the zero "+
			"value, because neither measured anything", golden, isolation)
	}
}

// TestOutcome_ScoreIsAbsentNotZero pins the one field shape S10 and S11 must not collapse.
//
// S11's task document requires its report to carry no numeric score anywhere: a tenant-isolation
// suite that could be reported as 90% passing is one somebody can argue down. A float64 would make
// "no score" indistinguishable from "scored zero", which is the worst possible reading of an
// isolation run. The pointer is what keeps the absence sayable.
func TestOutcome_ScoreIsAbsentNotZero(t *testing.T) {
	zero := 0.0
	scored := Outcome{Score: &zero}
	unscored := Outcome{}

	if unscored.Score != nil {
		t.Error("an Outcome with no score carries one")
	}
	if scored.Score == nil || *scored.Score != 0 {
		t.Error("a score of zero is not representable, so a gate that scored zero reports as unscored")
	}
}
