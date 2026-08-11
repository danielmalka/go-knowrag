package ingest

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielmalka/go-knowrag/internal/chunk"
	"github.com/danielmalka/go-knowrag/internal/embed"
	"github.com/danielmalka/go-knowrag/internal/vault"
)

// The §2.4 payload keys this package writes and compares. They are constants rather than inline
// strings because three call sites have to agree on every one of them — the write (store.Client),
// the expected-value construction, and condition 4's comparison — and a typo in any one of them
// produces a note that reprocesses on every run with nothing to show for it.
const (
	fieldPointHash      = "point_hash"
	fieldTenantID       = "tenant_id"
	fieldUID            = "uid"
	fieldChunkIndex     = "chunk_index"
	fieldText           = "text"
	fieldVault          = "vault"
	fieldArea           = "area"
	fieldSub            = "sub"
	fieldPath           = "path"
	fieldType           = "type"
	fieldStatus         = "status"
	fieldVisibility     = "visibility"
	fieldTags           = "tags"
	fieldTitle          = "title"
	fieldHeadings       = "headings"
	fieldLang           = "lang"
	fieldCreated        = "created"
	fieldOversize       = "oversize"
	fieldEmbeddingModel = "embedding_model"

	// fieldUpdated is the one payload field kept out of both point_hash and condition 4 (owner
	// decision 2026-08-08; PRD-contrato §2.4, ADR-004 §5.1). It is still written — it is what the
	// UI displays and orders by — but it is derived from git/mtime, so it moves without the content
	// moving, and an identity that depends on a clock that walks on its own re-embeds a vault on a
	// git checkout.
	//
	// This constant is the single definition site of the exclusion: PayloadFields writes it under
	// this name and comparedExclusions skips it under this name, so the two cannot drift into a
	// state where the field is written but compared, which would bring mtime noise back through the
	// side door.
	fieldUpdated = "updated"
)

// comparedExclusions are the payload keys condition 4 never compares. point_hash is here for
// completeness — it lives in its own struct field rather than in Fields, so it never reaches the
// comparison anyway — and `updated` is here for the reason spelled out above it.
var comparedExclusions = map[string]struct{}{
	fieldPointHash: {},
	fieldUpdated:   {},
}

// PayloadFields builds the §2.4 payload of one point, minus point_hash.
//
// Values are native Go types, not pre-stringified: chunk_index has to reach Qdrant as an integer
// (a range filter on a keyword index is refused, and the prune is a range filter), tags and
// headings as lists (S07 filters on individual tags, which a joined string would defeat), oversize
// as a boolean. The store writes them as-is and reads them back the same way, so the comparison in
// condition 4 is between two values of the same shape.
//
// embeddingModel is the revision **confirmed by S04's handshake**, never the configured or
// self-attested one (PRD-contrato §2.4) — that is the whole reason ComputePointHash and this
// function both take it from the same BackendHandshakeInfo.
func PayloadFields(n vault.Note, c chunk.Chunk, tenantID, embeddingModel string) map[string]any {
	return map[string]any{
		fieldTenantID:   tenantID,
		fieldUID:        n.UID.String(),
		fieldChunkIndex: c.Index,
		fieldText:       c.Text,

		fieldVault:      n.Vault,
		fieldArea:       n.Area,
		fieldSub:        n.Sub,
		fieldPath:       n.Path,
		fieldType:       n.Type.String(),
		fieldStatus:     n.Status.String(),
		fieldVisibility: n.Visibility.String(),
		// Cloned so a later mutation of the Note's slice cannot reach a payload map already handed
		// to the store — the same reason schema.SerializeList copies before sorting.
		fieldTags:     slices.Clone(n.Tags),
		fieldTitle:    n.Title,
		fieldHeadings: slices.Clone(c.Breadcrumb),
		fieldLang:     n.Lang,
		fieldCreated:  n.Created.UTC().Format(time.RFC3339),
		fieldUpdated:  n.Updated.UTC().Format(time.RFC3339),
		fieldOversize: c.Oversize,

		fieldEmbeddingModel: embeddingModel,
	}
}

// ExpectedPoints is what the note on disk says its points must look like, right now: one entry per
// chunk, carrying the freshly recomputed point_hash and the freshly recomputed payload.
//
// It takes no embedder and produces no vectors on purpose. The integrity check runs before any
// embedding happens — that is the entire point of the short-circuit — so everything it compares
// against has to be derivable from the note and the confirmed config alone.
func ExpectedPoints(
	n vault.Note,
	chunks []chunk.Chunk,
	tenantID string,
	pipe PipelineConfig,
	emb embed.BackendHandshakeInfo,
) []ExpectedPoint {
	out := make([]ExpectedPoint, len(chunks))
	for i, c := range chunks {
		out[i] = ExpectedPoint{
			ChunkIndex: c.Index,
			PointHash:  ComputePointHash(n, c, tenantID, pipe, emb),
			Fields:     PayloadFields(n, c, tenantID, emb.ModelRevision),
		}
	}
	return out
}

// equalFields compares two payload maps field by field, skipping comparedExclusions.
//
// It is a hand-written comparison rather than reflect.DeepEqual because the exclusion has to be
// honoured on both sides — a key present only in the stored payload is a difference too — and
// because the value domain is closed and tiny: the four shapes PayloadFields produces.
func equalFields(got, want map[string]any) (string, bool) {
	for k, w := range want {
		if _, skip := comparedExclusions[k]; skip {
			continue
		}
		g, ok := got[k]
		if !ok {
			return k, false
		}
		if !equalValue(g, w) {
			return k, false
		}
	}
	for k := range got {
		if _, skip := comparedExclusions[k]; skip {
			continue
		}
		if _, ok := want[k]; !ok {
			return k, false
		}
	}
	return "", true
}

// equalValue compares two payload values. Lists compare in order — headings is a breadcrumb, and
// "A > B" is not "B > A".
func equalValue(a, b any) bool {
	as, aok := a.([]string)
	bs, bok := b.([]string)
	if aok || bok {
		return aok && bok && slices.Equal(as, bs)
	}
	return a == b
}

// formatValue renders a payload value for a failure message. Only the shapes PayloadFields
// produces are handled; anything else prints through the default case rather than panicking, since
// this only ever runs on a path that is already reporting a problem.
func formatValue(v any) string {
	switch t := v.(type) {
	case string:
		return strconv.Quote(t)
	case int:
		return strconv.Itoa(t)
	case bool:
		return strconv.FormatBool(t)
	case []string:
		quoted := make([]string, len(t))
		for i, s := range t {
			quoted[i] = strconv.Quote(s)
		}
		return "[" + strings.Join(quoted, " ") + "]"
	default:
		return "<missing>"
	}
}
