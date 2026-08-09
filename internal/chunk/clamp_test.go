package chunk

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// clampOf runs the whole boundary + clamp pipeline the way ChunkNote does, so these tests measure
// the real composition rather than a hand-assembled section list.
func clampOf(t *testing.T, body string, cfg Config) []piece {
	t.Helper()
	fences := scanFences(body)
	sections := splitSections(body, mergeUnits(fences, scanTables(body, fences)))
	pieces, err := clamp(context.Background(), body, sections, cfg, FakeTokenCounter{})
	if err != nil {
		t.Fatalf("clamp: %v", err)
	}
	return pieces
}

func TestClamp_ConsecutiveSectionsBelowFloor_Merge(t *testing.T) {
	body := "## A\n\nalpha\n\n## B\n\nbeta\n\n## C\n\ngamma\n"

	pieces := clampOf(t, body, Config{FloorTokens: 20, CeilingTokens: 100})

	if len(pieces) != 1 {
		t.Fatalf("got %d chunks, want 1 — three sections under the floor merge: %s",
			len(pieces), renderPieces(body, pieces))
	}
	if got := body[pieces[0].Start:pieces[0].End]; got != body {
		t.Errorf("merged chunk covers %q, want the whole body %q", got, body)
	}
}

func TestClamp_SectionsAboveFloorAreNotMerged(t *testing.T) {
	body := "## A\n\nalpha\n\n## B\n\nbeta\n"

	pieces := clampOf(t, body, Config{FloorTokens: 2, CeilingTokens: 100})

	if len(pieces) != 2 {
		t.Fatalf("got %d chunks, want 2 — both sections are already at the floor: %s",
			len(pieces), renderPieces(body, pieces))
	}
}

func TestCommonBreadcrumbPrefix(t *testing.T) {
	tests := []struct {
		name  string
		input [][]string
		want  []string
	}{
		{
			"diverge at H3",
			[][]string{{"Title", "Setup", "Prerequisites"}, {"Title", "Setup", "Troubleshooting"}},
			[]string{"Title", "Setup"},
		},
		{
			"diverge at H2",
			[][]string{{"Title", "SectionA"}, {"Title", "SectionB"}},
			[]string{"Title"},
		},
		{
			"no common ancestor at all",
			[][]string{{"AlphaDoc", "One"}, {"BetaDoc", "Two"}},
			nil,
		},
		{
			"one is a prefix of the other",
			[][]string{{"Title", "Setup"}, {"Title", "Setup", "Deep"}},
			[]string{"Title", "Setup"},
		},
		{"identical", [][]string{{"T", "S"}, {"T", "S"}}, []string{"T", "S"}},
		{"three sections", [][]string{{"T", "A"}, {"T", "B"}, {"T", "C"}}, []string{"T"}},
		{"one empty breadcrumb collapses everything", [][]string{{"T", "A"}, nil}, nil},
		{"single input is returned whole", [][]string{{"T", "A", "B"}}, []string{"T", "A", "B"}},
		{"no input", nil, nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := commonBreadcrumbPrefix(tc.input)
			if !slices.Equal(got, tc.want) {
				t.Errorf("commonBreadcrumbPrefix(%v) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// TestCommonBreadcrumbPrefix_DoesNotAliasItsInput guards the one way a pure helper can still corrupt
// the output: returning a sub-slice of a caller's breadcrumb, which a later append would overwrite
// in place, changing a chunk that was already emitted.
func TestCommonBreadcrumbPrefix_DoesNotAliasItsInput(t *testing.T) {
	first := []string{"Title", "Setup", "Prerequisites"}
	got := commonBreadcrumbPrefix([][]string{first, {"Title", "Setup", "Troubleshooting"}})

	first[1] = "MUTATED"

	if !slices.Equal(got, []string{"Title", "Setup"}) {
		t.Errorf("result = %v after mutating the input, want [Title Setup]", got)
	}
}

func TestClamp_MergeBreadcrumb_DeepestCommonPrefix(t *testing.T) {
	// Two H3 pieces of the same H2, handed straight to the merge pass: this is the shape the PRD
	// names — merging `H1 > H2 > H3a` with `H1 > H2 > H3b` yields `H1 > H2`.
	body := "### Prerequisites\n\np\n\n### Troubleshooting\n\nt\n"
	split := strings.Index(body, "### Troubleshooting")
	pieces := []piece{
		{Breadcrumb: []string{"Title", "Setup", "Prerequisites"}, Start: 0, End: split},
		{Breadcrumb: []string{"Title", "Setup", "Troubleshooting"}, Start: split, End: len(body)},
	}

	merged, err := mergePieces(context.Background(), body, pieces,
		Config{FloorTokens: 50, CeilingTokens: 100}, FakeTokenCounter{})
	if err != nil {
		t.Fatalf("mergePieces: %v", err)
	}

	if len(merged) != 1 {
		t.Fatalf("got %d chunks, want 1: %s", len(merged), renderPieces(body, merged))
	}
	if !slices.Equal(merged[0].Breadcrumb, []string{"Title", "Setup"}) {
		t.Errorf("merged breadcrumb = %v, want [Title Setup] — not the first section's",
			merged[0].Breadcrumb)
	}
}

func TestClamp_MergeBreadcrumb_SiblingH2sCollapseToTheH1(t *testing.T) {
	body := "# Title\n\n## SectionA\n\na\n\n## SectionB\n\nb\n"

	pieces := clampOf(t, body, Config{FloorTokens: 50, CeilingTokens: 100})

	if len(pieces) != 1 {
		t.Fatalf("got %d chunks, want 1: %s", len(pieces), renderPieces(body, pieces))
	}
	if !slices.Equal(pieces[0].Breadcrumb, []string{"Title"}) {
		t.Errorf("merged breadcrumb = %v, want [Title]", pieces[0].Breadcrumb)
	}
}

// TestClamp_MergeBreadcrumb_NoCommonAncestorDegradesToEmpty is the degenerate case T7's notes name:
// sections sharing nothing (a note with no H1) merge under no breadcrumb at all. Inheriting the
// first section's would put "SectionA" in front of text that is half SectionB — and that prefix is
// embedded, so the model would read the claim.
func TestClamp_MergeBreadcrumb_NoCommonAncestorDegradesToEmpty(t *testing.T) {
	body := "## SectionA\n\na\n\n## SectionB\n\nb\n"

	pieces := clampOf(t, body, Config{FloorTokens: 50, CeilingTokens: 100})

	if len(pieces) != 1 {
		t.Fatalf("got %d chunks, want 1: %s", len(pieces), renderPieces(body, pieces))
	}
	if len(pieces[0].Breadcrumb) != 0 {
		t.Errorf("merged breadcrumb = %v, want none", pieces[0].Breadcrumb)
	}
}

func TestClamp_MergeNeverExceedsCeiling(t *testing.T) {
	// Three sections of ~40 tokens each. The floor is unreachable without crossing the ceiling, so
	// the merge must stop at two — a merge pass that only watched the floor would produce one chunk.
	section := func(name string) string {
		return "## " + name + "\n\n" + strings.Repeat("w ", 38) + "\n\n"
	}
	body := section("A") + section("B") + section("C")

	pieces := clampOf(t, body, Config{FloorTokens: 500, CeilingTokens: 100})

	if len(pieces) != 2 {
		t.Fatalf("got %d chunks, want 2: %s", len(pieces), renderPieces(body, pieces))
	}
	for i, p := range pieces {
		n, err := measureTokens(context.Background(), FakeTokenCounter{}, p.Breadcrumb, body[p.Start:p.End])
		if err != nil {
			t.Fatalf("measureTokens: %v", err)
		}
		if n > 100 {
			t.Errorf("chunk %d is %d tokens, above the 100 ceiling", i, n)
		}
	}
}

// ceilingSplitBody builds an H2 with two H3 children, each carrying `words` filler words.
//
// Token arithmetic, with the fake counter (whitespace-separated fields): the H2 section's bare text
// is 2 (`## Big`) + 2 (`### One`) + words + 2 (`### Two`) + words; its breadcrumb `T > Big` adds 3.
func ceilingSplitBody(words int) string {
	filler := strings.TrimSpace(strings.Repeat("w ", words))
	return "# T\n\n## Big\n\n### One\n\n" + filler + "\n\n### Two\n\n" + filler + "\n"
}

func TestClamp_SectionAboveCeiling_SplitsAtH3(t *testing.T) {
	body := ceilingSplitBody(10) // section: 26 bare tokens, 29 with breadcrumb

	pieces := clampOf(t, body, Config{FloorTokens: 1, CeilingTokens: 20})

	// The preamble section (`# T`) plus three children of the split H2.
	if len(pieces) != 4 {
		t.Fatalf("got %d chunks, want 4: %s", len(pieces), renderPieces(body, pieces))
	}
	want := [][]string{{"T"}, {"T", "Big"}, {"T", "Big", "One"}, {"T", "Big", "Two"}}
	for i, p := range pieces {
		if !slices.Equal(p.Breadcrumb, want[i]) {
			t.Errorf("chunk %d breadcrumb = %v, want %v", i, p.Breadcrumb, want[i])
		}
	}
	if got, want := body[pieces[2].Start:pieces[2].End], "### One\n\n"+strings.TrimSpace(strings.Repeat("w ", 10))+"\n\n"; got != want {
		t.Errorf("H3 chunk = %q, want %q", got, want)
	}
}

// TestClamp_CeilingDecidedOnBreadcrumbInclusiveCount is the test T6 asks for: with the ceiling set
// between the section's bare count (26) and its breadcrumb-inclusive count (29), the section must
// split. Counting the bare body would leave it whole and this test red.
func TestClamp_CeilingDecidedOnBreadcrumbInclusiveCount(t *testing.T) {
	body := ceilingSplitBody(10)

	pieces := clampOf(t, body, Config{FloorTokens: 1, CeilingTokens: 27})

	if len(pieces) < 3 {
		t.Fatalf("got %d chunks, want the H2 split at its H3 children — the ceiling of 27 sits "+
			"between the bare body (26 tokens) and the breadcrumb-prefixed text (29): %s",
			len(pieces), renderPieces(body, pieces))
	}
}

func TestClamp_SectionAboveCeilingWithNoH3_LeftIntact(t *testing.T) {
	body := "## Big\n\n" + strings.Repeat("w ", 200) + "\n"

	pieces := clampOf(t, body, Config{FloorTokens: 10, CeilingTokens: 50})

	if len(pieces) != 1 {
		t.Fatalf("got %d chunks, want 1 — there is no H3 to split on, so oversize classification "+
			"owns this case: %s", len(pieces), renderPieces(body, pieces))
	}
	if got := body[pieces[0].Start:pieces[0].End]; got != body {
		t.Errorf("chunk = %q, want the section untouched", got)
	}
}

// TestClamp_FenceIsNeverSplit is the atomic-unit guarantee seen from the clamp: the fence holds
// `### ` lines, and the only legal cut inside an over-ceiling section is an H3 boundary. Since
// boundaries are never detected inside a fence, the fence survives whole.
func TestClamp_FenceIsNeverSplit(t *testing.T) {
	fence := "```go\n" + strings.Repeat("### fake\n", 40) + "```\n"
	body := "## Code\n\n" + fence

	pieces := clampOf(t, body, Config{FloorTokens: 10, CeilingTokens: 20})

	if len(pieces) != 1 {
		t.Fatalf("got %d chunks, want 1 — the fence is atomic: %s", len(pieces), renderPieces(body, pieces))
	}
	if !strings.Contains(body[pieces[0].Start:pieces[0].End], fence) {
		t.Error("the fence was cut")
	}
}

func TestClamp_WhitespaceOnlyPiecesAreDropped(t *testing.T) {
	body := "\n\n\n## A\n\nalpha\n"

	pieces := clampOf(t, body, Config{FloorTokens: 1, CeilingTokens: 100})

	if len(pieces) != 1 {
		t.Fatalf("got %d chunks, want 1 — the blank preamble is not a chunk: %s",
			len(pieces), renderPieces(body, pieces))
	}
	if got, want := body[pieces[0].Start:pieces[0].End], "## A\n\nalpha\n"; got != want {
		t.Errorf("chunk = %q, want %q", got, want)
	}
}

func TestClamp_PropagatesCounterError(t *testing.T) {
	body := "## A\n\nalpha\n"
	fences := scanFences(body)
	sections := splitSections(body, mergeUnits(fences, scanTables(body, fences)))

	_, err := clamp(context.Background(), body, sections,
		Config{FloorTokens: 1, CeilingTokens: 10}, failingCounter{})
	if err == nil {
		t.Fatal("clamp = nil error, want the tokenizer failure propagated")
	}
}

func renderPieces(body string, pieces []piece) string {
	var b strings.Builder
	for _, p := range pieces {
		b.WriteString("\n  [")
		b.WriteString(strings.Join(p.Breadcrumb, " > "))
		b.WriteString("] ")
		b.WriteString(strconvQuoteLimited(body[p.Start:p.End]))
	}
	return b.String()
}

func strconvQuoteLimited(s string) string {
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return strconv.Quote(s)
}

// TestClamp_BlankChildOfASplitSectionIsDropped covers the one shape that produces an empty child:
// a note whose content starts with blank lines and whose first heading is an H3. The parent section
// is not blank (it holds the H3), so the drop has to happen at child level too.
func TestClamp_BlankChildOfASplitSectionIsDropped(t *testing.T) {
	body := "\n\n### A\n\n" + strings.Repeat("w ", 30) + "\n\n### B\n\n" + strings.Repeat("w ", 30) + "\n"

	pieces := clampOf(t, body, Config{FloorTokens: 1, CeilingTokens: 40})

	if len(pieces) != 2 {
		t.Fatalf("got %d chunks, want 2 — the leading blank child is not a chunk: %s",
			len(pieces), renderPieces(body, pieces))
	}
	for i, p := range pieces {
		if blank(body[p.Start:p.End]) {
			t.Errorf("chunk %d is whitespace only", i)
		}
	}
}
