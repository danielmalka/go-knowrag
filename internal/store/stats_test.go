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
