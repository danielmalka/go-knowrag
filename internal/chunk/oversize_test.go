package chunk

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestClassifyOversize(t *testing.T) {
	cfg := Config{FloorTokens: 256, CeilingTokens: 1024}

	tests := []struct {
		name         string
		tokens       int
		wantOversize bool
		wantErr      bool
	}{
		{"under the ceiling", 1023, false, false},
		{"exactly at the ceiling", 1024, false, false},
		{"one token over the ceiling", 1025, true, false},
		{"between ceiling and hard limit", 4000, true, false},
		{"exactly at the hard limit", HardTokenLimit, true, false},
		{"one token over the hard limit", HardTokenLimit + 1, false, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := classifyOversize("research/go/note.md", []string{"T", "Big"}, tc.tokens, cfg)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("classifyOversize(%d) = nil error, want the hard-limit refusal", tc.tokens)
				}
				return
			}
			if err != nil {
				t.Fatalf("classifyOversize(%d): %v", tc.tokens, err)
			}
			if got != tc.wantOversize {
				t.Errorf("classifyOversize(%d) = %v, want %v", tc.tokens, got, tc.wantOversize)
			}
		})
	}
}

// TestHardLimitError_NamesTheFile is the acceptance criterion verbatim: over 8192 tokens is an
// explicit error naming the file, never silent truncation. An operator holding this message must
// know which of 731 notes to open.
func TestHardLimitError_NamesTheFile(t *testing.T) {
	const path = "research/golang/enorme.md"

	_, err := classifyOversize(path, []string{"Title", "Dump"}, HardTokenLimit+1,
		Config{FloorTokens: 256, CeilingTokens: 1024})

	var hard *HardLimitError
	if !errors.As(err, &hard) {
		t.Fatalf("error is %T (%v), want *HardLimitError", err, err)
	}
	if hard.Path != path {
		t.Errorf("HardLimitError.Path = %q, want %q", hard.Path, path)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("message %q does not name the file", err.Error())
	}
	if !strings.Contains(err.Error(), "Title > Dump") {
		t.Errorf("message %q does not locate the chunk inside the file", err.Error())
	}
}

// TestHardLimitError_NoBreadcrumb keeps the message readable for a note with no headings at all,
// where there is no path to point at. An empty quoted string would read as a bug in the tool
// rather than as a fact about the note.
func TestHardLimitError_NoBreadcrumb(t *testing.T) {
	err := &HardLimitError{Path: "research/n.md", Tokens: HardTokenLimit + 1}

	if !strings.Contains(err.Error(), "(no heading)") {
		t.Errorf("message %q does not say the chunk has no heading", err.Error())
	}
}

// oversizeCfg is deliberately small so a fixture of a few hundred words crosses the ceiling without
// approaching the 8192 hard limit. The two thresholds are independent and the tests below need to
// land between them.
var oversizeCfg = Config{FloorTokens: 10, CeilingTokens: 100}

func TestOversizeFence_CeilingButUnder8192_FlaggedNotSplit(t *testing.T) {
	fence := "```go\n" + strings.Repeat("token ", 300) + "\n```\n"
	body := "# T\n\n## Code\n\n" + fence

	chunks, err := ChunkNote(context.Background(), testNote("research/code.md", body),
		oversizeCfg, FakeTokenCounter{})
	if err != nil {
		t.Fatalf("ChunkNote: %v", err)
	}

	var got *Chunk
	for i := range chunks {
		if strings.Contains(chunks[i].Text, "```go") {
			got = &chunks[i]
		}
	}
	if got == nil {
		t.Fatalf("no chunk contains the fence: %s", renderChunks(chunks))
	}
	if !got.Oversize {
		t.Error("Oversize = false, want true for a fence above the ceiling")
	}
	if !strings.Contains(got.Text, fence) {
		t.Errorf("the fence was modified or split; chunk text is %q", strconvQuoteLimited(got.Text))
	}
}

func TestOversizeTable_SameRuleAsFence(t *testing.T) {
	var rows strings.Builder
	rows.WriteString("| Campo | Valor |\n|---|---|\n")
	for range 150 {
		rows.WriteString("| " + strings.Repeat("a ", 3) + "| " + strings.Repeat("b ", 3) + "|\n")
	}
	table := rows.String()
	body := "# T\n\n## Data\n\n" + table

	chunks, err := ChunkNote(context.Background(), testNote("research/table.md", body),
		oversizeCfg, FakeTokenCounter{})
	if err != nil {
		t.Fatalf("ChunkNote: %v", err)
	}

	var got *Chunk
	for i := range chunks {
		if strings.Contains(chunks[i].Text, "| Campo | Valor |") {
			got = &chunks[i]
		}
	}
	if got == nil {
		t.Fatalf("no chunk contains the table: %s", renderChunks(chunks))
	}
	if !got.Oversize {
		t.Error("Oversize = false, want true for a table above the ceiling — same rule as a fence")
	}
	if !strings.Contains(got.Text, table) {
		t.Error("the table was split; its header row no longer travels with every data row")
	}
}

func TestFenceOver8192Tokens_ReturnsExplicitErrorNamingFile(t *testing.T) {
	const path = "research/golang/dump.md"
	body := "# T\n\n## Code\n\n```go\n" + strings.Repeat("token ", HardTokenLimit+100) + "\n```\n"

	chunks, err := ChunkNote(context.Background(), testNote(path, body), oversizeCfg, FakeTokenCounter{})

	if err == nil {
		t.Fatalf("ChunkNote = nil error, want the hard-limit refusal; got %d chunks", len(chunks))
	}
	if chunks != nil {
		t.Errorf("ChunkNote returned %d chunks alongside the error, want none", len(chunks))
	}
	var hard *HardLimitError
	if !errors.As(err, &hard) {
		t.Fatalf("error is %T (%v), want *HardLimitError", err, err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("message %q does not name the file", err.Error())
	}
}

func TestTableOver8192Tokens_ReturnsExplicitErrorNamingFile(t *testing.T) {
	const path = "research/golang/tabelao.md"
	var rows strings.Builder
	rows.WriteString("| Campo | Valor |\n|---|---|\n")
	for range 3000 {
		rows.WriteString("| " + strings.Repeat("a ", 3) + "| " + strings.Repeat("b ", 3) + "|\n")
	}
	body := "# T\n\n## Data\n\n" + rows.String()

	_, err := ChunkNote(context.Background(), testNote(path, body), oversizeCfg, FakeTokenCounter{})

	var hard *HardLimitError
	if !errors.As(err, &hard) {
		t.Fatalf("error is %T (%v), want *HardLimitError — same hard ceiling as a fence", err, err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("message %q does not name the file", err.Error())
	}
}
