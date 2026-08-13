package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/danielmalka/go-knowrag/internal/archtest"
	"github.com/danielmalka/go-knowrag/internal/clicmd"
	"github.com/danielmalka/go-knowrag/internal/config"
	"github.com/danielmalka/go-knowrag/internal/eval"
)

// The fixture uids. Area names are the neutral ones the rest of this repository's fixtures use: the
// real ones are the owner's vault folders and this repository is public (CLAUDE.md).
const (
	uidA1 = "11111111-1111-4111-8111-111111111111"
	uidA2 = "22222222-2222-4222-8222-222222222222"
	uidA3 = "33333333-3333-4333-8333-333333333333"
	uidB1 = "44444444-4444-4444-8444-444444444444"
	uidB2 = "55555555-5555-4555-8555-555555555555"
)

// goldenTable is the coverage table every fixture below declares: two areas, two questions each at
// minimum, four at most, four to eight in total.
const goldenTable = `coverage:
  min_total: 4
  max_total: 8
  groups:
    - name: core
      areas: [alfa, beta]
      min: 2
      max: 4
questions:
`

// entry renders one hand-written entry, the way the owner's file holds them.
func entry(question, uid, area string) string {
	return "  - question: " + question + "\n" +
		"    uid: " + uid + "\n" +
		"    area: " + area + "\n" +
		"    author: owner\n" +
		"    date: \"2026-08-12\"\n"
}

func writeSet(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "golden-set.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}
	return path
}

func read(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path) // #nosec G304 -- a path this test just created
	if err != nil {
		t.Fatalf("reading %s back: %v", path, err)
	}
	return string(data)
}

func loadSet(t *testing.T, path string) eval.GoldenSet {
	t.Helper()

	set, err := eval.ReadGoldenSet(path)
	if err != nil {
		t.Fatalf("the golden set at %s did not load: %v\n%s", path, err, read(t, path))
	}
	return set
}

// deck is the notes a session draws from. Paths are what sorts the candidates, so they are spelled
// in the order the draw will see them.
func deck() []noteCard {
	return []noteCard{
		{UID: uidA1, Area: "alfa", Title: "first alfa note", Path: "v/alfa/1.md", Excerpt: []string{"body of a1"}},
		{UID: uidA2, Area: "alfa", Title: "second alfa note", Path: "v/alfa/2.md", Excerpt: []string{"body of a2"}},
		{UID: uidA3, Area: "alfa", Title: "third alfa note", Path: "v/alfa/3.md", Excerpt: []string{"body of a3"}},
		{UID: uidB1, Area: "beta", Title: "first beta note", Path: "v/beta/1.md", Excerpt: []string{"body of b1"}},
		{UID: uidB2, Area: "beta", Title: "second beta note", Path: "v/beta/2.md", Excerpt: []string{"body of b2"}},
	}
}

// firstCandidate is the draw the tests use: always the first candidate of the chosen area. The
// randomness is not what any of these assert — which *area* the candidate came from is — and a
// random draw would make every one of them flaky for no coverage.
func firstCandidate(int) int { return 0 }

// session runs one authoring session over path with the given typed input, and answers what the
// command printed.
func session(t *testing.T, path string, cards []noteCard, typed string) string {
	t.Helper()

	var out bytes.Buffer
	err := authorGolden(&out, strings.NewReader(typed), path, cards, loadSet(t, path),
		"owner", "2026-08-13", firstCandidate)
	if err != nil {
		t.Fatalf("authorGolden: %v\n%s", err, out.String())
	}
	return out.String()
}

// TestAuthorGolden_DrawsFromTheAreaTheTableIsShortestOn is the inversion the whole tool exists for.
// A uniform draw over this deck would offer alfa three times out of five while beta sits on nothing.
func TestAuthorGolden_DrawsFromTheAreaTheTableIsShortestOn(t *testing.T) {
	// alfa already meets its minimum of 2; beta has none. Both still have unasked notes in the deck,
	// which is what makes this an assertion about the ordering rather than about what is left.
	path := writeSet(t, goldenTable+
		entry("an alfa question", uidA1, "alfa")+
		entry("another alfa question", uidA2, "alfa"))

	session(t, path, deck(), "what did I write about the beta thing?\nq\n")

	set := loadSet(t, path)
	if len(set.Questions) != 3 {
		t.Fatalf("the set holds %d entr(ies), want 3", len(set.Questions))
	}
	added := set.Questions[2]
	if added.Area != "beta" {
		t.Errorf("the session drew from area %q (uid %s). beta has 0 questions against a minimum of "+
			"2 and alfa already has its 2, so the draw had to come from beta", added.Area, added.UID)
	}
}

// TestAuthorGolden_NeverDrawsANoteThatAlreadyHasAQuestion covers both halves of "already has one":
// an entry that was in the file when the session started, and one this session just wrote.
//
// The deck is two notes and the input answers three times. A note drawn twice would produce a second
// entry for the same uid, and a note redrawn forever would hang this test rather than fail it — so
// the count is the assertion and the termination is the test.
func TestAuthorGolden_NeverDrawsANoteThatAlreadyHasAQuestion(t *testing.T) {
	path := writeSet(t, goldenTable+entry("an alfa question", uidA1, "alfa"))
	cards := deck()[:2] // a1 and a2 only

	out := session(t, path, cards, "one\ntwo\nthree\n")

	set := loadSet(t, path)
	if len(set.Questions) != 2 {
		t.Fatalf("the set holds %d entr(ies), want 2 — one note had a question already and the "+
			"other could only be asked about once:\n%s", len(set.Questions), read(t, path))
	}
	if set.Questions[1].UID != uidA2 {
		t.Errorf("the appended entry is for uid %s, want %s — the note that already had a question "+
			"was drawn again", set.Questions[1].UID, uidA2)
	}
	if strings.Contains(out, "first alfa note") {
		t.Errorf("the session showed the note that already has a question:\n%s", out)
	}
}

// TestAuthorGolden_SkippingWritesNothing — not every note yields a good question, and a tool that
// made you write one anyway would collect bad ones.
func TestAuthorGolden_SkippingWritesNothing(t *testing.T) {
	path := writeSet(t, goldenTable)
	before := read(t, path)

	out := session(t, path, deck()[:1], "\n")

	if after := read(t, path); after != before {
		t.Errorf("skipping changed the file:\n%s", after)
	}
	if !strings.Contains(out, "skipped, nothing written") {
		t.Errorf("the session did not say the note was skipped:\n%s", out)
	}
}

// TestAuthorGolden_AppendsAcrossSessionsAndKeepsHandEdits is the resumability property. The file is
// edited by hand between the two runs — a comment added and an entry reworded — because that is what
// a set authored over several evenings actually looks like, and a writer that re-marshalled the
// document would silently drop both.
func TestAuthorGolden_AppendsAcrossSessionsAndKeepsHandEdits(t *testing.T) {
	path := writeSet(t, goldenTable)

	session(t, path, deck(), "the first question\nq\n")

	// The hand edit: a comment, and a reworded question. Both have to survive the next session.
	const comment = "  # I should split this one in two later\n"
	edited := strings.Replace(read(t, path), "the first question", "the first question, reworded", 1)
	if err := os.WriteFile(path, []byte(edited+comment), 0o600); err != nil {
		t.Fatalf("hand-editing the fixture: %v", err)
	}
	handEdited := read(t, path)

	session(t, path, deck(), "the second question\nq\n")

	after := read(t, path)
	if !strings.HasPrefix(after, handEdited) {
		t.Fatalf("the second session rewrote what the first one and the hand edit left:\n%s", after)
	}
	set := loadSet(t, path)
	if len(set.Questions) != 2 {
		t.Fatalf("the set holds %d entr(ies), want 2:\n%s", len(set.Questions), after)
	}
	if set.Questions[0].Question != "the first question, reworded" {
		t.Errorf("the hand-edited entry came back as %q", set.Questions[0].Question)
	}
	if set.Questions[1].Question != "the second question" {
		t.Errorf("the appended entry is %q", set.Questions[1].Question)
	}
	if !strings.Contains(after, strings.TrimSuffix(comment, "\n")) {
		t.Errorf("the hand-written comment is gone:\n%s", after)
	}
}

// TestAuthorGolden_WritesExactlyTheGoldenQuestionFields reads the raw YAML rather than the decoded
// struct, because the decoded struct cannot see the failure: a key the type does not declare is what
// strict decoding would reject on the *next* load, and a key it does declare but that nothing filled
// is invisible once it is a zero value.
//
// chunk_index is absent on purpose. Absent means "any chunk of that uid counts" (goldenset.go), which
// is the right answer for a question authored from a whole note — and asking the author for a chunk
// index would be noise they cannot answer.
func TestAuthorGolden_WritesExactlyTheGoldenQuestionFields(t *testing.T) {
	path := writeSet(t, goldenTable)

	session(t, path, deck()[:1], "what did I decide here?\n")

	var raw struct {
		Questions []map[string]any `yaml:"questions"`
	}
	if err := yaml.Unmarshal([]byte(read(t, path)), &raw); err != nil {
		t.Fatalf("re-reading the file as plain YAML: %v", err)
	}
	if len(raw.Questions) != 1 {
		t.Fatalf("the file holds %d entr(ies), want 1", len(raw.Questions))
	}

	got := raw.Questions[0]
	want := map[string]any{
		"question": "what did I decide here?",
		"uid":      uidA1,
		"area":     "alfa",
		"author":   "owner",
		"date":     "2026-08-13",
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("field %q is %v, want %v", key, got[key], value)
		}
	}
	for key := range got {
		if _, declared := want[key]; !declared {
			t.Errorf("the entry carries a field %q that GoldenQuestion does not fill here — "+
				"chunk_index in particular has to stay absent, because absent is what means "+
				"\"any chunk of that uid counts\"", key)
		}
	}
}

// goldenOutputShapes is every line this command is allowed to print. It is an allow-list and not a
// list of forbidden strings, and that is the whole assertion: a search result has no fixed wording,
// so the only way to prove one is never printed is to prove that nothing outside these shapes ever
// is.
//
// The rule it enforces is the reason the golden set is worth anything. A question written after
// seeing what the index returns is a question adjusted — unconsciously — until it passes, and a set
// of those measures the tool that produced it. Showing the *note* is not the same thing and is
// required: the note is where the expected uid comes from.
var goldenOutputShapes = []*regexp.Regexp{
	regexp.MustCompile(`^$`),
	regexp.MustCompile(`^golden set \S+: \d+ question\(s\), target \d+-\d+$`),
	regexp.MustCompile(`^ {2}area [a-z0-9-]+: \d+ question\(s\), min \d+, max \d+ - (ok|full|\d+ more needed)$`),
	regexp.MustCompile(`^` + regexp.QuoteMeta(progressMix) + `$`),
	regexp.MustCompile(`^(area:  |title: |path:  |body:  )`),
	regexp.MustCompile(`^` + regexp.QuoteMeta(promptRule) + `$`),
	regexp.MustCompile(`^` + regexp.QuoteMeta(promptCriterion) + `$`),
	regexp.MustCompile(`^try .+: .+$`),
	regexp.MustCompile(`^saved: \d+ question\(s\)$`),
	regexp.MustCompile(`^skipped, nothing written$`),
}

// TestAuthorGolden_PrintsNothingButItsOwnShapes is the guard on the rule this command exists to
// keep. See goldenOutputShapes.
func TestAuthorGolden_PrintsNothingButItsOwnShapes(t *testing.T) {
	path := writeSet(t, goldenTable+entry("an alfa question", uidA1, "alfa"))

	// A session with every branch in it: a save, a skip, and a stop.
	out := session(t, path, deck(), "a question about beta\n\nanother question\nq\n")

	lines := strings.Split(out, "\n")
	for i, line := range lines {
		// The prompt is written without a newline, because on a terminal the author types on the
		// same line and their Enter is what ends it. In a buffer it therefore runs into whatever is
		// printed next, so it is stripped as a prefix and the remainder still has to be a shape.
		line = strings.TrimPrefix(line, goldenPrompt)
		if !slicesAny(goldenOutputShapes, line) {
			t.Errorf("line %d of the session is not one of this command's shapes: %q\n\nfull "+
				"session:\n%s", i+1, line, out)
		}
	}

	// Non-vacuity. An allow-list passes trivially over an empty session, and an empty session is
	// exactly what a broken fixture produces.
	for _, want := range []string{"saved: ", "skipped, nothing written", "title: ", "body:  ", "try "} {
		if !strings.Contains(out, want) {
			t.Fatalf("the session never printed %q, so this allow-list checked nothing:\n%s", want, out)
		}
	}
}

func slicesAny(shapes []*regexp.Regexp, line string) bool {
	for _, shape := range shapes {
		if shape.MatchString(line) {
			return true
		}
	}
	return false
}

// TestAuthorGolden_GuidesAtEveryPrompt covers where the guidance is, which is the whole reason it
// works: instructions that live in a file are instructions nobody is reading at question forty, and
// a rule stated once at the top of a fifty-question session is a rule that applied to question one.
func TestAuthorGolden_GuidesAtEveryPrompt(t *testing.T) {
	path := writeSet(t, goldenTable)

	out := session(t, path, deck(), "one\ntwo\nthree\nq\n")

	prompts := strings.Count(out, goldenPrompt)
	if prompts < 3 {
		t.Fatalf("the session showed %d prompt(s); this test needs at least three to mean "+
			"anything:\n%s", prompts, out)
	}
	for name, line := range map[string]string{"rule": promptRule, "criterion": promptCriterion} {
		if got := strings.Count(out, line); got != prompts {
			t.Errorf("%d prompt(s) but %d %s line(s) — it has to be at every one of them:\n%s",
				prompts, got, name, out)
		}
	}
	if got := strings.Count(out, "\ntry "); got != prompts {
		t.Errorf("%d prompt(s) but %d suggested kind(s):\n%s", prompts, got, out)
	}
}

// replacedPromptWordings are the drafts this prompt must never go back to, and the list is the point
// of this test rather than a footnote to it.
//
// A test that only checks the new sentence is present passes with the old one still in the file — and
// worse, a test that asserts prose without refusing the wording it replaced becomes the lock that
// holds the stale version in place the day the behaviour moves on. This repository has paid for that
// exact shape before (CLAUDE.md, "Teste que certifica o texto errado").
//
// Each of these was a real draft of this prompt, and each is wrong in a way that reads as fine:
// they ask for distance from the note's wording without giving the author any way to produce it. The
// question that came back from the first one was "what kind of question should I even ask?", which is
// precisely what a rule with no procedure attached earns.
var replacedPromptWordings = []string{
	"Write it in your own words. A question that reuses the note's own phrasing measures string " +
		"matching, not retrieval.",
	"Write the question in your own words.",
	"Be original.",
}

// TestPromptText_SaysWhatItMustAndNotWhatItReplaced — see replacedPromptWordings.
func TestPromptText_SaysWhatItMustAndNotWhatItReplaced(t *testing.T) {
	whole := strings.Join([]string{promptRule, promptCriterion, progressMix}, "\n")
	for _, nudge := range promptNudges {
		whole += "\n" + nudge.kind + ": " + nudge.hint
	}

	// The load-bearing halves: a situation the author can reproduce, and the mechanism that says why
	// it matters. Either alone is advice; together they are an instruction.
	for _, want := range []string{
		"11pm",            // the situation, which is what makes the rule followable
		"file name",       // and the thing he has to not be looking at
		"retrieval",       // what a good question measures
		"string matching", // what a bad one measures instead
		"hard",            // the difficulty that keeps the set from saturating
		"associative",     // the kind he will not write on his own
	} {
		if !strings.Contains(whole, want) {
			t.Errorf("the prompt guidance never says %q:\n%s", want, whole)
		}
	}

	for _, gone := range replacedPromptWordings {
		if strings.Contains(whole, gone) {
			t.Errorf("the prompt guidance still carries a wording it replaced: %q", gone)
		}
	}
}

// TestPromptNudges_RealiseTheTargetMix checks the rotation against the proportion the progress report
// states, because the two are one decision written in two places and nothing else compares them.
//
// A set of easy questions saturates: at 95% recall there is no room left to get worse, so no future
// change shows up as a regression and the set certifies that all is well, forever. The mix is what
// prevents that, and the rotation is the only thing that produces it — nothing records difficulty, so
// nothing downstream can notice it drifting.
func TestPromptNudges_RealiseTheTargetMix(t *testing.T) {
	var hard, associative, natural int
	for _, nudge := range promptNudges {
		switch {
		case strings.Contains(nudge.kind, "hard"):
			hard++
		case strings.Contains(nudge.kind, "associative"):
			associative++
		default:
			natural++
		}
	}

	// Half natural, a third hard, the rest associative — the proportion progressMix names.
	total := len(promptNudges)
	if natural*2 != total {
		t.Errorf("%d of %d nudges are natural kinds, want half", natural, total)
	}
	if hard*3 != total {
		t.Errorf("%d of %d nudges are hard, want a third", hard, total)
	}
	if associative == 0 || natural+hard+associative != total {
		t.Errorf("the cycle is %d natural, %d hard, %d associative of %d",
			natural, hard, associative, total)
	}
	for _, want := range []string{"half natural", "a third hard", "the rest associative"} {
		if !strings.Contains(progressMix, want) {
			t.Errorf("the progress report does not state %q, so the cycle above realises a "+
				"proportion the author is never told: %q", want, progressMix)
		}
	}
}

// TestAuthorGolden_RotatesTheSuggestedKind is the half the counting test cannot see: a cycle with the
// right proportions in it is worth nothing if every prompt shows the same entry of it.
//
// Left alone, an author writes precise-fact questions in a row — that is the kind that comes to mind
// while looking at a note, and it is the least informative here, because a precise fact usually
// shares its rare term with the note and retrieves on that term alone.
func TestAuthorGolden_RotatesTheSuggestedKind(t *testing.T) {
	path := writeSet(t, goldenTable)

	out := session(t, path, deck(), "one\ntwo\nthree\nfour\n")

	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if line, ok := strings.CutPrefix(line, "try "); ok {
			seen[line] = true
		}
	}
	if len(seen) < 3 {
		t.Errorf("four prompts offered %d distinct suggestion(s): %v\n%s", len(seen), seen, out)
	}
}

// TestProgressReport_NamesTheMixItDoesNotTrack. The report has to state the target and has to not
// imply it is counting it: GoldenQuestion carries no difficulty (internal/eval/goldenset.go), so a
// per-difficulty number here would be a figure nothing measured — the failure this repository names
// as "not having looked must never render as having found nothing".
func TestProgressReport_NamesTheMixItDoesNotTrack(t *testing.T) {
	path := writeSet(t, goldenTable)

	out := session(t, path, deck()[:1], "q\n")

	if !strings.Contains(out, progressMix) {
		t.Errorf("the progress report does not state the target mix:\n%s", out)
	}
	// The absent half. A line of the shape `hard: 4` would be a count of something nothing counts.
	for _, gone := range []string{"hard: ", "difficulty:", "natural: ", "associative: "} {
		if strings.Contains(out, gone) {
			t.Errorf("the session prints %q, which reads as a tally of a field the golden set does "+
				"not carry:\n%s", gone, out)
		}
	}
}

// unboundedAreas is a table whose per-area maxima cannot bind before max_total does.
//
// goldenTable cannot be used for the test below and the reason is the whole point of this fixture: it
// declares two areas capped at four each, so eight is also the sum of the per-area maxima and a
// session stops at eight whether or not anything reads max_total. A planted off-by-one in the loop
// condition stayed green against it — the deck had run out, and the test could not tell the two
// reasons apart. Here the areas allow forty and only max_total says eight.
const unboundedAreas = `coverage:
  min_total: 4
  max_total: 8
  groups:
    - name: core
      areas: [alfa, beta]
      min: 2
      max: 20
questions:
`

// TestAuthorGolden_StopsAtTheTableMaximum keeps a session from writing past the top of the range the
// table declares. Nothing downstream would refuse the entries — ValidateCoverage is a warning at run
// time (coverage.go) — so this is the only place that number stops anything.
func TestAuthorGolden_StopsAtTheTableMaximum(t *testing.T) {
	// Seven entries against a max_total of eight, so the session may write exactly one more.
	body := unboundedAreas
	for i, uid := range []string{uidA1, uidA2, uidA3, uidB1, uidB2,
		"66666666-6666-4666-8666-666666666666", "77777777-7777-4777-8777-777777777777"} {
		area := "alfa"
		if i%2 == 1 {
			area = "beta"
		}
		body += entry("a question for "+uid, uid, area)
	}
	path := writeSet(t, body)

	// A deck of notes none of which has been asked about, and an author who would keep answering all
	// of them. Only max_total stands between this session and ten more entries.
	var cards []noteCard
	for i := range 10 {
		area, folder := "alfa", "alfa"
		if i%2 == 1 {
			area, folder = "beta", "beta"
		}
		cards = append(cards, noteCard{
			UID:   fmt.Sprintf("aaaaaaaa-aaaa-4aaa-8aaa-%012d", i),
			Area:  area,
			Title: fmt.Sprintf("note %d", i),
			Path:  fmt.Sprintf("v/%s/%d.md", folder, i),
		})
	}
	session(t, path, cards, strings.Repeat("a question\n", 10))

	if got := len(loadSet(t, path).Questions); got != 8 {
		t.Errorf("the session left %d entr(ies); the table's max_total is 8 and the deck had ten "+
			"unasked notes left", got)
	}
}

// TestRunGolden_RefusesANonTerminalStdin is the refusal that keeps this command from hanging.
//
// A command that waits for an answer nobody is going to type is worse than either answer: a script
// or a scheduled run that reached this prompt would sit there until somebody noticed. --prune makes
// the same refusal for the same reason (cmd/cli/ingest_modes.go, confirmPrune).
func TestRunGolden_RefusesANonTerminalStdin(t *testing.T) {
	// A pipe: os.ModeCharDevice is clear on it, which is exactly the case a cron job presents.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("opening a pipe: %v", err)
	}
	defer func() { _ = r.Close() }()
	defer func() { _ = w.Close() }()

	// A golden set that exists and loads, so a refusal for any other reason cannot be mistaken for
	// this one.
	path := writeSet(t, goldenTable+entry("an alfa question", uidA1, "alfa"))

	var out bytes.Buffer
	err = runGolden(r, &out, r, &config.Config{}, path)
	if err == nil {
		t.Fatal("the command was accepted with a pipe on stdin, so a scheduled run would reach the " +
			"prompt and wait for an answer nobody is typing")
	}
	if !errors.Is(err, errUsage) {
		t.Errorf("the refusal is %v, which does not carry errUsage — it would exit on the code that "+
			"tells a caller the run broke and is worth retrying", err)
	}
	if !strings.Contains(err.Error(), "terminal") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("the refused run still printed something: %q", out.String())
	}
}

// TestExcerpt_ShowsEnoughToRecogniseAndNoMore is the other half of the phrasing rule, and it is the
// half a reminder cannot enforce.
//
// The card exists so the author remembers what the note is about. Show the whole note and he reads it
// instead of remembering it, and then writes a question in the note's vocabulary — which is exactly
// the string-matching failure every prompt warns about, arrived at by a route the prompt cannot
// close. So the bound is code, not advice.
// The ceilings the two bounds may be tuned within. They are separate literals from excerptLines and
// excerptRunes on purpose: asserting `shown <= excerptRunes` alone checks a constant against itself,
// so raising the constant to the size of a whole note would move both sides and stay green. That is
// the shape CLAUDE.md names — a test that confirms the chosen number without confirming it is used.
const (
	maxExcerptLines = 12
	maxExcerptRunes = 800
)

func TestExcerpt_ShowsEnoughToRecogniseAndNoMore(t *testing.T) {
	if excerptLines > maxExcerptLines || excerptRunes > maxExcerptRunes {
		t.Errorf("the excerpt is bounded to %d lines and %d runes. Above roughly %d and %d the card "+
			"stops being a reminder and becomes the note, and an author who reads the note writes "+
			"the note's vocabulary back",
			excerptLines, excerptRunes, maxExcerptLines, maxExcerptRunes)
	}

	// Two bodies, because either bound alone hides the other: long lines exhaust the rune budget
	// before the line count is reached, and short lines exhaust the line count with runes to spare.
	// A planted defect that removed one bound stayed green against a body that only exercised the
	// other.
	t.Run("long lines", func(t *testing.T) {
		var body strings.Builder
		for range 50 {
			body.WriteString(strings.Repeat("palavra ", 20) + "\n\n")
		}
		lines, shown := excerptOf(t, body.String())
		if shown > excerptRunes {
			t.Errorf("the excerpt shows %d runes of the note over %d line(s), above the %d it is "+
				"bounded to", shown, len(lines), excerptRunes)
		}
	})

	t.Run("short lines", func(t *testing.T) {
		var body strings.Builder
		for range 200 {
			body.WriteString("uma linha curta\n")
		}
		lines, shown := excerptOf(t, body.String())
		if len(lines) > excerptLines {
			t.Errorf("the excerpt is %d lines showing %d runes, above the %d lines it is bounded to",
				len(lines), shown, excerptLines)
		}
	})
}

// excerptOf runs the excerpt and answers its lines and how many runes of the note they carry, with
// the non-vacuity check both sub-tests need: a bound that holds because nothing was shown is a card
// nobody can recognise.
func excerptOf(t *testing.T, body string) (lines []string, shown int) {
	t.Helper()

	lines = excerpt(body)
	for _, line := range lines {
		// The ellipsis is this function's own addition, so the count is of what the note contributed.
		shown += len([]rune(strings.TrimSuffix(line, "...")))
	}
	if len(lines) == 0 || shown == 0 {
		t.Fatalf("the excerpt of a body far longer than either bound is %d line(s) and %d rune(s), "+
			"so this test is checking a limit nothing approaches", len(lines), shown)
	}
	return lines, shown
}

// TestOpenGoldenSetForAuthoring_TellsTheTwoFailuresApart. A set that is not there and a set that is
// broken need different things from the author, and both used to arrive as the same wall of YAML.
func TestOpenGoldenSetForAuthoring_TellsTheTwoFailuresApart(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-here.yaml")

	_, err := openGoldenSetForAuthoring(missing)
	if err == nil {
		t.Fatal("a golden set that does not exist was accepted")
	}
	if clicmd.CategoryOf(err) != clicmd.CategoryUsage {
		t.Errorf("a missing golden set is reported as %q; the operator named a path and retrying "+
			"the same command line will fail identically", clicmd.CategoryOf(err))
	}
	for _, want := range []string{missing, "coverage:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q, so it does not say what to create: %v",
				want, err)
		}
	}

	// The other failure: a file that is there and unusable. It has to be a different message, because
	// telling the author to create a file he is looking at is the answer that wastes an evening.
	broken := writeSet(t, "coverage: [this is not a table]\n")
	_, err = openGoldenSetForAuthoring(broken)
	if err == nil {
		t.Fatal("a malformed golden set was accepted")
	}
	if strings.Contains(err.Error(), "Create it") {
		t.Errorf("a malformed golden set is reported as a missing one: %v", err)
	}

	// A zero-byte file is the third case and the one that looks like neither: it is there, and it
	// decodes into a valid empty GoldenSet. Refused here so the operator hears it before a
	// fifteen-second vault scan and a session that would print an empty report and exit.
	empty := writeSet(t, "")
	_, err = openGoldenSetForAuthoring(empty)
	if err == nil {
		t.Fatal("a zero-byte golden set was accepted, so the session would scan the vaults and then " +
			"end with nothing to draw — which reads as 'nothing left to ask'")
	}
	if clicmd.CategoryOf(err) != clicmd.CategoryUsage {
		t.Errorf("a zero-byte golden set is reported as %q", clicmd.CategoryOf(err))
	}

	// And the case that must not be refused at all: a table with no questions yet.
	if _, err := openGoldenSetForAuthoring(writeSet(t, goldenTable)); err != nil {
		t.Errorf("a golden set with a table and no questions was refused, which is the file every "+
			"first session starts from: %v", err)
	}
}

// How the no-search-results rule is actually held, in the order of how hard each part is to get past.
// Stated here because the next person has to know which of these to trust, and the weakest one is the
// one that looks the most reassuring.
//
//  1. TestAuthorGolden_PrintsNothingButItsOwnShapes. The strongest, because it constrains every line
//     printed regardless of what anything is named. Its limit is real: it only sees the sessions the
//     tests drive. Those cover save, skip, stop, both progress reports and the card, so a leak on any
//     of those paths is caught — a leak on a branch no test enters is not.
//  2. TestGoldenCmd_CannotReachSearch, below. It closes what (1) cannot: a leak on an undriven path,
//     and a leak whose printed line happens to fit an allowed shape — `fmt.Fprintln(out, "")` of an
//     empty summary is a blank line, and blank lines are allowed.
//  3. searchWords. Cosmetic. Two reviewers walked past it: one with symbols that already exist in the
//     tree and carry no banned substring, one by simply naming a function `askQdrantNearest`. It is
//     kept because it costs nothing and catches the first draft anybody would type, and it is named
//     here as the weak one so nobody mistakes it for the guarantee.
var searchWords = []string{"Search", "Retriev", "Query", "Hit", "Recall"}

// searchReachingImports are the packages through which this binary reaches the index. Everything that
// searches goes through one of them: internal/retrieval builds and runs the query, internal/store
// holds the client, internal/clicmd owns the Searcher interface and the Connect that produces one.
//
// This is a list of three, and it is the only hand-written list left in this test — everything else
// below is derived from it. It is defensible where the two lists it replaced were not: those named
// the *symbols* and *files* that happened to be on the route today, and grew stale the moment anybody
// wrote a fourth file or a new package. This names the packages the route runs through, and a new
// route has to run through one of them too.
var searchReachingImports = []string{
	"github.com/danielmalka/go-knowrag/internal/retrieval",
	"github.com/danielmalka/go-knowrag/internal/store",
	"github.com/danielmalka/go-knowrag/internal/clicmd",
}

// goldenAllowedSelectors is default-deny over golden.go's whole import block, and the "whole" is the
// correction: it used to be keyed only on `eval` and `clicmd`, so a package outside those two was not
// checked at all. A reviewer wrote internal/searchleak — one function forwarding to
// retrieval.Searcher.Search — imported it here, and the test stayed green because searchleak was not
// a key in this map. Now every non-stdlib import golden.go declares must appear as a key, whether or
// not any selector is used, so a new package fails on the import alone.
//
// Within a package it is still default-deny by symbol, which is what keeps internal/eval usable:
// that package holds Searcher, RunGolden, QuestionResult, GoldenGate and Options alongside the file
// schema this command needs. A denylist would have to chase every symbol it ever grows.
//
// Adding anything here is a deliberate act, and the question to answer first is whether the symbol
// can carry, or produce, anything the index returned. That is disciplined rather than proven: the
// check is by name, not by type. Deriving it from types would mean reading every symbol's signature,
// which is the brittle static analysis this repository avoids.
var goldenAllowedSelectors = map[string][]string{
	"eval": {
		"GoldenQuestion", "GoldenSet", "AreaStatus",
		"CoverageStatus", "ReadGoldenSet", "AppendQuestion", "ErrGoldenSetMissing",
	},
	"clicmd": {"Usage"},
	"config": {"Config"},
	"vault":  {"ScanResult", "Note"},
	"cobra":  {"Command", "NoArgs"},
}

// TestGoldenCmd_CannotReachSearch is the dependency-edge half of the rule. See the numbered list
// above for what it covers that the output allow-list does not.
//
// All three edges below are derived from searchReachingImports and from the real import graph.
// Nothing here enumerates a symbol, a file or a package that happens to be on the route today — that
// shape was got past three times, each time by writing something the list had not been updated for.
func TestGoldenCmd_CannotReachSearch(t *testing.T) {
	root := moduleRoot(t)
	const goldenFile = "cmd/cli/golden.go"

	// (a) golden.go's own imports. Reuses internal/archtest, which is where this module's other
	// architecture invariant already lives, so there is one walker and one definition of "imports".
	// clicmd is excluded here alone: golden.go imports it for clicmd.Usage, and which symbols it may
	// take from it is (c)'s job.
	for _, pkg := range searchReachingImports {
		if strings.HasSuffix(pkg, "/clicmd") {
			continue
		}
		violations, err := archtest.FindImporters(root, pkg, nil)
		if err != nil {
			t.Fatalf("walking %s for %s: %v", root, pkg, err)
		}
		for _, v := range violations {
			if v.File == goldenFile {
				t.Errorf("%s imports %q. `golden` reads the vaults and must have no route to the "+
					"index at all: a question written after seeing what a search returns is a "+
					"question tuned until it passes", v, pkg)
			}
		}
	}

	fset := token.NewFileSet()
	golden, err := parser.ParseFile(fset, filepath.Join(root, goldenFile), nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", goldenFile, err)
	}

	// (b) The same-package edge, which needs no import and is therefore the one a reader misses.
	// Derived, not listed: see tainted.
	owned := tainted(t, filepath.Join(root, "cmd", "cli"), filepath.Base(goldenFile))
	if len(owned) == 0 {
		t.Fatal("no declaration in cmd/cli was found to reach search, which cannot be true while " +
			"`search` and `eval --golden` exist — this half of the test is looking at nothing")
	}

	// (c) The cross-package edge, default-deny over every import and every symbol.
	for _, imp := range golden.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		// Standard library, identified the usual way: no dot in the first path element. Nothing in it
		// reaches this index, and listing every stdlib package golden.go uses would be a list that
		// rots for no benefit.
		if first, _, _ := strings.Cut(path, "/"); !strings.Contains(first, ".") {
			continue
		}
		name := path[strings.LastIndex(path, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if _, allowed := goldenAllowedSelectors[name]; !allowed {
			t.Errorf("%s: golden.go imports %q, which has no entry in goldenAllowedSelectors. Every "+
				"non-stdlib import has to be declared there with the symbols this file may take from "+
				"it — an unlisted package is a route to anything it can reach, including the index",
				fset.Position(imp.Pos()), path)
		}
	}

	used := map[string]bool{}
	selectors := 0
	ast.Inspect(golden, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			pkg, isIdent := sel.X.(*ast.Ident)
			if !isIdent {
				return true
			}
			if allowed, watched := goldenAllowedSelectors[pkg.Name]; watched {
				selectors++
				used[pkg.Name+"."+sel.Sel.Name] = true
				if !slices.Contains(allowed, sel.Sel.Name) {
					t.Errorf("%s: golden.go uses %s.%s, which is not on the list this file may use "+
						"from %s. If it cannot carry anything the index returned, add it to "+
						"goldenAllowedSelectors deliberately; otherwise this is the leak",
						fset.Position(sel.Pos()), pkg.Name, sel.Sel.Name, pkg.Name)
				}
			}
			return true
		}
		ident, ok := n.(*ast.Ident)
		// The package clause is skipped by identity: `package main` is an *ast.Ident named "main",
		// and so is the func main() declared in main.go, which reaches search like everything else
		// that builds the command tree.
		if !ok || ident == golden.Name {
			return true
		}
		if where, isOwned := owned[ident.Name]; isOwned {
			t.Errorf("%s: golden.go references %q, declared in %s, which reaches search. Package main "+
				"needs no import, so this is a route to the index with nothing in the import block to "+
				"show for it", fset.Position(ident.Pos()), ident.Name, where)
		}
		for _, word := range searchWords {
			if strings.Contains(ident.Name, word) {
				t.Errorf("%s: golden.go names %q", fset.Position(ident.Pos()), ident.Name)
			}
		}
		return true
	})

	// Non-vacuity, both directions. A walk that matched nothing passes forever; an allow-list entry
	// nothing uses is a hole opened for a call that no longer exists.
	if selectors < 10 {
		t.Fatalf("the scan found %d watched selector(s) in golden.go — it uses more than that, so "+
			"this test is passing because it is looking at nothing", selectors)
	}
	for pkg, names := range goldenAllowedSelectors {
		for _, name := range names {
			if !used[pkg+"."+name] {
				t.Errorf("goldenAllowedSelectors permits %s.%s, which golden.go does not use — an "+
					"allow-list entry nobody needs is a door left open", pkg, name)
			}
		}
	}
}

// TestEval_NoSearchOnTheTypesGoldenHolds closes the fourth bypass, which I built after the third fix
// and which the checks above do not see.
//
// (c) watches selectors whose left side is a package: `eval.RunGolden`. It cannot watch
// `set.Anything()`, because `set` is a value and knowing its type needs a type checker. So a method
// added to internal/eval on one of the types golden.go legitimately holds — `func (g GoldenSet)
// Fetch() []QuestionResult`, building its own searcher inside the package, which already imports
// internal/retrieval — is callable from golden.go with no new import, no new package, and no watched
// selector. It goes green.
//
// Package-level transitive reachability does not close it: internal/eval imports internal/retrieval
// today, on purpose, because that is where the gate lives. Neither does an exception list. What
// closes it is the same taint pass (b) uses, pointed at internal/eval and restricted to the types
// golden.go can hold — those are derived from the allow-list by following struct fields, not listed.
func TestEval_NoSearchOnTheTypesGoldenHolds(t *testing.T) {
	dir := filepath.Join(moduleRoot(t), "internal", "eval")

	held := typesReachableFrom(t, dir, goldenAllowedSelectors["eval"])
	if len(held) < 3 {
		t.Fatalf("only %d type(s) reachable from the allow-list: %v — golden.go holds GoldenSet, "+
			"which reaches CoverageTable through a field, so this is looking at nothing", len(held), held)
	}

	reaches := tainted(t, dir, "")
	fset := token.NewFileSet()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("globbing %s: %v", dir, err)
	}

	for _, path := range paths {
		base := filepath.Base(path)
		if strings.HasSuffix(base, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parsing %s: %v", base, perr)
		}
		reaching := reachingNames(file)

		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 {
				continue
			}
			receiver := receiverType(fn.Recv.List[0].Type)
			if !held[receiver] {
				continue
			}
			bad := namesReaching(fn, reaching)
			if !bad {
				for ref := range referencedNames(fn) {
					if _, isTainted := reaches[ref]; isTainted {
						bad = true
						break
					}
				}
			}
			if bad {
				t.Errorf("%s: %s.%s reaches search, and %s is a type golden.go holds — so golden.go "+
					"could call it with no import and no package selector for any check to see",
					fset.Position(fn.Pos()), receiver, fn.Name.Name, receiver)
			}
		}
	}
}

// typesReachableFrom closes over struct fields from the named types, so "the types golden.go can
// hold" is derived rather than listed: golden.go names GoldenSet, GoldenSet has a CoverageTable
// field, CoverageTable has CoverageGroups, and a method on any of them is equally callable.
func typesReachableFrom(t *testing.T, dir string, roots []string) map[string]bool {
	t.Helper()

	fset := token.NewFileSet()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("globbing %s: %v", dir, err)
	}

	fields := map[string][]string{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parsing %s: %v", filepath.Base(path), perr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			spec, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			structType, isStruct := spec.Type.(*ast.StructType)
			if !isStruct {
				return true
			}
			for _, field := range structType.Fields.List {
				// Every identifier in the field's type expression: this picks the element type out of
				// `[]CoverageGroup` and `*int` alike, and a name that is not a local type simply never
				// matches anything below.
				for name := range referencedNames(field.Type) {
					fields[spec.Name.Name] = append(fields[spec.Name.Name], name)
				}
			}
			return true
		})
	}

	held := map[string]bool{}
	var walk func(string)
	walk = func(name string) {
		if held[name] {
			return
		}
		held[name] = true
		for _, next := range fields[name] {
			walk(next)
		}
	}
	for _, root := range roots {
		walk(root)
	}
	return held
}

// receiverType is the type name a method is declared on, with any pointer star removed.
func receiverType(expr ast.Expr) string {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

// reachingNames maps a file's local identifiers for the search-reaching packages it imports.
func reachingNames(file *ast.File) map[string]bool {
	out := map[string]bool{}
	for _, imp := range file.Imports {
		p := strings.Trim(imp.Path.Value, `"`)
		if !slices.Contains(searchReachingImports, p) {
			continue
		}
		name := p[strings.LastIndex(p, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		out[name] = true
	}
	return out
}

// tainted returns every top-level declaration in dir that can reach search, mapped to its file, by
// least fixed point over the package's own reference graph. except is the file under test, whose
// declarations are the subject rather than the ruler.
//
// The seed is a declaration that names one of searchReachingImports directly. Everything that
// references a tainted declaration becomes tainted, until nothing changes. That is what replaces the
// hand-written list of two files: a reviewer added cmd/cli/leak_helper.go calling dialSearcher — a
// third file, so the list did not know about it, and the test passed. Under this it is tainted at the
// first round and golden.go referring to it fails, whatever the file or the function is called.
//
// Per declaration, not per file, and the distinction is what makes it usable: cmd/cli/ingest.go
// imports internal/store, so a file-level rule would taint everything in it — including selectVaults
// and scanVaults, which golden.go legitimately uses and which touch nothing but config and the vault
// scanner.
func tainted(t *testing.T, dir, except string) map[string]string {
	t.Helper()

	type decl struct {
		file  string
		names map[string]bool // what its body refers to
		seed  bool
	}

	fset := token.NewFileSet()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("globbing %s: %v", dir, err)
	}

	decls := map[string]*decl{}
	for _, path := range paths {
		base := filepath.Base(path)
		if base == except || strings.HasSuffix(base, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parsing %s: %v", base, perr)
		}

		// Which local identifier stands for a search-reaching package in this file.
		reaching := reachingNames(file)

		for _, d := range file.Decls {
			for _, name := range topLevelNames(d) {
				decls[name] = &decl{file: base, names: referencedNames(d), seed: namesReaching(d, reaching)}
			}
		}
	}

	out := map[string]string{}
	for name, d := range decls {
		if d.seed {
			out[name] = d.file
		}
	}
	// Least fixed point. The package is small enough that a naive re-scan per round costs nothing.
	for changed := true; changed; {
		changed = false
		for name, d := range decls {
			if _, already := out[name]; already {
				continue
			}
			for ref := range d.names {
				if _, isTainted := out[ref]; isTainted {
					out[name] = d.file
					changed = true
					break
				}
			}
		}
	}
	return out
}

// topLevelNames is what one declaration adds to the package scope. Methods are excluded: their names
// are reachable only through a value of the receiver type, and the receiver type is itself in the map
// if it is declared here.
func topLevelNames(d ast.Decl) []string {
	var out []string
	switch v := d.(type) {
	case *ast.FuncDecl:
		if v.Recv == nil {
			out = append(out, v.Name.Name)
		}
	case *ast.GenDecl:
		for _, spec := range v.Specs {
			switch s := spec.(type) {
			case *ast.TypeSpec:
				out = append(out, s.Name.Name)
			case *ast.ValueSpec:
				for _, ident := range s.Names {
					out = append(out, ident.Name)
				}
			}
		}
	}
	return out
}

// referencedNames is every identifier a declaration mentions, minus the selector halves — `Search` in
// `s.Search(...)` names a method on some value, not a package-level declaration, and counting it
// would make any struct field that shares a name with a tainted function look like a reference to it.
func referencedNames(n ast.Node) map[string]bool {
	selectors := map[*ast.Ident]bool{}
	ast.Inspect(n, func(node ast.Node) bool {
		if sel, ok := node.(*ast.SelectorExpr); ok {
			selectors[sel.Sel] = true
		}
		return true
	})

	out := map[string]bool{}
	ast.Inspect(n, func(node ast.Node) bool {
		if ident, ok := node.(*ast.Ident); ok && !selectors[ident] {
			out[ident.Name] = true
		}
		return true
	})
	return out
}

// namesReaching reports whether a declaration mentions one of the search-reaching packages by its
// local name — `store.NewQdrantClient`, `clicmd.Connect`, `retrieval.Query`.
func namesReaching(n ast.Node, reaching map[string]bool) bool {
	found := false
	ast.Inspect(n, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, isIdent := sel.X.(*ast.Ident); isIdent && reaching[pkg.Name] {
			found = true
		}
		return true
	})
	return found
}
