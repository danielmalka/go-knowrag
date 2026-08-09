package chunk

import "strings"

// fenceSpan is one fenced code block, as a byte range over the note body: [Start, End), End being
// the offset just past the closing delimiter's newline (or len(body) for an unclosed fence).
//
// Lang is the first word of the info string ("go" for ```go, "" for a bare fence). Closed records
// whether a terminator was found — an unclosed fence is not an error here: PRD-stories-fundacao §3
// (S03) requires it to have defined behaviour, which is that it runs to EOF rather than ending at
// the first line that happens to look like a heading.
type fenceSpan struct {
	Start, End int
	Lang       string
	Closed     bool
}

type fenceSpans []fenceSpan

// Contains reports whether the byte offset sits inside any fence. Callers use it to refuse to treat
// a line as structural markup — a heading, a table row — when it is really code being quoted.
func (f fenceSpans) Contains(offset int) bool {
	for _, s := range f {
		if offset >= s.Start && offset < s.End {
			return true
		}
	}
	return false
}

// scanFences finds every fenced code block in body, line by line.
//
// The body arrives LF-only (PRD-contrato §2.4, normalized once by S02) and this scanner depends on
// it: with CRLF, a trailing \r lands inside the info string, so ```go parses as Lang == "go\r" and
// the closing ``` line stops matching. There is deliberately no \r guard here — a second
// normalization site is a second rule free to drift from S02's.
//
// Fence syntax follows CommonMark closely enough for real notes: three or more backticks or tildes,
// indented at most three spaces, closed by a run of the *same* character at least as long with
// nothing but whitespace after it. The tilde half matters because a ``` line inside a ~~~ block is
// content, and closing on it would expose the rest of the document to heading detection.
func scanFences(body string) fenceSpans {
	var out fenceSpans

	open := false
	var openChar byte
	var openLen int

	eachLine(body, func(off int, line string, lineEnd int) {
		char, runLen, info, ok := fenceDelimiter(line)
		if !open {
			if ok {
				open, openChar, openLen = true, char, runLen
				out = append(out, fenceSpan{Start: off, Lang: firstWord(info)})
			}
			return
		}
		// Inside a fence: only a longer-or-equal run of the opening character with an empty info
		// string closes it. Everything else, including a delimiter of the other character, is code.
		if ok && char == openChar && runLen >= openLen && strings.TrimSpace(info) == "" {
			out[len(out)-1].End = lineEnd
			out[len(out)-1].Closed = true
			open = false
		}
	})

	if open {
		out[len(out)-1].End = len(body)
	}
	return out
}

// fenceDelimiter reports whether line is a fence delimiter, returning the delimiter character, its
// run length and the info string that follows it.
func fenceDelimiter(line string) (char byte, runLen int, info string, ok bool) {
	rest := trimIndent(line)
	if rest == "" {
		return 0, 0, "", false
	}
	char = rest[0]
	if char != '`' && char != '~' {
		return 0, 0, "", false
	}
	for runLen < len(rest) && rest[runLen] == char {
		runLen++
	}
	if runLen < 3 {
		return 0, 0, "", false
	}
	return char, runLen, rest[runLen:], true
}

// trimIndent removes up to three leading spaces, the most CommonMark allows before a construct
// stops being that construct (four spaces make it an indented code block). Applied to fences,
// headings and table rows alike so all three agree on what "at the left margin" means.
func trimIndent(line string) string {
	i := 0
	for i < 3 && i < len(line) && line[i] == ' ' {
		i++
	}
	return line[i:]
}

func firstWord(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// eachLine walks body's lines, handing the callback the line's start offset, its text without the
// terminating newline, and the offset just past that newline. It is the single place this package
// converts offsets to lines: the byte ranges every later pass slices with have to line up exactly,
// and three hand-rolled loops would be three chances for one of them to be off by one.
func eachLine(body string, fn func(off int, line string, lineEnd int)) {
	for off := 0; off < len(body); {
		nl := strings.IndexByte(body[off:], '\n')
		if nl < 0 {
			fn(off, body[off:], len(body))
			return
		}
		fn(off, body[off:off+nl], off+nl+1)
		off += nl + 1
	}
}
