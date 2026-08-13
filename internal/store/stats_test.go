package store

import (
	"context"
	"strings"
	"testing"

	"github.com/qdrant/go-client/qdrant"
)

// statsPoint is one row as the scroll returns it for this read: an id and a uid, which is all the
// projection asks for.
func statsPoint(id, uid string) *qdrant.RetrievedPoint {
	p := &qdrant.RetrievedPoint{Id: qdrant.NewIDUUID(id)}
	if uid != "" {
		p.Payload = map[string]*qdrant.Value{"uid": qdrant.NewValueString(uid)}
	}
	return p
}

// TestStats_CountsPointsAndDistinctUIDsAcrossPages is the whole command in one assertion. The two
// numbers have to disagree — five points from three notes — because a count that returned the same
// figure twice would satisfy any test written with one note per point, and the gap between them is
// the only thing `stats` exists to show.
//
// The uid repeated across the page boundary is the case a per-page count gets wrong: a note whose
// chunks straddle two pages is one note, and an implementation that summed distinct-per-page would
// report it as two.
func TestStats_CountsPointsAndDistinctUIDsAcrossPages(t *testing.T) {
	api := &fakePointAPI{
		pages: [][]*qdrant.RetrievedPoint{
			{statsPoint("p1", "uid-a"), statsPoint("p2", "uid-a"), statsPoint("p3", "uid-b")},
			{statsPoint("p4", "uid-b"), statsPoint("p5", "uid-c")},
		},
		offsets: []*qdrant.PointId{qdrant.NewIDUUID("p3"), nil},
	}

	got, err := newTestClient(t, api).Stats(context.Background(), testTenant)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if want := (Stats{Points: 5, UIDs: 3}); got != want {
		t.Errorf("Stats = %+v, want %+v", got, want)
	}
	if len(api.scrolls) != 2 {
		t.Errorf("the scroll made %d call(s), want 2 — a read that stopped at the first page would "+
			"report a fraction of the collection as the whole of it", len(api.scrolls))
	}
}

// TestStats_TenantFlagNarrowsOrDoesNot covers both scopes, and the empty one is the half worth
// writing: "no tenant given" has to mean every tenant, never a filter matching the empty string,
// which would report a healthy collection as holding nothing.
func TestStats_TenantFlagNarrowsOrDoesNot(t *testing.T) {
	tests := map[string]struct {
		tenantID   string
		wantFilter bool
	}{
		"narrowed to one tenant": {tenantID: testTenant, wantFilter: true},
		"every tenant":           {tenantID: "", wantFilter: false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			api := &fakePointAPI{pages: [][]*qdrant.RetrievedPoint{{statsPoint("p1", "uid-a")}}}
			if _, err := newTestClient(t, api).Stats(context.Background(), tc.tenantID); err != nil {
				t.Fatalf("Stats: %v", err)
			}

			filter := api.scrolls[0].GetFilter()
			if !tc.wantFilter {
				if filter != nil {
					t.Fatalf("an unscoped count carried a filter: %v", filter)
				}
				return
			}
			if filter == nil {
				t.Fatal("a count scoped to one tenant carried no filter, so it counted every tenant")
			}
			if got := filter.GetMust()[0].GetField().GetMatch().GetKeyword(); got != tc.tenantID {
				t.Errorf("the filter matches tenant %q, want %q", got, tc.tenantID)
			}
		})
	}
}

// TestStats_AsksForTheUIDAndNothingElse pins the cost. The payload of a point in this index holds
// the chunk's whole text, so a read that asked for all of it would drag the corpus across the link
// to count rows — and it would still return the right numbers, which is why this needs its own
// assertion rather than showing up as a wrong answer.
func TestStats_AsksForTheUIDAndNothingElse(t *testing.T) {
	api := &fakePointAPI{pages: [][]*qdrant.RetrievedPoint{{statsPoint("p1", "uid-a")}}}
	if _, err := newTestClient(t, api).Stats(context.Background(), testTenant); err != nil {
		t.Fatalf("Stats: %v", err)
	}

	req := api.scrolls[0]
	if fields := req.GetWithPayload().GetInclude().GetFields(); len(fields) != 1 || fields[0] != "uid" {
		t.Errorf("the read projects %v, want exactly [uid]", fields)
	}
	if req.GetWithVectors().GetEnable() {
		t.Error("the read asks for vectors, which are the largest thing a point carries")
	}
}

// TestStats_PointWithoutUID_IsAnErrorNamingIt covers the anomaly this command exists to surface.
// Counting such a point as a distinct uid of "" would fold every one of them into a single phantom
// note and report a corpus that looks very slightly off rather than one with a point nothing wrote.
func TestStats_PointWithoutUID_IsAnErrorNamingIt(t *testing.T) {
	api := &fakePointAPI{pages: [][]*qdrant.RetrievedPoint{
		{statsPoint("p1", "uid-a"), statsPoint("0198a7f2-4b31-7c42-9e15-3d8a92c47bff", "")},
	}}

	_, err := newTestClient(t, api).Stats(context.Background(), testTenant)
	if err == nil {
		t.Fatal("a point carrying no uid was counted instead of reported")
	}
	if got := err.Error(); !strings.Contains(got, "0198a7f2-4b31-7c42-9e15-3d8a92c47bff") {
		t.Errorf("the error %q does not name the point, so nobody can go and look at it", got)
	}
}

// TestStats_EmptyPageWithAnOffset_IsAnErrorNotAPartialCount closes the one gap between what this
// read promises and what its loop did. The doc comment says the pass cannot truncate in silence; a
// page that comes back empty while still handing over an offset used to stop the loop and return
// the count so far as if it were the total.
//
// It is not a shape Qdrant is documented to produce, and that is why it needs a test rather than a
// comment: "the server will not do this" is a claim about code that does not live here, and the cost
// of being wrong is a number that looks right.
func TestStats_EmptyPageWithAnOffset_IsAnErrorNotAPartialCount(t *testing.T) {
	api := &fakePointAPI{
		pages: [][]*qdrant.RetrievedPoint{
			{statsPoint("p1", "uid-a"), statsPoint("p2", "uid-b")},
			{}, // empty, and the offset below says there is more after it
		},
		offsets: []*qdrant.PointId{qdrant.NewIDUUID("p2"), qdrant.NewIDUUID("p2")},
	}

	got, err := newTestClient(t, api).Stats(context.Background(), testTenant)
	if err == nil {
		t.Fatalf("Stats returned %+v and no error for a scroll that stopped early", got)
	}
	if got != (Stats{}) {
		t.Errorf("Stats = %+v alongside an error; the partial count must not be readable as a total", got)
	}
}

// TestStats_OffsetRepeats_IsAnErrorNotInfiniteLoop mirrors
// TestClient_ScrollByUID_OffsetRepeats_IsAnErrorNotInfiniteLoop (D-38): a non-empty page whose next
// offset is the same one this page was requested with looks like "more to fetch" by shape alone, and
// continuing on that shape would re-issue the identical request forever. If this test hangs instead
// of failing, the offset comparison regressed.
func TestStats_OffsetRepeats_IsAnErrorNotInfiniteLoop(t *testing.T) {
	stuck := qdrant.NewIDUUID("p2")
	api := &fakePointAPI{
		pages: [][]*qdrant.RetrievedPoint{
			{statsPoint("p1", "uid-a")},
			{statsPoint("p2", "uid-b")}, // non-empty, but the offset below never moves past `stuck`
		},
		offsets: []*qdrant.PointId{stuck, stuck},
	}

	got, err := newTestClient(t, api).Stats(context.Background(), testTenant)
	if err == nil {
		t.Fatalf("Stats returned %+v and no error for an offset that never advanced", got)
	}
	if got != (Stats{}) {
		t.Errorf("Stats = %+v alongside an error; a stuck read must not be readable as a total", got)
	}
}

// TestStats_ScrollFailure_IsReportedNotCountedAsZero is the same rule the orphan scan follows: not
// having finished looking must never render as having found nothing.
func TestStats_ScrollFailure_IsReportedNotCountedAsZero(t *testing.T) {
	api := &fakePointAPI{
		pages:           [][]*qdrant.RetrievedPoint{{statsPoint("p1", "uid-a")}},
		offsets:         []*qdrant.PointId{qdrant.NewIDUUID("p1")},
		scrollErrOnCall: 2,
	}

	got, err := newTestClient(t, api).Stats(context.Background(), testTenant)
	if err == nil {
		t.Fatal("a scroll that died mid-pagination returned counts as if it had finished")
	}
	if got != (Stats{}) {
		t.Errorf("Stats = %+v alongside an error; a partial count read as a total is worse than none", got)
	}
}
