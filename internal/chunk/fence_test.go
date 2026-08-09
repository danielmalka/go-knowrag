package chunk

import (
	"strings"
	"testing"
)

func TestScanFences_LanguageIdentifierRecognized(t *testing.T) {
	body := "```go\ncode\n```\n"

	fences := scanFences(body)

	if len(fences) != 1 {
		t.Fatalf("scanFences returned %d fences, want 1: %+v", len(fences), fences)
	}
	f := fences[0]
	if f.Lang != "go" {
		t.Errorf("Lang = %q, want %q", f.Lang, "go")
	}
	if !f.Closed {
		t.Errorf("Closed = false, want true for a fence with a closing delimiter")
	}
	if got := body[f.Start:f.End]; got != body {
		t.Errorf("span covers %q, want the whole fence %q", got, body)
	}
}

// TestScanFences_InfoStringVariants covers the spellings the corpus actually contains: bare fences,
// a language plus attributes, tilde fences, and fences indented up to the three spaces CommonMark
// allows before the delimiter stops being a delimiter.
func TestScanFences_InfoStringVariants(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantLang string
	}{
		{"no language", "```\ncode\n```\n", ""},
		{"language plus attributes", "```go title=\"main.go\"\ncode\n```\n", "go"},
		{"longer opening run", "````md\ncode\n````\n", "md"},
		{"tilde fence", "~~~python\ncode\n~~~\n", "python"},
		{"indented three spaces", "   ```go\n   code\n   ```\n", "go"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fences := scanFences(tc.body)
			if len(fences) != 1 {
				t.Fatalf("scanFences returned %d fences, want 1: %+v", len(fences), fences)
			}
			if fences[0].Lang != tc.wantLang {
				t.Errorf("Lang = %q, want %q", fences[0].Lang, tc.wantLang)
			}
			if !fences[0].Closed {
				t.Errorf("Closed = false, want true")
			}
		})
	}
}

// TestScanFences_TildeFenceNotClosedByBackticks is the reason the scanner tracks which character
// opened the fence: a ``` line inside a ~~~ block is content, not a terminator. Closing on the
// wrong character would end the fence early and expose everything after it to heading detection.
func TestScanFences_TildeFenceNotClosedByBackticks(t *testing.T) {
	body := "~~~\n```\n## not a heading\n```\n~~~\n## real heading\n"

	fences := scanFences(body)

	if len(fences) != 1 {
		t.Fatalf("scanFences returned %d fences, want 1: %+v", len(fences), fences)
	}
	if !fences[0].Closed {
		t.Errorf("Closed = false, want true")
	}
	if got := body[fences[0].Start:fences[0].End]; !strings.HasSuffix(got, "~~~\n") {
		t.Errorf("fence span = %q, want it to end at the ~~~ terminator", got)
	}
	if fences.Contains(strings.Index(body, "## real heading")) {
		t.Error("the heading after the closing ~~~ is reported as inside the fence")
	}
}

func TestScanFences_UnclosedFenceSpansToEOF(t *testing.T) {
	body := "intro\n\n```go\ncode\n\n## Next Heading\n\nmore text\n"

	fences := scanFences(body)

	if len(fences) != 1 {
		t.Fatalf("scanFences returned %d fences, want 1: %+v", len(fences), fences)
	}
	f := fences[0]
	if f.Closed {
		t.Errorf("Closed = true, want false for a fence with no closing delimiter")
	}
	if f.End != len(body) {
		t.Errorf("End = %d, want %d (the unclosed fence runs to EOF)", f.End, len(body))
	}
	if f.Start != strings.Index(body, "```go") {
		t.Errorf("Start = %d, want %d", f.Start, strings.Index(body, "```go"))
	}
}

func TestFenceSpans_ContainsExcludesHeadingLookingLinesInsideFence(t *testing.T) {
	body := "# Real\n\n```md\n## fake heading\n```\n\n## Real Heading\n"
	fences := scanFences(body)

	fake := strings.Index(body, "## fake heading")
	real := strings.Index(body, "## Real Heading")

	if !fences.Contains(fake) {
		t.Errorf("offset %d (`## fake heading`, inside the fence) reported as outside", fake)
	}
	if fences.Contains(real) {
		t.Errorf("offset %d (`## Real Heading`, outside the fence) reported as inside", real)
	}
	if fences.Contains(0) {
		t.Error("offset 0 (`# Real`, before the fence) reported as inside")
	}
}

// TestScanFences_FinalLineWithoutNewline covers the note that does not end in a newline — an
// ordinary way for a file to be saved, and the one input where the line walk has to close the last
// line itself instead of finding a terminator.
func TestScanFences_FinalLineWithoutNewline(t *testing.T) {
	body := "```go\ncode\n```"

	fences := scanFences(body)

	if len(fences) != 1 {
		t.Fatalf("scanFences returned %d fences, want 1: %+v", len(fences), fences)
	}
	if !fences[0].Closed || fences[0].End != len(body) {
		t.Errorf("fence = %+v, want closed and ending at %d", fences[0], len(body))
	}
}

func TestScanFences_NoFences(t *testing.T) {
	if fences := scanFences("# Title\n\njust prose\n"); len(fences) != 0 {
		t.Errorf("scanFences returned %+v, want none", fences)
	}
	if fences := scanFences(""); len(fences) != 0 {
		t.Errorf("scanFences on an empty body returned %+v, want none", fences)
	}
}

// TestScanFences_ConsecutiveFences proves the scanner resumes after a close instead of treating the
// next opening delimiter as this fence's terminator.
func TestScanFences_ConsecutiveFences(t *testing.T) {
	body := "```go\na\n```\n\ntext\n\n```sh\nb\n```\n"

	fences := scanFences(body)

	if len(fences) != 2 {
		t.Fatalf("scanFences returned %d fences, want 2: %+v", len(fences), fences)
	}
	if fences[0].Lang != "go" || fences[1].Lang != "sh" {
		t.Errorf("langs = %q, %q, want go, sh", fences[0].Lang, fences[1].Lang)
	}
	if fences.Contains(strings.Index(body, "text")) {
		t.Error("the text between the two fences is reported as fenced")
	}
}
