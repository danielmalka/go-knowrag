package chunk

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

// sectionsOf runs the real boundary pipeline (fences, then tables, then the split) so every test
// below exercises the masking the splitter actually receives at run time.
func sectionsOf(body string) []section {
	fences := scanFences(body)
	return splitSections(body, mergeUnits(fences, scanTables(body, fences)))
}

func TestSplitSections_H2IsPrimaryBoundary(t *testing.T) {
	body := "## Alpha\n\nfirst body\n\n## Beta\n\nsecond body\n"

	sections := sectionsOf(body)

	if len(sections) != 2 {
		t.Fatalf("got %d sections, want 2: %s", len(sections), renderSections(body, sections))
	}
	if got, want := body[sections[0].Start:sections[0].End], "## Alpha\n\nfirst body\n\n"; got != want {
		t.Errorf("section 0 = %q, want %q", got, want)
	}
	if got, want := body[sections[1].Start:sections[1].End], "## Beta\n\nsecond body\n"; got != want {
		t.Errorf("section 1 = %q, want %q", got, want)
	}
	// A note opening straight into `## Alpha` has an H2 at offset 0, not a preamble. The section is
	// still named by its own heading; treating offset 0 as "the preamble" instead would silently
	// drop `Alpha` from the breadcrumb, and the payload's `headings` with it.
	if !slices.Equal(sections[0].Breadcrumb, []string{"Alpha"}) {
		t.Errorf("section 0 breadcrumb = %v, want [Alpha]", sections[0].Breadcrumb)
	}
	if !slices.Equal(sections[1].Breadcrumb, []string{"Beta"}) {
		t.Errorf("section 1 breadcrumb = %v, want [Beta]", sections[1].Breadcrumb)
	}
}

// TestSplitSections_LaterH1BecomesTheAncestor pins the heading stack: a note that opens a second H1
// halfway through files the sections after it under the new one, not under the note's first title.
func TestSplitSections_LaterH1BecomesTheAncestor(t *testing.T) {
	body := "# One\n\n## A\n\na\n\n# Two\n\n## B\n\nb\n"

	sections := sectionsOf(body)

	last := sections[len(sections)-1]
	if !slices.Equal(last.Breadcrumb, []string{"Two", "B"}) {
		t.Errorf("last section breadcrumb = %v, want [Two B]", last.Breadcrumb)
	}
}

func TestSplitSections_NoH2_ReturnsSingleSection(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"H1 only", "# Title\n\njust one heading and prose\n"},
		{"no headings at all", "plain prose with no heading whatsoever\n"},
		{"H3 without any H2", "### Deep\n\nprose\n"},
		// Seven hashes is not a heading at any level, and a single backtick is inline code, not a
		// fence. Both are lines the scanners must walk past without opening anything.
		{"seven hashes and inline code", "####### not a heading\n\n`inline` code\n"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sections := sectionsOf(tc.body)

			if len(sections) != 1 {
				t.Fatalf("got %d sections, want 1: %s", len(sections), renderSections(tc.body, sections))
			}
			if sections[0].Start != 0 || sections[0].End != len(tc.body) {
				t.Errorf("section = [%d,%d), want the whole body [0,%d)",
					sections[0].Start, sections[0].End, len(tc.body))
			}
		})
	}
}

func TestSplitSections_PreambleBeforeFirstH2IsItsOwnSection(t *testing.T) {
	body := "# Title\n\nintro paragraph\n\n## Alpha\n\nbody\n"

	sections := sectionsOf(body)

	if len(sections) != 2 {
		t.Fatalf("got %d sections, want 2: %s", len(sections), renderSections(body, sections))
	}
	if got, want := body[sections[0].Start:sections[0].End], "# Title\n\nintro paragraph\n\n"; got != want {
		t.Errorf("preamble section = %q, want %q", got, want)
	}
	if !slices.Equal(sections[0].Breadcrumb, []string{"Title"}) {
		t.Errorf("preamble breadcrumb = %v, want [Title]", sections[0].Breadcrumb)
	}
}

func TestBreadcrumb_H3NestedUnderH2NestedUnderH1(t *testing.T) {
	body := "# Title\n\n## Setup\n\nsetup body\n\n### Prerequisites\n\ndeep body\n"

	sections := sectionsOf(body)

	if len(sections) != 2 {
		t.Fatalf("got %d sections, want 2: %s", len(sections), renderSections(body, sections))
	}
	setup := sections[1]
	if !slices.Equal(setup.Breadcrumb, []string{"Title", "Setup"}) {
		t.Errorf("H2 breadcrumb = %v, want [Title Setup]", setup.Breadcrumb)
	}
	if len(setup.Children) != 2 {
		t.Fatalf("got %d children, want 2 (the H2's own preamble, then the H3): %+v",
			len(setup.Children), setup.Children)
	}
	innermost := setup.Children[len(setup.Children)-1]
	if !slices.Equal(innermost.Breadcrumb, []string{"Title", "Setup", "Prerequisites"}) {
		t.Errorf("H3 breadcrumb = %v, want [Title Setup Prerequisites]", innermost.Breadcrumb)
	}
	if got, want := body[innermost.Start:innermost.End], "### Prerequisites\n\ndeep body\n"; got != want {
		t.Errorf("H3 child = %q, want %q", got, want)
	}
}

// TestSplitSections_NoH3_HasNoChildren is what tells the clamp there is nothing to split on: an
// over-ceiling section with no H3 must reach oversize classification, not be silently cut somewhere.
func TestSplitSections_NoH3_HasNoChildren(t *testing.T) {
	body := "# Title\n\n## Alpha\n\nbody with no subheadings\n"

	sections := sectionsOf(body)

	if len(sections) != 2 {
		t.Fatalf("got %d sections, want 2: %s", len(sections), renderSections(body, sections))
	}
	if len(sections[1].Children) != 0 {
		t.Errorf("children = %+v, want none", sections[1].Children)
	}
}

// TestSplitSections_BareHashesAreABoundaryWithNoName pins the degenerate heading: `##` with no
// text still opens a section — the author wrote a boundary — but contributes nothing to the
// breadcrumb, rather than a stray " > " in the text the model reads.
func TestSplitSections_BareHashesAreABoundaryWithNoName(t *testing.T) {
	body := "# T\n\nintro\n\n##\n\nbody\n"

	sections := sectionsOf(body)

	if len(sections) != 2 {
		t.Fatalf("got %d sections, want 2: %s", len(sections), renderSections(body, sections))
	}
	if !slices.Equal(sections[1].Breadcrumb, []string{"T"}) {
		t.Errorf("breadcrumb = %v, want [T] — the nameless H2 adds nothing", sections[1].Breadcrumb)
	}
}

// TestSplitSections_HashtagIsNotAHeading matters because both vaults are full of `#tag` lines: a
// heading needs a space after its hashes. Without that rule every tag line would open a section and
// put its own text into the breadcrumb of everything under it.
func TestSplitSections_HashtagIsNotAHeading(t *testing.T) {
	body := "#golang #arquitetura\n\n##tambem-nao\n\nprose\n\n## Real\n\nbody\n"

	sections := sectionsOf(body)

	if len(sections) != 2 {
		t.Fatalf("got %d sections, want 2 (the tag preamble + `## Real`): %s",
			len(sections), renderSections(body, sections))
	}
	if len(sections[0].Breadcrumb) != 0 {
		t.Errorf("preamble breadcrumb = %v, want none — `#golang` is a tag", sections[0].Breadcrumb)
	}
	if !slices.Equal(sections[1].Breadcrumb, []string{"Real"}) {
		t.Errorf("section 1 breadcrumb = %v, want [Real]", sections[1].Breadcrumb)
	}
}

func TestSplitSections_HeadingsInsideFenceAreNotBoundaries(t *testing.T) {
	body := "# Title\n\n## Alpha\n\n```md\n## fake\n\n### also fake\n```\n\nstill alpha\n"

	sections := sectionsOf(body)

	if len(sections) != 2 {
		t.Fatalf("got %d sections, want 2: %s", len(sections), renderSections(body, sections))
	}
	if !strings.Contains(body[sections[1].Start:sections[1].End], "still alpha") {
		t.Error("the fenced `## fake` line split the section: the text after the fence is elsewhere")
	}
	if len(sections[1].Children) != 0 {
		t.Errorf("the fenced `### also fake` line produced children %+v, want none", sections[1].Children)
	}
}

// TestSplitSections_ATXHeadingBeatsTableRow pins the precedence GFM fixes and this package once had
// backwards: a line that is a syntactically valid ATX heading is a heading, whether or not it also
// carries pipes and whether or not table rows sit immediately above it. Table detection may not
// claim it — a table starts only where a paragraph could, and its body breaks at the next
// block-level structure (GFM §4.10).
//
// The failure this replaces was silent: the heading was absorbed into the table's atomic unit, so it
// stopped being the `##` boundary T4 calls primary and stopped appearing in any breadcrumb, while
// its text stayed in the chunk. Nothing downstream could tell.
func TestSplitSections_ATXHeadingBeatsTableRow(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		wantSections int
		wantLastBC   []string
	}{
		{
			// No blank line between the table and the heading — the shape that used to lose the
			// boundary, because the heading was accepted as a table continuation row.
			"heading flush against the rows above it",
			"| a | b |\n|---|---|\n| 1 | 2 |\n## A | B heading\nBody text.\n",
			2,
			[]string{"A | B heading"},
		},
		{
			// A heading can never be the header row that opens a table either, so the two lines
			// below it are ordinary prose inside the section the heading opens.
			"heading is not a table header row",
			"## Alpha\n\n## a | b\n--- | ---\n1 | 2\n\ntail\n",
			2,
			[]string{"a | b"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sections := sectionsOf(tc.body)

			if len(sections) != tc.wantSections {
				t.Fatalf("got %d sections, want %d: %s",
					len(sections), tc.wantSections, renderSections(tc.body, sections))
			}
			last := sections[len(sections)-1].Breadcrumb
			if !slices.Equal(last, tc.wantLastBC) {
				t.Errorf("last section breadcrumb = %v, want %v", last, tc.wantLastBC)
			}
		})
	}
}

// TestSplitSections_HeadingInsideFenceStillBeatsNothing is the guard rail on the fix above: the
// precedence rule is table-versus-heading only. Inside a fenced block a `## ` line is code, and the
// fence — not the heading — is what wins. Correcting one precedence must not invert the other.
func TestSplitSections_HeadingInsideFenceStillBeatsNothing(t *testing.T) {
	body := "## Alpha\n\n```md\n| a | b |\n|---|---|\n## fake | heading\n```\n\ntail\n"

	sections := sectionsOf(body)

	if len(sections) != 1 {
		t.Fatalf("got %d sections, want 1 — the `## fake | heading` line is inside a fence: %s",
			len(sections), renderSections(body, sections))
	}
}

// TestSplitSections_UnclosedFenceDoesNotSwallowTheDocumentIntoNothing keeps the two rules honest
// together: the unclosed fence runs to EOF (T3), so everything after it is one section rather than
// several — and still exactly one section, never zero.
func TestSplitSections_UnclosedFenceSwallowsLaterHeadings(t *testing.T) {
	body := "## Alpha\n\n```go\ncode\n\n## Beta\n\nmore\n"

	sections := sectionsOf(body)

	if len(sections) != 1 {
		t.Fatalf("got %d sections, want 1 — `## Beta` is inside the unclosed fence: %s",
			len(sections), renderSections(body, sections))
	}
}

// TestSplitSections_PartitionTheBody is the determinism guarantee the whole package rests on: every
// section is a byte range of the original body, they are ordered, they do not overlap, and
// concatenated they reproduce the body exactly. Chunk text is never assembled from copies, so there
// is no buffer to reuse and nothing to reorder.
func TestSplitSections_PartitionTheBody(t *testing.T) {
	body := "# Title\n\nintro\n\n## Alpha\n\na\n\n### A1\n\na1\n\n## Beta\n\n```go\n## fake\n```\n\nb\n"

	sections := sectionsOf(body)

	var rebuilt strings.Builder
	prevEnd := 0
	for i, s := range sections {
		if s.Start != prevEnd {
			t.Fatalf("section %d starts at %d, want %d (gap or overlap)", i, s.Start, prevEnd)
		}
		if s.End <= s.Start {
			t.Fatalf("section %d is empty: [%d,%d)", i, s.Start, s.End)
		}
		rebuilt.WriteString(body[s.Start:s.End])
		prevEnd = s.End

		childEnd := s.Start
		for j, c := range s.Children {
			if c.Start != childEnd {
				t.Fatalf("section %d child %d starts at %d, want %d", i, j, c.Start, childEnd)
			}
			childEnd = c.End
		}
		if len(s.Children) > 0 && childEnd != s.End {
			t.Fatalf("section %d children end at %d, want %d", i, childEnd, s.End)
		}
	}
	if prevEnd != len(body) {
		t.Fatalf("sections cover [0,%d), want [0,%d)", prevEnd, len(body))
	}
	if rebuilt.String() != body {
		t.Error("concatenating the sections does not reproduce the body byte for byte")
	}
}

func TestSplitSections_EmptyBody(t *testing.T) {
	if got := sectionsOf(""); len(got) != 0 {
		t.Errorf("got %+v sections for an empty body, want none", got)
	}
}

func renderSections(body string, sections []section) string {
	var b strings.Builder
	for _, s := range sections {
		b.WriteString("\n  [")
		b.WriteString(strings.Join(s.Breadcrumb, " > "))
		b.WriteString("] ")
		b.WriteString(strconv.Quote(body[s.Start:s.End]))
	}
	return b.String()
}
