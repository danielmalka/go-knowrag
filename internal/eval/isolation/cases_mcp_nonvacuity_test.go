package isolation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file is MCPScopeBindingCase's answer to the question every source-reading check has to
// answer: would it go red if the thing it guards disappeared? It cannot be driven by the hostile
// store — the case never searches — so it gets the same treatment as the architecture case: trees
// built to violate it, trees built to be empty in each of the ways a scan can hold over nothing,
// and one tree built to escape it on purpose.

func mcpScopeTree(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("testdata", "mcp-scope", name)
}

// TestMCPScopeCase_FiresOnEveryEscalationRoute is the case proving it is not always-green, once per
// rule.
//
// One tree with all six, and every one of them named in the failure, because the case reports the
// routes it found rather than the first: a tree with six and a message about one would leave five
// rules unproven while looking like a red run.
//
// The alias and the embedded input are the two that matter most here. Both passed the earlier
// version of this case, which read declarations instead of resolved types, and both were found by
// review rather than by this harness — so they are pinned by name, not folded into a count.
func TestMCPScopeCase_FiresOnEveryEscalationRoute(t *testing.T) {
	detail := MCPScopeBindingCase(mcpScopeTree(t, "violations")).Run(t.Context())
	if detail == "" {
		t.Fatal("the scan found no escalation in a tree whose whole purpose is to contain six")
	}

	for what, want := range map[string]string{
		"the scope read straight off the tool input": "sets TenantID from in.TenantID",
		"the same read laundered through a call":     "assembled by a call",
		"the input renamed by a bare := alias":       "sets TenantID from proxy.TenantID",
		"the input reached through an embedded type": "sets TenantID from c.TenantID",
		"a query built with no tenant at all":        "without setting TenantID",
		"a correct literal overwritten afterwards":   "assigns q.TenantID after the fact",
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("%s is not reported: the failure %q does not carry %q", what, detail, want)
		}
	}
	// And it has to be actionable. A finding nobody can locate sends the reader to read the whole
	// package looking for which of six shapes fired.
	if !strings.Contains(detail, "handler.go:") {
		t.Errorf("the failure names no file and line, so it cannot be acted on: %s", detail)
	}
}

// TestMCPScopeCase_DoesNotFollowIndirectionOutOfThePackage is the limitation, written as a test that
// passes.
//
// It is green by design and it is not a plant that failed to fire. The fixture it reads escalates —
// it hands the caller's tenant to another package, which builds the query — and this case reports
// it clean, because there is no retrieval.Query literal in the scanned directory to check. Pinning
// that is the opposite of hiding it: anyone who later reads a green isolation run as "no tool input
// can reach the scope" is contradicted by a test, and anyone who closes the gap has to delete this
// test deliberately rather than discover the claim was never true.
//
// The report says the same thing in prose, and TestReport_SummaryStatesWhatItDoesNotProve holds it
// there.
func TestMCPScopeCase_DoesNotFollowIndirectionOutOfThePackage(t *testing.T) {
	root := mcpScopeTree(t, "cross-package")
	if detail := MCPScopeBindingCase(root).Run(t.Context()); detail != "" {
		t.Fatalf("the scan now catches a query built in another package: %s.\nThat is an improvement, "+
			"not a failure — but the report and this case's doc comment both say it does not, and "+
			"both have to be corrected before this test is deleted", detail)
	}
	// The escape has to be a real one, not a fixture that quietly stopped escalating: the tenant a
	// caller named has to be what leaves this package.
	src, err := readTree(t, root)
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}
	for _, want := range []string{"escalate.Build(cfg.Collection, in.TenantID)", "retrieval.Query{"} {
		if !strings.Contains(src, want) {
			t.Errorf("the fixture no longer contains %q, so the green above is a fact about a tree "+
				"that does not escalate rather than about this case's reach", want)
		}
	}
}

// TestMCPScopeCase_DoesNotFailACorrectRefactor is the other direction, and it is the one that
// decides whether anyone keeps reading this case's failures.
//
// A configuration that groups its scope one level down (`cfg.Defaults.TenantID`) is correct and
// failed the earlier version of this case, which demanded a single selector on a plain identifier.
// Resolving the type rather than the shape is what makes this pass; a case that cries wolf on a
// correct refactor is one whose next real finding is argued down.
func TestMCPScopeCase_DoesNotFailACorrectRefactor(t *testing.T) {
	if detail := MCPScopeBindingCase(mcpScopeTree(t, "grouped-config")).Run(t.Context()); detail != "" {
		t.Errorf("a scope taken wholly from a grouped configuration was reported as an escalation: %s", detail)
	}
}

// TestMCPScopeCase_FailsWhenItLookedAtNothing covers the three greens that mean "there was nothing
// here", which are the ones indistinguishable from a held rule in the report.
//
// A missing tree is the realistic one: this case reads a build-time shape, so on a deployed host
// with no source `cli eval --isolation` has to fail loudly rather than report a rule nobody applied
// — the same reasoning ModuleRoot's doc comment gives for the architecture case.
func TestMCPScopeCase_FailsWhenItLookedAtNothing(t *testing.T) {
	for name, root := range map[string]string{
		"no source tree at all":        "/nonexistent-root-for-this-test",
		"the working directory":        "",
		"a server that decodes input":  mcpScopeTree(t, "no-query"),
		"a server that builds a query": mcpScopeTree(t, "no-input"),
	} {
		t.Run(name, func(t *testing.T) {
			detail := MCPScopeBindingCase(root).Run(t.Context())
			if detail == "" {
				t.Fatal("the case reported the scope shape as held having applied its rules to nothing")
			}
			if !strings.Contains(detail, "nothing") {
				t.Errorf("the failure does not say it checked nothing: %s", detail)
			}
		})
	}
}

// TestMCPScopeCase_FailsOnSourceItCannotRead is the same rule for the file it could not parse. A
// scan that skipped it would report on the files it happened to understand, which is a subset
// nobody chose.
func TestMCPScopeCase_FailsOnSourceItCannotRead(t *testing.T) {
	detail := MCPScopeBindingCase(mcpScopeTree(t, "unparsable")).Run(t.Context())
	if !strings.Contains(detail, "could not run") || !strings.Contains(detail, "handler.go") {
		t.Errorf("a tree holding a file that does not parse reported %q; want the scan called unable "+
			"to run and the file named", detail)
	}
}

// TestMCPScopeScan_ResolvesTheRealPackage pins that the real run is looking at cmd/mcp-server, and
// that the type check it depends on resolved the types it asks about.
//
// The type check runs with no importer, so most of the package's types are unresolvable by design.
// This is what stops that from quietly becoming "no type resolved at all", which would make every
// tool-input rule below hold over nothing while the counters still looked healthy.
func TestMCPScopeScan_ResolvesTheRealPackage(t *testing.T) {
	scan, err := scanMCPScope(moduleRoot(t))
	if err != nil {
		t.Fatalf("scanning %s: %v", mcpServerDir, err)
	}
	if len(scan.inputTypes) == 0 {
		t.Fatalf("no JSON-decoded input type was resolved in %s, so the rule about tool input is "+
			"applied to nothing on the real build", mcpServerDir)
	}
	if scan.queries == 0 {
		t.Errorf("no retrieval.Query is built in %s on the real build", mcpServerDir)
	}
	// And the resolved type has to be recognisable as input, or carriesInput answers false for the
	// one type the whole case is about.
	for _, in := range scan.inputTypes {
		if !scan.carriesInput(in) {
			t.Errorf("the input type %s is not recognised as carrying input by the check that reads "+
				"every scope value", in)
		}
	}
}

// TestMCPScopeScan_SkipsTests keeps the case from reading the proof as the defect.
//
// cmd/mcp-server's own escalation tests send `tenant_id` on purpose and assert it is inert
// (TestSearchKnowledge_ExtraTenantAndCollectionInput_IgnoredUsesConfigScope, handler_test.go); a
// scan that read them would fail the release gate over the tests written to guard the same thing.
func TestMCPScopeScan_SkipsTests(t *testing.T) {
	root := moduleRoot(t)
	scan, err := scanMCPScope(root)
	if err != nil {
		t.Fatalf("scanning %s: %v", mcpServerDir, err)
	}
	for _, p := range scan.problems {
		if strings.Contains(p, "_test.go") {
			t.Errorf("the scan reported a finding in a test file: %s", p)
		}
	}
	// And the skip has to be about tests rather than about there being nothing in them: the package
	// does build queries in its tests, so a scan that stopped skipping would have something to find.
	tests, err := filepath.Glob(filepath.Join(root, mcpServerDir, "*_test.go"))
	if err != nil || len(tests) == 0 {
		t.Fatalf("found %d test file(s) in %s (%v); this test is asserting against an empty skip",
			len(tests), mcpServerDir, err)
	}
}

// readTree returns a fixture tree's source, so a test can assert about what the fixture still says
// rather than trusting it to have stayed what it was written as.
func readTree(t *testing.T, root string) (string, error) {
	t.Helper()
	// #nosec G304 -- root is one of this package's own testdata directory names, spelled in the test
	// that calls this; there is no external input anywhere on this path.
	src, err := os.ReadFile(filepath.Join(root, mcpServerDir, "handler.go"))
	return string(src), err
}
