package ingest

import (
	"errors"
	"slices"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/vault"
)

// globNotes are two vaults with parallel trees, which is the shape that makes the vault half of the
// match matter: `areas/*.md` written without a vault would take both.
func globNotes(t *testing.T) []vault.Note {
	t.Helper()
	notes := []vault.Note{
		testNote(t, "areas/one.md", 1),
		testNote(t, "areas/deep/two.md", 1),
		testNote(t, "outra/three.md", 1),
		testNote(t, "areas/one.md", 1),
	}
	notes[3].Vault = "trabalho"
	notes[3].UID = uuidFromPath(t, "trabalho/areas/one.md")
	return notes
}

func pathsOf(notes []vault.Note) []string {
	out := make([]string, len(notes))
	for i, n := range notes {
		out[i] = n.Vault + "/" + n.Path
	}
	return out
}

func TestFilterByGlob(t *testing.T) {
	tests := map[string]struct {
		pattern string
		want    []string
	}{
		// The vault is part of the target, so an identical path under the other vault is not taken.
		"one file in one vault": {
			pattern: "pessoal/areas/one.md",
			want:    []string{"pessoal/areas/one.md"},
		},
		// path.Match's * does not cross a separator: this is one level, deliberately.
		"a single level": {
			pattern: "pessoal/areas/*",
			want:    []string{"pessoal/areas/one.md"},
		},
		// The trailing form is the recursive one, and it is handled as a prefix rather than by
		// path.Match — which is why it reaches the nested note that `*` cannot.
		"a subtree": {
			pattern: "pessoal/areas/**",
			want:    []string{"pessoal/areas/one.md", "pessoal/areas/deep/two.md"},
		},
		"a whole vault": {
			pattern: "trabalho/**",
			want:    []string{"trabalho/areas/one.md"},
		},
		"an empty pattern is the whole corpus": {
			pattern: "",
			want: []string{"pessoal/areas/one.md", "pessoal/areas/deep/two.md",
				"pessoal/outra/three.md", "trabalho/areas/one.md"},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := FilterByGlob(globNotes(t), tc.pattern)
			if err != nil {
				t.Fatalf("FilterByGlob(%q): %v", tc.pattern, err)
			}
			if !slices.Equal(pathsOf(got), tc.want) {
				t.Errorf("FilterByGlob(%q) = %v, want %v", tc.pattern, pathsOf(got), tc.want)
			}
		})
	}
}

// TestFilterByGlob_BadPattern_IsAnErrorNotAnEmptySet covers the failure that has no symptom.
//
// A pattern this build cannot honour must not resolve to zero notes: the run would report "0 notes"
// and exit clean, and an operator reads that as a corpus with nothing to do rather than as a glob
// that never matched. The `**` cases are the sharp ones — path.Match has no recursive wildcard, so
// it reads `**` as `*` and matches one level while looking like it matched every one.
func TestFilterByGlob_BadPattern_IsAnErrorNotAnEmptySet(t *testing.T) {
	for _, pattern := range []string{
		"pessoal/**/one.md", // ** in the middle: would silently become a single level
		"**/one.md",         // ** at the front, same trap
		"pessoal/**/**",     // one trailing form is fine, two is not
		"pessoal/[",         // malformed character class
	} {
		t.Run(pattern, func(t *testing.T) {
			got, err := FilterByGlob(globNotes(t), pattern)
			if !errors.Is(err, ErrBadPattern) {
				t.Fatalf("FilterByGlob(%q) = %v, %v; want ErrBadPattern", pattern, pathsOf(got), err)
			}
			if got != nil {
				t.Errorf("FilterByGlob(%q) returned %v alongside its error", pattern, pathsOf(got))
			}
		})
	}
}

// TestFilterByGlob_BadPatternOverNoNotes_StillErrors is the same rule where it is easiest to get
// wrong: validating inside the loop makes a malformed pattern silent whenever the note set is empty,
// which is exactly the run whose empty result nobody questions.
func TestFilterByGlob_BadPatternOverNoNotes_StillErrors(t *testing.T) {
	if _, err := FilterByGlob(nil, "pessoal/["); !errors.Is(err, ErrBadPattern) {
		t.Errorf("FilterByGlob over no notes = %v, want ErrBadPattern", err)
	}
}
