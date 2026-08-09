package ingest

// The crash and corruption scenarios of S06a's companion doc (T9–T14, T17, T20). They live apart
// from note_test.go only to keep both files under the repo's 500-line cap — the fixtures and the
// harness are shared, and every one of them exercises the same ProcessNote.

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestProcessNote_CrashBetweenEmbedAndUpsert is T9: a run that stops after embedding leaves nothing
// behind, and the next round reprocesses cleanly.
func TestProcessNote_CrashBetweenEmbedAndUpsert(t *testing.T) {
	h := newHarness()
	n := testNote(t, "research/curadoria/nota.md", 3)

	// The crash: the embedder succeeds, then the store is never reached.
	h.store.upsertHook = func(_ int, _ []Point) (int, UpsertOutcome, error) {
		return 0, UpsertAmbiguous, errors.New("process terminated before the write landed")
	}
	if res := ProcessNote(t.Context(), h.deps, n); res.State != StateFailed {
		t.Fatalf("State = %s, want %s", res.State, StateFailed)
	}
	if got := h.store.indices(testTenant, n.UID); len(got) != 0 {
		t.Fatalf("the store holds %v after a crash before the write", got)
	}

	// The next round: full reprocess, no orphans.
	h.store.upsertHook = nil
	h.reset()
	assertReembedded(t, h, ProcessNote(t.Context(), h.deps, n))
	if got, want := h.store.indices(testTenant, n.UID), []int{0, 1, 2}; !slices.Equal(got, want) {
		t.Errorf("point set = %v, want %v", got, want)
	}
}

// TestProcessNote_CrashBetweenUpsertAndPrune is T10: the window insert-then-prune deliberately
// accepts. Points 0..N-1 already carry the *current* hash, so conditions 1, 3 and 4 all pass — only
// condition 2 sees the orphan tail, and it is what forces the reprocess.
func TestProcessNote_CrashBetweenUpsertAndPrune(t *testing.T) {
	h := newHarness()
	n := testNote(t, "research/curadoria/nota.md", 3)

	// The state a crash between a confirmed upsert and its prune leaves: the three current points,
	// plus the tail of the five-chunk version that came before.
	h.store.seed(testTenant, n.UID, expectedFor(t, n)...)
	h.store.seed(testTenant, n.UID,
		PointRecord{ChunkIndex: 3, PointHash: "stale-3", Fields: map[string]any{}},
		PointRecord{ChunkIndex: 4, PointHash: "stale-4", Fields: map[string]any{}},
	)

	res := ProcessNote(t.Context(), h.deps, n)
	assertReembedded(t, h, res)
	if got, want := h.store.indices(testTenant, n.UID), []int{0, 1, 2}; !slices.Equal(got, want) {
		t.Errorf("point set = %v, want %v — the orphan tail survived the round", got, want)
	}
}

// TestProcessNote_OrphansPresent_NeverSkips is T13, stated as the property rather than as a
// convergence check: condition 2 failing alone must be enough to block the short-circuit, even
// though every point that *is* present has the right hash and the right payload.
func TestProcessNote_OrphansPresent_NeverSkips(t *testing.T) {
	h := newHarness()
	n := testNote(t, "research/curadoria/nota.md", 3)
	h.store.seed(testTenant, n.UID, expectedFor(t, n)...)
	h.store.seed(testTenant, n.UID,
		PointRecord{ChunkIndex: 3, PointHash: "orphan", Fields: map[string]any{}})

	res := ProcessNote(t.Context(), h.deps, n)
	if res.State == StateSkipped {
		t.Fatal("a uid with an orphan point short-circuited to skipped; the temporary orphan just " +
			"became a permanent one (risk R10)")
	}
	if h.embed.calls != 1 {
		t.Errorf("the embedder was called %d time(s), want 1", h.embed.calls)
	}
}

// TestProcessNote_PartialUpsert_Converges is T11: whatever arbitrary subset of the N points a crash
// leaves written, the next round detects it and converges. The failure is injected *inside* the
// upsert call, not in the upsert→prune window — the upsert is never assumed atomic (ADR-006 §4).
func TestProcessNote_PartialUpsert_Converges(t *testing.T) {
	const chunks = 6
	cases := []struct {
		name    string
		written int
	}{
		{"first point only", 1},
		{"half", chunks / 2},
		{"all but one", chunks - 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			n := testNote(t, "research/curadoria/nota.md", chunks)
			h.store.upsertHook = func(_ int, _ []Point) (int, UpsertOutcome, error) {
				return tc.written, UpsertFailed, errors.New("connection dropped mid-batch")
			}

			if res := ProcessNote(t.Context(), h.deps, n); res.State != StateFailed {
				t.Fatalf("State = %s, want %s", res.State, StateFailed)
			}
			if got := len(h.store.indices(testTenant, n.UID)); got != tc.written {
				t.Fatalf("the store holds %d point(s), want the %d that landed", got, tc.written)
			}

			// The next round sees the truth and finishes the job.
			h.store.upsertHook = nil
			h.reset()
			res := ProcessNote(t.Context(), h.deps, n)
			assertReembedded(t, h, res)
			if got, want := h.store.indices(testTenant, n.UID), rangeTo(chunks); !slices.Equal(got, want) {
				t.Errorf("point set = %v, want %v", got, want)
			}
		})
	}
}

// TestProcessNote_LocalizedEditFailure_ResultsInIntegral is T12, and it is a *documented limit*, not
// a bug.
//
// Only chunk 0 changed, and its point was written before the crash. The other points never needed to
// change, so their stored hash still matches the recomputed one. The index is structurally correct,
// so the predicate says integral and nothing happens. Contrast T20, which is the case where a point
// that *did* need updating did not get it — that one is caught.
//
// If this ever fails, condition 3 or 4 is over-triggering on unchanged points, which is a real bug.
func TestProcessNote_LocalizedEditFailure_ResultsInIntegral(t *testing.T) {
	h := newHarness()
	path := "research/curadoria/nota.md"
	before := noteWithBodies(t, path, "original zero", "body of section 1", "body of section 2")
	h.ingestOnce(t, before)

	after := noteWithBodies(t, path, "edited zero", "body of section 1", "body of section 2")
	h.reset()
	h.store.upsertHook = func(_ int, _ []Point) (int, UpsertOutcome, error) {
		return 1, UpsertFailed, errors.New("crashed after writing point 0")
	}
	if res := ProcessNote(t.Context(), h.deps, after); res.State != StateFailed {
		t.Fatalf("setup: State = %s, want %s", res.State, StateFailed)
	}

	h.store.upsertHook = nil
	h.reset()
	res := ProcessNote(t.Context(), h.deps, after)
	if res.State != StateSkipped {
		t.Fatalf("State = %s, want %s — the index is structurally correct and reprocessing it would "+
			"be work with nothing to show for it", res.State, StateSkipped)
	}
	if h.embed.calls != 0 {
		t.Errorf("the embedder was called %d time(s) for an index that is already correct", h.embed.calls)
	}
}

// TestProcessNote_DangerousPartialWrite_CatchesUnwrittenChange is T20, the contrast to T12: chunks 0
// and 2 both changed, only point 0 was written, and point 2 is still serving text the note no longer
// contains. Condition 3 catches it.
func TestProcessNote_DangerousPartialWrite_CatchesUnwrittenChange(t *testing.T) {
	h := newHarness()
	path := "research/curadoria/nota.md"
	before := noteWithBodies(t, path, "original zero", "body of section 1", "original two")
	h.ingestOnce(t, before)

	after := noteWithBodies(t, path, "edited zero", "body of section 1", "edited two")
	h.reset()
	h.store.upsertHook = func(_ int, _ []Point) (int, UpsertOutcome, error) {
		return 1, UpsertFailed, errors.New("crashed after writing point 0")
	}
	if res := ProcessNote(t.Context(), h.deps, after); res.State != StateFailed {
		t.Fatalf("setup: State = %s, want %s", res.State, StateFailed)
	}

	// Point 0 is correct; point 2 is stale. The predicate has to see the second one.
	expected := ExpectedPoints(after, chunksOf(t, after), testTenant,
		NewPipelineConfig(testChunkConfig()), testHandshake())
	records, err := h.store.ScrollByUID(t.Context(), testTenant, after.UID)
	if err != nil {
		t.Fatalf("ScrollByUID: %v", err)
	}
	check := CheckIntegrity(records, expected)
	if check.Integral {
		t.Fatal("a chunk that changed and was never written left the uid integral — the index serves " +
			"text the note no longer contains, permanently")
	}
	if want := "chunk_index 2"; !containsAll(check.Reason, "condition 3", want) {
		t.Errorf("reason %q does not blame condition 3 at %s", check.Reason, want)
	}

	// And the full reprocess restores integrity for the whole uid.
	h.store.upsertHook = nil
	h.reset()
	assertReembedded(t, h, ProcessNote(t.Context(), h.deps, after))
	h.reset()
	if res := ProcessNote(t.Context(), h.deps, after); res.State != StateSkipped {
		t.Errorf("after the reprocess the note is still %s, want %s", res.State, StateSkipped)
	}
}

// TestProcessNote_ChunkCountChanges is T17's two directions: the same insert-then-prune mechanism
// has to handle a note that shrank and one that grew.
func TestProcessNote_ChunkCountChanges(t *testing.T) {
	cases := []struct {
		name     string
		from, to int
	}{
		{"shrink 8 to 5", 8, 5},
		{"grow 5 to 8", 5, 8},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			path := "research/curadoria/nota.md"
			h.ingestOnce(t, testNote(t, path, tc.from))
			h.reset()

			res := ProcessNote(t.Context(), h.deps, testNote(t, path, tc.to))
			if res.State != StatePruned {
				t.Fatalf("State = %s, want %s (%v)", res.State, StatePruned, res.Err)
			}
			uid := testNote(t, path, tc.to).UID
			if got, want := h.store.indices(testTenant, uid), rangeTo(tc.to); !slices.Equal(got, want) {
				t.Errorf("point set = %v, want %v", got, want)
			}
		})
	}
}

// TestProcessNote_CrossTenantSameUID_PruneDoesNotCrossTenants: two tenants legitimately share a uid
// in one collection — which is exactly what putting tenant_id in the point ID made possible — and
// reingesting one must leave the other untouched, byte for byte.
func TestProcessNote_CrossTenantSameUID_PruneDoesNotCrossTenants(t *testing.T) {
	const tenantB = "tenant-b"
	h := newHarness()
	path := "research/curadoria/nota.md"
	big := testNote(t, path, 8)

	// Tenant A ingests the eight-chunk note; tenant B holds its own eight points for the same uid.
	h.ingestOnce(t, big)
	neighbour := expectedFor(t, big)
	h.store.seed(tenantB, big.UID, neighbour...)
	h.reset()

	if res := ProcessNote(t.Context(), h.deps, testNote(t, path, 5)); res.State != StatePruned {
		t.Fatalf("State = %s, want %s (%v)", res.State, StatePruned, res.Err)
	}

	if got, want := h.store.indices(testTenant, big.UID), rangeTo(5); !slices.Equal(got, want) {
		t.Errorf("tenant A point set = %v, want %v", got, want)
	}
	if got, want := h.store.indices(tenantB, big.UID), rangeTo(8); !slices.Equal(got, want) {
		t.Fatalf("tenant B point set = %v, want %v — the prune crossed the tenant boundary and "+
			"destroyed the neighbour's data", got, want)
	}
	for _, rec := range neighbour {
		got, ok := h.store.record(tenantB, big.UID, rec.ChunkIndex)
		if !ok || got.PointHash != rec.PointHash {
			t.Errorf("tenant B's point %d changed", rec.ChunkIndex)
		}
	}
	for _, d := range h.store.deletes {
		if d.TenantID != testTenant {
			t.Errorf("a delete was scoped to %q, which is not the tenant being ingested", d.TenantID)
		}
	}
}

func rangeTo(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
