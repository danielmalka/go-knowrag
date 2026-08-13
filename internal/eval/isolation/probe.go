package isolation

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/danielmalka/go-knowrag/internal/embed"
	"github.com/danielmalka/go-knowrag/internal/retrieval"
)

// The fixture: three tenants sharing a collection, plus one in each of the other two collections.
//
// The content overlaps on purpose. Two of the three are consultancies whose notes both discuss
// contract renewal terms in near-identical wording, because similar content is the case that breaks
// isolation — distinct content would pass a broken filter by accident, since nothing would score
// high enough to leak. The names are invented; this repository is public.
const (
	tenantA = "tenant-alfa"
	tenantB = "tenant-beta"
	tenantC = "tenant-gama"

	collClientes = "clientes"
	collInterno  = "interno"
	collBasePaga = "base_paga"
)

// payloadTenantField is the payload key the whole of this suite turns on: the one internal/retrieval
// puts in its `must` condition and the one internal/ingest writes into every point.
//
// It is one constant used by both halves, and that is what keeps it from being a comment nobody
// rechecks. requestsCarryTenant below inspects the real request under this name, so a rename in
// internal/retrieval/build.go fails every search case; WritePathTenantCase reads the stored payload
// under it, so a rename in internal/ingest/payload.go fails the write case. Neither side can drift
// away from this spelling quietly.
const payloadTenantField = "tenant_id"

// point is one indexed chunk in the probe store.
type point struct {
	uid        string
	chunkIndex int
	collection string
	tenantID   string
	visibility string
	status     string
	text       string
}

// fixture is the corpus every case searches. Deterministic by construction: a package-level literal
// built by a function, so two calls and two processes see the same thing in the same order.
func fixture() []point {
	return []point{
		// tenant-alfa and tenant-beta: same domain, near-identical phrasing. This overlap is the
		// point — a filter that leaked would leak here first, because these score alike.
		{uid: "aaaaaaaa-1111-4111-8111-111111111111", collection: collClientes, tenantID: tenantA,
			text: "contract renewal terms: the renewal window opens ninety days before expiry"},
		{uid: "aaaaaaaa-2222-4222-8222-222222222222", collection: collClientes, tenantID: tenantA,
			text: "contract renewal terms: escalation contacts during the renewal window"},
		{uid: "bbbbbbbb-1111-4111-8111-111111111111", collection: collClientes, tenantID: tenantB,
			text: "contract renewal terms: the renewal window opens sixty days before expiry"},
		{uid: "bbbbbbbb-2222-4222-8222-222222222222", collection: collClientes, tenantID: tenantB,
			text: "contract renewal terms: penalties that apply inside the renewal window"},
		{uid: "cccccccc-1111-4111-8111-111111111111", collection: collClientes, tenantID: tenantC,
			text: "contract renewal terms: the renewal window is negotiated per engagement"},

		// One private note per collection. The private rule is not keyed on collection (S07 T5), and
		// a fixture that only had a private note in `interno` would pass a rule that was.
		{uid: "dddddddd-1111-4111-8111-111111111111", collection: collClientes, tenantID: tenantA,
			visibility: "private", text: "contract renewal terms: private margin notes on this account"},
		{uid: "dddddddd-2222-4222-8222-222222222222", collection: collInterno, tenantID: tenantA,
			visibility: "private", text: "contract renewal terms: private internal position"},
		{uid: "dddddddd-3333-4333-8333-333333333333", collection: collBasePaga, tenantID: tenantC,
			visibility: "private", text: "contract renewal terms: private paid-base annotation"},

		// Archived, to keep the default status exclusion honest alongside the visibility one.
		{uid: "eeeeeeee-1111-4111-8111-111111111111", collection: collClientes, tenantID: tenantB,
			status: "archived", text: "contract renewal terms: superseded renewal policy"},
	}
}

func tenants() []string { return []string{tenantA, tenantB, tenantC} }

// archivedFixture is the seeded archived note, and reports whether there is one.
//
// The bool is the point: ArchivedExclusionCase asserts that a note does not come back, and over a
// fixture that seeds no archived note that assertion holds against nothing at all.
func archivedFixture() (point, bool) {
	for _, p := range fixture() {
		if p.status == "archived" {
			return p, true
		}
	}
	return point{}, false
}

// probeStore is a deliberately hostile stand-in for Qdrant.
//
// It is hostile in the one way that matters: it applies **only** the conditions the request carries,
// with no scoping of its own and no memory of who asked. Drop the tenant condition from
// internal/retrieval and this store answers with every tenant's notes, which is exactly what makes
// the zero-leak assertions non-vacuous — they fail because a foreign point came back, not because a
// mock politely returned nothing.
//
// The matcher is deliberately trivial (payload field equals value) and is not a second
// implementation of internal/retrieval's filter: it interprets conditions, it never invents them.
// Everything that decides *which* conditions exist lives in internal/retrieval/build.go.
type probeStore struct {
	points []point
	// requests is every request that reached the store, which is how a case proves what was asked
	// rather than inferring it from what came back.
	requests []retrieval.SearchRequest

	// ignoreFilter turns the store from hostile into openly leaking: it answers with the whole
	// collection whatever the request said. Nothing in the suite sets it — cases_nonvacuity_test.go
	// does, to drive every case into the failure it is supposed to detect. Without a way to do that,
	// a case whose assertions had all been disabled would still return "" and still report a pass.
	ignoreFilter bool

	// recordAs rewrites the request this store logs, leaving the request it answers untouched.
	//
	// It is the seam for the two assertions no result can reach. A tenant condition missing from the
	// prefetch legs alone changes nothing about the answer, because matches() below reads the outer
	// filter and never the legs; a MustNot that was never sent changes nothing either, if the store
	// excludes those points on its own account — which is the helpful mock this package exists not
	// to be. Nothing in the suite sets it; cases_nonvacuity_test.go does, to prove the request
	// inspections in cases.go are load-bearing rather than decorative.
	recordAs func(retrieval.SearchRequest) retrieval.SearchRequest
}

func (s *probeStore) ExecuteQuery(_ context.Context, req retrieval.SearchRequest) ([]retrieval.ScoredPoint, error) {
	logged := req
	if s.recordAs != nil {
		logged = s.recordAs(req)
	}
	s.requests = append(s.requests, logged)

	var out []retrieval.ScoredPoint
	for _, p := range s.points {
		// The collection is part of the request, and a store that ignored it would let a case pass
		// while the wrong index was named.
		if p.collection != req.Collection {
			continue
		}
		if !s.ignoreFilter && !matches(p, req.Filter) {
			continue
		}
		out = append(out, retrieval.ScoredPoint{
			ID:    p.uid,
			Score: 1,
			Payload: map[string]any{
				"uid": p.uid, "chunk_index": p.chunkIndex, "text": p.text, "path": p.uid + ".md",
			},
		})
	}
	return out, nil
}

// matches applies one filter to one point. Absent payload fields compare as empty, which is what
// makes `visibility != private` exclude only the notes that say private.
func matches(p point, f retrieval.Filter) bool {
	for _, c := range f.Must {
		if field(p, c.Field) != c.Value {
			return false
		}
	}
	for _, c := range f.MustNot {
		if field(p, c.Field) == c.Value {
			return false
		}
	}
	return true
}

func field(p point, name string) string {
	switch name {
	case payloadTenantField:
		return p.tenantID
	case "visibility":
		return p.visibility
	case "status":
		return p.status
	case "uid":
		return p.uid
	default:
		// An unknown field matches nothing rather than everything. A store that shrugged at a
		// condition it did not recognise would let a renamed payload field silently widen the scope.
		return "\x00unknown-field:" + name
	}
}

// caseSearcher is the one thing a case needs: something that answers a Query.
//
// It is an interface only so the suite's own tests can put a case in front of a searcher that
// misbehaves. That is not a convenience: AdversarialTenantEmptyCase asserts a refusal that happens
// in Query.Validate (retrieval/query.go) before any store is reached, so no store — however hostile
// — can drive that case red, and a searcher that accepts what production refuses is the only seam
// that can. TestDefaultSuite_UsesTheRealProbe pins that the shipped probe is the production type.
type caseSearcher interface {
	Search(ctx context.Context, q retrieval.Query) ([]retrieval.Result, error)
}

// newProbe builds the real *retrieval.Searcher over the hostile store.
//
// The searcher is the production type with the production config: nothing about the query shape,
// the filter or the validation is stubbed. Only the two ends are — a deterministic offline embedder
// (internal/embed's own fake) and the store above.
//
// It is a var, not a func, for one reason: the suite's own tests swap it for a leaking probe to
// prove each case fails when isolation is gone (cases_nonvacuity_test.go). Nothing in the shipped
// suite reassigns it, and TestDefaultSuite_UsesTheRealProbe pins that.
var newProbe = func() (caseSearcher, *probeStore) {
	store := &probeStore{points: fixture()}
	return newSearcherOver(store), store
}

// newSearcherOver wraps one store in the production searcher. Spelled once so the suite and its own
// tests cannot end up constructing it differently — a test searcher built with a different Config
// would be measuring a query shape production never sends.
func newSearcherOver(store *probeStore) *retrieval.Searcher {
	return retrieval.NewSearcher(embed.FakeEmbedder{}, store, retrieval.DefaultConfig())
}

// tenantOf reports which fixture tenant a returned result belongs to. Results carry no tenant_id —
// the payload projection does not include it (retrieval/build.go) — so ownership is resolved
// through the fixture the probe seeded, by uid.
func tenantOf(uid string) string {
	for _, p := range fixture() {
		if p.uid == uid {
			return p.tenantID
		}
	}
	return "unknown:" + uid
}

// foreignResults names every result that does not belong to tenantID. Exact equality, never a
// prefix or substring test: `tenant-alfa` and `tenant-alfa-2` are different tenants, and a
// HasPrefix check would call the second one the first one's own data.
func foreignResults(tenantID string, results []retrieval.Result) []string {
	var leaks []string
	for _, r := range results {
		if owner := tenantOf(r.UID); owner != tenantID {
			leaks = append(leaks, fmt.Sprintf("uid=%s belongs to %q", r.UID, owner))
		}
	}
	return leaks
}

// requestsCarryTenant checks every request that reached the store, and every prefetch leg inside
// it, for the tenant condition.
//
// This is the assertion that does not depend on what came back. A case that only checked results
// would pass against a store that happened to hold nothing for the other tenants — the "empty for
// the wrong reason" failure this suite exists to avoid.
func requestsCarryTenant(reqs []retrieval.SearchRequest, tenantID string) string {
	if len(reqs) == 0 {
		return "no request reached the store, so nothing was proven about the filter it would carry"
	}
	want := retrieval.Condition{Field: payloadTenantField, Value: tenantID}
	for i, req := range reqs {
		if !slices.Contains(req.Filter.Must, want) {
			return fmt.Sprintf("request %d went out with no tenant condition on its own filter: %+v",
				i+1, req.Filter)
		}
		for j, leg := range req.Prefetch {
			if !slices.Contains(leg.Filter.Must, want) {
				return fmt.Sprintf("request %d prefetch leg %d (%s) went out with no tenant condition: %+v",
					i+1, j+1, leg.Vector, leg.Filter)
			}
		}
		// And no second tenant condition naming somebody else — one `must` per field is what makes
		// the scope a scope, and two would be a contradiction Qdrant resolves by matching nothing,
		// which would look like isolation while meaning confusion.
		for _, c := range req.Filter.Must {
			if c.Field == payloadTenantField && c.Value != tenantID {
				return fmt.Sprintf("request %d carries a second tenant condition %q alongside %q",
					i+1, c.Value, tenantID)
			}
		}
	}
	return ""
}

// query is the shape every case attacks with.
func query(collection, tenantID, text string) retrieval.Query {
	return retrieval.Query{Collection: collection, TenantID: tenantID, Text: text, TopK: 10}
}

// overlappingQueries are the phrases the fixture's tenants share. Every tenant's notes score alike
// on these, so a leak has something to leak.
func overlappingQueries() []string {
	return []string{
		"contract renewal terms",
		"when does the renewal window open",
		"escalation contacts and penalties during renewal",
	}
}

func joinLeaks(leaks []string) string { return strings.Join(leaks, "; ") }
