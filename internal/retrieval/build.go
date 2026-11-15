package retrieval

import (
	"github.com/danielmalka/go-knowrag/internal/embed"
	"github.com/danielmalka/go-knowrag/internal/schema"
)

// The search request is expressed in this package's own types, not in the Qdrant client's.
//
// That is forced, not stylistic: internal/archtest enforces "nothing outside internal/store imports
// github.com/qdrant/go-client/qdrant" (S09 T11 invariant 1), so a *qdrant.QueryPoints built here
// would fail the build's own architecture test. The shape below is one dense vector search over a
// filter, expressed the way internal/schema already expresses collection provisioning: this package
// decides *what* the query is, internal/store/query.go transcribes it into *how* Qdrant is asked. The
// transcription is mechanical and carries no decision of its own; every choice that matters (which
// filter conditions exist, that ACORN is on, what the limits are) is made in this file.

// Payload field names this package filters and reads on. They are spelled here because
// internal/ingest keeps its copy unexported and internal/schema's manifest carries them as literals
// in the index list; a third copy is the least-bad option until one of those exports them. Every
// name below is in the schema manifest's Indexes (filters) or in the §2.4 payload (reads) — strict
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

// SearchRequest is a finished search, ready to execute. Nothing downstream adds to it.
//
// A request with a Dense vector is a nearest-neighbour search over the filter. A request without one
// is the filter alone (Searcher.FilterMatchesAnything): internal/store leaves the query unset and
// Qdrant reads the filter, which is what makes that probe cheap.
type SearchRequest struct {
	Collection string
	// Dense is the query vector and DenseVector the named vector it is searched in.
	Dense       []float32
	DenseVector string
	// Acorn asks Qdrant to consider the ACORN traversal for the filtered HNSW search (PRD-contrato
	// §2.3c). It carries no selectivity threshold on purpose: ACORN pays off for filters that let
	// *little* through, Qdrant decides per request from a cardinality estimate this process does not
	// have, and tenant_id — one populated tenant per collection in this build — is the least
	// selective condition here, not the most. Pinning a threshold from here would be guessing with
	// strictly less information.
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

// buildQueryRequest is the search shape: one dense vector query over the filter, with ACORN on.
func buildQueryRequest(q Query, emb embed.Embedding) SearchRequest {
	return SearchRequest{
		Collection:    q.Collection,
		Dense:         emb.Dense,
		DenseVector:   schema.DenseVectorName,
		Acorn:         true,
		Filter:        buildFilter(q),
		Limit:         q.TopK,
		Offset:        q.Offset,
		PayloadFields: resultPayloadFields(),
	}
}
