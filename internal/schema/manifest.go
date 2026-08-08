package schema

import (
	"slices"

	"github.com/qdrant/go-client/qdrant"
)

// The Qdrant schema is code, not a curl script kept in a runbook. This file is the single source of
// truth for what a collection must look like — dimension, distance, named vectors, payload indexes
// and the pinned model revision — and Apply (apply.go) is the only thing that turns it into calls.
//
// Three collections, one per trust boundary (design brief §2.1): multi-tenancy happens *inside* a
// collection through the indexed tenant_id payload field, never by minting a collection per tenant.
// Only `interno` carries data in this build, and the manifest is still parameterized over all three
// from day one — no code path anywhere may name `interno` on its own.

const (
	// DenseVectorName and SparseVectorName are the named vectors every collection carries. Named,
	// not default: S07's QueryPoints fuses two PrefetchQuery branches (`using: dense`,
	// `using: sparse`) with RRF, and an unnamed vector has nothing for `using` to select.
	DenseVectorName  = "dense"
	SparseVectorName = "sparse"

	// DenseDim is BGE-M3's dense output width, confirmed on the model card (PRD-contrato §2.3b).
	// It is uint64 because that is what qdrant.VectorParams.Size is; converting at the call site
	// would just move the conversion somewhere less visible.
	DenseDim uint64 = 1024
)

// CollectionManifest is the desired state of one Qdrant collection.
//
// Every field here is a property Qdrant really stores and really reports back, with one exception:
// ModelRevision. Qdrant has no attribute for "which embedding model wrote these vectors", so that
// one is compared against the applied-state record (applied_state.go), never against Qdrant.
type CollectionManifest struct {
	Name             string
	DenseVectorName  string
	DenseDim         uint64
	Distance         qdrant.Distance
	SparseVectorName string

	// ModelRevision is the embedding model revision these vectors were produced by. Changing it
	// means every vector already stored was produced by a different model, so it is drift, and
	// resolving it means reindexing all three collections (PRD-contrato §2.3b).
	ModelRevision string

	Indexes []PayloadIndex
}

// PayloadIndex is one payload field index. The type is carried explicitly because an index created
// with the wrong type is drift Apply has to be able to name, and because Qdrant refuses a range
// filter on a field indexed as a keyword.
type PayloadIndex struct {
	Field    string
	Kind     qdrant.FieldType
	IsTenant bool
}

// Manifest returns the three collections' desired state.
//
// It is a function returning a fresh deep copy rather than an exported slice var, for the same
// reason the enums in enums.go are accessor functions: an exported var is assignable and its
// backing array is writable from any package in the module, so a single stray write would redefine
// what "correct" means for every later Apply in the process. Drift tests that need a mutated
// manifest get one by mutating their own copy.
func Manifest() []CollectionManifest {
	out := make([]CollectionManifest, len(manifest))
	for i, c := range manifest {
		c.Indexes = slices.Clone(c.Indexes)
		out[i] = c
	}
	return out
}

var manifest = []CollectionManifest{
	// interno: the operator's own vaults. The only collection populated in this build.
	newCollectionManifest("interno"),
	// clientes and base_paga: provisioned empty. They exist now so that nothing — not Apply, not
	// the CLI, not the integration test — ever grows a special case for the one collection that
	// happens to have data in it.
	newCollectionManifest("clientes"),
	newCollectionManifest("base_paga"),
}

// newCollectionManifest builds one entry. The three collections are deliberately identical in
// shape: they isolate risk, they do not shard by feature, so a difference between them would be a
// bug rather than a design.
func newCollectionManifest(name string) CollectionManifest {
	keyword := func(field string) PayloadIndex {
		return PayloadIndex{Field: field, Kind: qdrant.FieldType_FieldTypeKeyword}
	}
	return CollectionManifest{
		Name:             name,
		DenseVectorName:  DenseVectorName,
		DenseDim:         DenseDim,
		Distance:         qdrant.Distance_Cosine,
		SparseVectorName: SparseVectorName,
		ModelRevision:    BGEM3Revision,
		// This list is derived from who filters, not from what looks important. Strict mode
		// (apply.go) refuses a filter on an unindexed field, so a field this list misses is not a
		// slow query — it is an InvalidArgument at runtime, in whichever story owns the filter. Two
		// stories filter today, and both are represented below; the rule for the next one is the
		// same: add the filter, add the index in the same change.
		Indexes: []PayloadIndex{
			// tenant_id is the isolation key (PRD-contrato §2.4), so its index carries is_tenant:
			// Qdrant then groups points by tenant on disk instead of merely filtering them. Both
			// stories filter on it — S06a scopes every read and delete by it, S07 makes it a
			// mandatory must condition.
			{Field: "tenant_id", Kind: qdrant.FieldType_FieldTypeKeyword, IsTenant: true},
			// S06a: ScrollByUID filters tenant_id + uid, and DeleteByFilter adds
			// chunk_index >= N to prune the tail of a note that got shorter. chunk_index is the
			// one integer here — a range filter on a keyword index is refused, so the type is
			// load-bearing, not decoration.
			keyword("uid"),
			{Field: "chunk_index", Kind: qdrant.FieldType_FieldTypeInteger},
			// S07's facets.
			keyword("status"),
			keyword("area"),
			keyword("vault"),
			keyword("type"),
			keyword("visibility"),
			keyword("tags"),
		},
	}
}
