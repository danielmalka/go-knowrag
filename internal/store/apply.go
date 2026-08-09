package store

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/qdrant/go-client/qdrant"

	"github.com/danielmalka/go-knowrag/internal/schema"
)

// Apply makes a live Qdrant match the manifest, and refuses to make it match by force.
//
// It lives here rather than beside the manifest because everything below is Qdrant vocabulary —
// requests, enums, the shape of a CollectionInfo — and this package is the only one allowed to
// speak it (enforced by internal/archtest). internal/schema states what the collections must be;
// this file is the translation of that statement into calls.
//
// The split that governs everything below: Qdrant is the authority on the properties it really
// stores (named vectors, dimension, distance, payload indexes), and the applied-state record is the
// authority on the one property it does not store (which embedding revision the vectors came from).
// Neither is ever compared against itself, and neither is ever overwritten to end a disagreement —
// a disagreement is drift, and drift means a deliberate reindex, not a silent rewrite.

// ErrSchemaDrift is what every drift check returns, whatever property drifted. Callers that need
// to distinguish drift from a transport failure match on this; callers that only need to print
// something useful print the wrapped *DriftError's message.
var ErrSchemaDrift = errors.New("qdrant schema drift")

// DriftError names one disagreement between the manifest and reality.
type DriftError struct {
	Collection string
	Property   string
	Expected   string // what the manifest says
	Actual     string // what Observed reports
	Observed   string // where Actual was read from — Qdrant, or the applied-state record
}

func (e *DriftError) Error() string {
	return fmt.Sprintf(
		"collection %q: %s is %q per %s, but the manifest says %q — this is schema drift, and it is "+
			"not repaired in place: dimension, distance, named vectors and the embedding model "+
			"revision are all properties of the vectors already stored, so resolving a mismatch means "+
			"deliberately reindexing the three collections together (PRD-contrato §2.3b)",
		e.Collection, e.Property, e.Actual, e.Observed, e.Expected)
}

func (e *DriftError) Unwrap() error { return ErrSchemaDrift }

// QdrantAPI is the slice of the Qdrant client Apply uses. Every method matches *qdrant.Client's
// signature exactly, so the real client satisfies it with no adapter.
//
// It is an interface rather than the concrete client because Apply's whole contract is "which
// calls, carrying what, in what order" — and that is testable against a fake, without a container,
// which is what keeps T4–T9's proofs in `go test` rather than behind a build tag.
type QdrantAPI interface {
	CollectionExists(ctx context.Context, collectionName string) (bool, error)
	CreateCollection(ctx context.Context, request *qdrant.CreateCollection) error
	GetCollectionInfo(ctx context.Context, collectionName string) (*qdrant.CollectionInfo, error)
	CreateFieldIndex(ctx context.Context, request *qdrant.CreateFieldIndexCollection) (*qdrant.UpdateResult, error)
}

// Report is what Apply did, per collection, in manifest order.
type Report struct {
	Collections []CollectionReport
}

// CollectionReport is one collection's outcome.
type CollectionReport struct {
	Name           string
	Created        bool
	IndexesCreated []string
}

// UpToDate reports whether this collection needed nothing at all — the state a second run must
// produce for every entry.
func (r CollectionReport) UpToDate() bool { return !r.Created && len(r.IndexesCreated) == 0 }

func (r CollectionReport) String() string {
	if r.UpToDate() {
		return r.Name + ": already up to date"
	}
	var parts []string
	if r.Created {
		parts = append(parts, "collection created")
	}
	if len(r.IndexesCreated) > 0 {
		parts = append(parts, fmt.Sprintf("%d index(es) created: %s",
			len(r.IndexesCreated), strings.Join(r.IndexesCreated, ", ")))
	}
	return r.Name + ": " + strings.Join(parts, ", ")
}

// Apply provisions every collection in manifest and returns what it did.
//
// manifest is a parameter, not a read of the package-level Manifest(), for two reasons: nothing
// here may special-case the one collection that happens to hold data in this build, and the drift
// tests need to hand it a deliberately wrong manifest.
//
// On error the Report returned covers the collections completed before the failure — partial, and
// labelled as such by simply being shorter than the manifest.
func Apply(
	ctx context.Context,
	api QdrantAPI,
	manifest []schema.CollectionManifest,
	state schema.AppliedStateStore,
) (Report, error) {
	loaded, err := state.Load()
	if err != nil {
		return Report{}, err
	}

	var report Report
	next := loaded
	for _, c := range manifest {
		// The revision check runs before anything touches Qdrant, and independently of the
		// dimension check below: a revision bump on a collection whose geometry is unchanged is
		// exactly the case Qdrant cannot see, so gating it behind a Qdrant comparison would hide
		// the only failure it exists to catch.
		if err := checkModelRevision(loaded, c); err != nil {
			return report, err
		}

		cr, err := applyCollection(ctx, api, c)
		report.Collections = append(report.Collections, cr)
		if err != nil {
			return report, err
		}
		next = next.With(c.Name, c.ModelRevision)
	}

	// Written last, and only when a revision actually changed: last so a failure above records no
	// success, and only-when-changed so a no-op run stays a no-op all the way to disk.
	if !next.SameRevisions(loaded) {
		if err := state.Save(next); err != nil {
			return report, err
		}
	}
	return report, nil
}

// checkModelRevision compares the manifest against the committed record. No record is the first
// run — or a checkout that has never applied — and is not drift.
func checkModelRevision(state schema.AppliedState, c schema.CollectionManifest) error {
	rec, ok := state.Find(c.Name)
	if !ok || rec.EmbeddingModel == c.ModelRevision {
		return nil
	}
	return &DriftError{
		Collection: c.Name,
		Property:   "embedding model revision",
		Expected:   c.ModelRevision,
		Actual:     rec.EmbeddingModel,
		Observed:   "the applied-state record",
	}
}

// applyCollection brings one collection to the manifest's state: create it if missing, verify it if
// present, then fill in whichever payload indexes are absent.
func applyCollection(ctx context.Context, api QdrantAPI, c schema.CollectionManifest) (CollectionReport, error) {
	report := CollectionReport{Name: c.Name}

	exists, err := api.CollectionExists(ctx, c.Name)
	if err != nil {
		return report, fmt.Errorf("checking whether collection %q exists: %w", c.Name, err)
	}

	// indexed stays nil for a collection just created: it has no payload index yet, so every
	// manifest index is missing, and asking Qdrant to confirm that costs a round trip to learn
	// what we just caused.
	var indexed map[string]*qdrant.PayloadSchemaInfo
	if exists {
		info, err := api.GetCollectionInfo(ctx, c.Name)
		if err != nil {
			return report, fmt.Errorf("reading collection %q: %w", c.Name, err)
		}
		if err := checkVectorDrift(c, info); err != nil {
			return report, err
		}
		indexed = info.GetPayloadSchema()
	} else {
		if err := api.CreateCollection(ctx, createCollectionRequest(c)); err != nil {
			return report, fmt.Errorf("creating collection %q: %w", c.Name, err)
		}
		report.Created = true
	}

	for _, idx := range c.Indexes {
		if present, ok := indexed[idx.Field]; ok {
			if err := checkIndexDrift(c.Name, idx, present); err != nil {
				return report, err
			}
			continue
		}
		if _, err := api.CreateFieldIndex(ctx, fieldIndexRequest(c.Name, idx)); err != nil {
			return report, fmt.Errorf("creating index %q on collection %q: %w", idx.Field, c.Name, err)
		}
		report.IndexesCreated = append(report.IndexesCreated, idx.Field)
	}
	return report, nil
}

// qdrantDistance and qdrantFieldType translate the manifest's own enums into the wire ones. They
// are the whole reason internal/schema can describe a collection without importing this client.
//
// Both are total by falling back to the wire enum's zero value, which is exactly what the manifest
// produced before these types existed: an unset schema.Distance was qdrant.Distance_UnknownDistance
// — a metric the server rejects, not a silent default to cosine — and an unset schema.FieldType was
// qdrant.FieldType_FieldTypeKeyword, since keyword is the zero value on both sides.
var (
	qdrantDistances = map[schema.Distance]qdrant.Distance{
		schema.DistanceCosine: qdrant.Distance_Cosine,
	}
	qdrantFieldTypes = map[schema.FieldType]qdrant.FieldType{
		schema.FieldTypeKeyword: qdrant.FieldType_FieldTypeKeyword,
		schema.FieldTypeInteger: qdrant.FieldType_FieldTypeInteger,
	}
)

func qdrantDistance(d schema.Distance) qdrant.Distance { return qdrantDistances[d] }

func qdrantFieldType(f schema.FieldType) qdrant.FieldType { return qdrantFieldTypes[f] }

// createCollectionRequest builds the create call from c alone — every value in it comes off the
// manifest entry, so the three collections differ only where the manifest says they differ.
func createCollectionRequest(c schema.CollectionManifest) *qdrant.CreateCollection {
	return &qdrant.CreateCollection{
		CollectionName: c.Name,
		VectorsConfig: qdrant.NewVectorsConfigMap(map[string]*qdrant.VectorParams{
			c.DenseVectorName: {Size: c.DenseDim, Distance: qdrantDistance(c.Distance)},
		}),
		// The sparse side takes Qdrant's defaults: BGE-M3 emits learned lexical weights, so there
		// is no IDF modifier to apply on top of them.
		SparseVectorsConfig: qdrant.NewSparseVectorsConfig(map[string]*qdrant.SparseVectorParams{
			c.SparseVectorName: {},
		}),
		// Strict mode, with unindexed filtering off on both paths. Without it a query filtering on
		// a field nobody indexed is answered by a full scan — the right answer, arrived at slowly
		// enough that it looks like a performance mystery instead of the missing index it is.
		StrictModeConfig: &qdrant.StrictModeConfig{
			Enabled:                    qdrant.PtrOf(true),
			UnindexedFilteringRetrieve: qdrant.PtrOf(false),
			UnindexedFilteringUpdate:   qdrant.PtrOf(false),
		},
	}
}

func fieldIndexRequest(collection string, idx schema.PayloadIndex) *qdrant.CreateFieldIndexCollection {
	req := &qdrant.CreateFieldIndexCollection{
		CollectionName: collection,
		FieldName:      idx.Field,
		FieldType:      qdrant.PtrOf(qdrantFieldType(idx.Kind)),
		// Wait for the index to be built before returning: an apply that reports success while the
		// index is still pending would let the very next run see it missing and create it again.
		Wait: qdrant.PtrOf(true),
	}
	// Params are sent only for keyword, because is_tenant is the only non-default this manifest
	// asks for and the params message has to match the field type — keyword params on an integer
	// field is a request Qdrant rejects. The integer index takes the server's defaults (lookup and
	// range both on), which is what a chunk_index >= N filter needs.
	if idx.Kind == schema.FieldTypeKeyword {
		req.FieldIndexParams = qdrant.NewPayloadIndexParamsKeyword(&qdrant.KeywordIndexParams{
			IsTenant: qdrant.PtrOf(idx.IsTenant),
		})
	}
	return req
}

// checkVectorDrift compares the live collection's vector geometry against the manifest. It reads
// only properties Qdrant genuinely stores and reports back.
func checkVectorDrift(c schema.CollectionManifest, info *qdrant.CollectionInfo) error {
	params := info.GetConfig().GetParams()

	denseByName := params.GetVectorsConfig().GetParamsMap().GetMap()
	dense, ok := denseByName[c.DenseVectorName]
	if !ok {
		return &DriftError{
			Collection: c.Name,
			Property:   "named dense vector",
			Expected:   c.DenseVectorName,
			// What is actually there, so the message distinguishes "renamed" from "absent".
			Actual:   strings.Join(slices.Sorted(maps.Keys(denseByName)), ", "),
			Observed: "Qdrant",
		}
	}
	if dense.GetSize() != c.DenseDim {
		return &DriftError{
			Collection: c.Name,
			Property:   "dense vector dimension",
			Expected:   strconv.FormatUint(c.DenseDim, 10),
			Actual:     strconv.FormatUint(dense.GetSize(), 10),
			Observed:   "Qdrant",
		}
	}
	// Compared after translation, not before: the manifest's metric and the live one are two
	// spellings of the same thing, and the message names the wire spelling on both sides.
	if want := qdrantDistance(c.Distance); dense.GetDistance() != want {
		return &DriftError{
			Collection: c.Name,
			Property:   "dense vector distance",
			Expected:   want.String(),
			Actual:     dense.GetDistance().String(),
			Observed:   "Qdrant",
		}
	}
	sparseByName := params.GetSparseVectorsConfig().GetMap()
	if _, ok := sparseByName[c.SparseVectorName]; !ok {
		return &DriftError{
			Collection: c.Name,
			Property:   "named sparse vector",
			Expected:   c.SparseVectorName,
			Actual:     strings.Join(slices.Sorted(maps.Keys(sparseByName)), ", "),
			Observed:   "Qdrant",
		}
	}
	return nil
}

// payloadSchemaTypes maps the manifest's field types to what GetCollectionInfo reports for an
// index of that type — two different enums for the same idea, with no conversion in the client.
var payloadSchemaTypes = map[schema.FieldType]qdrant.PayloadSchemaType{
	schema.FieldTypeKeyword: qdrant.PayloadSchemaType_Keyword,
	schema.FieldTypeInteger: qdrant.PayloadSchemaType_Integer,
}

// checkIndexDrift verifies an index that already exists actually matches the manifest. Presence
// alone is not enough: an index on tenant_id built without is_tenant looks identical from a
// distance and lays the collection out the wrong way.
func checkIndexDrift(collection string, idx schema.PayloadIndex, live *qdrant.PayloadSchemaInfo) error {
	// ponytail: the table covers only the two field types the manifest actually uses; an unmapped
	// kind skips the type check rather than inventing an expectation for it. Extend the table when
	// a third type enters the manifest.
	if want, ok := payloadSchemaTypes[idx.Kind]; ok && live.GetDataType() != want {
		return &DriftError{
			Collection: collection,
			Property:   fmt.Sprintf("index %q data type", idx.Field),
			Expected:   want.String(),
			Actual:     live.GetDataType().String(),
			Observed:   "Qdrant",
		}
	}
	// is_tenant is a keyword-index property; on any other type there is nothing to compare and the
	// manifest cannot ask for it either.
	if idx.Kind != schema.FieldTypeKeyword {
		return nil
	}
	if got := live.GetParams().GetKeywordIndexParams().GetIsTenant(); got != idx.IsTenant {
		return &DriftError{
			Collection: collection,
			Property:   fmt.Sprintf("index %q is_tenant", idx.Field),
			Expected:   strconv.FormatBool(idx.IsTenant),
			Actual:     strconv.FormatBool(got),
			Observed:   "Qdrant",
		}
	}
	return nil
}
