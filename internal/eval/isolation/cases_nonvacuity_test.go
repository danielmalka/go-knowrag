package isolation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/retrieval"
)

// This file answers the only question that matters about a security suite: would it go red if the
// thing it guards disappeared?
//
// TestCases_AllHoldOnTheRealBuild proves each case returns "" on a clean tree, which is satisfied
// just as well by a case whose assertions have all been disabled. A defect-plant round found
// exactly that: eight assertions inside the cases could be switched off with the whole package
// staying green. Everything here exists to make that impossible.

// withProbe swaps the probe every case builds, and restores it. It is the seam that lets a test put
// the cases in front of a store that leaks.
func withProbe(t *testing.T, replacement func() (caseSearcher, *probeStore)) {
	t.Helper()
	original := newProbe
	newProbe = replacement
	t.Cleanup(func() { newProbe = original })
}

// leakingProbe answers with the whole collection whatever the request asked for — the store you
// would have if the tenant filter were gone from internal/retrieval and Qdrant had no opinion.
func leakingProbe() (caseSearcher, *probeStore) {
	store := &probeStore{points: fixture(), ignoreFilter: true}
	return newSearcherOver(store), store
}

// rewritingProbe puts the cases in front of the real searcher over a store that logs a rewritten
// copy of every request. The answers are the healthy build's answers; only what the cases inspect
// changes, which is the point — see probeStore.recordAs.
func rewritingProbe(rewrite func(retrieval.SearchRequest) retrieval.SearchRequest) func() (caseSearcher, *probeStore) {
	return func() (caseSearcher, *probeStore) {
		store := &probeStore{points: fixture(), recordAs: rewrite}
		return newSearcherOver(store), store
	}
}

// TestEveryCase_FailsAgainstALeakingStore is the whole point of this file.
//
// Each search-driven case is run against a store that ignores the filter entirely. Every one of
// them has to notice. A case that still returned "" here would be a case whose green means "the
// search returned something" — and it would keep meaning that on the day the filter went away.
func TestEveryCase_FailsAgainstALeakingStore(t *testing.T) {
	withProbe(t, leakingProbe)

	for _, c := range searchDrivenCases() {
		t.Run(c.Name, func(t *testing.T) {
			// The empty-tenant case asserts a refusal that happens in Query.Validate
			// (retrieval/query.go), before any store is reached, so a leaking store cannot move it —
			// and for that case an empty answer is the *correct* outcome, which is why it needs a
			// seam of its own rather than this one. That seam is a searcher which accepts what
			// production refuses: TestEmptyTenantCase_FailsWhenTheEmptyTenantIsHonoured.
			if strings.Contains(c.Name, "empty is refused") {
				t.Skip("decided before the store is reached; driven red by its own searcher seam")
			}
			detail := c.Run(t.Context())
			if detail == "" {
				t.Fatal("this case passed against a store that ignores the tenant filter and hands " +
					"back every tenant's notes, so its green on a clean build proves nothing")
			}
			t.Logf("caught: %s", detail)
		})
	}
}

// TestEveryCase_FailsWhenNothingIsAsked covers the other direction of the same vacuity: a searcher
// that never reaches the store at all.
//
// This is the "empty for the wrong reason" case in its purest form — no results, no leak, no error.
// A case that only checked for foreign results would call this a pass, which is how a broken build
// that silently returned nothing would ship as isolated.
func TestEveryCase_FailsWhenNothingIsAsked(t *testing.T) {
	withProbe(t, func() (caseSearcher, *probeStore) {
		store := &probeStore{} // no points, so every search is a clean empty answer
		return newSearcherOver(store), store
	})

	for _, c := range searchDrivenCases() {
		t.Run(c.Name, func(t *testing.T) {
			// The empty-tenant case is the exception by construction: its whole assertion is that
			// nothing is asked and nothing comes back, so an empty corpus is what it expects.
			if strings.Contains(c.Name, "empty is refused") {
				t.Skip("this case asserts that nothing is asked, so an empty corpus is its passing state")
			}
			if detail := c.Run(t.Context()); detail == "" {
				t.Fatal("this case passed against an index holding nothing at all; its assertions " +
					"cannot tell isolation from an empty answer")
			}
		})
	}
}

// TestAdversarialCases_FailWhenTheSearcherMisbehaves drives the payload case into the failure a
// results-only check would miss: results where none should be.
func TestAdversarialCases_FailWhenTheSearcherMisbehaves(t *testing.T) {
	t.Run("payload tenant starts matching", func(t *testing.T) {
		withProbe(t, leakingProbe)
		if detail := AdversarialTenantPayloadCase().Run(t.Context()); detail == "" {
			t.Fatal("a tenant_id of \"*\" matched every note and the case reported a pass")
		}
	})
}

// riggedSearcher is a build that misbehaves in exactly one way. It is the seam
// AdversarialTenantEmptyCase needs and no store can provide.
//
// reaching decides whether the query reaches the store before the answer is given, which is the
// difference between "refused" and "refused after the fact".
type riggedSearcher struct {
	store    *probeStore
	answer   []retrieval.Result
	err      error
	reaching bool
}

func (s riggedSearcher) Search(ctx context.Context, q retrieval.Query) ([]retrieval.Result, error) {
	if s.reaching {
		// The request an unvalidated empty tenant would produce: a collection, and no scope at all.
		if _, err := s.store.ExecuteQuery(ctx, retrieval.SearchRequest{Collection: q.Collection}); err != nil {
			return nil, err
		}
	}
	return s.answer, s.err
}

// TestEmptyTenantCase_FailsWhenTheEmptyTenantIsHonoured is the seam every other case gets from a
// leaking store, built for the one case that cannot use it.
//
// For AdversarialTenantEmptyCase an empty answer is the *correct* outcome, so a store that hands
// over every tenant's notes cannot make it fail: it does not assert "no foreign note came back", it
// asserts "the query was refused". What can drive it red is a searcher that accepts the value
// Query.Validate refuses (retrieval/query.go). Each row below disables exactly one of the case's
// three assertions, so emptying the case body to `return ""` fails all three.
func TestEmptyTenantCase_FailsWhenTheEmptyTenantIsHonoured(t *testing.T) {
	// A note owned by somebody, so "results came back" is concrete rather than a bare length.
	answer := []retrieval.Result{{UID: "bbbbbbbb-1111-4111-8111-111111111111"}}

	for _, tc := range []struct {
		name   string
		rigged riggedSearcher
	}{
		{"accepts the empty tenant and searches anyway", riggedSearcher{answer: answer, reaching: true}},
		{"refuses and answers with notes regardless", riggedSearcher{answer: answer, err: retrieval.ErrEmptyTenant}},
		{"refuses only after the query reached the store", riggedSearcher{err: retrieval.ErrEmptyTenant, reaching: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withProbe(t, func() (caseSearcher, *probeStore) {
				store := &probeStore{points: fixture()}
				rigged := tc.rigged
				rigged.store = store
				return rigged, store
			})
			if detail := AdversarialTenantEmptyCase().Run(t.Context()); detail == "" {
				t.Fatal("the case reported a pass against a searcher that did not refuse an empty " +
					"tenant_id outright and before any request was built, so its green on a clean " +
					"build says nothing about the refusal")
			}
		})
	}
}

// TestEmptyTenantCase_ReadsTheSentinelProductionRaises keeps the case's expectation attached to the
// error internal/retrieval actually raises, rather than to any error at all.
func TestEmptyTenantCase_ReadsTheSentinelProductionRaises(t *testing.T) {
	if !errors.Is(sentinelFor(t, ""), retrieval.ErrEmptyTenant) {
		t.Error("the empty tenant no longer answers with retrieval.ErrEmptyTenant, so the case's " +
			"assertion is checking a sentinel nothing produces")
	}
}

// withoutLegTenant is the request internal/retrieval would send if buildQueryRequest stopped putting
// the filter on its prefetch legs (retrieval/build.go), with the outer filter left intact.
//
// The store's answer is unchanged by it, because matches() in probe.go reads the outer filter and
// never the legs — so the results stay clean and only an inspection of what was asked can notice.
// That is the whole reason requestsCarryTenant walks the legs.
func withoutLegTenant(req retrieval.SearchRequest) retrieval.SearchRequest {
	req.Prefetch = append([]retrieval.PrefetchLeg(nil), req.Prefetch...)
	for i := range req.Prefetch {
		must := append([]retrieval.Condition(nil), req.Prefetch[i].Filter.Must...)
		req.Prefetch[i].Filter.Must = slices.DeleteFunc(must, func(c retrieval.Condition) bool {
			return c.Field == "tenant_id"
		})
	}
	return req
}

// TestEveryCase_FailsWhenOnlyTheLegsLoseTheirTenant is what makes requestsCarryTenant impossible to
// delete quietly.
//
// Deleting every call to it left this package green, because with the hostile store the outer filter
// still carries the tenant and the results are therefore still clean. The concrete regression that
// hides in that gap is a change dropping the condition from the prefetch legs alone: nothing about
// the answer moves, and without this test nothing about the suite moves either.
func TestEveryCase_FailsWhenOnlyTheLegsLoseTheirTenant(t *testing.T) {
	withProbe(t, rewritingProbe(withoutLegTenant))

	for _, c := range searchDrivenCases() {
		t.Run(c.Name, func(t *testing.T) {
			detail := c.Run(t.Context())
			// One structural exception, and it is not a name-shaped dodge: the empty-tenant case
			// asserts that no request is built at all, so a rewrite of requests that are never sent
			// cannot reach it either way. Its seam is
			// TestEmptyTenantCase_FailsWhenTheEmptyTenantIsHonoured.
			if strings.Contains(c.Name, "empty is refused") {
				if detail != "" {
					t.Fatalf("this case failed against a rewrite of requests it never sends: %s", detail)
				}
				return
			}
			if detail == "" {
				t.Fatal("this case passed while every prefetch leg went out with no tenant condition; " +
					"the results are identical either way, so without inspecting the request the " +
					"suite cannot see a leg that lost its scope")
			}
		})
	}
}

// probeReplacing swaps every point the predicate names for an ordinary visible note of the same
// tenant and collection, under a uid the fixture does not know.
//
// Dropping the guarded note outright would be the obvious corpus and it is too easy on the cases:
// `interno` and `base_paga` hold one private note each, so removing it leaves the collection empty
// and the lifted search returns nothing — a control that settles for "something came back" then
// fails for that reason rather than for the right one. Replacing keeps the collection populated, so
// only a control that looks for the guarded note itself can tell the difference.
func probeReplacing(guarded func(point) bool) func() (caseSearcher, *probeStore) {
	return func() (caseSearcher, *probeStore) {
		points := fixture()
		for i, p := range points {
			if guarded(p) {
				points[i] = point{
					uid:        fmt.Sprintf("ffffffff-%04d-4000-8000-000000000000", i),
					collection: p.collection, tenantID: p.tenantID, text: p.text,
				}
			}
		}
		store := &probeStore{points: points}
		return newSearcherOver(store), store
	}
}

// TestExclusionCases_FailWhenTheGuardedNoteIsNotThere is the "empty for the wrong reason" check on
// the exclusion cases' own controls.
//
// Each case proves a note does not come back, then proves it does once the guard is lifted; the
// second half is the only thing stopping the first from being a fact about a note nothing could ever
// return. A control that settled for "something came back" would be satisfied by the tenant's other
// notes, because the probe store matches on the filter and never on the text, so all of them are in
// the answer. A defect plant found exactly that. Over a corpus where the guarded note has been
// replaced by an unguarded one, both cases have to notice.
func TestExclusionCases_FailWhenTheGuardedNoteIsNotThere(t *testing.T) {
	for _, tc := range []struct {
		name    string
		guarded func(point) bool
		c       Case
	}{
		{"no private note seeded", func(p point) bool { return p.visibility == "private" }, PrivateVisibilityCase()},
		{"no archived note seeded", func(p point) bool { return p.status == "archived" }, ArchivedExclusionCase()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withProbe(t, probeReplacing(tc.guarded))
			if detail := tc.c.Run(t.Context()); detail == "" {
				t.Fatal("the case reported a pass over a corpus where the guarded note it looks for " +
					"is not there at all, so its green means the note was absent rather than excluded")
			}
		})
	}
}

// withoutExclusions is the request internal/retrieval would send if buildFilter stopped adding its
// MustNot guards (retrieval/build.go), answered by a store that excludes those points on its own
// account anyway — the helpful mock this package exists not to be.
func withoutExclusions(req retrieval.SearchRequest) retrieval.SearchRequest {
	req.Filter.MustNot = nil
	return req
}

// TestExclusionCases_FailWhenTheGuardIsNeverSent is the same guard for excludes.
//
// Deleting every call to it left this package green too. It checks the whole set rather than two
// cases by name, so a case that stops asserting its exclusion and a case added with one nobody
// checks both surface here as a mismatch.
func TestExclusionCases_FailWhenTheGuardIsNeverSent(t *testing.T) {
	withProbe(t, rewritingProbe(withoutExclusions))

	var red []string
	for _, c := range searchDrivenCases() {
		if c.Run(t.Context()) != "" {
			red = append(red, c.Name)
		}
	}
	want := []string{PrivateVisibilityCase().Name, ArchivedExclusionCase().Name}
	slices.Sort(red)
	slices.Sort(want)
	if !slices.Equal(red, want) {
		t.Errorf("with the MustNot guards never sent, the cases that failed were %v; want exactly "+
			"%v — every other case asserts no exclusion, and these two assert one each", red, want)
	}
}

// TestArchitectureCase_FiresOnAViolatingTree proves the case is not always-green.
//
// internal/archtest keeps a file under testdata/ that imports the Qdrant client precisely so this
// can be shown. Pointing the scan at that tree has to fail, with the file and line named — a case
// that reported "no violations" over a tree containing one would be reporting on nothing.
func TestArchitectureCase_FiresOnAViolatingTree(t *testing.T) {
	detail := ArchitectureBoundaryCase("../../archtest/testdata").Run(t.Context())
	if detail == "" {
		t.Fatal("the scan found no violation in a tree whose whole purpose is to contain one")
	}
	for _, want := range []string{"violation.go", ":", "internal/store"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the failure %q does not carry %q, so it cannot be acted on", detail, want)
		}
	}
}

// TestArchitectureCase_FailsWhenItCannotScan is the rule this repository applies everywhere: a
// check that could not run is not a check that passed. On a host with no source tree the boundary
// is unverified, and the case has to say so rather than shrug.
func TestArchitectureCase_FailsWhenItCannotScan(t *testing.T) {
	for _, root := range []string{"", "/nonexistent-root-for-this-test"} {
		detail := ArchitectureBoundaryCase(root).Run(t.Context())
		if detail == "" {
			t.Errorf("a scan over root %q reported the boundary as held having looked at nothing", root)
		}
		if !strings.Contains(detail, "unproven") {
			t.Errorf("the failure for root %q does not say the boundary is unproven: %s", root, detail)
		}
	}
}

// TestArchitectureCase_AgreesWithArchtestsOwnInvariant keeps the two copies of the rule equal.
// internal/archtest holds its import path and its allowed directory unexported, so this package
// spells them again; a change on one side that did not reach the other would leave the suite
// enforcing a boundary nobody else believes in.
func TestArchitectureCase_AgreesWithArchtestsOwnInvariant(t *testing.T) {
	source, err := os.ReadFile("../../archtest/boundary_test.go")
	if err != nil {
		t.Fatalf("reading archtest's own test: %v", err)
	}
	for _, want := range []string{
		`qdrantClientImport = "` + qdrantClientImport + `"`,
		`storeDir = "` + storeDir + `"`,
	} {
		if !strings.Contains(string(source), want) {
			t.Errorf("internal/archtest no longer declares %s; this package's copy has drifted", want)
		}
	}
}

// TestDefaultSuite_RegistersEveryCase is the guard on the registry itself. Every assertion in this
// package is worth nothing if the case carrying it is not in the suite the CLI runs, and a deleted
// line in DefaultSuite is invisible in a green run.
func TestDefaultSuite_RegistersEveryCase(t *testing.T) {
	var names []string
	for _, c := range DefaultSuite("").Cases {
		names = append(names, c.Name)
	}

	for _, want := range []string{
		"cross-tenant",
		"empty is refused",
		"anything that is not a tenant",
		"query text",
		"private is never returned",
		"archived is returned only when asked for",
		"privileged",
		"write path",
		"mcp server",
		"architecture",
	} {
		if !slices.ContainsFunc(names, func(n string) bool { return strings.Contains(n, want) }) {
			t.Errorf("no case in DefaultSuite mentions %q; it is not being run: %v", want, names)
		}
	}
	if len(names) != 10 {
		t.Errorf("DefaultSuite has %d case(s): %v. A case added without a line here is a case whose "+
			"absence from this list is the only thing that would have flagged it", len(names), names)
	}
}

// TestDefaultSuite_UsesTheRealProbe pins that the seam this file relies on is a test seam and
// nothing more: on a normal run the cases search the hostile probe, not a leaking one.
func TestDefaultSuite_UsesTheRealProbe(t *testing.T) {
	searcher, store := newProbe()
	// The searcher is an interface so the tests above can swap in one that misbehaves. That seam is
	// wide enough to run the whole suite against a fake, so what ships through it is pinned here.
	if _, ok := searcher.(*retrieval.Searcher); !ok {
		t.Fatalf("the shipped probe searches through a %T, not the production *retrieval.Searcher, "+
			"so every case is measuring a stand-in", searcher)
	}
	if store.ignoreFilter {
		t.Fatal("the shipped probe ignores the filter, so every case in the suite is vacuous")
	}
	if len(store.points) == 0 {
		t.Fatal("the shipped probe holds no points, so every case searches an empty index")
	}
}

// sentinelFor asks the real searcher what a given tenant_id produces, bypassing the swapped probe.
func sentinelFor(t *testing.T, tenantID string) error {
	t.Helper()
	store := &probeStore{points: fixture()}
	_, err := newSearcherOver(store).Search(t.Context(), query(collClientes, tenantID, "contract renewal terms"))
	return err
}

// searchDrivenCases is every case that reaches the store through Searcher.Search. The architecture
// case is excluded: it scans source and never searches, so a leaking store says nothing about it.
// So is the write case, which never searches either — its harness is cases_write_nonvacuity_test.go.
func searchDrivenCases() []Case {
	return []Case{
		CrossTenantCase(),
		AdversarialTenantEmptyCase(),
		AdversarialTenantPayloadCase(),
		QueryTextInjectionCase(),
		PrivateVisibilityCase(),
		ArchivedExclusionCase(),
		PrivilegedPathCase(),
	}
}

// TestEveryCase_IsCoveredByANonVacuityHarness closes the gap the two lists above would otherwise
// leave between them.
//
// The harnesses in this package iterate hand-maintained lists, and a case that appears in none of
// them is a case whose assertions can all be switched off with the package green — which is the
// defect this whole file was written to answer. Splitting the suite into a second entry point made
// that reachable for the first time: before the write case there was one list, and a case missing
// from it also failed TestDefaultSuite_RegistersEveryCase's count.
//
// The two source-reading cases are the deliberate omissions, named rather than skipped by shape:
// they drive neither a store nor an ingestion, so a leaking probe says nothing about either, and
// each has its own failure harnesses — TestArchitectureCase_FiresOnAViolatingTree and
// TestArchitectureCase_FailsWhenItCannotScan above, and cases_mcp_nonvacuity_test.go for the MCP
// scope binding. Named here rather than matched by a shape test, because "it reads source" is a
// property of the case's body and a rule keyed on it would exempt whatever future case happens to
// read a file.
func TestEveryCase_IsCoveredByANonVacuityHarness(t *testing.T) {
	covered := map[string]bool{
		ArchitectureBoundaryCase("").Name: true,
		MCPScopeBindingCase("").Name:      true,
	}
	for _, c := range searchDrivenCases() {
		covered[c.Name] = true
	}
	for _, c := range writeDrivenCases() {
		covered[c.Name] = true
	}

	var shipped []string
	for _, c := range DefaultSuite("").Cases {
		shipped = append(shipped, c.Name)
		if !covered[c.Name] {
			t.Errorf("the case %q is in DefaultSuite and in no non-vacuity harness, so every "+
				"assertion inside it could be deleted with this package still green", c.Name)
		}
		delete(covered, c.Name)
	}
	for name := range covered {
		t.Errorf("the harnesses drive a case %q that DefaultSuite does not ship, so what they prove "+
			"is not what the CLI runs: %v", name, shipped)
	}
}

// writeDrivenCases is every case that drives an ingestion. Its harness is
// cases_write_nonvacuity_test.go.
func writeDrivenCases() []Case { return []Case{WritePathTenantCase()} }
