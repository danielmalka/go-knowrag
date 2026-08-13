package eval

import (
	"os"
	"strings"
	"testing"
)

func readSet(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path) // #nosec G304 -- a path this test just created
	if err != nil {
		t.Fatalf("reading %s back: %v", path, err)
	}
	return string(data)
}

// coverageOnly is a set that declares its table and no questions — what an author starts from.
const coverageOnly = `coverage:
  min_total: 4
  max_total: 8
  groups:
    - name: core
      areas: [alfa, beta]
      min: 2
      max: 4
questions:
`

func aQuestion(text, uid, area string) GoldenQuestion {
	return GoldenQuestion{Question: text, UID: uid, Area: area, Author: "someone", Date: "2026-08-13"}
}

// TestAppendQuestion_AddsToAnEmptySet covers the first entry, which is the case with no existing
// item to copy indentation from.
func TestAppendQuestion_AddsToAnEmptySet(t *testing.T) {
	path := writeGoldenSet(t, coverageOnly)

	if err := AppendQuestion(path, aQuestion("what did I decide about X?", uidA, "alfa")); err != nil {
		t.Fatalf("AppendQuestion: %v", err)
	}

	set, err := LoadGoldenSet(path)
	if err != nil {
		t.Fatalf("the file did not load back: %v\n%s", err, readSet(t, path))
	}
	if len(set.Questions) != 1 || set.Questions[0].UID != uidA {
		t.Fatalf("the set holds %+v, want the one entry that was appended", set.Questions)
	}
	// The coverage table has to survive: it is the part an author typed by hand, and re-marshalling
	// the document is exactly what this function refuses to do.
	if set.Coverage.MinTotal != 4 || len(set.Coverage.Groups) != 1 {
		t.Errorf("the coverage table came back as %+v, want the one the file declared", set.Coverage)
	}
}

// TestAppendQuestion_PreservesEveryPriorByte is the property that makes this an append rather than a
// rewrite: comments, hand edits and the author's own formatting are still there afterwards, byte for
// byte, and the new entry is strictly after them.
func TestAppendQuestion_PreservesEveryPriorByte(t *testing.T) {
	before := coverageOnly + `  # the owner's note to self, written by hand
  - question: an entry somebody typed
    uid: ` + uidA + `
    area: alfa
    author: someone
    date: 2026-08-12
`
	path := writeGoldenSet(t, before)

	if err := AppendQuestion(path, aQuestion("a second one", uidB, "beta")); err != nil {
		t.Fatalf("AppendQuestion: %v", err)
	}

	after := readSet(t, path)
	if !strings.HasPrefix(after, before) {
		t.Fatalf("the file no longer starts with what it held before the append — something earlier "+
			"was rewritten:\n%s", after)
	}
	set, err := LoadGoldenSet(path)
	if err != nil {
		t.Fatalf("the file did not load back: %v\n%s", err, after)
	}
	if len(set.Questions) != 2 || set.Questions[0].UID != uidA || set.Questions[1].UID != uidB {
		t.Errorf("the set holds %+v, want the prior entry followed by the appended one", set.Questions)
	}
}

// TestAppendQuestion_MatchesTheIndentationInUse covers the file yaml.Marshal itself would have
// produced: gopkg.in/yaml.v3 indents a nested sequence by four spaces, and an entry appended at two
// would be a different sequence and a parse error.
func TestAppendQuestion_MatchesTheIndentationInUse(t *testing.T) {
	path := writeGoldenSet(t, `coverage:
    min_total: 4
    max_total: 8
    groups:
        - name: core
          areas: [alfa]
          min: 2
          max: 4
questions:
    - question: an entry indented by four
      uid: `+uidA+`
      area: alfa
      author: someone
      date: 2026-08-12
`)

	if err := AppendQuestion(path, aQuestion("a second one", uidB, "alfa")); err != nil {
		t.Fatalf("AppendQuestion: %v\n%s", err, readSet(t, path))
	}
	set, err := LoadGoldenSet(path)
	if err != nil {
		t.Fatalf("the file did not load back: %v\n%s", err, readSet(t, path))
	}
	if len(set.Questions) != 2 {
		t.Errorf("the set holds %d entr(ies), want 2:\n%s", len(set.Questions), readSet(t, path))
	}
}

// TestAppendQuestion_RefusesWhenQuestionsIsNotLast is the failure the read-back exists for. Appending
// to a file whose `questions:` key is followed by another top-level key would attach the entry to
// whatever is at the end, or not parse at all — and the file must come out of that untouched.
func TestAppendQuestion_RefusesWhenQuestionsIsNotLast(t *testing.T) {
	before := `questions:
  - question: an entry somebody typed
    uid: ` + uidA + `
    area: alfa
    author: someone
    date: 2026-08-12
coverage:
  min_total: 4
  max_total: 8
  groups:
    - name: core
      areas: [alfa]
      min: 2
      max: 4
`
	path := writeGoldenSet(t, before)

	err := AppendQuestion(path, aQuestion("a second one", uidB, "alfa"))
	if err == nil {
		t.Fatalf("the append was accepted over a file whose `questions:` key is not last:\n%s",
			readSet(t, path))
	}
	if got := readSet(t, path); got != before {
		t.Errorf("the refused append still changed the file:\n%s", got)
	}
}

// TestAppendQuestion_KeepsWorkingAfterAHandWrittenComment. The tool is built for sessions with hand
// edits between them, and a note-to-self at column zero used to read as the next top-level key: every
// later append was refused, telling the owner to move `questions:` to the end of a file where it
// already was.
func TestAppendQuestion_KeepsWorkingAfterAHandWrittenComment(t *testing.T) {
	before := coverageOnly + `  - question: an entry somebody typed
    uid: ` + uidA + `
    area: alfa
    author: someone
    date: "2026-08-12"
# lembrar de dividir essa em duas
`
	path := writeGoldenSet(t, before)

	if err := AppendQuestion(path, aQuestion("a second one", uidB, "beta")); err != nil {
		t.Fatalf("AppendQuestion after a hand-written comment: %v", err)
	}
	if got := readSet(t, path); !strings.HasPrefix(got, before) {
		t.Errorf("the comment did not survive the append:\n%s", got)
	}
	if got := len(loadFixture(t, readSet(t, path)).Questions); got != 2 {
		t.Errorf("the set holds %d entr(ies), want 2:\n%s", got, readSet(t, path))
	}
}

// TestAppendQuestion_KeepsWorkingOnACRLFFile is the same bug through the other door, and it is the
// owner's actual environment: the vaults sit on a Windows mount and the global CLAUDE.md lists CRLF
// as a known trap. A blank line in a CRLF file arrives as "\r", which is neither empty nor indented,
// so it read as a top-level key and produced the same wrong refusal.
//
// The blank line is placed both between entries and at the end, because only one of those positions
// was reported and a fix for one is not a fix for the other.
func TestAppendQuestion_KeepsWorkingOnACRLFFile(t *testing.T) {
	body := coverageOnly + `  - question: an entry somebody typed
    uid: ` + uidA + `
    area: alfa
    author: someone
    date: "2026-08-12"

  - question: another entry
    uid: ` + uidC + `
    area: alfa
    author: someone
    date: "2026-08-12"

`
	before := strings.ReplaceAll(body, "\n", "\r\n")
	path := writeGoldenSet(t, before)

	if err := AppendQuestion(path, aQuestion("a third one", uidB, "beta")); err != nil {
		t.Fatalf("AppendQuestion over a CRLF file with blank lines: %v", err)
	}
	if got := readSet(t, path); !strings.HasPrefix(got, before) {
		t.Errorf("the CRLF bytes did not survive the append:\n%q", got)
	}
	if got := len(loadFixture(t, readSet(t, path)).Questions); got != 3 {
		t.Errorf("the set holds %d entr(ies), want 3:\n%q", got, readSet(t, path))
	}
}

// TestAppendQuestion_RefusesATableThatGovernsNothing moves the guard to the function that writes.
//
// A zero-byte file decodes into a valid empty GoldenSet, so nothing about reading it says no. What
// said no until now was an accident of one caller — the session's draw walks CoverageStatus, which
// returns nothing for a table with no groups, so it ended before writing — and an accident at the
// call site protects no other caller.
func TestAppendQuestion_RefusesATableThatGovernsNothing(t *testing.T) {
	for name, before := range map[string]string{
		"a zero-byte file":       "",
		"a table with no groups": "coverage:\n  min_total: 4\n  max_total: 8\nquestions:\n",
		"a table bounding no total": "coverage:\n  groups:\n    - name: core\n      areas: [alfa]\n" +
			"      min: 2\n      max: 4\nquestions:\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := writeGoldenSet(t, before)

			if err := AppendQuestion(path, aQuestion("a question", uidA, "alfa")); err == nil {
				t.Fatalf("the append was accepted against %s:\n%s", name, readSet(t, path))
			}
			if got := readSet(t, path); got != before {
				t.Errorf("the refused append still changed the file:\n%s", got)
			}
		})
	}
}

// TestAppendQuestion_RefusesAnEmptyEntryList is the failure the read-back catches and the layout
// check cannot: `questions: []` is the last top-level key of this file, so the layout is right, and
// an item still cannot be appended under a flow-style empty list.
//
// It is the case that keeps the read-back from being a second copy of the check above it. Without it
// a plant that removed the read-back would turn nothing red, which is how a guard stops guarding.
func TestAppendQuestion_RefusesAnEmptyEntryList(t *testing.T) {
	before := `coverage:
  min_total: 4
  max_total: 8
  groups:
    - name: core
      areas: [alfa]
      min: 2
      max: 4
questions: []
`
	path := writeGoldenSet(t, before)

	if err := AppendQuestion(path, aQuestion("a question", uidA, "alfa")); err == nil {
		t.Fatalf("the append was accepted over `questions: []`:\n%s", readSet(t, path))
	}
	if got := readSet(t, path); got != before {
		t.Errorf("the refused append still changed the file:\n%s", got)
	}
}

// TestAppendQuestion_RefusesAnInvalidEntry keeps an unloadable file from being built one entry at a
// time — the author would only find out on the next session, after typing another twenty.
func TestAppendQuestion_RefusesAnInvalidEntry(t *testing.T) {
	path := writeGoldenSet(t, coverageOnly)

	for name, q := range map[string]GoldenQuestion{
		"no uid":        aQuestion("a question", "", "alfa"),
		"uid not a uid": aQuestion("a question", "not-a-uuid", "alfa"),
		"no area":       aQuestion("a question", uidA, ""),
		"no question":   aQuestion("", uidA, "alfa"),
	} {
		t.Run(name, func(t *testing.T) {
			if err := AppendQuestion(path, q); err == nil {
				t.Fatalf("appending %+v was accepted", q)
			}
			if got := readSet(t, path); got != coverageOnly {
				t.Errorf("the refused append still changed the file:\n%s", got)
			}
		})
	}
}

// TestReadGoldenSet_AcceptsASetWithNoQuestions is the one difference from LoadGoldenSet, and the
// assertion covers both halves: this one accepts the file an author starts from, and LoadGoldenSet
// still refuses to *measure* it.
func TestReadGoldenSet_AcceptsASetWithNoQuestions(t *testing.T) {
	path := writeGoldenSet(t, coverageOnly)

	set, err := ReadGoldenSet(path)
	if err != nil {
		t.Fatalf("ReadGoldenSet over a set with no questions: %v", err)
	}
	if len(set.Questions) != 0 || set.Coverage.MinTotal != 4 {
		t.Errorf("ReadGoldenSet returned %+v, want the table and no questions", set)
	}
	if _, err := LoadGoldenSet(path); err == nil {
		t.Error("LoadGoldenSet accepted a set with no questions, so a gate over it would report a " +
			"pass nobody measured")
	}
}

// TestCoverageStatus_PutsTheShortestAreaFirst is the ordering both the progress report and the draw
// read. The shape is the brief's own example: an area sitting on 18 must not be offered ahead of one
// sitting on 3.
func TestCoverageStatus_PutsTheShortestAreaFirst(t *testing.T) {
	table := CoverageTable{
		MinTotal: 20, MaxTotal: 60,
		Groups: []CoverageGroup{
			{Name: "core", Areas: []string{"alfa", "beta", "gama"}, Min: 8, Max: 20},
		},
	}
	questions := questionsFor(map[string]int{"alfa": 18, "beta": 3, "gama": 20})

	status := CoverageStatus(questions, table)
	if len(status) != 3 {
		t.Fatalf("CoverageStatus returned %d area(s), want 3", len(status))
	}
	if status[0].Area != "beta" || status[0].Needs() != 5 {
		t.Errorf("the first area is %q with %d question(s) and needs %d; beta has 3 against a "+
			"minimum of 8 and has to come first", status[0].Area, status[0].Have, status[0].Needs())
	}
	// gama is at its maximum, so it sorts last and no draw may come from it — the ordering is what
	// carries that, because the draw walks this list and stops at the first area it can use.
	last := status[len(status)-1]
	if last.Area != "gama" || !last.Full() {
		t.Errorf("the last area is %q (full=%v), want the one at its maximum", last.Area, last.Full())
	}
}
