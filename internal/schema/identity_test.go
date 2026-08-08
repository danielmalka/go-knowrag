package schema

import (
	"testing"

	"github.com/google/uuid"
)

const (
	knownTenant = "interno"
	knownUID    = "0198a7f2-4b31-7c42-9e15-3d8a92c47b6a"
)

// TestPointID_KnownVector_MatchesLiteralUUID pins the namespace and the ID formula to a vector
// recorded once and cross-checked in an independent implementation (Python's uuid.uuid5).
// Changing NamespaceKnowrag or the name layout invalidates every point ID already written, so this
// test must break loudly.
func TestPointID_KnownVector_MatchesLiteralUUID(t *testing.T) {
	want := uuid.MustParse("c28d712e-ecf8-56c4-9c65-e0170a932878")

	got := PointID(knownTenant, uuid.MustParse(knownUID), 3)
	if got != want {
		t.Fatalf("PointID(%q, %q, 3) = %s, want %s", knownTenant, knownUID, got, want)
	}
}

func TestPointID_DifferentChunkIndex_ProducesDifferentID(t *testing.T) {
	uid := uuid.MustParse(knownUID)
	if first, second := PointID(knownTenant, uid, 3), PointID(knownTenant, uid, 4); first == second {
		t.Fatalf("chunkIndex does not participate in the hash: both indexes produced %s", first)
	}
}

func TestPointID_DifferentTenant_ProducesDifferentID(t *testing.T) {
	uid := uuid.MustParse(knownUID)
	if first, second := PointID("interno", uid, 3), PointID("externo", uid, 3); first == second {
		t.Fatalf("tenantID does not participate in the hash: both tenants produced %s", first)
	}
}

// TestPointID_TenantWithColon_DoesNotCollide is the regression test for the ambiguous-concatenation
// bug. When uid was a string, PointID("interno", "urn:uuid:<u>", 3) and
// PointID("interno:urn:uuid", "<u>", 3) hashed the identical name and returned the identical point
// ID — two different entries, one point, silent overwrite. The uuid.UUID parameter makes the first
// call unrepresentable, so what is left to prove is that a tenantID containing ':' still cannot
// steal characters from the uid field.
func TestPointID_TenantWithColon_DoesNotCollide(t *testing.T) {
	uid := uuid.MustParse(knownUID)
	prefixed := PointID("interno:urn:uuid", uid, 3)

	for name, other := range map[string]uuid.UUID{
		"plain tenant":       PointID("interno", uid, 3),
		"tenant with suffix": PointID("interno:urn", uid, 3),
		"tenant absorbed":    PointID("interno:urn:uuid:"+knownUID, uid, 3),
	} {
		if prefixed == other {
			t.Fatalf("%s collides with tenant %q: both produced %s", name, "interno:urn:uuid", prefixed)
		}
	}
}

// TestPointID_UIDSpellings_ProduceOneID proves the canonicalization the type change bought: every
// spelling uuid.Parse accepts collapses to one point ID, because PointID hashes uid.String().
// Before, these five inputs produced five different IDs for the same logical note.
func TestPointID_UIDSpellings_ProduceOneID(t *testing.T) {
	spellings := []string{
		"0198a7f2-4b31-7c42-9e15-3d8a92c47b6a",
		"0198A7F2-4B31-7C42-9E15-3D8A92C47B6A",
		"0198a7f24b317c429e153d8a92c47b6a",
		"urn:uuid:0198a7f2-4b31-7c42-9e15-3d8a92c47b6a",
		"{0198a7f2-4b31-7c42-9e15-3d8a92c47b6a}",
	}

	want := PointID(knownTenant, uuid.MustParse(knownUID), 3)
	for _, spelling := range spellings {
		uid, err := uuid.Parse(spelling)
		if err != nil {
			t.Fatalf("uuid.Parse(%q) returned error: %v", spelling, err)
		}
		if got := PointID(knownTenant, uid, 3); got != want {
			t.Fatalf("PointID with uid spelled %q = %s, want %s", spelling, got, want)
		}
	}
}

// TestNamespaceKnowrag_IsTheRecordedLiteral guards the constant itself, independent of the formula:
// the known vector above would also break, but this failure names the actual cause.
func TestNamespaceKnowrag_IsTheRecordedLiteral(t *testing.T) {
	if NamespaceKnowrag != "34bf96b9-3a72-4c17-8711-9addd5d8e946" {
		t.Fatalf("NamespaceKnowrag = %q — it must never change; every point ID already written is derived from it", NamespaceKnowrag)
	}
}
