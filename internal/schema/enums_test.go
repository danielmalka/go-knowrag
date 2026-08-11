package schema

import "testing"

func TestNoteTypeValues(t *testing.T) {
	cases := []struct {
		name string
		want NoteType
	}{
		{"concept", NoteTypeConcept()},
		{"moc", NoteTypeMOC()},
		{"project", NoteTypeProject()},
		{"post", NoteTypePost()},
		{"lesson", NoteTypeLesson()},
		{"reference", NoteTypeReference()},
		{"template", NoteTypeTemplate()},
		{"log", NoteTypeLog()},
		{"lore", NoteTypeLore()},
		{"character", NoteTypeCharacter()},
		{"script", NoteTypeScript()},
		{"prompt", NoteTypePrompt()},
		{"index", NoteTypeIndex()},
		{"agent", NoteTypeAgent()},
		{"skill", NoteTypeSkill()},
	}
	// Coverage, not a drift count: the bound is derived from the registry via AllNoteTypes(), so
	// registering a value without adding a case here fails, and there is no hand-maintained number
	// anyone has to remember to bump.
	if got := len(AllNoteTypes()); got != len(cases) {
		t.Fatalf("AllNoteTypes() has %d values, cases cover %d", got, len(cases))
	}
	for _, c := range cases {
		got, ok := ParseNoteType(c.name)
		if !ok {
			t.Errorf("ParseNoteType(%q) failed", c.name)
			continue
		}
		if got != c.want {
			t.Errorf("ParseNoteType(%q) = %v, want %v", c.name, got, c.want)
		}
		if !got.Valid() {
			t.Errorf("NoteType %q not Valid()", c.name)
		}
		if got.String() != c.name {
			t.Errorf("NoteType %q String() = %q", c.name, got.String())
		}
	}
	if _, ok := ParseNoteType("bogus"); ok {
		t.Error("ParseNoteType(\"bogus\") should fail")
	}
	var zero NoteType
	if zero.Valid() {
		t.Error("zero-value NoteType should not be Valid()")
	}
}

func TestStatusValues(t *testing.T) {
	cases := []struct {
		name string
		want Status
	}{
		{"draft", StatusDraft()},
		{"in-progress", StatusInProgress()},
		{"stable", StatusStable()},
		{"archived", StatusArchived()},
	}
	if got := len(AllStatuses()); got != len(cases) {
		t.Fatalf("AllStatuses() has %d values, cases cover %d", got, len(cases))
	}
	for _, c := range cases {
		got, ok := ParseStatus(c.name)
		if !ok {
			t.Errorf("ParseStatus(%q) failed", c.name)
			continue
		}
		if got != c.want {
			t.Errorf("ParseStatus(%q) = %v, want %v", c.name, got, c.want)
		}
		if !got.Valid() {
			t.Errorf("Status %q not Valid()", c.name)
		}
		if got.String() != c.name {
			t.Errorf("Status %q String() = %q", c.name, got.String())
		}
	}
	if _, ok := ParseStatus("bogus"); ok {
		t.Error("ParseStatus(\"bogus\") should fail")
	}
	var zero Status
	if zero.Valid() {
		t.Error("zero-value Status should not be Valid()")
	}
}

func TestVisibilityValues(t *testing.T) {
	cases := []struct {
		name string
		want Visibility
	}{
		{"private", VisibilityPrivate()},
		{"internal", VisibilityInternal()},
		{"shareable", VisibilityShareable()},
	}
	if got := len(AllVisibilities()); got != len(cases) {
		t.Fatalf("AllVisibilities() has %d values, cases cover %d", got, len(cases))
	}
	for _, c := range cases {
		got, ok := ParseVisibility(c.name)
		if !ok {
			t.Errorf("ParseVisibility(%q) failed", c.name)
			continue
		}
		if got != c.want {
			t.Errorf("ParseVisibility(%q) = %v, want %v", c.name, got, c.want)
		}
		if !got.Valid() {
			t.Errorf("Visibility %q not Valid()", c.name)
		}
		if got.String() != c.name {
			t.Errorf("Visibility %q String() = %q", c.name, got.String())
		}
	}
	if _, ok := ParseVisibility("bogus"); ok {
		t.Error("ParseVisibility(\"bogus\") should fail")
	}
	var zero Visibility
	if zero.Valid() {
		t.Error("zero-value Visibility should not be Valid()")
	}
}

// enumValue is what checkEnumeration needs from each enum type: comparable (to match a returned
// value against the registered one) and String() (to check ordering by the canonical string).
type enumValue interface {
	comparable
	String() string
}

// checkEnumeration asserts the three properties every All*() accessor promises: it returns exactly
// the registered values, ordered by string value, and the caller gets values identical to the
// registered ones rather than look-alikes.
func checkEnumeration[T enumValue](t *testing.T, name string, got []T, registry map[string]T) {
	t.Helper()
	if len(got) != len(registry) {
		t.Errorf("%s returned %d values, registry holds %d", name, len(got), len(registry))
	}
	prev := ""
	for i, v := range got {
		s := v.String()
		if i > 0 && s <= prev {
			t.Errorf("%s not sorted ascending: %q at index %d follows %q", name, s, i, prev)
		}
		prev = s
		if reg, ok := registry[s]; !ok || reg != v {
			t.Errorf("%s[%d] = %q is not the registered value", name, i, s)
		}
	}
}

func TestEnumerationAccessors(t *testing.T) {
	checkEnumeration(t, "AllNoteTypes", AllNoteTypes(), noteTypeSet)
	checkEnumeration(t, "AllStatuses", AllStatuses(), statusSet)
	checkEnumeration(t, "AllVisibilities", AllVisibilities(), visibilitySet)

	// The returned slices must be copies: a consumer mutating one cannot reach the registry.
	want := AllStatuses()[0]
	mutated := AllStatuses()
	mutated[0] = StatusStable()
	if AllStatuses()[0] != want {
		t.Error("AllStatuses returned a slice aliasing the registry")
	}
}
