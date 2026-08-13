package goldenset

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixture UIDs. Fixed literals rather than uuid.New(), so a failure prints the same value on
// every run and a diff of the test output means something.
//
// The same three literals are also declared in internal/eval/runner_test.go — see the comment there
// for why they are duplicated rather than shared through a third package.
const (
	uidA = "11111111-1111-4111-8111-111111111111"
	uidB = "22222222-2222-4222-8222-222222222222"
	uidC = "33333333-3333-4333-8333-333333333333"
)

// Area names in every fixture below are the neutral ones commit "chore: neutral tenant names in
// test fixtures" established. The real ones are the owner's vault folders and this repository is
// public (CLAUDE.md) — which is also why the coverage table is data in the golden-set file rather
// than a literal in coverage.go (this package).
const validGoldenSetYAML = `
coverage:
  min_total: 2
  max_total: 10
  groups:
    - name: high
      areas: [alfa, beta]
      min: 1
      max: 5
    - name: low
      areas: [gama]
questions:
  - question: "what does the alfa runbook say about restarts"
    uid: "11111111-1111-4111-8111-111111111111"
    area: alfa
    author: owner
    date: "2026-08-11"
  - question: "where is the beta retention policy written down"
    uid: "22222222-2222-4222-8222-222222222222"
    chunk_index: 3
    area: beta
    author: owner
    date: "2026-08-11"
`

func writeGoldenSet(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "golden-set.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}
	return path
}

func loadFixture(t *testing.T, body string) GoldenSet {
	t.Helper()
	set, err := LoadGoldenSet(writeGoldenSet(t, body))
	if err != nil {
		t.Fatalf("LoadGoldenSet: %v", err)
	}
	return set
}

func TestLoadGoldenSet_ValidFile_ReturnsQuestions(t *testing.T) {
	set := loadFixture(t, validGoldenSetYAML)

	if len(set.Questions) != 2 {
		t.Fatalf("%d question(s) parsed, want 2", len(set.Questions))
	}

	first := set.Questions[0]
	if first.Question != "what does the alfa runbook say about restarts" {
		t.Errorf("question = %q", first.Question)
	}
	if first.UID != uidA {
		t.Errorf("uid = %q, want %q", first.UID, uidA)
	}
	if first.Area != "alfa" || first.Author != "owner" || first.Date != "2026-08-11" {
		t.Errorf("area/author/date = %q/%q/%q", first.Area, first.Author, first.Date)
	}
	if first.ChunkIndex != nil {
		t.Errorf("chunk_index = %d, want absent — an entry with no index accepts any chunk", *first.ChunkIndex)
	}

	if set.Questions[1].ChunkIndex == nil || *set.Questions[1].ChunkIndex != 3 {
		t.Errorf("the second entry's chunk_index did not parse: %v", set.Questions[1].ChunkIndex)
	}

	// The coverage table travels with the questions, so a run has the numbers it is judged by
	// without a second file.
	if set.Coverage.MinTotal != 2 || set.Coverage.MaxTotal != 10 || len(set.Coverage.Groups) != 2 {
		t.Errorf("the coverage table did not parse: %+v", set.Coverage)
	}
	if set.Coverage.Groups[1].Min != 0 {
		t.Errorf("group %q parsed a minimum of %d; a group that states none has none",
			set.Coverage.Groups[1].Name, set.Coverage.Groups[1].Min)
	}
}

// TestLoadGoldenSet_MissingRequiredField_NamesTheEntryAndTheField is the whole of T1's "missing
// required field" done-when, one subtest per field. Each case blanks exactly one field of an
// otherwise valid entry, so a failure names the field that stopped being checked.
func TestLoadGoldenSet_MissingRequiredField_NamesTheEntryAndTheField(t *testing.T) {
	entry := map[string]string{
		"question": `"the beta retention policy"`,
		"uid":      `"` + uidB + `"`,
		"area":     "beta",
		"author":   "owner",
		"date":     `"2026-08-11"`,
	}

	for _, field := range []string{"question", "uid", "area", "author", "date"} {
		t.Run(field, func(t *testing.T) {
			var b strings.Builder
			b.WriteString("questions:\n")
			for _, k := range []string{"question", "uid", "area", "author", "date"} {
				value := entry[k]
				if k == field {
					value = `""`
				}
				prefix := "    "
				if k == "question" {
					prefix = "  - "
				}
				b.WriteString(prefix + k + ": " + value + "\n")
			}

			_, err := LoadGoldenSet(writeGoldenSet(t, b.String()))
			if err == nil {
				t.Fatalf("an entry with an empty %q loaded without error", field)
			}
			for _, want := range []string{"entry 1", field} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error %q does not name %q", err, want)
				}
			}
		})
	}
}

func TestLoadGoldenSet_RejectsWhatCannotBeMeasured(t *testing.T) {
	cases := map[string]struct {
		body string
		want []string
	}{
		"uid is not a UUID": {
			body: strings.Replace(validGoldenSetYAML, `"`+uidA+`"`, `"not-a-uuid"`, 1),
			want: []string{"entry 1", "uid", "not-a-uuid", "not a UUID"},
		},
		"area is not a slug": {
			body: strings.Replace(validGoldenSetYAML, "area: alfa", "area: Alfa Notes", 1),
			want: []string{"entry 1", "area", "Alfa Notes"},
		},
		"negative chunk index": {
			body: strings.Replace(validGoldenSetYAML, "chunk_index: 3", "chunk_index: -1", 1),
			want: []string{"entry 2", "chunk_index", "-1"},
		},
		"unknown field": {
			body: strings.Replace(validGoldenSetYAML, "    area: alfa", "    are: alfa\n    area: alfa", 1),
			want: []string{"are"},
		},
		"no questions at all": {
			body: "coverage:\n  min_total: 2\n  max_total: 10\n",
			want: []string{"no questions", "must not be run"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadGoldenSet(writeGoldenSet(t, tc.body)); err != nil {
				for _, want := range tc.want {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("the error %q does not mention %q", err, want)
					}
				}
				return
			}
			t.Fatalf("%s loaded without error", name)
		})
	}
}

// TestLoadGoldenSet_ReportsEveryBadEntryAtOnce is why validateQuestions joins its errors instead of
// returning the first: an author fixing a sixty-question file one error per run stops running it.
func TestLoadGoldenSet_ReportsEveryBadEntryAtOnce(t *testing.T) {
	body := strings.NewReplacer(
		`"`+uidA+`"`, `"not-a-uuid"`,
		"area: beta", `area: ""`,
	).Replace(validGoldenSetYAML)

	_, err := LoadGoldenSet(writeGoldenSet(t, body))
	if err == nil {
		t.Fatal("two broken entries loaded without error")
	}
	for _, want := range []string{"entry 1", "entry 2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error names only some of the bad entries, %q is missing: %v", want, err)
		}
	}
}

// TestLoadGoldenSet_MissingFileIsItsOwnAnswer keeps "you have not authored one" apart from "the one
// you have is broken". A caller that cannot tell them apart tells an operator to fix a file that
// does not exist.
func TestLoadGoldenSet_MissingFileIsItsOwnAnswer(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.yaml")

	_, err := LoadGoldenSet(missing)
	if !errors.Is(err, ErrGoldenSetMissing) {
		t.Fatalf("LoadGoldenSet on a missing file returned %v, want ErrGoldenSetMissing", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Error("the error does not unwrap to fs.ErrNotExist, so a caller matching on that misses it")
	}
	if !strings.Contains(err.Error(), "nothing was measured") {
		t.Errorf("the refusal %q does not say that nothing was measured", err)
	}

	// And the corrupt case must not answer with the same sentinel, or the distinction is decorative.
	if _, cerr := LoadGoldenSet(writeGoldenSet(t, "questions: [oops")); errors.Is(cerr, ErrGoldenSetMissing) {
		t.Errorf("a malformed golden set reports as a missing one: %v", cerr)
	}
}
