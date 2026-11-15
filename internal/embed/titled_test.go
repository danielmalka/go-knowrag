package embed

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/schema"
)

// PR 4 of the embedding migration: a profile with a document prefix ("title: <title> | text: <text>")
// needs the note title to reach the service, per text, in order. The BGE-M3 request must stay what
// it always was.

func TestHTTPTransport_EmbedTitled_SendsOneTitlePerInput(t *testing.T) {
	var body map[string]json.RawMessage
	tr := newTestTransport(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		_, _ = io.WriteString(w, embedBody(2))
	})
	if _, err := tr.EmbedTitled(context.Background(), []string{"a", "b"}, []string{"Titulo A", ""}, KindPassage); err != nil {
		t.Fatalf("EmbedTitled: %v", err)
	}
	var titles []string
	if err := json.Unmarshal(body["titles"], &titles); err != nil || !reflect.DeepEqual(titles, []string{"Titulo A", ""}) {
		t.Errorf("titles = %s (%v), want [\"Titulo A\", \"\"] — an empty title stays an entry so the positions line up", body["titles"], err)
	}
}

// Without titles the body has no `titles` key at all: the BGE-M3 service sees the request it always saw.
func TestHTTPTransport_Embed_PlainRequestHasNoTitlesKey(t *testing.T) {
	var raw []byte
	tr := newTestTransport(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, embedBody(1))
	})
	if _, err := tr.Embed(context.Background(), []string{"a"}, KindPassage); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "titles") {
		t.Errorf("the plain request carries a titles key: %s", raw)
	}
	if string(raw) != `{"inputs":["a"],"kind":"passage"}` {
		t.Errorf("the plain request changed: %s", raw)
	}
}

func TestHTTPTransport_EmbedTitled_RefusesMismatchedLengths(t *testing.T) {
	tr := newTestTransport(t, func(w http.ResponseWriter, _ *http.Request) { t.Error("the server was called") })
	if _, err := tr.EmbedTitled(context.Background(), []string{"a", "b"}, []string{"only one"}, KindPassage); err == nil {
		t.Fatal("1 title for 2 texts was sent")
	}
}

// titleSpy is a TitledTransport that records what each sub-batch carried and answers dense-only vectors.
type titleSpy struct {
	stubTransport
	mu     sync.Mutex
	titled [][]string // one entry per call: "title|text" pairs
	plain  int
	dim    int
}

func (s *titleSpy) answer(n int) []Embedding {
	out := make([]Embedding, n)
	for i := range out {
		d := make([]float32, s.dim)
		d[0] = 1
		out[i] = Embedding{Dense: d}
	}
	return out
}

func (s *titleSpy) Embed(_ context.Context, texts []string, _ Kind) ([]Embedding, error) {
	s.mu.Lock()
	s.plain++
	s.mu.Unlock()
	return s.answer(len(texts)), nil
}

func (s *titleSpy) EmbedTitled(_ context.Context, texts, titles []string, _ Kind) ([]Embedding, error) {
	s.mu.Lock()
	pairs := make([]string, len(texts))
	for i := range texts {
		pairs[i] = titles[i] + "|" + texts[i]
	}
	s.titled = append(s.titled, pairs)
	s.mu.Unlock()
	return s.answer(len(texts)), nil
}

func eg2ProfileForTitles(t *testing.T) schema.ModelProfile {
	t.Helper()
	m, ok := schema.ProfileByID("embeddinggemma-2")
	if !ok {
		t.Fatal("embeddinggemma-2 not registered")
	}
	return m
}

// Titles follow their texts through the sub-batching, in order, even when the batch is split.
func TestServiceEmbedder_EmbedTitledDocuments_SplitsTitlesWithTheirTexts(t *testing.T) {
	m := eg2ProfileForTitles(t)
	spy := &titleSpy{dim: 768}
	spy.info = func(context.Context) (BackendHandshakeInfo, error) { return reportFor(m, "bf16"), nil }
	p := testProfile()
	p.BatchSize, p.MaxConcurrent = 2, 1
	e, err := NewServiceEmbedderFor(p, spy, m)
	if err != nil {
		t.Fatal(err)
	}
	docs := []Document{{"T0", "a"}, {"T1", "b"}, {"T2", "c"}, {"", "d"}, {"T4", "e"}}
	got, err := e.EmbedTitledDocuments(context.Background(), docs)
	if err != nil || len(got) != 5 {
		t.Fatalf("EmbedTitledDocuments = %d, %v", len(got), err)
	}
	var all []string
	spy.mu.Lock()
	for _, batch := range spy.titled {
		all = append(all, batch...)
	}
	n := len(spy.titled)
	spy.mu.Unlock()
	if n != 3 {
		t.Errorf("%d titled call(s), want 3 sub-batches of 2,2,1", n)
	}
	// Sub-batches run concurrently, so call order is not the contract; the pairing is: every title must
	// arrive with the text it belongs to.
	slices.Sort(all)
	if want := []string{"T0|a", "T1|b", "T2|c", "T4|e", "|d"}; !reflect.DeepEqual(all, want) {
		t.Errorf("(title|text) pairs seen = %v, want %v", all, want)
	}
	if spy.plain != 0 {
		t.Errorf("%d plain Embed call(s) on the titled path", spy.plain)
	}
}

// A transport that cannot carry titles must fail the call, not drop them and embed under "title: none".
func TestServiceEmbedder_EmbedTitledDocuments_TransportWithoutTitlesFails(t *testing.T) {
	m := eg2ProfileForTitles(t)
	tr := &stubTransport{info: func(context.Context) (BackendHandshakeInfo, error) { return reportFor(m, "bf16"), nil }}
	e, err := NewServiceEmbedderFor(testProfile(), tr, m)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.EmbedTitledDocuments(context.Background(), []Document{{"T", "x"}})
	if err == nil || !strings.Contains(err.Error(), "cannot carry document titles") {
		t.Fatalf("EmbedTitledDocuments = %v, want an error saying the transport cannot carry titles", err)
	}
}

// The untitled path is untouched.
func TestServiceEmbedder_EmbedDocuments_StillPlain(t *testing.T) {
	m := eg2ProfileForTitles(t)
	spy := &titleSpy{dim: 768}
	spy.info = func(context.Context) (BackendHandshakeInfo, error) { return reportFor(m, "bf16"), nil }
	e, _ := NewServiceEmbedderFor(testProfile(), spy, m)
	if _, err := e.EmbedDocuments(context.Background(), []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if spy.plain != 1 || len(spy.titled) != 0 {
		t.Errorf("plain=%d titled=%d, want 1 and 0", spy.plain, len(spy.titled))
	}
}

func TestFakeEmbedder_IsATitledEmbedder(t *testing.T) {
	var _ TitledEmbedder = FakeEmbedder{}
	plain, _ := FakeEmbedder{}.EmbedDocuments(context.Background(), []string{"x"})
	titled, err := FakeEmbedder{}.EmbedTitledDocuments(context.Background(), []Document{{Title: "T", Text: "x"}})
	if err != nil || !reflect.DeepEqual(plain, titled) {
		t.Errorf("the fake's titled vector differs from the plain one: %v", err)
	}
}
