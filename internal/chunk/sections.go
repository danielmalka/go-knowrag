package chunk

import "strings"

// section is one H2-level slice of the body, as a byte range: the heading line that opens it plus
// everything up to the next H2 (or the preamble before the first one).
//
// The heading line is kept inside the range rather than stripped, and Text is never assembled from
// pieces — a chunk is always body[Start:End] with a breadcrumb prefixed. That is what makes the
// output deterministic by construction: there is no buffer to reuse across chunks, no separator to
// invent, and editing one line of the note can only change the one range that contains it.
//
// Children are the H3-level subdivisions, present only when the section has at least one H3 outside
// an atomic unit. They partition the parent exactly — Children[0] carries the H2's own heading line
// and preamble under the parent's breadcrumb — so the ceiling split can cut at H3 without moving a
// byte. No children means the clamp has nothing legal to split on, which is how an over-ceiling
// section reaches oversize classification instead of being cut somewhere arbitrary.
type section struct {
	Breadcrumb []string
	Start, End int
	Children   []section
}

// heading is one ATX heading of level 1–3 found outside every atomic unit. Levels 4 and deeper are
// not collected: the breadcrumb contract is `H1 > H2 [> H3]` (PRD-stories-fundacao §3, S03), so a
// deeper heading is neither a boundary nor a breadcrumb component.
type heading struct {
	level int
	off   int
	text  string
}

// splitSections cuts the body at H2 boundaries and computes each section's breadcrumb from its
// ancestor headings.
//
// units are the fences and tables whose interiors are not markup (T3, T5). A `## ` line inside one
// is a line of code or a table cell, and treating it as a boundary would cut an atomic unit in half
// — atomicity is enforced here, by never producing a boundary inside a unit, rather than by
// checking afterwards.
//
// A body with no H2 yields exactly one section covering everything, never zero: PRD's acceptance
// criterion is that a note without H2 still becomes at least one chunk.
func splitSections(body string, units atomicUnits) []section {
	if body == "" {
		return nil
	}
	headings := scanHeadings(body, units)

	// Boundaries: every H2, plus the start of the body when there is content before the first one.
	var sections []section
	starts := []int{}
	for _, h := range headings {
		if h.level == 2 {
			starts = append(starts, h.off)
		}
	}
	// preamble records whether starts[0] is a real H2 or the synthetic boundary at offset 0. The two
	// are not interchangeable: a note opening directly with `## Alpha` has an H2 *at* offset 0, and
	// naming that section after a preamble it does not have would drop its own heading from the
	// breadcrumb — a silent loss, since the heading line is still in the text.
	preamble := len(starts) == 0 || starts[0] != 0
	if preamble {
		starts = append([]int{0}, starts...)
	}

	for i, start := range starts {
		end := len(body)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		bc := breadcrumbAt(headings, start)
		if i == 0 && preamble {
			bc = preambleBreadcrumb(headings)
		}
		s := section{Breadcrumb: bc, Start: start, End: end}
		s.Children = splitAtH3(headings, s)
		sections = append(sections, s)
	}
	return sections
}

// preambleBreadcrumb names the content before the first `##` after its own H1, the heading that
// titles it — the same rule as "an H2 section is named by its H2", one level up. A preamble with no
// H1 gets no breadcrumb rather than borrowing one from a later section.
func preambleBreadcrumb(headings []heading) []string {
	for _, h := range headings {
		if h.level == 2 {
			return nil
		}
		if h.level == 1 {
			return []string{h.text}
		}
	}
	return nil
}

// breadcrumbAt builds the `H1 > H2` path for a section opened by the H2 at off.
//
// The section's own heading counts as part of its breadcrumb — an H2 section is named by its H2 —
// and its ancestor is the last H1 at or before it, so a note that opens a second H1 halfway through
// files everything after it under the new one.
func breadcrumbAt(headings []heading, off int) []string {
	var h1, h2 string
	for _, h := range headings {
		if h.off > off {
			break
		}
		switch h.level {
		case 1:
			h1 = h.text
		case 2:
			h2 = h.text
		}
	}
	return nonEmpty(h1, h2)
}

// splitAtH3 subdivides a section at its H3 headings, returning nil when it has none. The first
// child covers the section's own heading line and preamble under the section's breadcrumb; the rest
// each start at an H3 line.
func splitAtH3(headings []heading, s section) []section {
	var offs []int
	var texts []string
	for _, h := range headings {
		if h.level != 3 || h.off < s.Start || h.off >= s.End {
			continue
		}
		offs = append(offs, h.off)
		texts = append(texts, h.text)
	}
	if len(offs) == 0 {
		return nil
	}

	var children []section
	if offs[0] > s.Start {
		children = append(children, section{Breadcrumb: s.Breadcrumb, Start: s.Start, End: offs[0]})
	}
	for i, off := range offs {
		end := s.End
		if i+1 < len(offs) {
			end = offs[i+1]
		}
		children = append(children, section{
			Breadcrumb: append(append([]string{}, s.Breadcrumb...), texts[i]),
			Start:      off,
			End:        end,
		})
	}
	return children
}

// scanHeadings collects every ATX heading of level 1–3 that is not inside an atomic unit.
func scanHeadings(body string, units atomicUnits) []heading {
	var out []heading
	eachLine(body, func(off int, line string, _ int) {
		if units.Contains(off) {
			return
		}
		level, text, ok := atxHeading(line)
		if ok && level <= 3 {
			out = append(out, heading{level: level, off: off, text: text})
		}
	})
	return out
}

// atxHeading matches `#`-style headings: one to six hashes, at most three spaces of indent, and at
// least one space before the text (`#hashtag` is a tag, not a heading — the vaults are full of
// them). Trailing closing hashes are stripped, as CommonMark does.
func atxHeading(line string) (level int, text string, ok bool) {
	rest := trimIndent(line)
	for level < len(rest) && rest[level] == '#' {
		level++
	}
	if level == 0 || level > 6 {
		return 0, "", false
	}
	if level == len(rest) {
		// A bare `##` line with no text: a heading with an empty name. It is still a boundary, but
		// it contributes nothing to the breadcrumb.
		return level, "", true
	}
	if rest[level] != ' ' && rest[level] != '\t' {
		return 0, "", false
	}
	text = strings.TrimSpace(rest[level:])
	text = strings.TrimRight(text, "#")
	return level, strings.TrimSpace(text), true
}

func nonEmpty(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
