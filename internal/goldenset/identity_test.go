package goldenset

import "testing"

// TestEntryIdentity_IsContentNotPosition moved here with EntryIdentity itself. It hashes only
// question and uid, so the assertions are about this package's type and need nothing from the
// gate that consumes the hash (internal/eval/provenance.go).
func TestEntryIdentity_IsContentNotPosition(t *testing.T) {
	q := GoldenQuestion{Question: "what does the runbook say", UID: uidA, Area: "alfa",
		Author: "owner", Date: "2026-08-11"}

	same := q
	same.Area, same.Author, same.Date = "beta", "somebody else", "2030-01-01"
	if EntryIdentity(q) != EntryIdentity(same) {
		t.Error("changing area/author/date changed the identity; those are not what makes an entry " +
			"the same question, and reordering the file would then rewrite provenance")
	}

	for _, changed := range []GoldenQuestion{
		{Question: q.Question + "?", UID: q.UID},
		{Question: q.Question, UID: uidB},
	} {
		if EntryIdentity(q) == EntryIdentity(changed) {
			t.Errorf("a different question/uid pair hashes the same: %+v", changed)
		}
	}

	// The NUL separator, so ("ab","c") and ("a","bc") cannot collide. A UUID contains no NUL, so the
	// split point is unambiguous — this asserts the separator is actually there.
	if EntryIdentity(GoldenQuestion{Question: "ab", UID: "c"}) ==
		EntryIdentity(GoldenQuestion{Question: "a", UID: "bc"}) {
		t.Error("the identity concatenates without a separator, so two different entries collide")
	}
}
