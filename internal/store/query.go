package store

import (
	"context"
	"fmt"
	"strconv"

	"github.com/qdrant/go-client/qdrant"

	"github.com/danielmalka/go-knowrag/internal/retrieval"
)

// This file is the one query-shaped thing internal/store owns, and it owns it for a single reason:
// this package holds the Qdrant connection and the Qdrant client import, and both are confined here
// by internal/archtest (S09 T11 invariant 1). Nothing below decides anything about a search.
// internal/retrieval chooses the collection, the filter conditions, the fusion mode, the per-leg
// limits and whether ACORN is on; this file transcribes that finished decision into the wire types
// and hands back what came off the wire. It is the same division internal/schema.CollectionManifest
// already uses for provisioning: the caller says what, this package says how.
//
// The rule that follows from that, and that S07 T8's architecture test enforces: no condition may
// be constructed here that retrieval.SearchRequest did not already ask for. If a future change
// needs a filter this cannot express, the field goes on SearchRequest, not into a branch here.

// QueryAPI is the query slice of the Qdrant client, in the same style and for the same reason as
// PointAPI and QdrantAPI: what is worth testing is which request goes out and what comes back, and
// an interface is what makes that provable without a container. *qdrant.Client satisfies it.
type QueryAPI interface {
	Query(ctx context.Context, request *qdrant.QueryPoints) ([]*qdrant.ScoredPoint, error)
}

// Querier executes searches. It is a separate type from Client on purpose: Client is bound to one
// collection at construction because no ingestion caller may name a collection, whereas a search
// carries its collection in the request (retrieval.Query.Collection), so binding one here would
// mean two disagreeing sources for the same field.
type Querier struct {
	api QueryAPI
}

// NewQuerier wraps an API. It takes the same shape of dependency as NewClient so a test can pass a
// fake and production can pass the shared *qdrant.Client.
func NewQuerier(api QueryAPI) (*Querier, error) {
	if api == nil {
		return nil, fmt.Errorf("store: QueryAPI is nil")
	}
	return &Querier{api: api}, nil
}

// ExecuteQuery sends one already-built request and returns the raw hits.
//
// No filter is added, no filter is removed, and nothing here inspects a condition's meaning: a
// request that arrives without a tenant condition goes out without one. That is not an oversight —
// making it impossible to arrive that way is internal/retrieval's job, and duplicating the check
// here would create a second place the rule lives and therefore a second place it can drift.
func (q *Querier) ExecuteQuery(ctx context.Context, req retrieval.SearchRequest) ([]retrieval.ScoredPoint, error) {
	request, err := queryPoints(req)
	if err != nil {
		return nil, err
	}

	points, err := q.api.Query(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("querying %s: %w", req.Collection, err)
	}

	out := make([]retrieval.ScoredPoint, len(points))
	for i, p := range points {
		payload := make(map[string]any, len(p.GetPayload()))
		for k, v := range p.GetPayload() {
			value, verr := payloadValue(v)
			if verr != nil {
				return nil, fmt.Errorf("querying %s: point %s: field %q: %w",
					req.Collection, pointID(p.GetId()), k, verr)
			}
			payload[k] = value
		}
		out[i] = retrieval.ScoredPoint{ID: pointID(p.GetId()), Score: p.GetScore(), Payload: payload}
	}
	return out, nil
}

// queryPoints transcribes a SearchRequest. Every field it sets comes from the request; the only
// thing it supplies on its own is WithVectors(false), because nothing reads vectors back and they
// are three orders of magnitude larger than the payload.
func queryPoints(req retrieval.SearchRequest) (*qdrant.QueryPoints, error) {
	out := &qdrant.QueryPoints{
		CollectionName: req.Collection,
		Filter:         qdrantFilter(req.Filter),
		Limit:          qdrant.PtrOf(wireCount(req.Limit)),
		Offset:         qdrant.PtrOf(wireCount(req.Offset)),
		WithPayload:    withPayload(req.PayloadFields),
		WithVectors:    qdrant.NewWithVectors(false),
	}
	// Refused rather than resolved. The two describe different searches, and honouring either one
	// would send a query the caller did not build — the failure mode this whole file exists to make
	// impossible. It is a check on the request's own shape, not a filter condition invented here.
	if len(req.Dense) > 0 && (len(req.Prefetch) > 0 || req.FusionRRF) {
		return nil, fmt.Errorf("building the query for %s: the request carries both a top-level "+
			"dense query and %d prefetch leg(s) (fusion=%t); exactly one of the two is a search",
			req.Collection, len(req.Prefetch), req.FusionRRF)
	}

	switch {
	case req.FusionRRF:
		// nil leaves k and the per-leg weights at the server's defaults, which is what "RRF" with no
		// further qualification means in PRD-contrato §2.3b.
		out.Query = qdrant.NewQueryRRF(nil)
	case len(req.Dense) > 0:
		out.Query = qdrant.NewQueryDense(req.Dense)
		out.Using = qdrant.PtrOf(req.DenseVector)
	}
	if req.Acorn {
		// Same enable-only shape as a prefetch leg's, and for the same reason (PRD-contrato §2.3c):
		// MaxSelectivity stays at the server default because Qdrant estimates cardinality per
		// request and this process cannot.
		out.Params = &qdrant.SearchParams{Acorn: &qdrant.AcornSearchParams{Enable: qdrant.PtrOf(true)}}
	}
	for i, leg := range req.Prefetch {
		p, err := prefetchQuery(leg)
		if err != nil {
			return nil, fmt.Errorf("building the query for %s: prefetch %d: %w", req.Collection, i, err)
		}
		out.Prefetch = append(out.Prefetch, p)
	}
	return out, nil
}

func prefetchQuery(leg retrieval.PrefetchLeg) (*qdrant.PrefetchQuery, error) {
	out := &qdrant.PrefetchQuery{
		Using:  qdrant.PtrOf(leg.Vector),
		Filter: qdrantFilter(leg.Filter),
		Limit:  qdrant.PtrOf(wireCount(leg.Limit)),
	}

	dense, sparse := len(leg.Dense) > 0, len(leg.Sparse.Indices) > 0
	switch {
	case dense && !sparse:
		out.Query = qdrant.NewQueryDense(leg.Dense)
	case sparse && !dense:
		out.Query = qdrant.NewQuerySparse(leg.Sparse.Indices, leg.Sparse.Values)
	default:
		return nil, fmt.Errorf("leg %q carries %d dense element(s) and %d sparse index(es); "+
			"exactly one of the two is a query", leg.Vector, len(leg.Dense), len(leg.Sparse.Indices))
	}

	if leg.Acorn {
		// Enable only. MaxSelectivity stays at the server default because Qdrant decides per request
		// from a cardinality estimate this process does not have (PRD-contrato §2.3c).
		out.Params = &qdrant.SearchParams{Acorn: &qdrant.AcornSearchParams{Enable: qdrant.PtrOf(true)}}
	}
	return out, nil
}

func qdrantFilter(f retrieval.Filter) *qdrant.Filter {
	if len(f.Must) == 0 && len(f.MustNot) == 0 {
		return nil
	}
	out := &qdrant.Filter{}
	for _, c := range f.Must {
		out.Must = append(out.Must, qdrant.NewMatchKeyword(c.Field, c.Value))
	}
	for _, c := range f.MustNot {
		out.MustNot = append(out.MustNot, qdrant.NewMatchKeyword(c.Field, c.Value))
	}
	return out
}

func withPayload(fields []string) *qdrant.WithPayloadSelector {
	if len(fields) == 0 {
		return qdrant.NewWithPayload(true)
	}
	return qdrant.NewWithPayloadInclude(fields...)
}

// wireCount converts a count to the unsigned width the wire wants. A negative arrives only from a
// request that never went through retrieval.Query.Validate; clamping to zero makes that return
// nothing, which is visible, instead of wrapping to 18 quintillion, which is a full scan.
func wireCount(n int) uint64 {
	if n < 0 {
		return 0
	}
	return uint64(n)
}

// pointID renders either half of the point-ID oneof. Every point this pipeline writes is a UUID
// (schema.PointID), so the numeric branch only ever names a point written by something else — which
// is exactly when an error message needs to identify it.
func pointID(id *qdrant.PointId) string {
	if u := id.GetUuid(); u != "" {
		return u
	}
	return strconv.FormatUint(id.GetNum(), 10)
}
