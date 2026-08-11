package ingest

import (
	"context"

	"github.com/google/uuid"
)

// BulkScroller is the optional half of Store: a store that can read a whole tenant in pages instead
// of one note at a time.
//
// It is a separate, optionally-implemented interface rather than a method on Store because every
// fake in every test satisfies Store, and widening Store would break all of them to buy an
// optimization none of them need. A store that does not implement this keeps the exact behaviour it
// had — one ScrollByUID per note — so the batch path is an accelerator, never a second semantics.
type BulkScroller interface {
	ScrollTenant(ctx context.Context, tenantID string) (map[uuid.UUID][]PointRecord, error)
}

// prefetchedStore answers ScrollByUID from a snapshot taken once, and forwards every write
// untouched.
//
// The write path is deliberately not wrapped: this type exists to change how the pipeline *learns*
// the current state, never how it changes it. That containment is what makes the optimization safe
// to land — the worst a stale or wrong snapshot can do is make a note look non-integral and get
// re-embedded, which costs time. It cannot lose a point, because it never decides what to write.
type prefetchedStore struct {
	Store
	byUID map[uuid.UUID][]PointRecord
}

// ScrollByUID serves the snapshot. A uid absent from it has no points, which is the same answer the
// per-note scroll gives — and the reason this returns nil rather than falling through to the store:
// falling through would reintroduce exactly the round trip this type exists to remove, for every
// note that is new.
func (p prefetchedStore) ScrollByUID(_ context.Context, _ string, uid uuid.UUID) ([]PointRecord, error) {
	return p.byUID[uid], nil
}

// withPrefetch returns a store whose reads come from one snapshot, when the underlying store can
// produce one. Any failure to take the snapshot returns the original store rather than an error:
// the batch is still correct without it, only slower, and failing an ingestion over a missed
// optimization would trade a working slow path for no path at all.
//
// The snapshot is taken once, before the batch, and is not refreshed. That is sound only while this
// process is the single writer for the scope, and nothing enforces that today: ADR-005 specifies a
// local flock for exactly this, S06b carries the tasks for it, and it has not been built. Until it
// is, "single writer" is operator discipline, not an invariant — two concurrent ingestions of the
// same scope make this snapshot a view of a state that no longer holds, and ADR-006's
// insert-then-prune can then delete points the other run just confirmed.
//
// An earlier version of this comment named the lock as the thing that made the snapshot sound. It
// was describing a design, not the code, and a comment that asserts a guarantee nobody implemented
// is worse than no comment: it answers the question a reader would otherwise have gone and checked.
// Tracked as D-31.
func withPrefetch(ctx context.Context, d Deps) Deps {
	bulk, ok := d.Store.(BulkScroller)
	if !ok {
		return d
	}
	snapshot, err := bulk.ScrollTenant(ctx, d.TenantID)
	if err != nil {
		return d
	}
	d.Store = prefetchedStore{Store: d.Store, byUID: snapshot}
	return d
}
