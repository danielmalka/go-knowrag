package eval

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/retrieval"
)

// retrievalQuery is the shape RunGolden builds, spelled once so the corpus tests and the hermetic
// tests ask the same way the harness does.
func retrievalQuery(text, tenant string) retrieval.Query {
	return retrieval.Query{Collection: "hermetic", TenantID: tenant, Text: text, TopK: DefaultK}
}

const corpusYAML = `
chunks:
  - uid: "11111111-1111-4111-8111-111111111111"
    chunk_index: 0
    tenant_id: tenant-one
    area: alfa
    path: alfa/one.md
    text: "the beacon rotation replaces the signing key"
  - uid: "22222222-2222-4222-8222-222222222222"
    chunk_index: 0
    tenant_id: tenant-one
    area: beta
    path: beta/two.md
    text: "the harbour ledger records every crossing"
  - uid: "33333333-3333-4333-8333-333333333333"
    chunk_index: 0
    tenant_id: tenant-two
    area: alfa
    path: alfa/three.md
    text: "the beacon rotation replaces the signing key exactly"
`

func writeCorpus(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "corpus.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}
	return path
}

func loadCorpusFixture(t *testing.T, body string) *CorpusSearcher {
	t.Helper()
	c, err := LoadCorpus(writeCorpus(t, body))
	if err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}
	return NewCorpusSearcher(c)
}

func TestLoadCorpus_ParsesChunks(t *testing.T) {
	c, err := LoadCorpus(writeCorpus(t, corpusYAML))
	if err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}
	if len(c.Chunks) != 3 {
		t.Fatalf("%d chunk(s), want 3", len(c.Chunks))
	}
	first := c.Chunks[0]
	if first.UID != uidA || first.TenantID != "tenant-one" || first.Area != "alfa" || first.Path != "alfa/one.md" {
		t.Errorf("the first chunk did not parse: %+v", first)
	}
}

func TestLoadCorpus_RejectsWhatCannotBeSearched(t *testing.T) {
	cases := map[string]struct {
		body string
		want []string
	}{
		"no chunks":      {"chunks: []\n", []string{"no chunks", "not a measurement"}},
		"missing tenant": {strings.Replace(corpusYAML, "    tenant_id: tenant-one\n", "", 1), []string{"chunk 1", "tenant_id"}},
		"missing uid":    {strings.Replace(corpusYAML, `  - uid: "11111111-1111-4111-8111-111111111111"`, `  - uid: ""`, 1), []string{"chunk 1", "uid"}},
		"missing text":   {strings.Replace(corpusYAML, `    text: "the harbour ledger records every crossing"`, `    text: ""`, 1), []string{"chunk 2", "text"}},
		"negative index": {strings.Replace(corpusYAML, "    chunk_index: 0\n    tenant_id: tenant-two", "    chunk_index: -1\n    tenant_id: tenant-two", 1), []string{"chunk 3", "-1"}},
		"unknown field":  {strings.Replace(corpusYAML, "    area: alfa\n    path: alfa/one.md", "    are: alfa\n    area: alfa\n    path: alfa/one.md", 1), []string{"are"}},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadCorpus(writeCorpus(t, tc.body)); err != nil {
				for _, want := range tc.want {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("the error %q does not mention %q", err, want)
					}
				}
				return
			}
			t.Fatalf("%s loaded without error", name)
		})
	}
}

func TestLoadCorpus_MissingFileIsAnError(t *testing.T) {
	_, err := LoadCorpus(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("a missing corpus loaded")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the error %v does not unwrap to fs.ErrNotExist", err)
	}
}

// TestCorpusSearcher_ScopesToTheTenant is the invariant internal/retrieval exists to hold, applied
// to the implementation that bypasses it. Chunk three is the better lexical match for this query
// and belongs to another tenant; a searcher that ignored the scope would rank it first.
func TestCorpusSearcher_ScopesToTheTenant(t *testing.T) {
	s := loadCorpusFixture(t, corpusYAML)

	hits, err := s.Search(t.Context(), retrievalQuery("beacon rotation signing key", "tenant-one"))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, hit := range hits {
		if hit.UID == uidC {
			t.Errorf("a tenant-one search returned %s, which belongs to tenant-two", hit.UID)
		}
	}
	if len(hits) == 0 || hits[0].UID != uidA {
		t.Errorf("the best tenant-one match is %v, want %s", joinUIDs(hits), uidA)
	}

	// And the other tenant does see its own chunk, so the assertion above is about the filter and
	// not about a corpus that simply never returns chunk three.
	other, err := s.Search(t.Context(), retrievalQuery("beacon rotation signing key", "tenant-two"))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(other) != 1 || other[0].UID != uidC {
		t.Errorf("tenant-two got %v, want only %s", joinUIDs(other), uidC)
	}
}

// TestCorpusSearcher_RefusesAQueryProductionWouldRefuse keeps the hermetic path from being more
// permissive than the real one: a gate that passes on a Query internal/retrieval rejects is a gate
// that proved nothing about the command an operator will actually run.
func TestCorpusSearcher_RefusesAQueryProductionWouldRefuse(t *testing.T) {
	s := loadCorpusFixture(t, corpusYAML)

	cases := map[string]struct {
		mutate func(*retrieval.Query)
		want   error
	}{
		"no tenant":     {func(q *retrieval.Query) { q.TenantID = "" }, retrieval.ErrEmptyTenant},
		"no text":       {func(q *retrieval.Query) { q.Text = "  " }, retrieval.ErrEmptyQuery},
		"zero top_k":    {func(q *retrieval.Query) { q.TopK = 0 }, retrieval.ErrInvalidTopK},
		"no collection": {func(q *retrieval.Query) { q.Collection = "" }, retrieval.ErrEmptyCollection},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			q := retrievalQuery("beacon", "tenant-one")
			tc.mutate(&q)
			_, err := s.Search(context.Background(), q)
			if !errors.Is(err, tc.want) {
				t.Errorf("Search returned %v, want %v", err, tc.want)
			}
		})
	}
}

// TestCorpusSearcher_HonoursTopKAndDropsNonMatches covers the two things that decide what the
// runner is handed at all: nothing scoring zero comes back, and no more than TopK does.
func TestCorpusSearcher_HonoursTopKAndDropsNonMatches(t *testing.T) {
	s := loadCorpusFixture(t, corpusYAML)

	none, err := s.Search(t.Context(), retrievalQuery("zeppelin quarry pennant", "tenant-one"))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("a query sharing no term with any chunk returned %v", joinUIDs(none))
	}

	// Both chunks match this query, chunk one by more terms. Asserting *which* one survives the cut
	// is the point: a count-only assertion stays green with the score comparator reversed, which is
	// how a plant on it went unnoticed.
	q := retrievalQuery("the beacon rotation replaces the signing key and the harbour ledger", "tenant-one")
	all, err := s.Search(t.Context(), q)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("%d result(s) matched, want both chunks: %s", len(all), joinUIDs(all))
	}
	if all[0].UID != uidA || all[0].Score <= all[1].Score {
		t.Fatalf("results came back %s with scores %v/%v; the better match is %s and it has to be "+
			"first", joinUIDs(all), all[0].Score, all[1].Score, uidA)
	}

	q.TopK = 1
	one, err := s.Search(t.Context(), q)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(one) != 1 {
		t.Fatalf("TopK=1 returned %d result(s)", len(one))
	}
	if one[0].UID != uidA {
		t.Errorf("TopK=1 kept %s; truncation has to keep the best-scoring chunk, %s", one[0].UID, uidA)
	}
}

// TestCorpusSearcher_TiesOrderByUIDThenChunk pins the order two equally-scored chunks come back in.
//
// TestCorpusSearcher_IsDeterministicAcrossRuns cannot do this: it compares runs against each other,
// so a consistently *wrong* order satisfies it. Reversing the tie-break comparator stayed green
// until this existed.
func TestCorpusSearcher_TiesOrderByUIDThenChunk(t *testing.T) {
	const tied = `
chunks:
  - uid: "22222222-2222-4222-8222-222222222222"
    chunk_index: 1
    tenant_id: tenant-one
    area: alfa
    path: a.md
    text: "beacon"
  - uid: "22222222-2222-4222-8222-222222222222"
    chunk_index: 0
    tenant_id: tenant-one
    area: alfa
    path: a.md
    text: "beacon"
  - uid: "11111111-1111-4111-8111-111111111111"
    chunk_index: 0
    tenant_id: tenant-one
    area: alfa
    path: b.md
    text: "beacon"
`
	hits, err := loadCorpusFixture(t, tied).Search(t.Context(), retrievalQuery("beacon", "tenant-one"))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	// All three score 1. Ascending uid first, then ascending chunk index — and the corpus lists them
	// in exactly the opposite order, so arrival order cannot produce this by accident.
	want := uidA + "#0, " + uidB + "#0, " + uidB + "#1"
	if got := joinUIDs(hits); got != want {
		t.Errorf("tied results came back as\n  %s\nwant\n  %s", got, want)
	}
}

// TestCorpusSearcher_MatchesRegardlessOfCase is why tokenSet lowercases. Real questions are typed in
// prose and real notes begin sentences with a capital; a searcher that matched case-sensitively
// would miss the first word of every sentence, and the fixture's own recall would be a measurement
// of capitalisation.
func TestCorpusSearcher_MatchesRegardlessOfCase(t *testing.T) {
	const cased = `
chunks:
  - uid: "11111111-1111-4111-8111-111111111111"
    chunk_index: 0
    tenant_id: tenant-one
    area: alfa
    path: a.md
    text: "Rotation Of The Beacon"
`
	s := loadCorpusFixture(t, cased)
	for _, text := range []string{"rotation of the beacon", "ROTATION OF THE BEACON", "Rotation of the Beacon"} {
		hits, err := s.Search(t.Context(), retrievalQuery(text, "tenant-one"))
		if err != nil {
			t.Fatalf("Search(%q): %v", text, err)
		}
		if len(hits) != 1 {
			t.Errorf("Search(%q) matched %d chunk(s), want the one that differs only in case", text, len(hits))
			continue
		}
		if hits[0].Score != 4 {
			t.Errorf("Search(%q) scored %v, want 4 — every term should have matched", text, hits[0].Score)
		}
	}
}

// TestCorpusSearcher_IsDeterministicAcrossRuns is the property the CI gate rests on. tokenSet builds
// maps, and map iteration order in Go is randomised per run — an ordering that leaked that would
// make the gate flap.
func TestCorpusSearcher_IsDeterministicAcrossRuns(t *testing.T) {
	s := loadCorpusFixture(t, corpusYAML)
	q := retrievalQuery("the beacon rotation replaces the harbour ledger crossing key", "tenant-one")

	first, err := s.Search(t.Context(), q)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for i := range 20 {
		again, aerr := s.Search(t.Context(), q)
		if aerr != nil {
			t.Fatalf("Search: %v", aerr)
		}
		if joinUIDs(again) != joinUIDs(first) {
			t.Fatalf("run %d returned a different order:\n  %s\n  %s", i+2, joinUIDs(again), joinUIDs(first))
		}
	}
}

// TestCorpusSearcher_MarksEveryResultUntrusted mirrors internal/retrieval's formatResults: the whole
// corpus is text somebody wrote, and S08's framing must never be skipped for a result that came from
// the hermetic path instead of the real one.
func TestCorpusSearcher_MarksEveryResultUntrusted(t *testing.T) {
	s := loadCorpusFixture(t, corpusYAML)
	hits, err := s.Search(t.Context(), retrievalQuery("beacon rotation", "tenant-one"))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("no hits, so this test asserts nothing")
	}
	for _, hit := range hits {
		if !hit.Untrusted {
			t.Errorf("result %s came back trusted", hit.UID)
		}
	}
}
