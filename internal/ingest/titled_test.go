package ingest

import (
	"context"
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/embed"
)

// PR 4 of the embedding migration: when the backend reports a task-prefix scheme (EmbeddingGemma's
// "title: <title> | text: <text>"), the note title goes to the service with every chunk. Without a
// scheme (BGE-M3) the embedder is called exactly as before.

// titledSpy records which entry point ingest used and what the titled one was given.
type titledSpy struct {
	inner  embed.FakeEmbedder
	plain  int
	titled [][]embed.Document
}

func (s *titledSpy) ModelID() string { return s.inner.ModelID() }
func (s *titledSpy) EmbedQuery(ctx context.Context, text string) (embed.Embedding, error) {
	return s.inner.EmbedQuery(ctx, text)
}
func (s *titledSpy) EmbedDocuments(ctx context.Context, texts []string) ([]embed.Embedding, error) {
	s.plain++
	return s.inner.EmbedDocuments(ctx, texts)
}
func (s *titledSpy) EmbedTitledDocuments(ctx context.Context, docs []embed.Document) ([]embed.Embedding, error) {
	s.titled = append(s.titled, append([]embed.Document(nil), docs...))
	return s.inner.EmbedTitledDocuments(ctx, docs)
}

func TestProcessNote_PromptScheme_SendsTheNoteTitleWithEveryChunk(t *testing.T) {
	spy := &titledSpy{}
	store := newFakeStore(&journal{})
	deps := testDeps(store, spy)
	deps.Handshake.PromptVersion = "gemma-search-v1"
	deps.TitleMaxTokens = 96
	n := testNote(t, "research/curadoria/nota.md", 3)

	res := ProcessNote(t.Context(), deps, n)
	if res.State != StatePruned {
		t.Fatalf("State = %s (%v)", res.State, res.Err)
	}
	if spy.plain != 0 || len(spy.titled) != 1 || len(spy.titled[0]) != 3 {
		t.Fatalf("plain=%d titled calls=%d: want the titled entry point once, with all 3 chunks", spy.plain, len(spy.titled))
	}
	for i, d := range spy.titled[0] {
		if d.Title != n.Title || n.Title == "" {
			t.Errorf("chunk %d title = %q, want the note title %q", i, d.Title, n.Title)
		}
		if strings.TrimSpace(d.Text) == "" {
			t.Errorf("chunk %d text is empty", i)
		}
	}
}

func TestProcessNote_NoPromptScheme_UsesThePlainEntryPoint(t *testing.T) {
	spy := &titledSpy{}
	deps := testDeps(newFakeStore(&journal{}), spy)
	if deps.Handshake.PromptVersion != "" {
		t.Fatal("the default test handshake must be the BGE-M3 one (no prompt scheme)")
	}
	if res := ProcessNote(t.Context(), deps, testNote(t, "research/curadoria/nota.md", 2)); res.State != StatePruned {
		t.Fatalf("State = %s (%v)", res.State, res.Err)
	}
	if spy.plain != 1 || len(spy.titled) != 0 {
		t.Errorf("plain=%d titled=%d, want 1 and 0 — BGE-M3 ingestion must not change", spy.plain, len(spy.titled))
	}
}

// An embedder that cannot pass titles is refused when a scheme needs them, before anything is written:
// the alternative is an entire index embedded under "title: none".
func TestProcessNote_PromptScheme_EmbedderWithoutTitlesIsRefused(t *testing.T) {
	store := newFakeStore(&journal{})
	deps := testDeps(store, newSpyEmbedder(&journal{})) // spyEmbedder is not a TitledEmbedder
	deps.Handshake.PromptVersion = "gemma-search-v1"
	deps.TitleMaxTokens = 96

	res := ProcessNote(t.Context(), deps, testNote(t, "research/curadoria/nota.md", 2))
	if res.State == StatePruned || res.Err == nil || !strings.Contains(res.Err.Error(), "cannot pass titles") {
		t.Fatalf("State = %s, Err = %v; want a failure saying the embedder cannot pass titles", res.State, res.Err)
	}
	if len(store.upserts) != 0 {
		t.Errorf("%d upsert(s) happened before the refusal", len(store.upserts))
	}
}

// A different prompt scheme re-embeds an already-indexed note (the hash changed), a same one does not.
func TestProcessNote_PromptVersionChange_Reembeds(t *testing.T) {
	spy := &titledSpy{}
	store := newFakeStore(&journal{})
	deps := testDeps(store, spy)
	deps.Handshake.PromptVersion = "gemma-search-v1"
	deps.TitleMaxTokens = 96
	n := testNote(t, "research/curadoria/nota.md", 2)
	if res := ProcessNote(t.Context(), deps, n); res.State != StatePruned {
		t.Fatalf("first run: %s (%v)", res.State, res.Err)
	}
	if res := ProcessNote(t.Context(), deps, n); res.State != StateSkipped {
		t.Fatalf("second run with the same scheme: %s, want %s", res.State, StateSkipped)
	}
	deps.Handshake.PromptVersion = "gemma-search-v2"
	deps.TitleMaxTokens = 96
	if res := ProcessNote(t.Context(), deps, n); res.State != StatePruned {
		t.Fatalf("a new scheme did not re-embed: %s (%v)", res.State, res.Err)
	}
}
