package isolation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/retrieval"
)

// TestCases_AllHoldOnTheRealBuild is the suite doing its job on a clean tree. It is the least
// interesting assertion in this file and the only one anybody will see on a green day.
func TestCases_AllHoldOnTheRealBuild(t *testing.T) {
	for _, c := range DefaultSuite(moduleRoot(t)).Cases {
		if detail := c.Run(t.Context()); detail != "" {
			t.Errorf("case %q failed on a clean build: %s", c.Name, detail)
		}
	}
}

// TestCrossTenantCase_FailsWhenAForeignNoteComesBack proves the case is not vacuous, by the only
// route that does not require editing internal/retrieval: rewriting the fixture so a note the case
// will see is owned by somebody else.
//
// Every "no foreign results" assertion in this package rests on the case noticing this. A case that
// stayed green here would be one whose green means "the search returned something" and nothing more.
func TestCrossTenantCase_FailsWhenAForeignNoteComesBack(t *testing.T) {
	// The case resolves ownership through tenantOf, so this drives foreignResults directly with a
	// result the fixture says belongs to tenant-beta while the query claimed to be tenant-alfa.
	leaks := foreignResults(tenantA, resultsFor(t, tenantB))
	if len(leaks) == 0 {
		t.Fatal("foreignResults saw nothing wrong with a tenant-beta note returned to tenant-alfa, " +
			"so every zero-leak case in this package passes by construction")
	}
	if !strings.Contains(joinLeaks(leaks), tenantB) {
		t.Errorf("the leak description does not name the owning tenant: %s", joinLeaks(leaks))
	}
}

// TestForeignResults_UsesExactEquality is the assertion the task document calls out by name.
// `tenant-alfa` and `tenant-alfa-2` are different tenants, and a HasPrefix or Contains check would
// hand the second one's data to the first while reporting a clean run.
func TestForeignResults_UsesExactEquality(t *testing.T) {
	if leaks := foreignResults("tenant-alf", resultsFor(t, tenantA)); len(leaks) == 0 {
		t.Error("a tenant whose ID is a prefix of the owner's was treated as the owner")
	}
	if leaks := foreignResults(tenantA+"-2", resultsFor(t, tenantA)); len(leaks) == 0 {
		t.Error("a tenant whose ID extends the owner's was treated as the owner")
	}
	if leaks := foreignResults(tenantA, resultsFor(t, tenantA)); len(leaks) != 0 {
		t.Errorf("the owning tenant was reported as a leak: %s", joinLeaks(leaks))
	}
}

// TestRequestsCarryTenant_RejectsWhatItShould covers the second, independent half of every case:
// what was asked, not what came back. Results alone would pass against an index that happened to
// hold nothing for the other tenants.
func TestRequestsCarryTenant_RejectsWhatItShould(t *testing.T) {
	searcher, store := newProbe()
	if _, err := searcher.Search(t.Context(), query(collClientes, tenantA, "contract renewal terms")); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if problem := requestsCarryTenant(store.requests, tenantA); problem != "" {
		t.Fatalf("a real scoped search was rejected: %s", problem)
	}

	// No request at all is not a pass. A case whose search never ran would otherwise report that
	// the filter it never sent was correct.
	if requestsCarryTenant(nil, tenantA) == "" {
		t.Error("an empty request log was accepted as proof that the tenant condition was carried")
	}
	// The wrong tenant, and a request with no tenant condition anywhere, both have to be caught.
	if requestsCarryTenant(store.requests, tenantB) == "" {
		t.Error("a request scoped to tenant-alfa was accepted as proof of scoping to tenant-beta")
	}

	// The outer filter alone, with the legs left intact. internal/retrieval builds both from the
	// same Filter value, so stripping both at once cannot tell the two checks apart — and a defect
	// plant that disabled the outer-filter check stayed green until this case existed. The outer
	// filter is what survives if the request ever stops carrying prefetch legs.
	//
	// It runs before the strip-everything case below, and takes its own copy of the prefetch slice,
	// because a SearchRequest copy shares that backing array: mutating the legs for one assertion
	// silently disarmed this one, which is how it came to pass while proving nothing.
	outerOnly := deepCopy(store.requests[0])
	outerOnly.Filter.Must = []retrieval.Condition{{Field: "area", Value: "alfa"}}
	if requestsCarryTenant([]retrieval.SearchRequest{outerOnly}, tenantA) == "" {
		t.Error("a request whose outer filter lost the tenant condition was accepted because its " +
			"prefetch legs still carried one")
	}

	stripped := deepCopy(store.requests[0])
	stripped.Filter.Must = nil
	for i := range stripped.Prefetch {
		stripped.Prefetch[i].Filter.Must = nil
	}
	if requestsCarryTenant([]retrieval.SearchRequest{stripped}, tenantA) == "" {
		t.Error("a request carrying no tenant condition at all was accepted")
	}
}

// deepCopy detaches a request's prefetch legs so one assertion's mutation cannot reach another's.
func deepCopy(req retrieval.SearchRequest) retrieval.SearchRequest {
	req.Prefetch = append([]retrieval.PrefetchLeg(nil), req.Prefetch...)
	return req
}

// TestRequestsCarryTenant_ChecksEveryPrefetchLeg is the half most likely to rot. The hybrid query
// has two legs and an outer filter; the tenant condition has to be on all three, and a check that
// only read the outer one would miss a leg that lost it.
func TestRequestsCarryTenant_ChecksEveryPrefetchLeg(t *testing.T) {
	searcher, store := newProbe()
	if _, err := searcher.Search(t.Context(), query(collClientes, tenantA, "contract renewal terms")); err != nil {
		t.Fatalf("Search: %v", err)
	}
	req := store.requests[0]
	if len(req.Prefetch) < 2 {
		t.Fatalf("the request carries %d prefetch leg(s); this test needs the hybrid shape to have "+
			"something to strip", len(req.Prefetch))
	}

	for i := range req.Prefetch {
		mutated := deepCopy(req)
		mutated.Prefetch[i].Filter.Must = nil
		if requestsCarryTenant([]retrieval.SearchRequest{mutated}, tenantA) == "" {
			t.Errorf("a request whose prefetch leg %d lost its tenant condition was accepted", i+1)
		}
	}
}

// TestPrivateVisibilityCase_FixtureIsReachableWithoutTheGuard keeps the private case from passing
// because the note was unreachable anyway. The note has to come back once the exclusion is lifted,
// or its absence proves nothing about the exclusion.
func TestPrivateVisibilityCase_FixtureIsReachableWithoutTheGuard(t *testing.T) {
	for _, seeded := range fixture() {
		if seeded.visibility != "private" {
			continue
		}
		searcher, _ := newProbe()
		q := query(seeded.collection, seeded.tenantID, seeded.text)
		q.IncludePrivate = true

		results, err := searcher.Search(t.Context(), q)
		if err != nil {
			t.Fatalf("privileged search in %s: %v", seeded.collection, err)
		}
		var found bool
		for _, r := range results {
			if r.UID == seeded.uid {
				found = true
			}
		}
		if !found {
			t.Errorf("the private note in %s never comes back even with the guard lifted, so "+
				"PrivateVisibilityCase's assertion that it is absent is vacuous", seeded.collection)
		}
	}
}

func resultsFor(t *testing.T, tenant string) []retrieval.Result {
	t.Helper()
	searcher, _ := newProbe()
	results, err := searcher.Search(t.Context(), query(collClientes, tenant, "contract renewal terms"))
	if err != nil {
		t.Fatalf("Search as %s: %v", tenant, err)
	}
	if len(results) == 0 {
		t.Fatalf("tenant %s matched nothing, so this fixture cannot demonstrate a leak", tenant)
	}
	return results
}

// moduleRoot locates the repository root and refuses to guess: a wrong root would make the
// architecture case scan an empty tree and pass by looking at nothing.
func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolving the module root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("no go.mod at %s — the walk root is wrong, so the architecture case proves nothing: %v", root, err)
	}
	return root
}

// TestExcludes_RejectsWhatItShould drives the must-not helper directly.
//
// PrivateVisibilityCase calls it, but on a clean build the note is absent from the results anyway,
// so disabling this check changes nothing the case can see — a defect plant on it stayed green
// until this existed.
func TestExcludes_RejectsWhatItShould(t *testing.T) {
	with := []retrieval.SearchRequest{{Filter: retrieval.Filter{
		MustNot: []retrieval.Condition{{Field: "visibility", Value: "private"}},
	}}}
	if problem := excludes(with, "visibility", "private"); problem != "" {
		t.Errorf("a request that does exclude private was rejected: %s", problem)
	}

	without := []retrieval.SearchRequest{{Filter: retrieval.Filter{
		MustNot: []retrieval.Condition{{Field: "status", Value: "archived"}},
	}}}
	if excludes(without, "visibility", "private") == "" {
		t.Error("a request with no private exclusion was accepted")
	}
	if excludes(nil, "visibility", "private") == "" {
		t.Error("an empty request log was accepted as proof of an exclusion nothing carried")
	}
	// One request out of several missing the exclusion is still a failure: the case runs a search
	// per collection and every one of them has to carry it.
	mixed := append(append([]retrieval.SearchRequest{}, with...), without...)
	if excludes(mixed, "visibility", "private") == "" {
		t.Error("a batch where only some requests exclude private was accepted")
	}
}
