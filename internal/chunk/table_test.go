package chunk

import (
	"strings"
	"testing"
)

func TestScanTables_PipeTableIsSingleAtomicUnit(t *testing.T) {
	body := "intro\n\n| Campo | Valor |\n|---|---|\n| a | 1 |\n| b | 2 |\n\noutro parágrafo\n"

	tables := scanTables(body, nil)

	if len(tables) != 1 {
		t.Fatalf("scanTables returned %d units, want 1: %+v", len(tables), tables)
	}
	got := body[tables[0].Start:tables[0].End]
	want := "| Campo | Valor |\n|---|---|\n| a | 1 |\n| b | 2 |\n"
	if got != want {
		t.Errorf("table span = %q, want %q", got, want)
	}
	if tables[0].Kind != unitTable {
		t.Errorf("Kind = %v, want unitTable", tables[0].Kind)
	}
}

// TestScanTables_SeparatorVariants pins which shapes count as a table, and — just as important —
// which do not. `---` on its own is the frontmatter delimiter and a thematic break; treating it as a
// table separator would swallow arbitrary prose into an atomic unit.
func TestScanTables_SeparatorVariants(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		found bool
	}{
		{"leading and trailing pipes", "| a | b |\n| --- | --- |\n| 1 | 2 |\n", true},
		{"no outer pipes", "a | b\n--- | ---\n1 | 2\n", true},
		{"alignment colons", "| a | b |\n|:---|---:|\n| 1 | 2 |\n", true},
		{"centred alignment", "| a | b |\n|:---:|:---:|\n| 1 | 2 |\n", true},
		{"header only, no rows", "| a | b |\n| --- | --- |\n", true},
		{"thematic break", "prose\n\n---\n\nmore prose\n", false},
		// A pipe in the line above a bare `---` is the trap: that shape is a setext heading (or a
		// thematic break), not a table. Accepting it would open an atomic unit over ordinary prose
		// and forbid every section boundary inside it.
		{"pipe line above a bare ---", "some | text\n---\nprose below\n", false},
		{"separator without a header row above", "---|---\n| 1 | 2 |\n", false},
		{"empty separator cell", "| a | b |\n|  | --- |\n| 1 | 2 |\n", false},
		{"pipes but no separator", "| a | b |\n| 1 | 2 |\n", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tables := scanTables(tc.body, nil)
			if tc.found && len(tables) != 1 {
				t.Fatalf("scanTables returned %d units, want 1: %+v", len(tables), tables)
			}
			if !tc.found && len(tables) != 0 {
				t.Fatalf("scanTables returned %+v, want none", tables)
			}
		})
	}
}

func TestScanTables_TableInsideFenceNotDetected(t *testing.T) {
	body := "```md\n| a | b |\n|---|---|\n| 1 | 2 |\n```\n"

	tables := scanTables(body, scanFences(body))

	if len(tables) != 0 {
		t.Fatalf("scanTables returned %+v, want none — the table is quoted inside a fence", tables)
	}
}

// TestScanTables_TableEndsAtFirstNonRow proves the unit stops at the table instead of running to the
// next blank line or to EOF: an atomic unit that over-claims would drag unrelated prose into the
// no-split rule.
func TestScanTables_TableEndsAtFirstNonRow(t *testing.T) {
	body := "| a | b |\n|---|---|\n| 1 | 2 |\nprose right after, no pipe\n"

	tables := scanTables(body, nil)

	if len(tables) != 1 {
		t.Fatalf("scanTables returned %d units, want 1: %+v", len(tables), tables)
	}
	if got, want := body[tables[0].Start:tables[0].End], "| a | b |\n|---|---|\n| 1 | 2 |\n"; got != want {
		t.Errorf("table span = %q, want %q", got, want)
	}
}

// TestScanTables_BlockStartCarryingAPipeEndsTheTable covers the family
// TestScanTables_TableEndsAtFirstNonRow leaves open. That test stops the table with prose carrying
// no pipe, which the `strings.Contains(line, "|")` test alone already handles — so the whole class
// of "the table is followed, with no blank line, by another construct that *also* carries a pipe"
// went unasserted, and a suite at 100% never noticed the rule was missing.
//
// GFM breaks a table "at the first empty line, or beginning of another block-level structure"
// (github.github.com/gfm#tables-extension-). Every want below was confirmed against a real GFM
// renderer (markdown-it-py in gfm-like mode), not read off this implementation.
//
// The fence rows are load-bearing in a second way: they pin that scanTables gets this right through
// the fence spans it is handed, not through a duplicate test inside isTableRow. Drop the
// `!fences.Contains(...)` guard in the continuation loop and they go red.
func TestScanTables_BlockStartCarryingAPipeEndsTheTable(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string // the table span, or "" for no table at all
	}{
		{
			"ATX heading ends the body",
			"| a | b |\n|---|---|\n| 1 | 2 |\n## A | B heading\nBody text.\n",
			"| a | b |\n|---|---|\n| 1 | 2 |\n",
		},
		{
			"ATX heading cannot be the header row",
			"## a | b\n--- | ---\n1 | 2\n",
			"",
		},
		{
			// Levels the breadcrumb never uses are still block-level structures, so they break the
			// table just the same.
			"deep heading ends the body too",
			"| a | b |\n|---|---|\n#### x | y\n",
			"| a | b |\n|---|---|\n",
		},
		{
			// A pipe inside the info string is what makes this line look like a row at all.
			"backtick fence ends the body",
			"| a | b |\n|---|---|\n```| x\ncode\n```\n",
			"| a | b |\n|---|---|\n",
		},
		{
			"tilde fence ends the body",
			"| a | b |\n|---|---|\n~~~| x\ncode\n~~~\n",
			"| a | b |\n|---|---|\n",
		},
		{
			"block quote ends the body",
			"| a | b |\n|---|---|\n> | q |\n",
			"| a | b |\n|---|---|\n",
		},
		{
			// Seven hashes is not a heading at any level (CommonMark caps at six), so the line is an
			// ordinary row and the table keeps it.
			"seven hashes is still a row",
			"| a | b |\n|---|---|\n####### x | y\n",
			"| a | b |\n|---|---|\n####### x | y\n",
		},
		{
			// `#tag` lines fill both vaults: no space after the hashes, so not a heading, so a row.
			"a hashtag is still a row",
			"| a | b |\n|---|---|\n#golang | #arquitetura\n",
			"| a | b |\n|---|---|\n#golang | #arquitetura\n",
		},
		{
			// Two GFM tables with no blank line between them are ONE table: a delimiter row is not
			// the beginning of another block-level structure, so it does not break the first table's
			// body — it becomes a data row reading "---". Confirmed against markdown-it-py, which
			// renders exactly one <table> here with `---` as a <td>. Breaking here would contradict
			// the same spec sentence the rows above rest on.
			"two tables with no blank line between them are one table",
			"| a |\n|---|\n| 1 |\n| b |\n|---|\n| 2 |\n",
			"| a |\n|---|\n| 1 |\n| b |\n|---|\n| 2 |\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// The real fence spans, because that is what the pipeline hands scanTables and what the
			// fence rows above are asserting about.
			tables := scanTables(tc.body, scanFences(tc.body))

			if tc.want == "" {
				if len(tables) != 0 {
					t.Fatalf("scanTables returned %+v, want none", tables)
				}
				return
			}
			if len(tables) != 1 {
				t.Fatalf("scanTables returned %d units, want 1: %+v", len(tables), tables)
			}
			if got := tc.body[tables[0].Start:tables[0].End]; got != tc.want {
				t.Errorf("table span = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestScanTables_ConsecutiveTables(t *testing.T) {
	body := "| a |\n|---|\n| 1 |\n\n| b |\n|---|\n| 2 |\n"

	tables := scanTables(body, nil)

	if len(tables) != 2 {
		t.Fatalf("scanTables returned %d units, want 2: %+v", len(tables), tables)
	}
}

func TestAtomicUnits_Contains(t *testing.T) {
	body := "# T\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n## Real\n"
	units := scanTables(body, nil)

	row := strings.Index(body, "| 1 | 2 |")
	heading := strings.Index(body, "## Real")

	if !units.Contains(row) {
		t.Errorf("offset %d (a table row) reported as outside the table", row)
	}
	if units.Contains(heading) {
		t.Errorf("offset %d (a heading after the table) reported as inside it", heading)
	}
	if units.Contains(0) {
		t.Error("offset 0 (before the table) reported as inside it")
	}
}
