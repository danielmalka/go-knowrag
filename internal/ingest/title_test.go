package ingest

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/danielmalka/go-knowrag/internal/chunk"
	"github.com/danielmalka/go-knowrag/internal/schema"
)

// The note title has no length limit and goes in front of every chunk of the note. These tests pin
// the guarantee that makes the chunk window safe: whatever the title, the prefixed chunk never passes
// the model's window, because the title is cut and the chunk text is not.

// runeCounter counts one token per 4 runes, so a title made of one huge word still has a length.
type runeCounter struct{ calls int }

func (c *runeCounter) CountTokens(_ context.Context, text string) (int, error) {
	c.calls++
	return (utf8.RuneCountInString(text) + 3) / 4, nil
}

func TestFitTitle_ShortTitleIsKept(t *testing.T) {
	got, err := fitTitle(t.Context(), chunk.FakeTokenCounter{}, "  Plano de ingestão  ", 96)
	if err != nil || got != "Plano de ingestão" {
		t.Fatalf("fitTitle = %q, %v; want the trimmed title unchanged", got, err)
	}
}

func TestFitTitle_EmptyTitleCostsNoCount(t *testing.T) {
	c := &runeCounter{}
	got, err := fitTitle(t.Context(), c, "   ", 96)
	if err != nil || got != "" || c.calls != 0 {
		t.Fatalf("fitTitle = %q, %v after %d count(s); want empty and no tokenizer call", got, err, c.calls)
	}
}

func TestFitTitle_LongTitleIsCutToTheBudgetAsAPrefix(t *testing.T) {
	long := strings.Repeat("palavra ", 500)
	got, err := fitTitle(t.Context(), chunk.FakeTokenCounter{}, long, 96)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(strings.Fields(got)); n != 96 {
		t.Errorf("cut title has %d tokens, want exactly the budget 96 (the longest prefix that fits)", n)
	}
	if !strings.HasPrefix(long, got) {
		t.Errorf("cut title %q is not a prefix of the original", got)
	}
}

func TestFitTitle_CutsOnRuneBoundariesAndBinarySearches(t *testing.T) {
	c := &runeCounter{}
	long := strings.Repeat("ação ", 4000) // multi-byte runes, ~5000 tokens at 4 runes each
	got, err := fitTitle(t.Context(), c, long, 96)
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("cut title is not valid UTF-8: %q", got)
	}
	if n, _ := c.CountTokens(t.Context(), got); n > 96 {
		t.Errorf("cut title counts %d tokens, over the budget 96", n)
	}
	if c.calls > 40 {
		t.Errorf("%d tokenizer calls for one title; the search must be logarithmic, not linear", c.calls)
	}
}

func TestProcessNote_LongTitleNeverPushesAFullWindowChunkPastTheModelWindow(t *testing.T) {
	eg2, ok := schema.ProfileByID("embeddinggemma-2")
	if !ok {
		t.Fatal("embeddinggemma-2 profile missing")
	}
	spy := &titledSpy{}
	deps := testDeps(newFakeStore(&journal{}), spy)
	deps.Handshake.PromptVersion = eg2.PromptVersion
	deps.TitleMaxTokens = eg2.TitleMaxTokens()
	deps.Chunk = chunk.ConfigFor(eg2, 1, 1000)

	body := strings.TrimSpace(strings.Repeat("palavra ", 8000)) // one oversize chunk, near the window
	n := noteWithBodies(t, "research/curadoria/longa.md", body)
	n.Title = strings.Repeat("titulo ", 2000)

	if res := ProcessNote(t.Context(), deps, n); res.State != StatePruned {
		t.Fatalf("State = %s (%v)", res.State, res.Err)
	}
	sent := spy.titled[0][0]
	titleTokens, textTokens := len(strings.Fields(sent.Title)), len(strings.Fields(sent.Text))
	if titleTokens > eg2.TitleMaxTokens() {
		t.Errorf("title sent has %d tokens, over TitleMaxTokens %d", titleTokens, eg2.TitleMaxTokens())
	}
	if textTokens < 7000 {
		t.Fatalf("chunk text has only %d tokens: the fixture no longer exercises a near-window chunk", textTokens)
	}
	if total := titleTokens + textTokens + 32; total > eg2.MaxInputTokens { // 32 = prefix framing reserve
		t.Errorf("title %d + chunk %d + framing = %d tokens, over the model window %d", titleTokens, textTokens, total, eg2.MaxInputTokens)
	}
	if !strings.HasSuffix(strings.TrimSpace(sent.Text), body) { // the chunk is "breadcrumb + section"; the section must be whole
		t.Errorf("the chunk text was altered: %d tokens sent; head %q tail %q", textTokens, sent.Text[:60], sent.Text[len(sent.Text)-30:])
	}
}

func TestProcessNote_TitleCutOnlyAffectsWhatTheEmbedderGets_NotTheHash(t *testing.T) {
	// The payload title (and so point_hash) keeps the whole title: the cut is a property of the
	// embedding request, so a note whose title is long is still found "integral" on the next run.
	spy := &titledSpy{}
	store := newFakeStore(&journal{})
	deps := testDeps(store, spy)
	deps.Handshake.PromptVersion = "gemma-search-v1"
	deps.TitleMaxTokens = 3
	n := testNote(t, "research/curadoria/titulo.md", 1)
	n.Title = "um dois tres quatro cinco seis"

	if res := ProcessNote(t.Context(), deps, n); res.State != StatePruned {
		t.Fatalf("first run: %s (%v)", res.State, res.Err)
	}
	if got := spy.titled[0][0].Title; got != "um dois tres" {
		t.Errorf("title sent = %q, want %q", got, "um dois tres")
	}
	if res := ProcessNote(t.Context(), deps, n); res.State != StateSkipped {
		t.Errorf("second run: %s, want skipped (the cut must not change the hash)", res.State)
	}
}

func TestProcessNote_NoPromptScheme_NeverCountsOrCutsTheTitle(t *testing.T) {
	// BGE-M3 stays byte-identical: its title is not sent, so it is neither counted nor cut.
	titleSeen := false
	counter := titleWatcher{title: "titulo unico", seen: &titleSeen}
	deps := testDeps(newFakeStore(&journal{}), &titledSpy{})
	deps.Tokens = counter
	n := testNote(t, "research/curadoria/bge.md", 1)
	n.Title = "titulo unico"
	if res := ProcessNote(t.Context(), deps, n); res.State != StatePruned {
		t.Fatalf("State = %s (%v)", res.State, res.Err)
	}
	if titleSeen {
		t.Error("the title reached the token counter on a profile with no prompt scheme")
	}
}

type titleWatcher struct {
	title string
	seen  *bool
}

func (w titleWatcher) CountTokens(ctx context.Context, text string) (int, error) {
	if strings.Contains(text, w.title) {
		*w.seen = true
	}
	return chunk.FakeTokenCounter{}.CountTokens(ctx, text)
}

func TestDepsValidate_PromptSchemeNeedsATitleBound(t *testing.T) {
	deps := testDeps(newFakeStore(&journal{}), &titledSpy{})
	deps.Handshake.PromptVersion = "gemma-search-v1"
	if err := deps.Validate(); err == nil || !strings.Contains(err.Error(), "TitleMaxTokens") {
		t.Fatalf("Validate = %v, want a refusal naming TitleMaxTokens", err)
	}
	deps.TitleMaxTokens = 96
	if err := deps.Validate(); err != nil {
		t.Fatalf("Validate with a bound = %v", err)
	}
}
