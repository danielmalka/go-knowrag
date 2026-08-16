package retrieval

import (
	"github.com/danielmalka/go-knowrag/internal/embed"
	"github.com/danielmalka/go-knowrag/internal/schema"
)

// The search request is expressed in this package's own types, not in the Qdrant client's.
//
// That is forced, not stylistic: internal/archtest enforces "nothing outside internal/store imports
// github.com/qdrant/go-client/qdrant" (S09 T11 invariant 1), so a *qdrant.QueryPoints built here
// would fail the build's own architecture test. The shape below is the same shape PRD-contrato
// §2.3b mandates — two prefetch legs, one per named vector, RRF fusion over them — expressed the
// way internal/schema already expresses collection provisioning: this package decides *what* the
// query is, internal/store/query.go transcribes it into *how* Qdrant is asked. The transcription is
// mechanical and carries no decision of its own; every choice that matters (which filter conditions
// exist, that ACORN is on, what the limits are, that the fusion is RRF) is made in this file.
//
// See the report in the S07 handover: this differs from S07 T3/T7's literal signatures, which name
// *qdrant.QueryPoints inside this package. Those signatures and invariant 1 cannot both hold.

// Payload field names this package filters and reads on. They are spelled here because
// internal/ingest keeps its copy unexported and internal/schema's manifest carries them as literals
// in the index list; a third copy is the least-bad option until one of those exports them. Every
// name below is in schema.Manifest()'s Indexes (filters) or in the §2.4 payload (reads) — strict
// mode refuses a filter on anything else, so a typo here is an InvalidArgument, not a slow query.
const (
	fieldTenantID   = "tenant_id"
	fieldStatus     = "status"
	fieldVisibility = "visibility"
	fieldArea       = "area"
	fieldType       = "type"
	fieldVault      = "vault"
	fieldTags       = "tags"

	fieldUID        = "uid"
	fieldChunkIndex = "chunk_index"
	fieldText       = "text"
	fieldHeadings   = "headings"
	fieldPath       = "path"
	fieldTitle      = "title"
)

// Condition is one payload field matching one keyword. Every filter this package builds is a
// conjunction of these — there is no range, no nesting and no `should`, because nothing in §2.3b's
// query shape needs one, and a richer condition language would be a second place where a tenant
// scope could be expressed and therefore forgotten.
type Condition struct {
	Field string
	Value string
}

// Filter is `must` and `must_not` over Conditions. Must is never empty on a filter this package
// builds: the tenant condition is always in it.
type Filter struct {
	Must    []Condition
	MustNot []Condition
}

// PrefetchLeg is one branch of the hybrid query: one named vector, one candidate limit, one filter.
// Exactly one of Dense/Sparse carries a vector; which one is this package's decision, and
// internal/store only transcribes whichever is set.
type PrefetchLeg struct {
	Vector string
	Dense  []float32
	Sparse embed.Sparse
	Limit  int
	Filter Filter

	// Acorn asks Qdrant to consider the ACORN traversal for this leg's filtered HNSW search
	// (PRD-contrato §2.3c). It carries no selectivity threshold on purpose — see buildQueryRequest.
	Acorn bool
}

// SearchRequest is a finished search, ready to execute. Nothing downstream adds to it.
type SearchRequest struct {
	Collection string
	Prefetch   []PrefetchLeg
	// FusionRRF selects reciprocal-rank fusion over the prefetch legs, with the server's default
	// k — the fusion mode §2.3b fixes.
	FusionRRF bool

	// Dense and DenseVector are the top-level vector query, used by SearchModeDenseOnly: one dense
	// search over the filter, no prefetch leg and no fusion. Exactly one of (Dense, DenseVector) and
	// (Prefetch, FusionRRF) describes a given request; internal/store/query.go refuses a request
	// that sets both, because the two are different queries and Qdrant would silently honour one.
	//
	// A top-level query is needed rather than a single prefetch leg because a prefetch is by
	// definition a candidate source for an outer query to rerank or fuse — a request with prefetch
	// and no outer query is not a dense search, it is an incomplete one.
	Dense       []float32
	DenseVector string
	// Acorn asks for the ACORN traversal on the top-level filtered search, for the same reason the
	// legs carry it: in dense-only there is no prefetch, so the filtered HNSW traversal happens here.
	Acorn bool

	Filter Filter
	Limit  int
	Offset int
	// PayloadFields is the payload projection: only what formatResults reads comes back. Empty
	// would mean "the whole payload", which is a text field per chunk this package never looks at.
	PayloadFields []string
}

// resultPayloadFields is exactly what formatResults reads, and it is a function returning a fresh
// slice for the same reason schema.Manifest() is: a package-level slice var is writable from
// anywhere in the module.
func resultPayloadFields() []string {
	return []string{fieldUID, fieldChunkIndex, fieldText, fieldHeadings, fieldPath, fieldTitle}
}

// calibratedPrefetchLimit is the per-leg limit for a Query that Search has already validated.
//
// It returns a plain int because the error path lives in Query.Validate: by the time this runs, the
// same (topK, offset, multiplier) triple has already been proven to clear the floor. The fallback
// exists so a future call site that skipped validation degrades to the floor itself rather than to
// zero, which would silently return nothing.
func calibratedPrefetchLimit(q Query, multiplier int) int {
	limit, err := prefetchLimit(q.TopK, q.Offset, multiplier)
	if err != nil {
		return q.TopK + q.Offset
	}
	return limit
}

// buildFilter assembles the filter for one Query.
//
// The tenant condition is added first and unconditionally. There is no branch above it, no
// parameter that skips it and no code path that reaches the request builder without going through
// here — that is the whole invariant this package exists to hold (S07 T4).
func buildFilter(q Query) Filter {
	f := Filter{Must: []Condition{{Field: fieldTenantID, Value: q.TenantID}}}

	if !q.IncludeArchived {
		f.MustNot = append(f.MustNot, Condition{Field: fieldStatus, Value: schema.StatusArchived().String()})
	}
	// Keyed on IncludePrivate alone. Collection is deliberately not read here: it is a trust
	// boundary (who may query at all), not a confidentiality classification (whether this chunk is
	// shown), and the previous rule — "private never leaves `interno`" — gave a client marking their
	// own note private in `clientes` no protection whatsoever (S07 T5).
	if !q.IncludePrivate {
		f.MustNot = append(f.MustNot, Condition{Field: fieldVisibility, Value: schema.VisibilityPrivate().String()})
	}

	for _, c := range []Condition{
		{Field: fieldArea, Value: q.Area},
		{Field: fieldType, Value: q.Type},
		{Field: fieldVault, Value: q.Vault},
	} {
		if c.Value != "" {
			f.Must = append(f.Must, c)
		}
	}
	// One condition per tag, so several tags mean "has all of them". A single condition carrying
	// the list would mean "has any of them", which is a different question nobody asked for.
	for _, tag := range q.Tags {
		if tag != "" {
			f.Must = append(f.Must, Condition{Field: fieldTags, Value: tag})
		}
	}
	return f
}

// buildQueryRequest is the §2.3b query shape: two prefetch legs (dense and sparse, by the named
// vectors internal/schema owns), fused with RRF, over one filter.
//
// ACORN is enabled on both legs and not on the outer request, because the filter runs on the
// prefetches — that is where the filtered HNSW traversal ACORN modifies actually happens, and the
// outer stage only fuses two already-filtered lists. MaxSelectivity is left unset on purpose
// (PRD-contrato §2.3c): ACORN pays off for filters that let *little* through, Qdrant decides per
// request from a cardinality estimate this process does not have, and tenant_id — with one
// populated tenant per collection in this build — is the least selective condition here, not the
// most. Pinning a threshold from here would be guessing with strictly less information.
// The dense-only branch (SearchModeDenseOnly) drops the sparse leg and the fusion, keeping the same
// filter, the same limit/offset and the same payload projection: the two modes differ in ranking
// and in nothing else, which is what makes a hybrid-vs-dense recall comparison a comparison of
// ranking (internal/eval/hybrid_compare.go).
func buildQueryRequest(q Query, emb embed.Embedding, legLimit int) SearchRequest {
	f := buildFilter(q)

	if q.Mode == SearchModeDenseOnly {
		return SearchRequest{
			Collection:    q.Collection,
			Dense:         emb.Dense,
			DenseVector:   schema.DenseVectorName,
			Acorn:         true,
			Filter:        f,
			Limit:         q.TopK,
			Offset:        q.Offset,
			PayloadFields: resultPayloadFields(),
		}
	}

	leg := func(vector string) PrefetchLeg {
		return PrefetchLeg{Vector: vector, Limit: legLimit, Filter: f, Acorn: true}
	}

	dense := leg(schema.DenseVectorName)
	dense.Dense = emb.Dense
	sparse := leg(schema.SparseVectorName)
	sparse.Sparse = emb.Sparse

	return SearchRequest{
		Collection: q.Collection,
		Prefetch:   []PrefetchLeg{dense, sparse},
		FusionRRF:  true,
		// The same filter on the outer request too. It is redundant while both legs carry it, and
		// that is the point: the tenant scope then does not depend on the request keeping its
		// prefetch shape.
		Filter:        f,
		Limit:         q.TopK,
		Offset:        q.Offset,
		PayloadFields: resultPayloadFields(),
	}
}
