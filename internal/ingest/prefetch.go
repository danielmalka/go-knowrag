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
	// ScrollTenant returns every point of tenantID, grouped by uid.
	//
	// **A nil error means the map is complete**, and that is a hard requirement of this interface
	// rather than a description of one implementation. There is no partial marker: a caller cannot
	// tell a whole tenant from most of one, so an implementation that returned the pages it managed
	// to read alongside a nil error would be reporting a smaller corpus as the truth.
	//
	// What that costs is deletion. ScanOrphans (orphans.go) treats every uid *absent* from this map
	// as a note that no longer exists on disk, and --prune deletes exactly those — so a page quietly
	// dropped here is a set of live notes erased from the index. An implementation that cannot
	// guarantee completeness must return an error and discard what it has; the run degrades to the
	// per-note path and reports the orphan scan as not run, which is the safe answer.
	// *store.Client satisfies this by returning on the first failure of any kind, accumulator
	// included (internal/store/points.go).
	ScrollTenant(ctx context.Context, tenantID string) (map[uuid.UUID][]PointRecord, error)
}

// prefetchedStore answers ScrollByUID from a snapshot taken once, and forwards every write
// untouched.
//
// The write path is deliberately not wrapped: this type exists to change how the pipeline *learns*
// the current state, never how it changes it.
//
// That containment used to be the whole safety argument — the worst a wrong snapshot could do was
// make a note look non-integral and get re-embedded, which costs time and cannot lose a point. **That
// stopped being true when the snapshot gained a second consumer.** ScanOrphans (orphans.go) reads the
// same map to decide which uids no longer exist on disk, and --prune deletes them: a snapshot missing
// a live note now names that note as deleted. The read/write split still holds for this type, and it
// is no longer sufficient on its own — what carries the safety is BulkScroller's completeness
// contract above, and this comment is here to say so out loud, because the sentence it replaces was
// true for months and went false without a line of this file changing.
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
// produce one, plus that snapshot and whether it was actually taken. Any failure to take it returns
// the original store rather than an error: the batch is still correct without it, only slower, and
// failing an ingestion over a missed optimization would trade a working slow path for no path at all.
//
// The third return value is not redundant with a nil map, and the difference is the whole reason it
// exists. For the integrity check, "no snapshot" and "an empty snapshot" are the same thing — both
// mean read per note, and both are correct. For the orphan scan (orphans.go) they are opposites:
// an empty snapshot says the tenant has no points, a missing one says nobody looked. Collapsing the
// two would report zero orphans for a scroll that failed, which reads as "nothing was deleted" and
// is the one answer that is never safe to guess.
//
// The snapshot is taken once, before the batch, and is not refreshed. That is sound only while this
// process is the single writer for the scope. On this machine it is enforced: cmd/cli takes a local
// flock keyed by endpoint + collection + tenant before it scans anything (internal/ingest/lock,
// ADR-005), so a second ingestion of the same scope is refused rather than interleaved. Without it,
// ADR-006's insert-then-prune has one run deleting points the other just confirmed, and this
// snapshot becomes a view of a state that no longer holds.
//
// It does not cross machines, and the difference matters to anyone reading this line to decide
// whether the snapshot is safe. A second host with the binary, the credential and a route to Qdrant
// is physically an executor; nothing here stops it. Against that, "single writer" is still an
// agreement (ADR-005 §8).
//
// Two earlier versions of this comment were wrong in opposite directions — one named the lock as
// what made the snapshot sound while no lock existed, the other kept saying so after it was built.
// The lesson is the same either way: this sentence describes a mechanism in another package, so it
// goes stale silently and is worth re-reading whenever that package moves.
func withPrefetch(ctx context.Context, d Deps) (Deps, map[uuid.UUID][]PointRecord, bool) {
	bulk, ok := d.Store.(BulkScroller)
	if !ok {
		return d, nil, false
	}
	snapshot, err := bulk.ScrollTenant(ctx, d.TenantID)
	if err != nil {
		return d, nil, false
	}
	d.Store = prefetchedStore{Store: d.Store, byUID: snapshot}
	return d, snapshot, true
}
