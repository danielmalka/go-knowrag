package ingest

import "fmt"

// IntegrityResult is the predicate's answer. Reason is filled only when Integral is false, and it
// names the condition and the point that failed — a run that reprocesses 730 notes and says only
// "not integral" is a run nobody can debug.
type IntegrityResult struct {
	Integral bool
	Reason   string
}

// CheckIntegrity is the four-condition structural integrity predicate (ADR-006 §5): the only source
// of truth about what the previous round managed to write.
//
// It is checked point by point against values recomputed *now*, never sampled and never against a
// representative subset. That is not caution — without multi-point atomicity, an interrupted round
// can leave *some* points already updated, and a sampled check would find one of those, conclude
// "no change", and consolidate the broken state into a permanent one (risk R10).
//
// The four conditions, and the concrete partial write each one catches:
//
//  1. exactly one point per chunk_index in 0..N-1 — catches missing points;
//  2. the total point count in the tenant_id+uid scope is exactly N — catches the orphan tail of a
//     note that shrank, which condition 1 cannot see because every index it looks for is present;
//  3. the stored point_hash of point i equals the one recomputed from the note on disk — catches a
//     point carrying old text, old metadata or an old pipeline;
//  4. the stored payload of point i equals, field by field, the payload recomputed from disk —
//     catches an administrative edit made straight in Qdrant, which does not touch point_hash and
//     therefore walks past condition 3.
//
// Condition 4 is a direct field comparison and never recomputes point_hash from the read-back
// payload. That is not a shortcut avoided, it is a computation that cannot exist: the hash also
// covers pipeline and embedder config, which is not stored per point.
//
// `updated` is compared by neither condition 3 (it is not in the hash) nor condition 4 (it is in
// comparedExclusions). A point whose stored `updated` drifted from disk with byte-identical content
// is integral, and that is the decision of 2026-08-08 paying off: a git checkout costs nothing.
func CheckIntegrity(records []PointRecord, expected []ExpectedPoint) IntegrityResult {
	byIndex := make(map[int]PointRecord, len(records))
	for _, r := range records {
		if _, dup := byIndex[r.ChunkIndex]; dup {
			return notIntegral("condition 1: two points share chunk_index %d", r.ChunkIndex)
		}
		byIndex[r.ChunkIndex] = r
	}

	// Condition 1 is evaluated before condition 2 so that a *missing* point is reported as the
	// missing point it is, rather than as an off-by-one in the total — the count is also wrong when
	// a point is missing, and the less useful of the two messages would win.
	for _, want := range expected {
		if _, ok := byIndex[want.ChunkIndex]; !ok {
			return notIntegral("condition 1: no point with chunk_index %d", want.ChunkIndex)
		}
	}

	// Condition 2 is what catches the orphan tail of a note that shrank: every index 0..N-1 is
	// present and correct, and the excess points are invisible to every other condition.
	if len(records) != len(expected) {
		return notIntegral("condition 2: the scope holds %d point(s), but the note produces %d chunk(s)",
			len(records), len(expected))
	}

	for _, want := range expected {
		got := byIndex[want.ChunkIndex]
		if got.PointHash != want.PointHash {
			return notIntegral(
				"condition 3: chunk_index %d has point_hash %s, but the note on disk now hashes to %s",
				want.ChunkIndex, short(got.PointHash), short(want.PointHash))
		}
		if field, equal := equalFields(got.Fields, want.Fields); !equal {
			return notIntegral(
				"condition 4: chunk_index %d has %s = %s in the index, but the note on disk says %s "+
					"(point_hash matches, so only a write that bypassed the pipeline can have done this)",
				want.ChunkIndex, field, formatValue(got.Fields[field]), formatValue(want.Fields[field]))
		}
	}
	return IntegrityResult{Integral: true}
}

func notIntegral(format string, args ...any) IntegrityResult {
	return IntegrityResult{Reason: fmt.Sprintf(format, args...)}
}

// short trims a hex digest for a message. The first 12 characters are plenty to tell two hashes
// apart by eye, and a full 64-character digest twice per line makes the reason unreadable.
func short(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12] + "…"
}
