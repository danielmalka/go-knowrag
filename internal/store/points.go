package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/qdrant/go-client/qdrant"

	"github.com/danielmalka/go-knowrag/internal/ingest"
	"github.com/danielmalka/go-knowrag/internal/schema"
)

// scrollPageSize bounds one Scroll page. A note produces a handful of chunks, so one page is
// normally the whole answer; the pagination below exists for the orphan case, where a note that
// shrank from many chunks can still have an arbitrary tail in the index.
const scrollPageSize = 256

// PointAPI is the slice of the Qdrant client Client uses. Same shape and same reason as QdrantAPI
// in apply.go: the contract worth testing is "which calls, carrying what, in what order", and an
// interface is what lets that be proven without a container.
type PointAPI interface {
	Upsert(ctx context.Context, request *qdrant.UpsertPoints) (*qdrant.UpdateResult, error)
	Delete(ctx context.Context, request *qdrant.DeletePoints) (*qdrant.UpdateResult, error)
	ScrollAndOffset(ctx context.Context, request *qdrant.ScrollPoints) ([]*qdrant.RetrievedPoint, *qdrant.PointId, error)
}

// Client is point CRUD against one collection.
//
// The collection is bound at construction rather than passed per call, so no caller upstream ever
// names a collection — `interno` included. That is the same rule schema.Manifest() is built on: the
// pipeline is parameterized over the three collections, and a package that could name one could
// grow a special case for the one that happens to hold data.
//
// There is deliberately no search method here, and none may be added: search belongs to S07's
// retrieval package, and a query path that grew in here would be a second place where the mandatory
// tenant_id filter has to be remembered.
type Client struct {
	api        PointAPI
	collection string
}

// NewClient binds an API to a collection.
func NewClient(api PointAPI, collection string) (*Client, error) {
	if api == nil {
		return nil, fmt.Errorf("store: PointAPI is nil")
	}
	if collection == "" {
		return nil, fmt.Errorf("store: no collection name")
	}
	return &Client{api: api, collection: collection}, nil
}

// Client is what internal/ingest talks to. The assertion lives here rather than in ingest because
// ingest defines the interface and must not import this package — it would drag the Qdrant client
// into a package that internal/archtest forbids it in.
var _ ingest.Store = (*Client)(nil)

// UpsertPoints writes points and reports whether Qdrant confirmed the write.
//
// wait=true is not optional here: the whole insert-then-prune order (ADR-006) rests on knowing the
// write landed before the delete is issued, and an acknowledged-but-not-applied write is exactly
// the ambiguous case the outcome distinguishes.
//
// The mapping from Qdrant's answer to the outcome is deliberately conservative. Only
// UpdateStatus_Completed with no error is confirmed; a WaitTimeout — which is what wait=true
// reports when the write did not finish in time — is ambiguous, because the write may still land
// afterwards. Only an explicit RPC error is reported as failed, and even that never authorizes a
// prune (see ingest.ProcessNote): the distinction exists so the report can say which happened.
func (c *Client) UpsertPoints(
	ctx context.Context,
	tenantID string,
	uid uuid.UUID,
	points []ingest.Point,
) (ingest.UpsertOutcome, error) {
	structs := make([]*qdrant.PointStruct, len(points))
	for i, p := range points {
		payload, err := marshalPayload(p.Fields, p.PointHash)
		if err != nil {
			return ingest.UpsertFailed, fmt.Errorf("%s/%s chunk %d: %w", c.collection, uid, p.ChunkIndex, err)
		}
		structs[i] = &qdrant.PointStruct{
			Id: qdrant.NewIDUUID(schema.PointID(tenantID, uid, p.ChunkIndex).String()),
			Vectors: qdrant.NewVectorsMap(map[string]*qdrant.Vector{
				schema.DenseVectorName:  qdrant.NewVectorDense(p.Vector.Dense),
				schema.SparseVectorName: qdrant.NewVectorSparse(p.Vector.Sparse.Indices, p.Vector.Sparse.Values),
			}),
			Payload: payload,
		}
	}

	res, err := c.api.Upsert(ctx, &qdrant.UpsertPoints{
		CollectionName: c.collection,
		Points:         structs,
		Wait:           qdrant.PtrOf(true),
	})
	if err != nil {
		return ingest.UpsertFailed, fmt.Errorf("upserting %d point(s) for %s/%s: %w",
			len(points), c.collection, uid, err)
	}
	if status := res.GetStatus(); status != qdrant.UpdateStatus_Completed {
		return ingest.UpsertAmbiguous, fmt.Errorf(
			"upserting %d point(s) for %s/%s: Qdrant answered %s, not Completed, so the write is not "+
				"confirmed and nothing may be pruned on top of it (ADR-006 §2)",
			len(points), c.collection, uid, status)
	}
	return ingest.UpsertConfirmed, nil
}

// DeleteByFilter removes the points of tenantID+uid whose chunk_index is >= fromChunkIndex.
//
// The tenant_id condition is a correctness requirement, not defence in depth. tenant_id is part of
// the point ID (schema.PointID), so two tenants in one collection may legitimately hold the same
// uid; a delete scoped by uid alone would take the neighbour's points with it (ADR-006 §2).
//
// chunk_index is a range condition, which is why the manifest indexes it as an integer — Qdrant
// refuses a range filter on a keyword index, and strict mode refuses it on no index at all.
func (c *Client) DeleteByFilter(ctx context.Context, tenantID string, uid uuid.UUID, fromChunkIndex int) error {
	_, err := c.api.Delete(ctx, &qdrant.DeletePoints{
		CollectionName: c.collection,
		Points: qdrant.NewPointsSelectorFilter(&qdrant.Filter{
			Must: append(scopeConditions(tenantID, uid), qdrant.NewRange("chunk_index", &qdrant.Range{
				Gte: qdrant.PtrOf(float64(fromChunkIndex)),
			})),
		}),
		Wait: qdrant.PtrOf(true),
	})
	if err != nil {
		return fmt.Errorf("pruning chunk_index >= %d for %s/%s: %w", fromChunkIndex, c.collection, uid, err)
	}
	return nil
}

// ScrollByUID returns every point of tenantID+uid, payload only.
//
// It reads the full payload rather than just point_hash because condition 4 of the integrity
// predicate compares the stored fields directly, and the read has already happened by then — the
// extra fields are free (ADR-006 §5). Vectors are not read: they are large, and nothing in the
// predicate looks at them (point_hash proves structural integrity, not vector integrity —
// PRD-contrato §2.4).
//
// Pagination is a loop rather than a single large limit because "how many points does this uid
// have" is precisely the question being asked — a note that shrank can have an arbitrary tail, and
// a fixed limit that silently truncated it would report the orphans as absent and make the note
// look integral.
func (c *Client) ScrollByUID(ctx context.Context, tenantID string, uid uuid.UUID) ([]ingest.PointRecord, error) {
	var out []ingest.PointRecord
	var offset *qdrant.PointId

	for {
		page, next, err := c.api.ScrollAndOffset(ctx, &qdrant.ScrollPoints{
			CollectionName: c.collection,
			Filter:         &qdrant.Filter{Must: scopeConditions(tenantID, uid)},
			Limit:          qdrant.PtrOf(uint32(scrollPageSize)),
			Offset:         offset,
			WithPayload:    qdrant.NewWithPayload(true),
			WithVectors:    qdrant.NewWithVectors(false),
		})
		if err != nil {
			return nil, fmt.Errorf("scrolling %s/%s: %w", c.collection, uid, err)
		}
		for _, p := range page {
			rec, err := unmarshalPayload(p.GetPayload())
			if err != nil {
				return nil, fmt.Errorf("scrolling %s/%s: point %s: %w", c.collection, uid, p.GetId(), err)
			}
			out = append(out, rec)
		}
		if next == nil || len(page) == 0 {
			return out, nil
		}
		offset = next
	}
}

// scopeConditions is the tenant_id + uid scope, spelled once so no call site can forget half of it.
func scopeConditions(tenantID string, uid uuid.UUID) []*qdrant.Condition {
	return []*qdrant.Condition{
		qdrant.NewMatchKeyword("tenant_id", tenantID),
		qdrant.NewMatchKeyword("uid", uid.String()),
	}
}

// ScrollTenant returns every point of tenantID, grouped by uid.
//
// It exists because the integrity check is a read of the whole corpus asked one note at a time: a
// re-ingestion that changes nothing still paid one round trip per note, and against a Qdrant across
// a 257 ms link that made re-verifying 730 unchanged notes take 477 s against NFR-5's 60 s budget.
// The scroll is the same data by the same filter; what changes is that it crosses the network in
// pages instead of in notes.
//
// Grouping happens here rather than at the caller because the caller's question is per uid, and a
// flat slice would make every caller write the same grouping loop. A uid the tenant has no points
// for is simply absent from the map, which is the same answer ScrollByUID gives as an empty slice.
func (c *Client) ScrollTenant(ctx context.Context, tenantID string) (map[uuid.UUID][]ingest.PointRecord, error) {
	out := map[uuid.UUID][]ingest.PointRecord{}
	var offset *qdrant.PointId

	for {
		page, next, err := c.api.ScrollAndOffset(ctx, &qdrant.ScrollPoints{
			CollectionName: c.collection,
			Filter: &qdrant.Filter{
				Must: []*qdrant.Condition{qdrant.NewMatchKeyword("tenant_id", tenantID)},
			},
			Limit:       qdrant.PtrOf(uint32(scrollPageSize)),
			Offset:      offset,
			WithPayload: qdrant.NewWithPayload(true),
			WithVectors: qdrant.NewWithVectors(false),
		})
		if err != nil {
			return nil, fmt.Errorf("scrolling tenant %s in %s: %w", tenantID, c.collection, err)
		}
		for _, p := range page {
			rec, err := unmarshalPayload(p.GetPayload())
			if err != nil {
				return nil, fmt.Errorf("scrolling tenant %s in %s: point %s: %w",
					tenantID, c.collection, p.GetId(), err)
			}
			// uid comes back through the payload, not through PointRecord: the record type is
			// deliberately keyed by nothing, because every other read already knows which uid it
			// asked for. This is the one read that does not, so it recovers the key here rather
			// than widening the type for one caller.
			raw, _ := rec.Fields["uid"].(string)
			id, perr := uuid.Parse(raw)
			if perr != nil {
				return nil, fmt.Errorf("scrolling tenant %s in %s: point %s has uid %q: %w",
					tenantID, c.collection, p.GetId(), raw, perr)
			}
			out[id] = append(out[id], rec)
		}
		if next == nil || len(page) == 0 {
			return out, nil
		}
		offset = next
	}
}
