package ingest

import (
	"context"

	"github.com/google/uuid"

	"github.com/danielmalka/go-knowrag/internal/embed"
)

// NoteState is where one note ended up in a run. The set is closed and it is exactly the one
// PRD-stories-pipeline §3 S06a lists — there is no `payload_only` entry, because the metadata-only
// write path was removed together with the three extra hashes (ADR-004, PRD-contrato §2.4).
//
// The states are ordered by how far the note got: a note that reached StatePruned went through
// StateEmbedded and StateUpsertConfirmed on the way, and only the last one is reported.
type NoteState string

const (
	// StateSkipped is the short-circuit: the uid was structurally integral, so nothing was embedded
	// and nothing was written. This is the state a second run over an unchanged vault must produce
	// for every note.
	StateSkipped NoteState = "skipped"
	// StateEmbedded means the note was rechunked and reembedded but the upsert has not been
	// confirmed yet. As a terminal state it means the run stopped between embed and upsert.
	StateEmbedded NoteState = "embedded"
	// StateUpsertConfirmed means the N points were written and Qdrant said so explicitly. As a
	// terminal state it means the prune was never reached — the benign orphan case of ADR-006 §7.
	StateUpsertConfirmed NoteState = "upsert_confirmed"
	// StatePruned is the only fully successful terminal state: written, confirmed, tail removed.
	StatePruned NoteState = "pruned"
	// StateFailed is any note the run could not finish. It never implies "nothing was written" —
	// a partial upsert lands here too, and the next round converges on it (ADR-006 §4).
	StateFailed NoteState = "failed"
)

// NoteResult is one note's outcome. Path is carried rather than derived so a failure is reportable
// without holding the Note that produced it.
type NoteResult struct {
	Path  string
	UID   uuid.UUID
	State NoteState
	// Chunks is how many chunks the note produced this round. It is 0 for a note that failed
	// before chunking.
	Chunks int
	// Err is set exactly when State is StateFailed.
	Err error
}

// UpsertOutcome is what the store knows about a write it just attempted, and it exists because
// Qdrant offers no multi-point transaction (ADR-006 §4): "the request errored" and "the request
// may or may not have landed" are different facts, and only one of them is safe to prune on.
//
// The zero value is UpsertAmbiguous on purpose. A Store implementation — or a fake — that forgets
// to set an outcome must not accidentally authorize a prune, because pruning on top of an
// unconfirmed write is how data is lost.
type UpsertOutcome int

const (
	// UpsertAmbiguous: the server did not say the write completed. A timeout is the canonical case.
	// Never prune on this.
	UpsertAmbiguous UpsertOutcome = iota
	// UpsertConfirmed: the server said the write completed. The only outcome that authorizes a prune.
	UpsertConfirmed
	// UpsertFailed: the call returned an explicit error. Never prune on this either — the
	// distinction from ambiguous exists for the report, not for the control flow.
	UpsertFailed
)

func (o UpsertOutcome) String() string {
	switch o {
	case UpsertConfirmed:
		return "confirmed"
	case UpsertFailed:
		return "failed"
	default:
		return "ambiguous"
	}
}

// Point is one chunk ready to be written: identity, payload, fingerprint and both vector halves.
//
// Fields carries the §2.4 payload minus point_hash, which travels in its own field so no caller can
// accidentally fold the fingerprint into the field-by-field comparison it is supposed to be
// independent of (condition 4, integrity.go).
type Point struct {
	ChunkIndex int
	PointHash  string
	Fields     map[string]any
	Vector     embed.Embedding
}

// PointRecord is one point as read back from the store: payload only, no vectors. The full payload
// is read rather than just the hash because condition 4 compares the stored fields directly, and
// the read has already happened by then — the extra fields cost nothing (ADR-006 §5).
type PointRecord struct {
	ChunkIndex int
	PointHash  string
	Fields     map[string]any
}

// ExpectedPoint is what a point *should* look like, computed from the note on disk. It carries no
// vector, and that is the whole reason it is a separate type from Point: the integrity check runs
// *before* the embedder is called, so anything it compares against has to be derivable without one.
type ExpectedPoint struct {
	ChunkIndex int
	PointHash  string
	Fields     map[string]any
}

// Store is the point CRUD this package needs, defined here — by the consumer — so that
// internal/ingest never imports the Qdrant client. *store.Client satisfies it; the compile-time
// assertion lives in internal/store, which is the side that would break.
//
// Every method is scoped by tenantID *and* uid, with no overload that takes uid alone. That is not
// defence in depth: since tenant_id is part of the point ID (schema.PointID), two tenants in one
// collection can legitimately share a uid, so a read or a delete keyed on bare uid would report or
// destroy the neighbour's data (ADR-006 §2).
type Store interface {
	// ScrollByUID returns every point for tenantID+uid, payload only, in no guaranteed order.
	ScrollByUID(ctx context.Context, tenantID string, uid uuid.UUID) ([]PointRecord, error)

	// UpsertPoints writes points for tenantID+uid and reports whether the write is confirmed.
	// A non-nil error and a non-confirmed outcome are independent facts: an ambiguous result can
	// arrive with an error attached, and the caller must gate the prune on the outcome, never on
	// err == nil.
	UpsertPoints(ctx context.Context, tenantID string, uid uuid.UUID, points []Point) (UpsertOutcome, error)

	// DeleteByFilter removes the points of tenantID+uid whose chunk_index is >= fromChunkIndex.
	DeleteByFilter(ctx context.Context, tenantID string, uid uuid.UUID, fromChunkIndex int) error
}
