package schema

import "testing"

func TestSerializeFields_KnownVectors(t *testing.T) {
	tests := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{"empty", map[string]string{}, ""},
		{"single", map[string]string{"vault": "malkalife"}, "vault=malkalife"},
		{
			"keys sorted lexicographically, not by insertion",
			map[string]string{"type": "concept", "area": "research", "sub": "golang"},
			"area=research\x00sub=golang\x00type=concept",
		},
		{
			"empty value keeps its key",
			map[string]string{"sub": "", "area": "mocs"},
			"area=mocs\x00sub=",
		},
		{
			"booleans and RFC 3339 UTC dates are caller-formatted strings",
			map[string]string{"oversize": "false", "created": "2026-08-07T00:00:00Z"},
			"created=2026-08-07T00:00:00Z\x00oversize=false",
		},
		{
			"a pre-serialized list value nests \\x1f inside the \\x00-joined pairs",
			map[string]string{"tags": "architecture\x1fgolang", "lang": "pt"},
			"lang=pt\x00tags=architecture\x1fgolang",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SerializeFields(tc.fields); got != tc.want {
				t.Errorf("SerializeFields() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSerializeFields_InsertionOrderIrrelevant is the property the vectors above cannot prove on
// their own: Go randomizes map iteration order, so a naive implementation that ranged over the map
// would pass a fixed vector only by luck. Building the same content through two different insertion
// sequences and demanding identical bytes is what pins determinism to the sort.
func TestSerializeFields_InsertionOrderIrrelevant(t *testing.T) {
	a := map[string]string{}
	for _, k := range []string{"vault", "path", "type", "status", "area"} {
		a[k] = "v-" + k
	}
	b := map[string]string{}
	for _, k := range []string{"area", "status", "type", "path", "vault"} {
		b[k] = "v-" + k
	}

	if SerializeFields(a) != SerializeFields(b) {
		t.Errorf("insertion order changed the serialization:\n a = %q\n b = %q",
			SerializeFields(a), SerializeFields(b))
	}
}

func TestSerializeList(t *testing.T) {
	tests := []struct {
		name  string
		items []string
		want  string
	}{
		{"empty", nil, ""},
		{"single", []string{"golang"}, "golang"},
		{"sorted on output", []string{"golang", "architecture"}, "architecture\x1fgolang"},
		{"already sorted", []string{"architecture", "golang"}, "architecture\x1fgolang"},
		{"duplicates are preserved, not deduplicated", []string{"go", "go"}, "go\x1fgo"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SerializeList(tc.items); got != tc.want {
				t.Errorf("SerializeList(%v) = %q, want %q", tc.items, got, tc.want)
			}
		})
	}
}

// TestSerializeList_DoesNotMutateCaller guards the copy in SerializeList: hashing a Note must not
// reorder the Tags slice the caller still holds and is about to write to the payload.
func TestSerializeList_DoesNotMutateCaller(t *testing.T) {
	items := []string{"golang", "architecture"}
	_ = SerializeList(items)

	if items[0] != "golang" || items[1] != "architecture" {
		t.Errorf("SerializeList mutated its argument: got %v, want [golang architecture]", items)
	}
}
