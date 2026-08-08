package vault

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danielmalka/go-knowrag/internal/schema"
)

func baselineNote(t *testing.T) Note {
	t.Helper()
	area, sub, err := deriveArea(schema.VaultMalkaLife(), "research/golang/concurrency.md")
	if err != nil {
		t.Fatalf("deriveArea: %v", err)
	}
	return Note{
		Vault:      schema.VaultMalkaLife(),
		Path:       "research/golang/concurrency.md",
		UID:        uuid.MustParse("0198a7f2-4b31-7c42-9e15-3d8a92c47b6a"),
		Type:       schema.NoteTypeConcept(),
		Status:     schema.StatusDraft(),
		Visibility: schema.VisibilityInternal(),
		Tags:       []string{"golang", "architecture"},
		Created:    time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC),
		Title:      "Go concurrency",
		Lang:       "en",
		Area:       area,
		Sub:        sub,
		Updated:    time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC),
		Body:       "# Go concurrency\n",
	}
}

func serialized(n Note) string { return schema.SerializeFields(NoteMetadataFields(n)) }

// TestNoteMetadataFields_ChangesPerField: one case per field of PRD-contrato §2.4's note-metadata
// list — eleven, not twelve. Each mutates exactly one field and demands the serialization move.
func TestNoteMetadataFields_ChangesPerField(t *testing.T) {
	tests := []struct {
		field  string
		mutate func(*Note)
	}{
		{"vault", func(n *Note) { n.Vault = schema.VaultMalkaWay() }},
		{"path", func(n *Note) { n.Path = "research/golang/other.md" }},
		{"type", func(n *Note) { n.Type = schema.NoteTypeReference() }},
		{"status", func(n *Note) { n.Status = schema.StatusArchived() }},
		{"visibility", func(n *Note) { n.Visibility = schema.VisibilityPrivate() }},
		{"tags", func(n *Note) { n.Tags = []string{"golang", "concurrency"} }},
		{"title", func(n *Note) { n.Title = "Concurrency in Go" }},
		{"lang", func(n *Note) { n.Lang = "pt" }},
		{"created", func(n *Note) { n.Created = n.Created.AddDate(0, 0, 1) }},
		{"area", func(n *Note) {
			area, _, err := deriveArea(schema.VaultMalkaLife(), "mocs/central.md")
			if err != nil {
				t.Fatalf("deriveArea: %v", err)
			}
			n.Area = area
		}},
		{"sub", func(n *Note) { n.Sub = "rust" }},
	}

	baseline := serialized(baselineNote(t))
	for _, tc := range tests {
		t.Run(tc.field, func(t *testing.T) {
			mutated := baselineNote(t)
			tc.mutate(&mutated)
			if got := serialized(mutated); got == baseline {
				t.Errorf("changing %s did not change the serialization (%q)", tc.field, got)
			}
			if _, ok := NoteMetadataFields(mutated)[tc.field]; !ok {
				t.Errorf("field %q is not a key of NoteMetadataFields", tc.field)
			}
		})
	}

	if got := len(NoteMetadataFields(baselineNote(t))); got != len(tests) {
		t.Errorf("NoteMetadataFields returned %d fields, want exactly %d", got, len(tests))
	}
}

// TestNoteMetadataFields_SerializationIsStableAcrossTheEnumTyping pins the exact bytes a valid note
// serializes to. Typing `type`, `status` and `visibility` as schema enums changed how those values
// are produced, not what they are — but point_hash is computed over these bytes, so a one-character
// difference would re-embed and re-upsert the entire corpus on the next run for no content change.
// The literal below is the output of the code as it stood before the typing, copied verbatim.
func TestNoteMetadataFields_SerializationIsStableAcrossTheEnumTyping(t *testing.T) {
	want := "area=research\x00created=2026-08-07T00:00:00Z\x00lang=en\x00" +
		"path=research/golang/concurrency.md\x00status=draft\x00sub=golang\x00" +
		"tags=architecture\x1fgolang\x00title=Go concurrency\x00type=concept\x00" +
		"vault=malkalife\x00visibility=internal"

	if got := serialized(baselineNote(t)); got != want {
		t.Errorf("serialization moved:\n before = %q\n after  = %q", want, got)
	}
}

func TestNoteMetadataFields_NoOpCloneSerializesIdentically(t *testing.T) {
	if a, b := serialized(baselineNote(t)), serialized(baselineNote(t)); a != b {
		t.Errorf("identical notes serialized differently:\n a = %q\n b = %q", a, b)
	}
}

// TestNoteMetadataFields_UpdatedIsExcluded asserts the owner's 2026-08-08 decision directly rather
// than leaving it implicit: `updated` is derived from git/mtime, so it moves without the content
// moving, and inside the hash every checkout and restored backup would re-embed the vault.
func TestNoteMetadataFields_UpdatedIsExcluded(t *testing.T) {
	baseline := baselineNote(t)
	touched := baselineNote(t)
	touched.Updated = baseline.Updated.AddDate(1, 0, 0)

	if a, b := serialized(baseline), serialized(touched); a != b {
		t.Errorf("changing only `updated` changed the serialization:\n before = %q\n after  = %q", a, b)
	}
	if _, present := NoteMetadataFields(touched)["updated"]; present {
		t.Error("`updated` is a key of NoteMetadataFields; PRD-contrato §2.4 keeps it out of the hash")
	}
}

// TestNoteMetadataFields_IgnoresFieldsOutsideTheList is the over-inclusion guard: a field the
// contract does not list must not reach the hash by accident.
func TestNoteMetadataFields_IgnoresFieldsOutsideTheList(t *testing.T) {
	baseline := baselineNote(t)
	for _, tc := range []struct {
		field  string
		mutate func(*Note)
	}{
		{"body", func(n *Note) { n.Body = "completely different body" }},
		{"updated", func(n *Note) { n.Updated = n.Updated.AddDate(0, 1, 0) }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			mutated := baselineNote(t)
			tc.mutate(&mutated)
			if serialized(mutated) != serialized(baseline) {
				t.Errorf("%s is not in PRD-contrato §2.4's note-metadata list but changed the serialization", tc.field)
			}
		})
	}
}

// TestNoteMetadataFields_TagOrderDoesNotMatter: tags are a list field, canonically sorted before
// joining, so two notes tagged the same in different orders are the same note.
func TestNoteMetadataFields_TagOrderDoesNotMatter(t *testing.T) {
	a := baselineNote(t)
	b := baselineNote(t)
	b.Tags = []string{"architecture", "golang"}

	if serialized(a) != serialized(b) {
		t.Error("reordering tags changed the serialization")
	}
}
