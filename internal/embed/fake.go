package embed

import (
	"context"
	"hash/fnv"
	"math"
	"math/rand/v2"

	"github.com/danielmalka/go-knowrag/internal/schema"
)

// FakeModelID is what FakeEmbedder reports. It is deliberately not schema.DefaultProfile().Revision: a fake
// that claims the pinned revision would let a test write a point_hash indistinguishable from one
// produced by the real model, which is the one thing a fake must never do.
const FakeModelID = "fake-embedder-v1"

// FakeEmbedder is a deterministic, offline Embedder: the same text always produces the same
// vectors, in the same process and across processes, because everything is derived from a hash of
// the text rather than from a seeded global.
//
// It exists so S06a's and S07's suites run with no GPU, no network and no sidecar. Its output is
// meaningless as semantics — nearby texts are not nearby vectors — but it is structurally
// indistinguishable from a real response: 1024 finite dense elements, L2-normalized as the model
// card pins (ADR-001 §4), and a strictly-ascending, non-zero sparse half. That is not decoration:
// a fake whose output validateEmbedding would reject is a fixture that hides bugs in every suite
// depending on it, which is what TestFakeEmbedder_OutputPassesValidation pins.
//
// The zero value is ready to use and fakes the default model profile. Profile fakes another one: a
// dense-only profile gets a vector of its own width and no sparse half, so suites for the
// EmbeddingGemma path run offline too (NewFakeEmbedderFor).
type FakeEmbedder struct {
	Profile *schema.ModelProfile
}

// NewFakeEmbedderFor returns a fake for one model profile, refusing a half-built profile for the
// same reason NewServiceEmbedderFor does.
func NewFakeEmbedderFor(m schema.ModelProfile) (FakeEmbedder, error) {
	if err := m.Validate(); err != nil {
		return FakeEmbedder{}, err
	}
	return FakeEmbedder{Profile: &m}, nil
}

var _ TitledEmbedder = FakeEmbedder{}

// EmbedTitledDocuments embeds the texts and ignores the titles: the fake's vectors are a function of
// the text alone, so a suite that wants to see the titles uses a spy around it.
func (f FakeEmbedder) EmbedTitledDocuments(ctx context.Context, docs []Document) ([]Embedding, error) {
	texts := make([]string, len(docs))
	for i, d := range docs {
		texts[i] = d.Text
	}
	return f.EmbedDocuments(ctx, texts)
}

func (f FakeEmbedder) model() schema.ModelProfile {
	if f.Profile == nil {
		return schema.DefaultProfile()
	}
	return *f.Profile
}

var _ Embedder = FakeEmbedder{}

func (FakeEmbedder) ModelID() string { return FakeModelID }

func (f FakeEmbedder) EmbedQuery(ctx context.Context, text string) (Embedding, error) {
	if err := ctx.Err(); err != nil {
		return Embedding{}, err
	}
	return fakeEmbedding(text, f.model()), nil
}

// EmbedDocuments embeds each text the same way EmbedQuery does. BGE-M3 has no separate query
// encoder, so a fake that treated the two paths differently would make S07's retrieval tests pass
// or fail for reasons unrelated to retrieval.
func (f FakeEmbedder) EmbedDocuments(ctx context.Context, texts []string) ([]Embedding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]Embedding, len(texts))
	for i, t := range texts {
		out[i] = fakeEmbedding(t, f.model())
	}
	return out, nil
}

func fakeEmbedding(text string, m schema.ModelProfile) Embedding {
	h := fnv.New64a()
	_, _ = h.Write([]byte(text)) // hash.Hash.Write never returns an error
	seed := h.Sum64()
	// #nosec G404 -- deterministic pseudo-randomness is the requirement, not a weakness: the whole
	// point is that the same text yields the same vector on every run and every machine. No secret
	// or identifier is derived from this stream.
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))

	dense := make([]float32, m.DenseWidth())
	var norm float64
	for i := range dense {
		v := rng.Float64()*2 - 1
		dense[i] = float32(v)
		norm += v * v
	}
	// L2-normalize, matching the pinned Normalization (ADR-001 §4; the measured dense norm is 1.0).
	norm = math.Sqrt(norm)
	if norm > 0 {
		for i := range dense {
			dense[i] = float32(float64(dense[i]) / norm)
		}
	}

	if !m.HasSparse() {
		return Embedding{Dense: dense}
	}

	// 8–15 strictly ascending token indices with non-zero weights: enough to exercise fusion, few
	// enough to read in a failure message.
	n := 8 + rng.IntN(8)
	sp := Sparse{Indices: make([]uint32, n), Values: make([]float32, n)}
	var idx uint32
	for i := range n {
		idx += 1 + rng.Uint32N(500)
		sp.Indices[i] = idx
		// Bounded away from zero so the fake can never trip ErrSparseZeroWeight by chance.
		sp.Values[i] = float32(0.01 + rng.Float64()*0.99)
	}
	return Embedding{Dense: dense, Sparse: sp}
}
