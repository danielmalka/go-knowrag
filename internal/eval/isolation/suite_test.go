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
// split is what stops this test from becoming the lock on a stale claim. The write path moved from
// the second list to the first when WritePathTenantCase was added, and a check for "internal/ingest
// appears somewhere in the summary" would have stayed green through that move and gone on requiring
// the report to disclaim what it now proves. That is why every entry is checked for presence in one
// half and absence in the other.
//
// The MCP scope binding moved the other way, which is the rarer direction and the reason the split
// is three paragraphs rather than two: it was claimed as proven for one commit, review escaped the
// case behind it three ways with running code, and it went back to being declared. The assertion
// that survives that round is the absence — "cmd/mcp-server" must not appear among what is proven.
func TestReport_SummaryStatesWhatItDoesNotProve(t *testing.T) {
	summary := Suite{Cases: []Case{passing("a")}}.Run(t.Context()).Summary()

	for _, want := range []string{"PASS", "Does not prove", "Qdrant", "integration"} {
		if !strings.Contains(summary, want) {
			t.Errorf("the summary does not mention %q:\n%s", want, summary)
		}
	}

	// Three paragraphs, cut apart before anything is read, because each names things the others must
	// not. The MCP paragraph is separate from what is proven precisely because it mentions
	// cmd/mcp-server: a check run over the two together cannot tell "proven" from "not proven here",
	// which is the difference this whole paragraph exists to state. A defect plant found that too —
	// deleting the claim sentence left the presence check satisfied by the paragraph beneath it.
	proven, rest, splitClaim := strings.Cut(summary, "The MCP scope binding is not proven here")
	mcp, gaps, splitGaps := strings.Cut(rest, "Untouched by every case above")
	if !splitClaim || !splitGaps {
		t.Fatalf("the summary no longer separates what it proves, how far the MCP tripwire reaches, "+
			"and what it leaves untouched, so no list can be checked against the paragraph it "+
			"belongs to:\n%s", summary)
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
	// The write path is proven now, so it has to be claimed and must not still be disclaimed. Both
	// halves are required: the first alone would pass over a summary that said it twice and in
	// opposite directions, which is worse than either sentence on its own.
	if !strings.Contains(proven, "internal/ingest") {
		t.Errorf("the summary does not say the write path is proven, so a reader who needs to know "+
			"whether ingestion is covered cannot find out from PASS:\n%s", summary)
	}
	for _, gone := range []string{"internal/ingest", "write path"} {
		if strings.Contains(gaps, gone) {
			t.Errorf("the summary still names %q among the things a PASS says nothing about, after "+
				"WritePathTenantCase started proving it:\n%s", gone, summary)
		}
	}
	// The MCP scope binding is the one this report has already got wrong once. An earlier version
	// moved it into the proven sentence; review then escaped the case behind it three ways, with
	// running code. So the absences here are the load-bearing half: the word "proven" must not
	// attach to it in the first paragraph, and the paragraph that owns it must keep saying what the
	// tripwire cannot see, or PASS goes back to reading as a claim nothing holds.
	if strings.Contains(proven, "cmd/mcp-server") {
		t.Errorf("the summary lists cmd/mcp-server among what it proves; one case reads that "+
			"directory's source and a query built in another package walks past it:\n%s", summary)
	}
	for _, want := range []string{
		"not a proof",     // the tripwire is named as one
		"another package", // and the escape that walks past it is named
		"main package",    // and why this suite cannot drive the server instead
		"own tests",       // pointing at where the real proof lives
	} {
		if !strings.Contains(mcp, want) {
			t.Errorf("the MCP paragraph does not carry %q, so a reader takes the case for a proof of "+
				"what no tool input can reach:\n%s", want, summary)
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
