package isolation

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func passing(name string) Case {
	return Case{Name: name, Run: func(context.Context) string { return "" }}
}

func failing(name, detail string) Case {
	return Case{Name: name, Run: func(context.Context) string { return detail }}
}

// TestSuite_AnyFailingCaseFailsTheWholeSuite is the binary verdict, which is the whole shape of this
// report type: there is no threshold to argue about and no majority to appeal to.
func TestSuite_AnyFailingCaseFailsTheWholeSuite(t *testing.T) {
	s := Suite{Cases: []Case{
		passing("first"),
		failing("rigged", "tenant-beta saw tenant-alfa's note"),
		passing("third"),
	}}

	report := s.Run(t.Context())

	if report.Pass {
		t.Error("a suite with a failing case reported a pass")
	}
	if got := report.FailingCases(); !slices.Equal(got, []string{"rigged"}) {
		t.Errorf("FailingCases() = %v, want exactly [rigged]", got)
	}
	if len(report.Cases) != 3 {
		t.Errorf("%d case result(s), want 3 — every case runs even after one fails", len(report.Cases))
	}
	// The passing cases still report as passing, so a reader can see the shape of the breakage
	// rather than a wall of red.
	for _, c := range report.Cases {
		if (c.Name == "rigged") == c.Pass {
			t.Errorf("case %q reported Pass=%t", c.Name, c.Pass)
		}
	}
	if report.Cases[1].Detail == "" {
		t.Error("the failing case carries no detail, so nobody can act on it")
	}
}

// TestSuite_NamesEveryFailingCase covers the half a first-failure-wins runner would lose. One
// change that drops the tenant condition fails several cases at once, and seeing one of them sends
// the reader looking for a narrower cause than the one that exists.
func TestSuite_NamesEveryFailingCase(t *testing.T) {
	s := Suite{Cases: []Case{
		failing("first", "leaked"), passing("second"), failing("third", "also leaked"),
	}}

	got := s.Run(t.Context()).FailingCases()
	if !slices.Equal(got, []string{"first", "third"}) {
		t.Errorf("FailingCases() = %v, want [first third]", got)
	}
}

func TestSuite_AllPassing(t *testing.T) {
	report := Suite{Cases: []Case{passing("a"), passing("b")}}.Run(t.Context())
	if !report.Pass {
		t.Errorf("a suite where every case held reported a failure: %v", report.FailingCases())
	}
	if len(report.FailingCases()) != 0 {
		t.Errorf("FailingCases() = %v on a clean run", report.FailingCases())
	}
}

// TestSuite_EmptySuiteIsNotAPass is the same rule internal/eval applies to an evaluation that did
// not run. Pass defaults to true while folding, so a registry that lost its cases would report the
// strongest claim this system can make from no evidence at all.
func TestSuite_EmptySuiteIsNotAPass(t *testing.T) {
	report := Suite{}.Run(t.Context())

	if report.Pass {
		t.Error("an empty suite reported a pass; nothing was attacked, so nothing was proven")
	}
	if len(report.FailingCases()) == 0 {
		t.Error("an empty suite named no failing case, so the report says nothing at all")
	}
}

// TestReport_HasNoScoreField is the PRD requirement written as a test rather than as a comment
// somebody has to keep believing.
//
// A tenant-isolation suite reportable as "90% passing" is one somebody argues down before a
// release. The type carries a bool and a list; the only number reachable from it is how many cases
// exist, which is coverage, not a grade.
func TestReport_HasNoScoreField(t *testing.T) {
	for _, banned := range []string{"Score", "Percent", "Ratio", "Rate", "Passed int", "Total"} {
		if fieldNames(t).has(banned) {
			t.Errorf("Report or CaseResult declares a %q field; this suite has no partial credit", banned)
		}
	}
}

// TestReport_SummaryStatesWhatItDoesNotProve keeps the report honest about its own reach. The suite
// runs with no Qdrant, so a summary that read as "isolation verified against the database" would
// overclaim in the one place overclaiming is most expensive.
//
// The second list is the other half of the same honesty, and it is the half a reader cannot recover
// on their own: the suite drives two entry points, so a green isolation gate can coexist with a leak
// in `stats` or in the tenant the MCP server binds, and whoever reads PASS has to be told that
// without going to read the suite.
//
// The list is checked against the paragraph it belongs to, not against the whole summary, and that
// split is what stops this test from becoming the lock on a stale claim. Two entries have already
// moved from the second list to the first — the write path with WritePathTenantCase, the MCP scope
// binding with MCPScopeBindingCase — and a check for "internal/ingest appears somewhere in the
// summary" would have stayed green through both moves and gone on requiring the report to disclaim
// what it now proves. That is why every entry is checked for presence in one half and absence in
// the other.
func TestReport_SummaryStatesWhatItDoesNotProve(t *testing.T) {
	summary := Suite{Cases: []Case{passing("a")}}.Run(t.Context()).Summary()

	for _, want := range []string{"PASS", "Does not prove", "Qdrant", "integration"} {
		if !strings.Contains(summary, want) {
			t.Errorf("the summary does not mention %q:\n%s", want, summary)
		}
	}

	proves, gaps, split := strings.Cut(summary, "Untouched by every case above")
	if !split {
		t.Fatalf("the summary no longer separates what it proves from what it leaves untouched, so "+
			"neither list can be checked against the paragraph it belongs to:\n%s", summary)
	}
	for _, want := range []string{
		"stats",                 // counts every tenant when none is named, by design
		"offset 0",              // pagination is never exercised
		"FilterMatchesAnything", // and neither is the filter probe
	} {
		if !strings.Contains(gaps, want) {
			t.Errorf("the summary does not name %q as outside the suite, so a reader takes PASS for "+
				"a claim about it:\n%s", want, summary)
		}
	}
	// The write path and the MCP scope binding are proven now, so each has to be claimed and must not
	// still be disclaimed. Both halves are required: the first alone would pass over a summary that
	// said it twice and in opposite directions, which is worse than either sentence on its own.
	for _, want := range []string{"internal/ingest", "cmd/mcp-server"} {
		if !strings.Contains(proves, want) {
			t.Errorf("the summary does not say %q is proven, so a reader who needs to know whether it "+
				"is covered cannot find out from PASS:\n%s", want, summary)
		}
	}
	for _, gone := range []string{
		"internal/ingest", "write path", // WritePathTenantCase
		"cmd/mcp-server", "binds from its environment", // MCPScopeBindingCase
	} {
		if strings.Contains(gaps, gone) {
			t.Errorf("the summary still names %q among the things a PASS says nothing about, after a "+
				"case started proving it:\n%s", gone, summary)
		}
	}
	// The MCP claim is the one a reader can overread, because the case behind it reads source rather
	// than driving the server. A summary that claimed it without that qualification would be selling
	// a build-time rule as a call that was made and refused.
	for _, want := range []string{"source", "main package"} {
		if !strings.Contains(proves, want) {
			t.Errorf("the summary claims the MCP scope binding without saying it is read off that "+
				"command's source, so PASS reads as an escalation attempt this suite made:\n%s", summary)
		}
	}

	failed := Suite{Cases: []Case{failing("a", "leaked")}}.Run(t.Context()).Summary()
	for _, want := range []string{"FAIL", "blocks release", "leaked"} {
		if !strings.Contains(failed, want) {
			t.Errorf("the failing summary does not mention %q:\n%s", want, failed)
		}
	}
	// And a failing run must not also claim to have passed.
	if strings.Contains(failed, "every case held") {
		t.Errorf("a failing summary carries the passing sentence:\n%s", failed)
	}
}

// fieldNames lists the declared fields of Report and CaseResult, so the no-score rule is checked
// against the types themselves rather than against a reading of the source.
type nameSet []string

func (n nameSet) has(want string) bool {
	for _, got := range n {
		if strings.Contains(got, strings.Fields(want)[0]) {
			return true
		}
	}
	return false
}

func fieldNames(t *testing.T) nameSet {
	t.Helper()
	var out nameSet
	for _, typ := range []reflect.Type{reflect.TypeOf(Report{}), reflect.TypeOf(CaseResult{})} {
		for i := range typ.NumField() {
			out = append(out, typ.Field(i).Name)
		}
	}
	if len(out) == 0 {
		t.Fatal("no fields found, so the no-score check is looking at nothing")
	}
	return out
}
