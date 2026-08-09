package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"strconv"

	"github.com/danielmalka/go-knowrag/internal/chunk"
	"github.com/danielmalka/go-knowrag/internal/embed"
	"github.com/danielmalka/go-knowrag/internal/schema"
	"github.com/danielmalka/go-knowrag/internal/vault"
)

// MergeRuleBelowFloor names S03's merge rule: consecutive sections below the floor are merged until
// they reach it, without crossing the ceiling (internal/chunk/clamp.go).
//
// It is a named value rather than an implicit one because PRD-contrato §2.4 lists the merge rule as
// its own component of the pipeline config, alongside the chunker version and the two bounds. The
// day the rule changes without the chunker version moving, this is the string that has to change so
// every point_hash moves with it.
const MergeRuleBelowFloor = "merge-below-floor"

// PipelineConfig is the "config do pipeline" half of point_hash (PRD-contrato §2.4): the four
// values that decide how a note becomes chunks. Changing any of them changes what was embedded,
// even with the note's bytes untouched, so all four are hashed.
type PipelineConfig struct {
	ChunkerVersion string
	FloorTokens    int
	CeilingTokens  int
	MergeRule      string
}

// NewPipelineConfig derives the pipeline config from the chunker config actually in use.
//
// It exists so no caller assembles the four fields by hand: a run that hashed a floor of 256 while
// chunking with a floor of 300 would produce points whose fingerprint describes a pipeline that
// never ran, and every one of them would look integral forever.
func NewPipelineConfig(cfg chunk.Config) PipelineConfig {
	return PipelineConfig{
		ChunkerVersion: chunk.Version,
		FloorTokens:    cfg.FloorTokens,
		CeilingTokens:  cfg.CeilingTokens,
		MergeRule:      MergeRuleBelowFloor,
	}
}

// ComputePointHash assembles PRD-contrato §2.4's point_hash for one chunk of one note.
//
// This is the only place in the codebase that assembles a full point_hash. S02 delivers the note
// metadata and the canonical serializer, S03 delivers the chunk text, breadcrumb and oversize flag,
// S04 delivers the confirmed embedder config, and this function combines them. Nothing else
// re-derives the formula, and nothing here reimplements the serialization: schema.SerializeFields
// and schema.SerializeList are the contract's own, and a second implementation of either is a
// second set of bytes for the same content.
//
// Every field of emb must come from ServiceEmbedder.Handshake — the config the backend reports it
// actually loaded — never from embed.Expected() or from a constant read blind. A hash built from
// configured values is a hash that attests to itself, which is exactly what the handshake exists to
// stop: a container quietly serving an unpinned revision would produce points claiming the pinned
// one, and nothing downstream would ever notice.
//
// `updated` is absent, and absent by construction rather than by a skip: the note metadata comes
// from vault.NoteMetadataFields, which has eleven fields and no `updated`, and nothing below adds
// it back. See fieldUpdated in payload.go for why.
func ComputePointHash(
	n vault.Note,
	c chunk.Chunk,
	tenantID string,
	pipe PipelineConfig,
	emb embed.BackendHandshakeInfo,
) string {
	fields := maps.Clone(vault.NoteMetadataFields(n))

	// Point identity. tenant_id is in here for the same reason it is in the point ID: it is the
	// field that decides isolation, and it was the only payload field once left out of the hash
	// (PRD-contrato §2.4).
	fields[fieldTenantID] = tenantID
	fields[fieldUID] = n.UID.String()
	fields[fieldChunkIndex] = strconv.Itoa(c.Index)

	// The two per-chunk payload fields S02's note metadata does not cover. Both are derived from
	// the content, so both are inside the hash — the rule is that every payload field enters the
	// hash and `updated` is the one exception (PRD-contrato §2.4).
	//
	// Known limit, stated rather than worked around: SerializeList sorts, so a breadcrumb reordered
	// without any other change would hash identically. It cannot happen in practice — the
	// breadcrumb is also embedded verbatim inside c.Text, which is hashed as its own component
	// below — and using the contract's canonical serializer matters more than covering a case the
	// text already covers.
	fields[fieldHeadings] = schema.SerializeList(c.Breadcrumb)
	fields[fieldOversize] = strconv.FormatBool(c.Oversize)

	fields["pipeline_chunker_version"] = pipe.ChunkerVersion
	fields["pipeline_floor_tokens"] = strconv.Itoa(pipe.FloorTokens)
	fields["pipeline_ceiling_tokens"] = strconv.Itoa(pipe.CeilingTokens)
	fields["pipeline_merge_rule"] = pipe.MergeRule

	// All seven handshake-confirmed fields, individually. Folding them into one opaque string would
	// make "the tokenizer revision moved" and "the pooling changed" indistinguishable in a failure,
	// and PRD-contrato §2.4 requires a test per field.
	fields["embedder_model_revision"] = emb.ModelRevision
	fields["embedder_tokenizer_revision"] = emb.TokenizerRevision
	fields["embedder_dim"] = strconv.Itoa(emb.Dim)
	fields["embedder_normalization"] = emb.Normalization
	fields["embedder_pooling"] = emb.Pooling
	fields["embedder_precision"] = emb.Precision
	fields["embedder_sparse_params"] = schema.SerializeFields(emb.SparseParams)

	// The chunk text is hashed as its own component instead of being folded into the sorted field
	// map, because it is the one input that can legitimately contain the canonical separators
	// (\x00, \x1f) and an `=`. It is length-prefixed so the concatenation stays uniquely decodable:
	// without the prefix, a text ending in a separator could be rebalanced against the field block
	// and two different inputs would hash the same.
	sum := sha256.Sum256([]byte(
		strconv.Itoa(len(c.Text)) + "\x00" + c.Text + "\x00" + schema.SerializeFields(fields)))
	return hex.EncodeToString(sum[:])
}
