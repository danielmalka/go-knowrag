package isolation

import (
	"errors"
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
func withProbe(t *testing.T, replacement func() (*retrieval.Searcher, *probeStore)) {
	t.Helper()
	original := newProbe
	newProbe = replacement
	t.Cleanup(func() { newProbe = original })
}

// leakingProbe answers with the whole collection whatever the request asked for — the store you
// would have if the tenant filter were gone from internal/retrieval and Qdrant had no opinion.
func leakingProbe() (*retrieval.Searcher, *probeStore) {
	store := &probeStore{points: fixture(), ignoreFilter: true}
	return newSearcherOver(store), store
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
			// The empty-tenant case asserts a refusal that happens in Query.Validate, before any
			// store is reached, so a leaking store cannot move it. What drives it red is the
			// production plant that stops internal/retrieval refusing an empty tenant, and
			// TestAdversarialCases_FailWhenTheSearcherMisbehaves pins the sentinel it reads.
			if strings.Contains(c.Name, "empty is refused") {
				t.Skip("this case is decided before the store is reached; see the production plant")
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
	withProbe(t, func() (*retrieval.Searcher, *probeStore) {
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

// TestAdversarialCases_FailWhenTheSearcherMisbehaves drives the two tenant-value cases into the
// failures a results-only check would miss: an error where none should be, and results where none
// should be.
func TestAdversarialCases_FailWhenTheSearcherMisbehaves(t *testing.T) {
	t.Run("empty tenant no longer refused", func(t *testing.T) {
		withProbe(t, func() (*retrieval.Searcher, *probeStore) {
			// A searcher that accepts an empty tenant and searches anyway is the exact regression
			// AdversarialTenantEmptyCase exists to catch.
			store := &probeStore{points: fixture(), ignoreFilter: true}
			return newSearcherOver(store), store
		})
		// The real Query.Validate still refuses the empty string, so the case cannot be driven red
		// through the probe alone — it is driven red by the production plant instead. What is
		// checked here is that the case reads the sentinel rather than merely "an error".
		if detail := AdversarialTenantEmptyCase().Run(t.Context()); detail != "" {
			t.Fatalf("the empty-tenant case failed on a healthy build: %s", detail)
		}
		if !errors.Is(sentinelFor(t, ""), retrieval.ErrEmptyTenant) {
			t.Error("the empty tenant no longer answers with retrieval.ErrEmptyTenant, so the case's " +
				"assertion is checking a sentinel nothing produces")
		}
	})

	t.Run("payload tenant starts matching", func(t *testing.T) {
		withProbe(t, leakingProbe)
		if detail := AdversarialTenantPayloadCase().Run(t.Context()); detail == "" {
			t.Fatal("a tenant_id of \"*\" matched every note and the case reported a pass")
		}
	})
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
		"privileged",
		"architecture",
	} {
		if !slices.ContainsFunc(names, func(n string) bool { return strings.Contains(n, want) }) {
			t.Errorf("no case in DefaultSuite mentions %q; it is not being run: %v", want, names)
		}
	}
	if len(names) != 7 {
		t.Errorf("DefaultSuite has %d case(s): %v. A case added without a line here is a case whose "+
			"absence from this list is the only thing that would have flagged it", len(names), names)
	}
}

// TestDefaultSuite_UsesTheRealProbe pins that the seam this file relies on is a test seam and
// nothing more: on a normal run the cases search the hostile probe, not a leaking one.
func TestDefaultSuite_UsesTheRealProbe(t *testing.T) {
	_, store := newProbe()
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

// searchDrivenCases is every case that reaches the store. The architecture case is excluded: it
// scans source and never searches, so a leaking store says nothing about it.
func searchDrivenCases() []Case {
	return []Case{
		CrossTenantCase(),
		AdversarialTenantEmptyCase(),
		AdversarialTenantPayloadCase(),
		QueryTextInjectionCase(),
		PrivateVisibilityCase(),
		PrivilegedPathCase(),
	}
}
