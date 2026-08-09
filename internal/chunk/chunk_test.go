package chunk

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/vault"
)

// testNote carries only what ChunkNote reads. The rest of vault.Note (uid, type, status, …) belongs
// to S06a's payload assembly, not to chunking — a fixture that filled it in would suggest otherwise.
func testNote(path, body string) vault.Note {
	return vault.Note{Path: path, Body: body}
}

func chunkOf(t *testing.T, note vault.Note, cfg Config) []Chunk {
	t.Helper()
	chunks, err := ChunkNote(context.Background(), note, cfg, FakeTokenCounter{})
	if err != nil {
		t.Fatalf("ChunkNote: %v", err)
	}
	return chunks
}

var wideCfg = Config{FloorTokens: 1, CeilingTokens: 4000}

func TestChunkNote_NoH2_ReturnsAtLeastOneChunk(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"H1 and prose", "# Title\n\nprose with no subheadings at all\n"},
		{"prose only", "just prose, no heading anywhere\n"},
		{"heading only", "# Title\n"},
		{"H3 but no H2", "### Deep\n\nprose\n"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			chunks := chunkOf(t, testNote("research/n.md", tc.body), wideCfg)

			if len(chunks) == 0 {
				t.Fatal("ChunkNote returned no chunks; a note with content must produce at least one")
			}
		})
	}
}

func TestChunkNote_ChunkIndexSequentialContiguousFromZero(t *testing.T) {
	body := "# T\n\nintro\n\n## A\n\na\n\n## B\n\nb\n\n## C\n\nc\n"

	chunks := chunkOf(t, testNote("research/n.md", body), wideCfg)

	if len(chunks) < 3 {
		t.Fatalf("got %d chunks, want at least 3", len(chunks))
	}
	for i, c := range chunks {
		if c.Index != i {
			t.Errorf("chunks[%d].Index = %d, want %d", i, c.Index, i)
		}
	}
}

func TestChunkNote_RunTwice_IdenticalIndicesAndText(t *testing.T) {
	body := "# T\n\nintro\n\n## A\n\na\n\n### A1\n\na1\n\n## B\n\n| x | y |\n|---|---|\n| 1 | 2 |\n\n" +
		"## C\n\n```go\nfunc main() {}\n```\n"
	note := testNote("research/n.md", body)

	first := chunkOf(t, note, wideCfg)
	second := chunkOf(t, note, wideCfg)

	// Two runs is the acceptance criterion; the loop below runs many more. A nondeterministic step
	// — map iteration, a shared buffer, anything reading a random source — only shows up in a given
	// pair of runs with some probability, so two comparisons can pass over a real defect. Repeating
	// the comparison makes that probability vanish instead of relying on luck.
	defer func() {
		for run := range 30 {
			again := chunkOf(t, note, wideCfg)
			if len(again) != len(first) {
				t.Fatalf("run %d produced %d chunks, run 1 produced %d", run+3, len(again), len(first))
			}
			for i := range first {
				if again[i].Text != first[i].Text {
					t.Fatalf("run %d chunk %d text differs from run 1:\n  run1 %q\n  run%d %q",
						run+3, i, first[i].Text, run+3, again[i].Text)
				}
			}
		}
	}()

	if len(first) != len(second) {
		t.Fatalf("run 1 produced %d chunks, run 2 produced %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Index != second[i].Index {
			t.Errorf("chunk %d index: %d then %d", i, first[i].Index, second[i].Index)
		}
		if first[i].Text != second[i].Text {
			t.Errorf("chunk %d text differs between runs:\n  run1 %q\n  run2 %q",
				i, first[i].Text, second[i].Text)
		}
		if !slices.Equal(first[i].Breadcrumb, second[i].Breadcrumb) {
			t.Errorf("chunk %d breadcrumb: %v then %v", i, first[i].Breadcrumb, second[i].Breadcrumb)
		}
		if first[i].Oversize != second[i].Oversize {
			t.Errorf("chunk %d oversize: %v then %v", i, first[i].Oversize, second[i].Oversize)
		}
	}
}

func TestChunkNote_EditOneLineInOneChunk_OnlyThatChunkTextChanges(t *testing.T) {
	before := "# T\n\n## A\n\nalpha line\n\n## B\n\nbeta line\n\n## C\n\ngamma line\n"
	after := strings.Replace(before, "beta line", "beta line edited", 1)
	if before == after {
		t.Fatal("fixture bug: the edit changed nothing")
	}

	first := chunkOf(t, testNote("research/n.md", before), wideCfg)
	second := chunkOf(t, testNote("research/n.md", after), wideCfg)

	if len(first) != len(second) {
		t.Fatalf("editing one line changed the chunk count: %d then %d", len(first), len(second))
	}
	changed := 0
	for i := range first {
		if first[i].Text != second[i].Text {
			changed++
			if !strings.Contains(second[i].Text, "beta line edited") {
				t.Errorf("chunk %d changed but does not contain the edit:\n  %q", i, second[i].Text)
			}
		}
	}
	if changed != 1 {
		t.Errorf("%d chunks changed, want exactly 1 — a change must stay inside its own point_hash", changed)
	}
}

// TestChunkNote_TextIsBreadcrumbPlusVerbatimBody is the byte-for-byte contract S06a hashes: the
// chunk text is the note's own bytes with a breadcrumb prefixed, nothing re-wrapped, re-indented or
// re-normalized on the way through.
func TestChunkNote_TextIsBreadcrumbPlusVerbatimBody(t *testing.T) {
	body := "# T\n\n## A\n\n  indented line\ttab inside\n\ntrailing spaces here   \n"

	chunks := chunkOf(t, testNote("research/n.md", body), wideCfg)

	for _, c := range chunks {
		stripped := c.Text
		if len(c.Breadcrumb) > 0 {
			prefix := strings.Join(c.Breadcrumb, " > ") + "\n\n"
			if !strings.HasPrefix(c.Text, prefix) {
				t.Fatalf("chunk %d text %q does not start with its breadcrumb %q", c.Index, c.Text, prefix)
			}
			stripped = strings.TrimPrefix(c.Text, prefix)
		}
		if !strings.Contains(body, stripped) {
			t.Errorf("chunk %d body %q is not a verbatim slice of the note", c.Index, stripped)
		}
	}
}

func TestChunkNote_EmptyBody_ReturnsNoChunks(t *testing.T) {
	for _, body := range []string{"", "\n", "   \n\n\t\n"} {
		chunks := chunkOf(t, testNote("research/n.md", body), wideCfg)
		if len(chunks) != 0 {
			t.Errorf("body %q produced %d chunks, want none — there is nothing to embed", body, len(chunks))
		}
	}
}

func TestChunkNote_InvalidConfig_IsRefusedUpFront(t *testing.T) {
	_, err := ChunkNote(context.Background(), testNote("research/n.md", "# T\n\nbody\n"),
		Config{FloorTokens: 1024, CeilingTokens: 256}, FakeTokenCounter{})
	if err == nil {
		t.Fatal("ChunkNote = nil error, want the config rejected before any chunk is produced")
	}
}

func TestChunkNote_PropagatesCounterError(t *testing.T) {
	_, err := ChunkNote(context.Background(), testNote("research/n.md", "# T\n\nbody\n"),
		wideCfg, failingCounter{})
	if err == nil {
		t.Fatal("ChunkNote = nil error, want the tokenizer failure propagated")
	}
	if !strings.Contains(err.Error(), "research/n.md") {
		t.Errorf("error %q does not name the note", err.Error())
	}
}

// TestChunkNote_FenceEdgeCasesEndToEnd exercises T3 through the public entrypoint: an unclosed
// fence must not let the headings it swallows become boundaries, and a fenced `##` line must not
// split a chunk.
func TestChunkNote_FenceEdgeCasesEndToEnd(t *testing.T) {
	t.Run("fenced heading is not a boundary", func(t *testing.T) {
		body := "# T\n\n## A\n\n```md\n## not a section\n```\n\ntail of A\n"

		chunks := chunkOf(t, testNote("research/n.md", body), wideCfg)

		if len(chunks) != 2 {
			t.Fatalf("got %d chunks, want 2 (preamble + A): %s", len(chunks), renderChunks(chunks))
		}
		if !strings.Contains(chunks[1].Text, "tail of A") {
			t.Errorf("chunk 1 %q lost the text after the fence", chunks[1].Text)
		}
	})

	t.Run("unclosed fence runs to EOF", func(t *testing.T) {
		body := "# T\n\n## A\n\n```go\ncode\n\n## B\n\nmore\n"

		chunks := chunkOf(t, testNote("research/n.md", body), wideCfg)

		if len(chunks) != 2 {
			t.Fatalf("got %d chunks, want 2 (preamble + A): %s", len(chunks), renderChunks(chunks))
		}
		if !strings.Contains(chunks[1].Text, "## B") {
			t.Error("`## B` sits inside the unclosed fence and must stay in the same chunk")
		}
	})
}

// TestChunkNote_TableStaysWholeEndToEnd is T5 through the entrypoint: a table is never cut, so its
// header row travels with every one of its data rows.
func TestChunkNote_TableStaysWholeEndToEnd(t *testing.T) {
	table := "| Campo | Valor |\n|---|---|\n| a | 1 |\n| b | 2 |\n| c | 3 |\n"
	body := "# T\n\n## Data\n\n" + table + "\n## Next\n\ntail\n"

	chunks := chunkOf(t, testNote("research/n.md", body), Config{FloorTokens: 1, CeilingTokens: 4000})

	var found bool
	for _, c := range chunks {
		if strings.Contains(c.Text, "| a | 1 |") {
			found = true
			if !strings.Contains(c.Text, table) {
				t.Errorf("chunk %d holds part of the table but not all of it:\n%q", c.Index, c.Text)
			}
		}
	}
	if !found {
		t.Fatalf("no chunk contains the table: %s", renderChunks(chunks))
	}
}

// TestChunkNote_ClampEndToEnd is T7 through the entrypoint: tiny sibling sections merge, and the
// merged chunk carries the deepest common breadcrumb rather than the first section's.
func TestChunkNote_ClampEndToEnd(t *testing.T) {
	body := "# Title\n\n## A\n\na\n\n## B\n\nb\n\n## C\n\nc\n"

	chunks := chunkOf(t, testNote("research/n.md", body), Config{FloorTokens: 100, CeilingTokens: 500})

	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1: %s", len(chunks), renderChunks(chunks))
	}
	if !slices.Equal(chunks[0].Breadcrumb, []string{"Title"}) {
		t.Errorf("breadcrumb = %v, want [Title]", chunks[0].Breadcrumb)
	}
}

// TestChunkNote_LargeNote is the "nota gigante" edge case: many sections, mixed content, one pass.
// It asserts the two invariants that must hold whatever the size — contiguous indices from zero,
// and every chunk body a verbatim slice of the note.
func TestChunkNote_LargeNote(t *testing.T) {
	var b strings.Builder
	b.WriteString("# Giant\n\nintro\n\n")
	for i := range 200 {
		b.WriteString("## Section ")
		b.WriteString(strings.Repeat("x", 1+i%5))
		b.WriteString("\n\n")
		b.WriteString(strings.Repeat("word ", 60))
		b.WriteString("\n\n### Sub\n\n")
		b.WriteString(strings.Repeat("more ", 40))
		b.WriteString("\n\n```go\nfunc f() {}\n```\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n")
	}
	body := b.String()

	chunks := chunkOf(t, testNote("research/giant.md", body), Config{FloorTokens: 256, CeilingTokens: 1024})

	if len(chunks) < 50 {
		t.Fatalf("got %d chunks for a %d-byte note, want many more", len(chunks), len(body))
	}
	for i, c := range chunks {
		if c.Index != i {
			t.Fatalf("chunks[%d].Index = %d, want %d", i, c.Index, i)
		}
		// The invariant that must survive scale: a chunk is either inside the ceiling or says so.
		n, err := FakeTokenCounter{}.CountTokens(context.Background(), c.Text)
		if err != nil {
			t.Fatalf("CountTokens: %v", err)
		}
		if n > 1024 && !c.Oversize {
			t.Fatalf("chunk %d is %d tokens, above the 1024 ceiling, and is not flagged oversize", i, n)
		}
	}
}

func renderChunks(chunks []Chunk) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString("\n  ")
		b.WriteString(strconvQuoteLimited(c.Text))
	}
	return b.String()
}

// countingCounter records every text it is handed, so a test can pin how many measurements a note
// costs. Nothing else in the gate notices a redundant call: it returns the same number, every
// assertion still passes, and the only symptom is the bill.
type countingCounter struct{ texts []string }

func (c *countingCounter) CountTokens(ctx context.Context, text string) (int, error) {
	c.texts = append(c.texts, text)
	return FakeTokenCounter{}.CountTokens(ctx, text)
}

// TestChunkNote_MeasuresEachChunkExactlyOnce fixes the tokenization cost of a note that neither
// splits nor merges at one call per emitted chunk — the irreducible amount, since every chunk needs
// a count to be clamped and the same count to be classified oversize.
//
// It is a cost test and it is worth a test because the cost is invisible from the outputs. Under
// ADR-001 the counter is an HTTP hop, so a second call on identical text is one more round trip per
// chunk across the whole corpus, spent against NFR-4's ≤30 min ingestion budget.
func TestChunkNote_MeasuresEachChunkExactlyOnce(t *testing.T) {
	body := "## A\n\nalpha\n\n## B\n\nbeta\n"
	// Floor 1: both sections already clear it, so the merge pass evaluates no candidate. Ceiling
	// 100: nothing splits at H3. What remains is the measurement each chunk genuinely needs.
	cfg := Config{FloorTokens: 1, CeilingTokens: 100}

	tc := &countingCounter{}
	chunks, err := ChunkNote(context.Background(), testNote("research/n.md", body), cfg, tc)
	if err != nil {
		t.Fatalf("ChunkNote: %v", err)
	}

	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2: %s", len(chunks), renderChunks(chunks))
	}
	if len(tc.texts) != len(chunks) {
		t.Errorf("tokenizer called %d times for %d chunks, want once each — the count the clamp "+
			"took is the count the oversize flag needs, on byte-identical text; calls were:\n  %q",
			len(tc.texts), len(chunks), tc.texts)
	}
	// And the call that was made is the one that matters: the chunk's own final text. A single call
	// on some intermediate string would satisfy the count above while measuring the wrong thing.
	for i, c := range chunks {
		if !slices.Contains(tc.texts, c.Text) {
			t.Errorf("chunk %d text was never measured: %q, measured:\n  %q", i, c.Text, tc.texts)
		}
	}
}

// flakyCounter fails on the nth call. The tokenizer is a network hop under ADR-001's likely
// answer, so it can fail at any point of a note, not only on the first count — and every one of
// those points has to abort the note rather than emit a chunk measured against nothing.
type flakyCounter struct {
	calls  int
	failOn int
}

func (f *flakyCounter) CountTokens(ctx context.Context, text string) (int, error) {
	f.calls++
	if f.calls >= f.failOn {
		return 0, errors.New("tokenizer went away mid-note")
	}
	return FakeTokenCounter{}.CountTokens(ctx, text)
}

func TestChunkNote_CounterFailureMidNote_AbortsTheWholeNote(t *testing.T) {
	body := "# T\n\n## A\n\na\n\n### A1\n\na1\n\n### A2\n\na2\n\n## B\n\nb\n\n## C\n\nc\n"

	// Walk the failure point across every counter call the note makes, under three configs: one that
	// neither splits nor merges, one whose floor forces the merge pass to count candidates, and one
	// whose ceiling forces the H3 split — whose children reach the merge pass unmeasured and are
	// counted there. Each of the three places a count happens gets to be the one that fails.
	cfgs := []Config{wideCfg, {FloorTokens: 100, CeilingTokens: 500}, {FloorTokens: 1, CeilingTokens: 3}}
	for _, cfg := range cfgs {
		for failOn := 1; failOn <= 12; failOn++ {
			chunks, err := ChunkNote(context.Background(), testNote("research/n.md", body),
				cfg, &flakyCounter{failOn: failOn})
			if err == nil {
				continue // the note needed fewer calls than this; nothing failed, nothing to check
			}
			if chunks != nil {
				t.Errorf("failOn=%d: %d chunks returned alongside the error, want none", failOn, len(chunks))
			}
			if !strings.Contains(err.Error(), "research/n.md") {
				t.Errorf("failOn=%d: error %q does not name the note", failOn, err.Error())
			}
		}
	}
}
