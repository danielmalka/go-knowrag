package isolation

import (
	"slices"
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/retrieval"
)

// TestProbeStore_LeaksWhenTheFilterDoesNotStopIt is the load-bearing test of this whole package.
//
// Every zero-leak case here is worth exactly as much as the store's willingness to leak. If the
// probe scoped results on its own — the way a helpful mock would — then "no foreign results" would
// be a fact about the mock, and the suite would stay green through a build with no tenant filter at
// all. So this drives the store directly with a filter that names no tenant and proves it hands
// over every tenant's notes.
//
// This is the "empty for the right reason" guard the suite is built around, stated as a test rather
// than as a comment.
func TestProbeStore_LeaksWhenTheFilterDoesNotStopIt(t *testing.T) {
	store := &probeStore{points: fixture()}

	unscoped, err := store.ExecuteQuery(t.Context(), retrieval.SearchRequest{Collection: collClientes})
	if err != nil {
		t.Fatalf("ExecuteQuery: %v", err)
	}

	owners := map[string]bool{}
	for _, p := range unscoped {
		owners[tenantOf(p.ID)] = true
	}
	if len(owners) < 3 {
		t.Fatalf("an unfiltered request saw %d tenant(s) in %s, want all three — the probe scopes on "+
			"its own, so every zero-leak case in this package is vacuous", len(owners), collClientes)
	}

	// And with the tenant condition it hands over one tenant's notes only, so the leak above is the
	// filter's absence rather than the store being broken.
	scoped, err := store.ExecuteQuery(t.Context(), retrieval.SearchRequest{
		Collection: collClientes,
		Filter:     retrieval.Filter{Must: []retrieval.Condition{{Field: "tenant_id", Value: tenantA}}},
	})
	if err != nil {
		t.Fatalf("ExecuteQuery: %v", err)
	}
	if len(scoped) == 0 {
		t.Fatal("a tenant-scoped request matched nothing, so the fixture cannot show a leak either way")
	}
	for _, p := range scoped {
		if owner := tenantOf(p.ID); owner != tenantA {
			t.Errorf("a request scoped to %s returned %s, owned by %s", tenantA, p.ID, owner)
		}
	}
}

// TestProbeStore_HonoursTheCollection keeps a case from passing against the wrong index. Without
// it, a request naming `base_paga` that came back with `clientes` notes would look like a clean run.
func TestProbeStore_HonoursTheCollection(t *testing.T) {
	store := &probeStore{points: fixture()}

	for _, collection := range []string{collClientes, collInterno, collBasePaga} {
		got, err := store.ExecuteQuery(t.Context(), retrieval.SearchRequest{Collection: collection})
		if err != nil {
			t.Fatalf("ExecuteQuery: %v", err)
		}
		if len(got) == 0 {
			t.Errorf("collection %s holds nothing, so no case scoped to it can prove anything", collection)
		}
		for _, p := range got {
			if seededIn(p.ID) != collection {
				t.Errorf("a request for %s returned %s, seeded in %s", collection, p.ID, seededIn(p.ID))
			}
		}
	}
}

// TestProbeStore_UnknownFieldMatchesNothing pins the fail-closed default. A store that shrugged at
// a condition it did not recognise would let a renamed payload field silently widen every scope
// this suite checks, and every case would stay green while doing it.
func TestProbeStore_UnknownFieldMatchesNothing(t *testing.T) {
	store := &probeStore{points: fixture()}

	// The value has to be the empty string. A condition of {tenant, tenant-alfa} excludes everything
	// whether the unknown field reads as "" or as a sentinel, so it would pass against a `field`
	// that had quietly started returning "" — which is the exact defect this test exists to catch.
	for _, value := range []string{"", tenantA} {
		got, err := store.ExecuteQuery(t.Context(), retrieval.SearchRequest{
			Collection: collClientes,
			Filter:     retrieval.Filter{Must: []retrieval.Condition{{Field: "tenant", Value: value}}},
		})
		if err != nil {
			t.Fatalf("ExecuteQuery: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("a condition on the unknown field %q=%q matched %d point(s); an unrecognised "+
				"condition must exclude, not be ignored", "tenant", value, len(got))
		}
	}
}

// TestFixture_TenantsOverlapInContent is the fixture requirement that makes the suite adversarial:
// similar content between tenants is the case that breaks isolation, because distinct content would
// not score high enough to leak even through a filter that was gone.
func TestFixture_TenantsOverlapInContent(t *testing.T) {
	if got := len(tenants()); got < 3 {
		t.Fatalf("%d tenant(s) in the fixture, want at least 3", got)
	}

	// Every tenant, not merely two of them. "At least two overlap" is satisfied by a fixture where
	// the third tenant talks about something else entirely, and that third tenant's cases would then
	// pass without the filter ever having to separate anything.
	// Only the notes the cross-tenant case actually searches count: `clientes`, visible, not
	// archived. A private note in another collection carrying the phrase satisfied this check while
	// contributing nothing to the overlap the case exercises — found by a defect plant.
	const shared = "contract renewal terms"
	owners := map[string]bool{}
	for _, p := range fixture() {
		if p.collection == collClientes && p.visibility == "" && p.status == "" &&
			strings.Contains(p.text, shared) {
			owners[p.tenantID] = true
		}
	}
	for _, tenant := range tenants() {
		if !owners[tenant] {
			t.Errorf("tenant %q has no note containing %q; similar content between tenants is the "+
				"case that breaks isolation, and distinct content proves nothing", tenant, shared)
		}
	}

	// Every tenant needs something to find, or its cases pass because it asked an empty index.
	for _, tenant := range tenants() {
		if !slices.ContainsFunc(fixture(), func(p point) bool {
			return p.tenantID == tenant && p.visibility == "" && p.status == ""
		}) {
			t.Errorf("tenant %q has no visible note, so its zero-leak result is indistinguishable "+
				"from an empty index", tenant)
		}
	}
}

// TestFixture_PrivateNoteInEveryCollection covers the rule that used to be keyed on the collection.
// A fixture with a private note only in `interno` would pass the old, broken rule as happily as the
// current one.
func TestFixture_PrivateNoteInEveryCollection(t *testing.T) {
	seeded := map[string]bool{}
	for _, p := range fixture() {
		if p.visibility == "private" {
			seeded[p.collection] = true
		}
	}
	for _, want := range []string{collInterno, collClientes, collBasePaga} {
		if !seeded[want] {
			t.Errorf("no private note seeded in %s, so PrivateVisibilityCase never checks it", want)
		}
	}
}

// TestFixture_IsDeterministic keeps two runs of the suite comparable. A map iteration or a random
// seed leaking into the fixture would make a failure impossible to reproduce from the report.
func TestFixture_IsDeterministic(t *testing.T) {
	first, second := fixture(), fixture()
	if !slices.Equal(first, second) {
		t.Error("two calls to fixture() produced different corpora")
	}
	// And callers cannot reach into it: fixture() returns a fresh slice, so a case that mutated its
	// copy cannot change what the next case searches.
	first[0].tenantID = "mutated"
	if fixture()[0].tenantID == "mutated" {
		t.Error("fixture() hands out a shared slice; one case could rewrite another case's corpus")
	}
}

func seededIn(uid string) string {
	for _, p := range fixture() {
		if p.uid == uid {
			return p.collection
		}
	}
	return "unknown"
}
