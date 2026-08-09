package chunk

import (
	"slices"
	"strings"
)

// atomicKind names what an atomic unit is, for error messages and for reading a failing test.
type atomicKind int

const (
	unitFence atomicKind = iota
	unitTable
)

// atomicUnit is a byte range of the body that must never be cut in half: a fenced code block or a
// pipe table (PRD-stories-fundacao §3, S03, "Blocos de código e tabelas são unidades atômicas").
// A table split row-by-row loses its column header and every row below the cut turns ambiguous;
// code split mid-function is as useless.
//
// Atomicity is enforced structurally rather than by a check after the fact: these ranges are the
// ranges where a line that looks like a heading is not treated as a section boundary, and section
// boundaries are the only places this package ever cuts. A unit that is itself larger than the
// ceiling therefore stays whole and is flagged `oversize` (see oversize.go).
type atomicUnit struct {
	Kind       atomicKind
	Start, End int
}

type atomicUnits []atomicUnit

// Contains reports whether the byte offset sits inside any unit.
func (u atomicUnits) Contains(offset int) bool {
	for _, a := range u {
		if offset >= a.Start && offset < a.End {
			return true
		}
	}
	return false
}

// units converts fences to the shared kind-tagged form, so the boundary passes have one list to
// consult instead of two.
func (f fenceSpans) units() atomicUnits {
	out := make(atomicUnits, 0, len(f))
	for _, s := range f {
		out = append(out, atomicUnit{Kind: unitFence, Start: s.Start, End: s.End})
	}
	return out
}

// mergeUnits returns fences and tables as one list ordered by start offset.
func mergeUnits(fences fenceSpans, tables atomicUnits) atomicUnits {
	all := append(fences.units(), tables...)
	slices.SortFunc(all, func(a, b atomicUnit) int { return a.Start - b.Start })
	return all
}

// scanTables finds GFM pipe tables — the syntax Obsidian renders — outside of any fence.
//
// A table is a row line immediately followed by a delimiter row (`|---|:--:|`), then every
// following line that still looks like a row. The delimiter row must itself contain a `|`: without
// that rule a bare `---` (a thematic break, and the frontmatter delimiter) would open a table and
// swallow the prose beneath it into an atomic unit that must never be split.
//
// Fenced regions are skipped by offset rather than by re-parsing, so a table quoted inside a
// ```md block is not detected twice — nor detected at all, which is the point: it is code.
func scanTables(body string, fences fenceSpans) atomicUnits {
	var out atomicUnits

	type lineInfo struct {
		off, end int
		text     string
	}
	var lines []lineInfo
	eachLine(body, func(off int, line string, lineEnd int) {
		lines = append(lines, lineInfo{off: off, end: lineEnd, text: line})
	})

	for i := 0; i < len(lines); i++ {
		if fences.Contains(lines[i].off) {
			continue
		}
		if i+1 >= len(lines) || !isTableRow(lines[i].text) || !isTableSeparator(lines[i+1].text) {
			continue
		}
		end := lines[i+1].end
		j := i + 2
		for ; j < len(lines) && isTableRow(lines[j].text) && !fences.Contains(lines[j].off); j++ {
			end = lines[j].end
		}
		out = append(out, atomicUnit{Kind: unitTable, Start: lines[i].off, End: end})
		i = j - 1
	}
	return out
}

// isTableRow is deliberately loose — any non-blank line carrying a pipe — except where GFM says a
// table has already ended. The strictness about whether a table exists at all lives in
// isTableSeparator; the strictness here is about where one stops.
//
// GFM breaks a table "at the first empty line, or beginning of another block-level structure"
// (github.github.com/gfm#tables-extension-). A blank line and a line without a pipe are handled by
// the two conditions above. What is left is a line that carries a pipe and is nonetheless the start
// of a block: an ATX heading, or a block quote. Neither can be a header row or a continuation row.
//
// The heading half is the one with teeth. Without it a table written flush against the next
// heading swallows it into the atomic unit and erases the `##` boundary T4 calls primary — a silent
// loss, since the heading text stays in the chunk but leaves every breadcrumb. The corpus already
// has `## Empresa | Cargo` headings for that to land on.
//
// Two neighbours of this rule are deliberately not here:
//
//   - Fence delimiters. `` ```| `` already stops the scan, because scanTables consults the fence
//     spans directly and a fence's opening line is inside its own span. Repeating the test here
//     would be a second rule free to disagree with the first. Pinned by
//     TestScanTables_BlockStartCarryingAPipeEndsTheTable.
//   - ponytail: list markers. `- | l |` is a list in GFM and this scanner keeps it as a row. The
//     over-claim is unobservable — an atomic unit's only effect is suppressing heading boundaries
//     inside it (scanHeadings), and after the heading rule above no heading can be inside a table
//     range at all. Getting the list grammar wrong (three bullet chars, two ordered delimiters,
//     indentation, `-` against `---`) would cost a real table row, which is a loss, not an
//     over-claim. Implement it when something other than heading suppression reads these ranges.
func isTableRow(line string) bool {
	if !strings.Contains(line, "|") || strings.TrimSpace(line) == "" {
		return false
	}
	if rest := trimIndent(line); strings.HasPrefix(rest, ">") {
		return false
	}
	_, _, isHeading := atxHeading(line)
	return !isHeading
}

// isTableSeparator matches the delimiter row: pipe-separated cells of dashes, with optional
// alignment colons.
func isTableSeparator(line string) bool {
	if !strings.Contains(line, "|") {
		return false
	}
	cells := strings.Split(strings.Trim(strings.TrimSpace(trimIndent(line)), "|"), "|")
	for _, c := range cells {
		if !isSeparatorCell(strings.TrimSpace(c)) {
			return false
		}
	}
	return len(cells) > 0
}

func isSeparatorCell(c string) bool {
	c = strings.TrimPrefix(c, ":")
	c = strings.TrimSuffix(c, ":")
	if c == "" {
		return false
	}
	return strings.Count(c, "-") == len(c)
}
