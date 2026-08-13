package isolation

import (
	"path/filepath"
	"strings"
	"testing"
)

// This file is MCPScopeBindingCase's answer to the question every source-reading check has to
// answer: would it go red if the thing it guards disappeared? It cannot be driven by the hostile
// store — the case never searches — so it gets the same treatment as the architecture case: trees
// built to violate it, and trees built to be empty in each of the ways a scan can hold over nothing.

func mcpScopeTree(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("testdata", "mcp-scope", name)
}

// TestMCPScopeCase_FiresOnEveryEscalationRoute is the case proving it is not always-green, once per
// rule.
//
// One tree with all four, and every one of them named in the failure, because the case reports the
// routes it found rather than the first: a tree with four and a message about one would leave three
// rules unproven while looking like a red run.
func TestMCPScopeCase_FiresOnEveryEscalationRoute(t *testing.T) {
	detail := MCPScopeBindingCase(mcpScopeTree(t, "violations")).Run(t.Context())
	if detail == "" {
		t.Fatal("the scan found no escalation in a tree whose whole purpose is to contain four")
	}

	for what, want := range map[string]string{
		"the scope read straight off the tool input": "from the tool input",
		"the same read laundered through a call":     "scopeFor(in)",
		"a query built with no tenant at all":        "without setting TenantID",
		"a correct literal overwritten afterwards":   "after the fact",
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("%s is not reported: the failure %q does not carry %q", what, detail, want)
		}
	}
	// And it has to be actionable. A finding nobody can locate sends the reader to read the whole
	// package looking for which of four shapes fired.
	if !strings.Contains(detail, "handler.go:") {
		t.Errorf("the failure names no file and line, so it cannot be acted on: %s", detail)
	}
}

// TestMCPScopeCase_FailsWhenItLookedAtNothing covers the three greens that mean "there was nothing
// here", which are the ones indistinguishable from a held invariant in the report.
//
// A missing tree is the realistic one: this case checks a build-time invariant, so on a deployed
// host with no source `cli eval --isolation` has to fail loudly rather than report a binding nobody
// verified — the same rule ModuleRoot's doc comment states for the architecture case.
func TestMCPScopeCase_FailsWhenItLookedAtNothing(t *testing.T) {
	for name, root := range map[string]string{
		"no source tree at all":       "/nonexistent-root-for-this-test",
		"the working directory":       "",
		"a server that decodes input": mcpScopeTree(t, "no-query"),
		"a server that builds a query": mcpScopeTree(t, "no-input"),
	} {
		t.Run(name, func(t *testing.T) {
			detail := MCPScopeBindingCase(root).Run(t.Context())
			if detail == "" {
				t.Fatal("the case reported the scope binding as held having applied its rules to nothing")
			}
			if !strings.Contains(detail, "unproven") {
				t.Errorf("the failure does not say the binding is unproven: %s", detail)
			}
		})
	}
}

// TestMCPScopeCase_FailsOnSourceItCannotRead is the same rule for the file it could not parse. A
// scan that skipped it would report on the files it happened to understand, which is a subset
// nobody chose.
func TestMCPScopeCase_FailsOnSourceItCannotRead(t *testing.T) {
	detail := MCPScopeBindingCase(mcpScopeTree(t, "unparsable")).Run(t.Context())
	if !strings.Contains(detail, "unproven") || !strings.Contains(detail, "handler.go") {
		t.Errorf("a tree holding a file that does not parse reported %q; want the binding called "+
			"unproven and the file named", detail)
	}
}

// TestMCPScopeScan_ReadsTheRealPackage pins that the real run is looking at cmd/mcp-server and
// finding both halves of what it checks.
//
// The counters are the case's own non-vacuity guard, and this is what stops them from being
// satisfied by a tree that is not the one anybody cares about: the real package has to be where the
// tool input type and the queries are found.
func TestMCPScopeScan_ReadsTheRealPackage(t *testing.T) {
	scan, err := scanMCPScope(moduleRoot(t))
	if err != nil {
		t.Fatalf("scanning %s: %v", mcpServerDir, err)
	}
	if len(scan.inputTypes) == 0 {
		t.Errorf("no JSON-decoded input type found in %s, so the rule about tool input is applied to "+
			"nothing on the real build", mcpServerDir)
	}
	if len(scan.inputVars) == 0 {
		t.Errorf("input types %v are declared in %s and no identifier is bound to one, so no "+
			"expression can ever be recognised as tool input", scan.inputTypes, mcpServerDir)
	}
	if scan.queries == 0 {
		t.Errorf("no retrieval.Query is built in %s on the real build", mcpServerDir)
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
