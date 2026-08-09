package embed

import "context"

// DenseDim is the BGE-M3 dense vector width (PRD-contrato §2.3b, confirmed in ADR-001 §6.2 against
// the pinned revision's config.json). It is an invariant, not a setting: a backend returning any
// other width is a backend serving a different model.
const DenseDim = 1024

// Sparse is the lexical half of a BGE-M3 embedding, in the shape PRD-contrato §2.3b fixes:
// the model's `token_id` becomes Indices (uint32) and its `lexical_weight` becomes Values
// (float32), positionally paired.
//
// A valid Sparse has strictly ascending Indices and no zero Value. Note the phrasing: that
// describes what a *valid* vector looks like, not a repair this package performs. Duplicates and
// zeros arriving from a backend are defects (validateEmbedding rejects them) — see validate.go.
type Sparse struct {
	Indices []uint32
	Values  []float32
}

// Embedding is one text's BGE-M3 output: both halves from the same forward pass, which is why they
// travel together and never separately.
type Embedding struct {
	Dense  []float32
	Sparse Sparse
}

// Embedder is the S04 contract (PRD-stories-pipeline §3, story S04), used unchanged by both the
// batch ingestion path (S06a) and the query path (S07/S08). The signature is fixed by the PRD:
// three methods, exactly these names and types.
//
// ModelID reports the model revision the Go side believes it is talking to — it is self-attested.
// What the backend actually loaded is confirmed separately by ServiceEmbedder.Handshake, and it is
// the handshake's answer, not this one, that S06a hashes into point_hash (PRD-contrato §2.4).
// Handshake is deliberately not part of this interface: FakeEmbedder has no backend to diverge
// from, so a handshake on it would be a no-op that proves nothing.
type Embedder interface {
	EmbedDocuments(ctx context.Context, texts []string) ([]Embedding, error)
	EmbedQuery(ctx context.Context, text string) (Embedding, error)
	ModelID() string
}
