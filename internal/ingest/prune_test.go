package ingest

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestPrune_Unauthorized_DeletesNothing is asserted against the store's call log rather than against
// the returned error, and that is the whole point of the test.
//
// "It returned an error" is compatible with an implementation that deleted everything and then
// complained, which is the exact failure this gate exists to prevent. What has to hold is that
// DeleteByFilter was never reached — so the assertion is on fakeStore.deletes, which records every
// call whether it succeeded or not.
func TestPrune_Unauthorized_DeletesNothing(t *testing.T) {
	orphans := []Orphan{{UID: uuidFromPath(t, "gone.md"), Vault: "pessoal", Path: "gone.md", Points: 2}}

	tests := map[string]struct {
		opts PruneOptions
		want error
	}{
		"nobody confirmed it": {
			opts: PruneOptions{Confirmed: false},
			want: ErrPruneNotConfirmed,
		},
		// A subset run knows which notes it looked at and nothing about the rest, so every uid outside
		// the filter is indistinguishable from a deleted one. Confirmed is true here on purpose: the
		// operator authorized a prune, and the refusal has to survive that.
		"the run only looked at part of the corpus": {
			opts: PruneOptions{Confirmed: true, Filtered: true},
			want: ErrPruneSubset,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			j := &journal{}
			fake := newFakeStore(j)

			pruned, err := Prune(t.Context(), fake, testTenant, orphans, tc.opts)

			if !errors.Is(err, tc.want) {
				t.Errorf("Prune(%+v) = %v, want %v", tc.opts, err, tc.want)
			}
			if pruned != 0 {
				t.Errorf("Prune(%+v) reported %d point(s) removed on a refusal", tc.opts, pruned)
			}
			if len(fake.deletes) != 0 {
				t.Errorf("Prune(%+v) called DeleteByFilter %v times before refusing; the refusal has "+
					"to come before the delete, not after it", tc.opts, fake.deletes)
			}
		})
	}
}

// TestPrune_RemovesTheOrphansAndNothingElse pins both halves of the scope: every point of every uid
// it was given goes, and a uid it was not given is untouched.
//
// The surviving uid is what makes this discriminate. A prune that deleted by tenant alone, or that
// ignored its argument and walked the store, would still empty the orphan and pass any assertion
// written only about it.
func TestPrune_RemovesTheOrphansAndNothingElse(t *testing.T) {
	j := &journal{}
	fake := newFakeStore(j)

	gone, alive := uuidFromPath(t, "gone.md"), uuidFromPath(t, "alive.md")
	fake.seed(testTenant, gone, orphanRecords("pessoal", "gone.md", 3)...)
	fake.seed(testTenant, alive, orphanRecords("pessoal", "alive.md", 2)...)

	orphans := []Orphan{{UID: gone, Vault: "pessoal", Path: "gone.md", Points: 3}}
	pruned, err := Prune(t.Context(), fake, testTenant, orphans, PruneOptions{Confirmed: true})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	if pruned != 3 {
		t.Errorf("Prune reported %d point(s) removed, want 3", pruned)
	}
	if got := fake.indices(testTenant, gone); len(got) != 0 {
		t.Errorf("the orphan still holds chunk_index %v", got)
	}
	if got := fake.indices(testTenant, alive); !slices.Equal(got, []int{0, 1}) {
		t.Errorf("the live note holds chunk_index %v, want [0 1] — the prune reached a uid it was "+
			"not given", got)
	}
	// From chunk_index 0, because the note is gone entirely: anything higher would leave the head of
	// a deleted note answering searches. This is what separates it from the tail prune in note.go.
	want := []deleteCall{{TenantID: testTenant, UID: gone, FromChunkIndex: 0}}
	if !slices.Equal(fake.deletes, want) {
		t.Errorf("Prune issued %v, want %v", fake.deletes, want)
	}
}

// TestPrune_StopsAtTheFirstFailure_ReturnsWhatItRemoved is the number the report renders, and the
// reason the report cannot use one verb over the whole orphan list.
//
// Prune deletes uid by uid, so a failure partway leaves the corpus split: the first note is gone,
// the second is still answering searches. The returned count has to be the points that actually
// left, because that is what an operator reconciles against — and a Prune that returned the total it
// was asked for, or zero, would make the report claim a state the index does not have.
func TestPrune_StopsAtTheFirstFailure_ReturnsWhatItRemoved(t *testing.T) {
	j := &journal{}
	fake := newFakeStore(j)
	fake.deleteErrAfter = 2

	first, second := uuidFromPath(t, "first.md"), uuidFromPath(t, "second.md")
	fake.seed(testTenant, first, orphanRecords("pessoal", "first.md", 2)...)
	fake.seed(testTenant, second, orphanRecords("pessoal", "second.md", 3)...)

	orphans := []Orphan{
		{UID: first, Vault: "pessoal", Path: "first.md", Points: 2},
		{UID: second, Vault: "pessoal", Path: "second.md", Points: 3},
	}
	pruned, err := Prune(t.Context(), fake, testTenant, orphans, PruneOptions{Confirmed: true})

	if err == nil {
		t.Fatal("Prune with a failing delete = nil; the run has to hear that the index is now split")
	}
	if pruned != 2 {
		t.Errorf("Prune reported %d point(s) removed, want 2 — the count is what actually left the "+
			"index, not what was asked for", pruned)
	}
	if !strings.Contains(err.Error(), "second.md") {
		t.Errorf("error %q does not name the orphan it stopped on", err)
	}
	// Stopped, not carried on: the notes after a failing delete would fail the same way, and trying
	// them turns one legible error into a pile.
	if len(fake.deletes) != 2 {
		t.Errorf("Prune issued %d delete(s), want 2 — it has to stop at the first failure",
			len(fake.deletes))
	}
}
