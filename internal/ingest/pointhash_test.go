package ingest

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danielmalka/go-knowrag/internal/chunk"
	"github.com/danielmalka/go-knowrag/internal/embed"
	"github.com/danielmalka/go-knowrag/internal/schema"
	"github.com/danielmalka/go-knowrag/internal/vault"
)

// hashInput is every argument ComputePointHash takes, in one value, so a table row can mutate
// exactly one component and leave the rest provably untouched.
type hashInput struct {
	note   vault.Note
	chunk  chunk.Chunk
	tenant string
	pipe   PipelineConfig
	emb    embed.BackendHandshakeInfo
}

func (h hashInput) hash() string {
	return ComputePointHash(h.note, h.chunk, h.tenant, h.pipe, h.emb)
}

// baselineHashInput is a fully-populated input: no field at its zero value, because a component
// mutated away from zero and one mutated away from a real value do not prove the same thing.
func baselineHashInput() hashInput {
	created := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	return hashInput{
		note: vault.Note{
			Vault:      schema.VaultMalkaLife(),
			Path:       "research/curadoria/nota.md",
			UID:        uuid.MustParse("0198a7f2-4b31-7c42-9e15-3d8a92c47b6a"),
			Type:       schema.NoteTypeConcept(),
			Status:     schema.StatusStable(),
			Visibility: schema.VisibilityInternal(),
			Tags:       []string{"golang", "architecture"},
			Created:    created,
			Title:      "Uma nota",
			Lang:       "pt",
			Area:       schema.AreaResearch(),
			Sub:        "curadoria",
			Updated:    created.Add(72 * time.Hour),
			Body:       "## Alpha\n\ncorpo\n",
		},
		chunk: chunk.Chunk{
			Text:       "Título > Alpha\n\n## Alpha\n\ncorpo\n",
			Index:      3,
			Breadcrumb: []string{"Título", "Alpha"},
			Oversize:   false,
		},
		tenant: "interno",
		pipe: PipelineConfig{
			ChunkerVersion: chunk.Version,
			FloorTokens:    256,
			CeilingTokens:  1024,
			MergeRule:      MergeRuleBelowFloor,
		},
		emb: embed.BackendHandshakeInfo{
			ModelRevision:     schema.BGEM3Revision,
			TokenizerRevision: schema.BGEM3Revision,
			Dim:               embed.DenseDim,
			Normalization:     embed.ExpectedNormalization,
			Pooling:           embed.ExpectedPooling,
			Precision:         embed.ExpectedPrecision,
			SparseParams:      map[string]string{"indices": "token_id", "values": "lexical_weight"},
		},
	}
}

// TestComputePointHash_ChangesPerComponent is the per-component proof PRD-contrato §2.4 requires:
// every input the hash claims to cover, mutated alone, moves the digest.
//
// The claim only means something with the inverse next to it, so each row also rebuilds the
// baseline untouched and asserts *that* hashes identically — otherwise a function returning a
// random string would pass every row here.
func TestComputePointHash_ChangesPerComponent(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*hashInput)
	}{
		// What was embedded.
		{"chunk text", func(h *hashInput) { h.chunk.Text += " edited" }},

		// Point identity.
		{"tenant_id", func(h *hashInput) { h.tenant = "clientes" }},
		{"uid", func(h *hashInput) { h.note.UID = uuid.MustParse("0198a7f2-4b31-7c42-9e15-3d8a92c47b6b") }},
		{"chunk_index", func(h *hashInput) { h.chunk.Index = 4 }},

		// The eleven note-metadata fields of vault.NoteMetadataFields.
		{"vault", func(h *hashInput) { h.note.Vault = schema.VaultMalkaWay() }},
		{"path", func(h *hashInput) { h.note.Path = "research/curadoria/outra.md" }},
		{"type", func(h *hashInput) { h.note.Type = schema.NoteTypeReference() }},
		{"status", func(h *hashInput) { h.note.Status = schema.StatusArchived() }},
		{"visibility", func(h *hashInput) { h.note.Visibility = schema.VisibilityPrivate() }},
		{"tags", func(h *hashInput) { h.note.Tags = []string{"golang", "qdrant"} }},
		{"title", func(h *hashInput) { h.note.Title = "Outra nota" }},
		{"lang", func(h *hashInput) { h.note.Lang = "en" }},
		{"created", func(h *hashInput) { h.note.Created = h.note.Created.Add(24 * time.Hour) }},
		{"area", func(h *hashInput) { h.note.Area = schema.AreaPersonal() }},
		{"sub", func(h *hashInput) { h.note.Sub = "outra-sub" }},

		// The two per-chunk payload fields S02's metadata does not carry.
		{"headings", func(h *hashInput) { h.chunk.Breadcrumb = []string{"Título", "Beta"} }},
		{"oversize", func(h *hashInput) { h.chunk.Oversize = true }},

		// Pipeline config, one row per field (PRD-contrato §2.4: "versão do chunker, piso/teto,
		// regra de fusão").
		{"pipeline chunker version", func(h *hashInput) { h.pipe.ChunkerVersion = "v2" }},
		{"pipeline floor", func(h *hashInput) { h.pipe.FloorTokens = 300 }},
		{"pipeline ceiling", func(h *hashInput) { h.pipe.CeilingTokens = 900 }},
		{"pipeline merge rule", func(h *hashInput) { h.pipe.MergeRule = "merge-nothing" }},

		// The seven handshake-confirmed embedder fields, one row each (S04).
		{"embedder model revision", func(h *hashInput) { h.emb.ModelRevision = "another-revision" }},
		{"embedder tokenizer revision", func(h *hashInput) { h.emb.TokenizerRevision = "another-revision" }},
		{"embedder dim", func(h *hashInput) { h.emb.Dim = 768 }},
		{"embedder normalization", func(h *hashInput) { h.emb.Normalization = "none" }},
		{"embedder pooling", func(h *hashInput) { h.emb.Pooling = "mean" }},
		{"embedder precision", func(h *hashInput) { h.emb.Precision = "fp32" }},
		{"embedder sparse params", func(h *hashInput) {
			h.emb.SparseParams = map[string]string{"indices": "token_id", "values": "weight"}
		}},
	}

	baseline := baselineHashInput().hash()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if again := baselineHashInput().hash(); again != baseline {
				t.Fatalf("an untouched clone of the baseline hashed to %s, want %s — ComputePointHash "+
					"is not a function of its inputs alone", again, baseline)
			}

			mutated := baselineHashInput()
			tc.mutate(&mutated)
			if got := mutated.hash(); got == baseline {
				t.Errorf("changing %s left point_hash at %s; that component is not covered by the hash, "+
					"so a change to it would never trigger a reingest", tc.name, got)
			}
		})
	}
}

// TestComputePointHash_UpdatedDoesNotChangeHash is the inverse of the table above, and it guards the
// owner's 2026-08-08 decision at the hash layer.
//
// If it ever fails, `updated` has found its way back into the field map and every git checkout,
// rebase, file sync or editor that rewrites on save re-embeds the whole vault for free.
func TestComputePointHash_UpdatedDoesNotChangeHash(t *testing.T) {
	baseline := baselineHashInput()
	moved := baselineHashInput()
	moved.note.Updated = baseline.note.Updated.Add(30 * 24 * time.Hour)

	if got, want := moved.hash(), baseline.hash(); got != want {
		t.Errorf("moving `updated` alone changed point_hash from %s to %s; `updated` is derived from "+
			"git/mtime and moves without the content moving (PRD-contrato §2.4, ADR-004 §5.1)", want, got)
	}
}

// TestComputePointHash_KnownVector pins one input set to one digest, so a change to the
// serialization order, the separators or the field names breaks visibly here instead of silently
// invalidating every point already in Qdrant.
//
// Updating this value is never a fix. It is a declaration that the index has to be rebuilt.
func TestComputePointHash_KnownVector(t *testing.T) {
	const want = "b5fc3efe06295fd0c4d80bbe32734265d7f451c3b113ca963b45d3ec11a649ba"
	if got := baselineHashInput().hash(); got != want {
		t.Errorf("point_hash of the pinned input = %s, want %s — if this change is intentional, it is "+
			"a full reindex of all three collections, not a test update (PRD-contrato §2.4)", got, want)
	}
}

// TestComputePointHash_UsesSharedSerialization proves ComputePointHash reads S02's metadata
// extractor rather than a local copy of the field list.
//
// The mechanism: NoteMetadataFields is the only thing that decides `tags` is serialized as a sorted
// \x1f-joined list. A note whose tags are reordered must therefore hash identically — which is true
// only if the shared serializer is what produced the value.
func TestComputePointHash_UsesSharedSerialization(t *testing.T) {
	baseline := baselineHashInput()
	reordered := baselineHashInput()
	reordered.note.Tags = []string{"architecture", "golang"}

	if got, want := reordered.hash(), baseline.hash(); got != want {
		t.Errorf("reordering tags changed point_hash (%s != %s); the tag list is not going through "+
			"schema.SerializeList, which sorts", got, want)
	}
}

// TestNewPipelineConfig_FollowsChunkerConfig pins the derivation of the pipeline half of the hash to
// the chunker config actually in use. A run that hashed different bounds from the ones it chunked
// with would write points whose fingerprint describes a pipeline that never ran.
func TestNewPipelineConfig_FollowsChunkerConfig(t *testing.T) {
	cfg := chunk.Config{FloorTokens: 300, CeilingTokens: 900}
	got := NewPipelineConfig(cfg)

	want := PipelineConfig{
		ChunkerVersion: chunk.Version,
		FloorTokens:    300,
		CeilingTokens:  900,
		MergeRule:      MergeRuleBelowFloor,
	}
	if got != want {
		t.Errorf("NewPipelineConfig(%+v) = %+v, want %+v", cfg, got, want)
	}
}
