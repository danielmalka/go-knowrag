package ingest

import (
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/danielmalka/go-knowrag/internal/schema"
	"github.com/danielmalka/go-knowrag/internal/vault"
)

// integrityFixture is a note, its expected points, and the records a healthy index would hold for
// it — the three things every predicate test starts from.
type integrityFixture struct {
	note     vault.Note
	expected []ExpectedPoint
	records  []PointRecord
}

func newIntegrityFixture(t *testing.T, chunks int) integrityFixture {
	t.Helper()
	n := testNote(t, "research/curadoria/nota.md", chunks)
	return integrityFixture{
		note:     n,
		expected: ExpectedPoints(n, chunksOf(t, n), testTenant, NewPipelineConfig(testChunkConfig()), testHandshake()),
		records:  expectedFor(t, n),
	}
}

// TestCheckIntegrity is the four-condition table: the all-good case, then each condition violated on
// its own.
func TestCheckIntegrity(t *testing.T) {
	cases := []struct {
		name     string
		corrupt  func(f *integrityFixture)
		integral bool
		mentions string
	}{
		{
			name:     "everything matches",
			corrupt:  func(*integrityFixture) {},
			integral: true,
		},
		{
			name: "condition 1: a chunk_index in 0..N-1 is missing",
			corrupt: func(f *integrityFixture) {
				// Removed, and one duplicate of another index put back, so the *count* still matches
				// N — otherwise condition 2 would fire and condition 1 would go untested.
				f.records[1] = f.records[2]
			},
			integral: false,
			mentions: "condition 1",
		},
		{
			name: "condition 2: an excess point beyond N-1",
			corrupt: func(f *integrityFixture) {
				orphan := PointRecord{
					ChunkIndex: len(f.expected),
					PointHash:  "whatever-the-previous-version-had",
					Fields:     maps.Clone(f.records[0].Fields),
				}
				f.records = append(f.records, orphan)
			},
			integral: false,
			mentions: "condition 2",
		},
		{
			name: "condition 3: one stale point_hash among fresh ones",
			corrupt: func(f *integrityFixture) {
				f.records[1].PointHash = "0000000000000000000000000000000000000000000000000000000000000000"
			},
			integral: false,
			mentions: "condition 3",
		},
		{
			name: "condition 4: a payload field edited without touching point_hash",
			corrupt: func(f *integrityFixture) {
				f.records[1].Fields[fieldVisibility] = schema.VisibilityShareable().String()
			},
			integral: false,
			mentions: "condition 4",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newIntegrityFixture(t, 3)
			tc.corrupt(&f)

			got := CheckIntegrity(f.records, f.expected)
			if got.Integral != tc.integral {
				t.Fatalf("Integral = %v, want %v (reason: %q)", got.Integral, tc.integral, got.Reason)
			}
			if tc.mentions != "" && !strings.Contains(got.Reason, tc.mentions) {
				t.Errorf("reason %q does not name %q, so an operator cannot tell which condition failed",
					got.Reason, tc.mentions)
			}
		})
	}
}

// TestCheckIntegrity_SinglePointHashMismatch_MarksNonIntegral is T14: one point out of N is enough,
// and the reason isolates that point rather than blaming the note as a whole.
func TestCheckIntegrity_SinglePointHashMismatch_MarksNonIntegral(t *testing.T) {
	f := newIntegrityFixture(t, 5)
	f.records[3].PointHash = "manually-edited"

	got := CheckIntegrity(f.records, f.expected)
	if got.Integral {
		t.Fatal("a single stale point_hash among four fresh ones left the uid integral; the note would " +
			"keep serving one chunk of text that is no longer in it")
	}
	if !strings.Contains(got.Reason, "chunk_index 3") {
		t.Errorf("reason %q does not isolate the offending point", got.Reason)
	}
}

// TestCheckIntegrity_ManualPayloadCorruption is T19: condition 4 catches what condition 3
// structurally cannot — a payload edited directly in Qdrant, leaving point_hash intact.
//
// The three fields are the ones PRD-contrato calls most sensitive. tenant_id is the one that
// matters most: it decides isolation, and a hand-edit of it moves a point into another tenant's
// search results without a single hash changing.
func TestCheckIntegrity_ManualPayloadCorruption(t *testing.T) {
	cases := []struct {
		field string
		value string
	}{
		{fieldVisibility, schema.VisibilityPrivate().String()},
		{fieldStatus, schema.StatusArchived().String()},
		{fieldTenantID, "tenant-b"},
	}

	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			f := newIntegrityFixture(t, 3)
			f.records[1].Fields[tc.field] = tc.value

			got := CheckIntegrity(f.records, f.expected)
			if got.Integral {
				t.Fatalf("%s was altered directly in the store and the uid stayed integral", tc.field)
			}
			if !strings.Contains(got.Reason, "condition 4") {
				t.Errorf("reason %q blames something other than condition 4", got.Reason)
			}

			// The other half of the claim: condition 3 alone would have missed it. Every stored hash
			// still equals the recomputed one, which is exactly why condition 4 is not redundant.
			for i, r := range f.records {
				if r.PointHash != f.expected[i].PointHash {
					t.Fatalf("point %d's hash moved; this fixture no longer proves condition 3 misses "+
						"the corruption", i)
				}
			}
		})
	}
}

// TestCheckIntegrity_UpdatedDrift_StaysIntegral is the predicate-layer proof of the 2026-08-08
// decision, and its boundary.
//
// First half: `updated` drifted from disk, everything else identical → integral, nothing happens.
// Second half: `updated` *and* `status` both drifted → not integral. The exclusion is
// `updated`-shaped; it is not a general loosening of condition 4.
func TestCheckIntegrity_UpdatedDrift_StaysIntegral(t *testing.T) {
	t.Run("updated alone", func(t *testing.T) {
		f := newIntegrityFixture(t, 3)
		for i := range f.records {
			f.records[i].Fields[fieldUpdated] = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
		}

		if got := CheckIntegrity(f.records, f.expected); !got.Integral {
			t.Fatalf("a note whose stored `updated` drifted was marked non-integral (%q); mtime noise "+
				"is back in condition 4 and every git checkout re-embeds the vault", got.Reason)
		}
	})

	t.Run("updated and status", func(t *testing.T) {
		f := newIntegrityFixture(t, 3)
		f.records[0].Fields[fieldUpdated] = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
		f.records[0].Fields[fieldStatus] = schema.StatusArchived().String()

		if got := CheckIntegrity(f.records, f.expected); got.Integral {
			t.Fatal("`status` drifted alongside `updated` and the uid stayed integral; the exclusion " +
				"has widened past the one field it was granted for")
		}
	})
}

// TestCheckIntegrity_ManualUpdatedEdit_NotDetected documents the accepted limit, on purpose.
//
// A hand-edited `updated` in Qdrant goes unnoticed. That is the exact cost the owner took on when
// `updated` left the hash and condition 4 (PRD-contrato §2.4, ADR-004 §5.1): it is display and
// ordering metadata, so drift there is tolerable, while drift in visibility/status/tenant_id is not
// — which is what the test above covers.
//
// If this test ever starts failing, condition 4 has re-absorbed `updated` and the mtime-noise
// re-embedding is back. It is not a gap to close.
func TestCheckIntegrity_ManualUpdatedEdit_NotDetected(t *testing.T) {
	f := newIntegrityFixture(t, 3)
	f.records[2].Fields[fieldUpdated] = "1999-12-31T23:59:59Z"

	if got := CheckIntegrity(f.records, f.expected); !got.Integral {
		t.Fatalf("Integral = false (%q); this fixture is the accepted limit of the 2026-08-08 "+
			"decision and must stay undetected", got.Reason)
	}
}

// TestCheckIntegrity_OrderIndependent pins that the predicate does not depend on the order the
// store returns points in. Qdrant's scroll makes no ordering promise, and a predicate that quietly
// assumed index order would pass every test here and fail against the real server.
func TestCheckIntegrity_OrderIndependent(t *testing.T) {
	f := newIntegrityFixture(t, 4)
	f.records[0], f.records[3] = f.records[3], f.records[0]

	if got := CheckIntegrity(f.records, f.expected); !got.Integral {
		t.Fatalf("reordering the read-back points made the uid non-integral: %q", got.Reason)
	}
}

// TestCheckIntegrity_MissingFieldIsNotIntegral covers the payload written by an older contract: a
// point with no `headings` at all compares unequal rather than silently matching on the fields it
// does have.
func TestCheckIntegrity_MissingFieldIsNotIntegral(t *testing.T) {
	f := newIntegrityFixture(t, 2)
	delete(f.records[1].Fields, fieldHeadings)

	got := CheckIntegrity(f.records, f.expected)
	if got.Integral {
		t.Fatal("a point missing a §2.4 payload field was accepted as integral")
	}
	if !strings.Contains(got.Reason, fieldHeadings) {
		t.Errorf("reason %q does not name the missing field", got.Reason)
	}
}
