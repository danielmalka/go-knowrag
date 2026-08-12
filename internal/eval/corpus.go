package eval

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"

	"github.com/danielmalka/go-knowrag/internal/retrieval"
)

// A corpus is a search index expressed as a file: chunks with their text, scored by a fixed lexical
// rule instead of by embeddings.
//
// It exists so `cli eval --golden` can run with no Qdrant, no embedding service and no GPU, which is
// what the hermetic CI gate requires (S10 T12/T13). What it measures is the harness — load, run,
// tie-break, aggregate, threshold, exit code — end to end on a number that *emerges* from a search
// rather than one written down next to the questions. It measures nothing about BGE-M3 or about
// Qdrant, and no number produced this way belongs in a baseline: the real gate runs against the real
// index (S10 T15), outside public CI.

// CorpusChunk is one indexed chunk. The fields are the ones retrieval.Result carries, because a
// caller downstream of the Searcher interface must not be able to tell which implementation answered.
type CorpusChunk struct {
	UID        string `yaml:"uid"`
	ChunkIndex int    `yaml:"chunk_index"`
	TenantID   string `yaml:"tenant_id"`
	Area       string `yaml:"area"`
	Path       string `yaml:"path"`
	Text       string `yaml:"text"`
}

// Corpus is the whole file.
type Corpus struct {
	Chunks []CorpusChunk `yaml:"chunks"`
}

// LoadCorpus reads a corpus, strictly: an unknown key is an error naming the key, the same
// KnownFields(true) decoder internal/embed/config.go uses and for the same reason. A misspelled
// `tenant` would otherwise become a chunk belonging to no tenant, which no search would ever return
// and which would read as a retrieval miss.
func LoadCorpus(path string) (Corpus, error) {
	// #nosec G304 -- the path is the corpus the operator named on the command line.
	data, err := os.ReadFile(path)
	if err != nil {
		return Corpus{}, fmt.Errorf("eval: reading the corpus at %s: %w", path, err)
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var c Corpus
	if derr := dec.Decode(&c); derr != nil && !errors.Is(derr, io.EOF) {
		return Corpus{}, fmt.Errorf("eval: parsing the corpus at %s: %w", path, derr)
	}
	if len(c.Chunks) == 0 {
		return Corpus{}, fmt.Errorf("eval: the corpus at %s holds no chunks — every question would "+
			"miss, and a recall of zero measured against an empty index is not a measurement of recall", path)
	}

	var errs []error
	for i, ch := range c.Chunks {
		for _, required := range []struct{ field, value string }{
			{"uid", ch.UID}, {"tenant_id", ch.TenantID}, {"area", ch.Area}, {"text", ch.Text},
		} {
			if required.value == "" {
				errs = append(errs, fmt.Errorf("chunk %d: field %q is missing or empty", i+1, required.field))
			}
		}
		if ch.ChunkIndex < 0 {
			errs = append(errs, fmt.Errorf("chunk %d: chunk_index is %d, which indexes no chunk", i+1, ch.ChunkIndex))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return Corpus{}, fmt.Errorf("eval: the corpus at %s is invalid: %w", path, err)
	}
	return c, nil
}

// CorpusSearcher answers searches out of a Corpus. It satisfies Searcher.
type CorpusSearcher struct {
	chunks []CorpusChunk
}

func NewCorpusSearcher(c Corpus) *CorpusSearcher {
	return &CorpusSearcher{chunks: slices.Clone(c.Chunks)}
}

// Search scores every chunk the query's scope admits and returns the best TopK.
//
// Query.Validate runs first and with the same multiplier the real searcher's DefaultConfig carries,
// so a Query this rejects is a Query internal/retrieval would have rejected too — the alternative is
// a hermetic run that passes on a query shape production refuses.
//
// The tenant condition is applied and is not optional, mirroring the invariant internal/retrieval
// exists to hold (ErrEmptyTenant, retrieval/query.go): a corpus searcher that ignored it would let a
// run with the wrong RunConfig.TenantID score full marks, and the CI gate would be green over a
// scope nobody asked for.
func (s *CorpusSearcher) Search(_ context.Context, q retrieval.Query) ([]retrieval.Result, error) {
	if s == nil {
		return nil, errors.New("eval: CorpusSearcher is nil")
	}
	if err := q.Validate(retrieval.DefaultPrefetchMultiplier); err != nil {
		return nil, err
	}

	wanted := tokenSet(q.Text)
	var hits []retrieval.Result
	for _, ch := range s.chunks {
		if ch.TenantID != q.TenantID {
			continue
		}
		// No facet filtering. Query.Area, Type, Vault and Tags are deliberately not read: RunGolden
		// builds a Query carrying none of them (runner.go), so a facet branch here would be code no
		// caller reaches and no plant can turn red — found exactly that way. The chunks carry `area`
		// because the report groups recall by it, not because this filters on it.
		score := overlap(wanted, tokenSet(ch.Text))
		if score == 0 {
			continue
		}
		hits = append(hits, retrieval.Result{
			UID: ch.UID, ChunkIndex: ch.ChunkIndex, Text: ch.Text, Path: ch.Path,
			Score: float32(score), Untrusted: true,
		})
	}

	// Sorted by (score desc, uid asc, chunk asc) before truncating, so which chunks survive the TopK
	// cut is a function of the corpus and never of map or slice order. RunGolden re-sorts what comes
	// back on its own key (runner.go); this ordering is about which results a caller is handed at
	// all, which no downstream sort can undo.
	slices.SortFunc(hits, func(a, b retrieval.Result) int {
		if a.Score != b.Score {
			return cmpFloat(b.Score, a.Score)
		}
		if a.UID != b.UID {
			return strings.Compare(a.UID, b.UID)
		}
		return a.ChunkIndex - b.ChunkIndex
	})
	return hits[:min(len(hits), q.TopK)], nil
}

func cmpFloat(a, b float32) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// overlap is the score: how many distinct query terms the chunk contains.
//
// Deliberately an integer count and not a length-normalised weight. Ties are the point — a whole-
// number score makes them common, so the fixture exercises the runner's point-ID tie-break instead
// of hiding it behind float noise, and the number a run produces is one a reader can recompute by
// hand from the corpus.
func overlap(query, chunk map[string]bool) int {
	n := 0
	for term := range query {
		if chunk[term] {
			n++
		}
	}
	return n
}

// tokenSet lowercases and splits on anything that is not a letter or a digit. No stemming and no
// stop-word list: both are tuning, and a hermetic fixture that needed tuning to hit its number would
// be measuring the tuning.
func tokenSet(text string) map[string]bool {
	out := map[string]bool{}
	for _, field := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		out[field] = true
	}
	return out
}
