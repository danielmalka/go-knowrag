package embed

import (
	"context"

	"github.com/danielmalka/go-knowrag/internal/schema"
)

// DefaultDenseDim is the dense vector width of the default model profile (schema.DefaultProfile). The
// width is a property of the model, not a setting: a backend returning any other width is a backend
// serving a different model. An embedder built for another profile (NewServiceEmbedderFor) validates
// against that profile's width; this is only what fixtures that use the default want.
func DefaultDenseDim() int { return schema.DefaultProfile().DenseWidth() }

// Embedding is one text's dense vector, L2-normalized by the model's own Normalize module.
type Embedding struct {
	Dense []float32
}

// Document is one text to embed together with the title of the note it came from. EmbeddingGemma's
// document prefix is "title: <title> | text: <text>", so for a profile that pins a prompt scheme the
// service needs the title as well as the text; an empty Title means "none" on the service side.
type Document struct {
	Title string
	Text  string
}

// TitledEmbedder is the optional capability of an Embedder that can pass the note title to a service
// that builds a titled document prefix. It is a separate interface rather than a change to Embedder
// so a profile without a prefix and every fake keep the contract they have: ingest uses it
// only when the profile asks for a title (schema.ModelProfile.NeedsTitle).
type TitledEmbedder interface {
	EmbedTitledDocuments(ctx context.Context, docs []Document) ([]Embedding, error)
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
