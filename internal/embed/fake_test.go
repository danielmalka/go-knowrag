package embed

import (
	"context"
	"reflect"
	"testing"
)

// TestFakeEmbedder_ImplementsEmbedder is a compile-time assertion made visible as a test, so a
// signature drift on the PRD-fixed interface (S04 T1) fails a run rather than only a build of
// whichever package happened to use it.
func TestFakeEmbedder_ImplementsEmbedder(t *testing.T) {
	var _ Embedder = (*FakeEmbedder)(nil)
	var _ Embedder = FakeEmbedder{}
}

func TestFakeEmbedder_EmbedDocuments_SameTextSameVector(t *testing.T) {
	f := FakeEmbedder{}
	ctx := context.Background()

	first, err := f.EmbedDocuments(ctx, []string{"hello", "world"})
	if err != nil {
		t.Fatalf("first EmbedDocuments: %v", err)
	}
	second, err := f.EmbedDocuments(ctx, []string{"hello", "world"})
	if err != nil {
		t.Fatalf("second EmbedDocuments: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("same texts produced different embeddings across two calls")
	}
	if reflect.DeepEqual(first[0], first[1]) {
		t.Fatal("different texts produced identical embeddings")
	}
}

func TestFakeEmbedder_EmbedQuery_IsDeterministicAndMatchesDocuments(t *testing.T) {
	f := FakeEmbedder{}
	ctx := context.Background()

	q1, err := f.EmbedQuery(ctx, "hello")
	if err != nil {
		t.Fatalf("first EmbedQuery: %v", err)
	}
	q2, err := f.EmbedQuery(ctx, "hello")
	if err != nil {
		t.Fatalf("second EmbedQuery: %v", err)
	}
	if !reflect.DeepEqual(q1, q2) {
		t.Fatal("EmbedQuery is not deterministic for the same text")
	}

	// A fake that embedded queries and documents differently would make every retrieval test in
	// S07 pass or fail for reasons that have nothing to do with retrieval.
	docs, err := f.EmbedDocuments(ctx, []string{"hello"})
	if err != nil {
		t.Fatalf("EmbedDocuments: %v", err)
	}
	if !reflect.DeepEqual(q1, docs[0]) {
		t.Fatal("EmbedQuery and EmbedDocuments disagree on the same text")
	}
}

// TestFakeEmbedder_OutputPassesValidation is what makes the fake usable as a stand-in: an
// embedding the real boundary check would reject is not a fake of this system's embedder, it is a
// fixture that hides bugs in every suite that depends on it.
func TestFakeEmbedder_OutputPassesValidation(t *testing.T) {
	f := FakeEmbedder{}
	texts := []string{"", "a", "como configurar o cron do n8n", "hello world", "ünïcödé ✅"}

	got, err := f.EmbedDocuments(context.Background(), texts)
	if err != nil {
		t.Fatalf("EmbedDocuments: %v", err)
	}
	if len(got) != len(texts) {
		t.Fatalf("got %d embeddings for %d texts", len(got), len(texts))
	}
	for i, e := range got {
		if err := validateEmbedding(e); err != nil {
			t.Errorf("text %d (%q): fake output fails validateEmbedding: %v", i, texts[i], err)
		}
	}
}

func TestFakeEmbedder_ModelID_IsNonEmptyConstant(t *testing.T) {
	f := FakeEmbedder{}
	if f.ModelID() == "" {
		t.Fatal("ModelID() is empty")
	}
	if f.ModelID() != (FakeEmbedder{}).ModelID() {
		t.Fatal("ModelID() is not constant")
	}
}

func TestFakeEmbedder_RespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	f := FakeEmbedder{}
	if _, err := f.EmbedDocuments(ctx, []string{"hello"}); err == nil {
		t.Error("EmbedDocuments on a cancelled context returned no error")
	}
	if _, err := f.EmbedQuery(ctx, "hello"); err == nil {
		t.Error("EmbedQuery on a cancelled context returned no error")
	}
}
