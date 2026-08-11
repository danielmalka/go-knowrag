package ingest

import (
	"context"
	"errors"
	"fmt"

	"github.com/danielmalka/go-knowrag/internal/chunk"
	"github.com/danielmalka/go-knowrag/internal/embed"
	"github.com/danielmalka/go-knowrag/internal/vault"
)

// Deps is everything ingestion needs from the outside world, in one value so that ProcessNote and
// RunBatch take the same thing and a test wires it once.
//
// Handshake is the config S04's ServiceEmbedder.Handshake *confirmed*, not the config this build
// expects. Whatever process starts a run must call Handshake and abort on error before reaching
// here: a backend quietly serving an unpinned revision would otherwise poison every point_hash in
// the run, and the poisoned points would look integral forever.
type Deps struct {
	TenantID  string
	Store     Store
	Embedder  embed.Embedder
	Handshake embed.BackendHandshakeInfo

	Chunk  chunk.Config
	Tokens chunk.TokenCounter

	// Vaults is the vault scope of the run: which vaults this batch actually looked at on disk.
	//
	// It is not used to select notes — the caller already did that — and exists for one thing only:
	// bounding the orphan scan (orphans.go). A uid in the index whose vault is not in this list was
	// never checked against a disk, so its absence from the note set is not evidence of anything.
	// Empty means the caller declares no scope, and the orphan scan does not run at all rather than
	// running over everything, because "I did not say" must not read as "all of it" for a comparison
	// whose output authorizes deletion.
	Vaults []string

	// UpsertAttempts bounds the retry of a non-confirmed upsert. Zero means one attempt, no retry.
	//
	// ponytail: no backoff between attempts. Retrying instantly is worth roughly nothing against a
	// server that is down, and worth a lot against a single dropped connection; add a delay when a
	// real run shows the first case dominating.
	UpsertAttempts int
}

// Validate rejects a configuration that would write points nobody can find.
//
// An empty TenantID is the one that matters and the reason this method exists at all: every point
// carries tenant_id, S07's queries filter on it as a mandatory must condition, and a point written
// with an empty one is invisible to every tenant-scoped search while still occupying the index.
// It fails here, once, rather than 730 times as a silent write.
func (d Deps) Validate() error {
	if d.TenantID == "" {
		return errors.New("ingest: TenantID is empty; every point carries tenant_id and a point " +
			"without it is invisible to every tenant-scoped search (PRD-contrato §2.4)")
	}
	if d.Store == nil {
		return errors.New("ingest: Store is nil")
	}
	if d.Embedder == nil {
		return errors.New("ingest: Embedder is nil")
	}
	if d.Tokens == nil {
		return errors.New("ingest: Tokens (chunk.TokenCounter) is nil")
	}
	if err := d.Chunk.Validate(); err != nil {
		return err
	}
	if d.Handshake.ModelRevision == "" {
		return errors.New("ingest: Handshake.ModelRevision is empty; the embedder config that feeds " +
			"point_hash must come from a completed S04 handshake, never from the configured values")
	}
	return nil
}

// ProcessNote runs one note through the state machine and never returns an error: a note that fails
// is a NoteResult carrying its reason, because a batch of 730 notes must not stop at the first bad
// one (T8).
//
// The machine has exactly two destinations after the integrity check, and that is the whole design:
// integral → skipped, anything else → full rechunk and reembed. No branch inspects *which*
// condition failed or which component of the hash diverged. The cheaper metadata-only path that
// used to exist was removed with the three extra hashes (ADR-004), so "not integral" has one
// meaning and one cost.
func ProcessNote(ctx context.Context, d Deps, n vault.Note) NoteResult {
	res := NoteResult{Path: n.Path, UID: n.UID}

	chunks, err := chunk.ChunkNote(ctx, n, d.Chunk, d.Tokens)
	if err != nil {
		return res.failed(fmt.Errorf("chunking: %w", err))
	}
	res.Chunks = len(chunks)

	expected := ExpectedPoints(n, chunks, d.TenantID, NewPipelineConfig(d.Chunk), d.Handshake)

	records, err := d.Store.ScrollByUID(ctx, d.TenantID, n.UID)
	if err != nil {
		return res.failed(fmt.Errorf("reading back the points of %s: %w", n.UID, err))
	}

	if CheckIntegrity(records, expected).Integral {
		// Nothing is written here, deliberately — not even a payload refresh to bring `updated` in
		// line with disk. That refresh would be the removed payload_only branch under a new name,
		// and the accepted cost of the 2026-08-08 decision is precisely that the stored `updated`
		// goes stale (PRD-contrato §2.4).
		res.State = StateSkipped
		return res
	}

	// Everything is embedded before anything is written. An embedder failure therefore leaves the
	// index exactly as it was, which is what makes T9's crash-between-embed-and-upsert benign.
	points, err := embedPoints(ctx, d, expected, chunks)
	if err != nil {
		return res.failed(err)
	}
	res.State = StateEmbedded

	outcome, err := upsertWithRetry(ctx, d, n, points)
	if outcome != UpsertConfirmed {
		// The prune is not reached, and that is the single most load-bearing line in this file.
		// Pruning on top of a write that was not confirmed is how a note loses points it still
		// needs; leaving a stale tail behind is how it keeps serving old text until the next round
		// converges. ADR-006 §3 chose the second, on purpose.
		return res.failed(fmt.Errorf("upsert %s: %w", outcome, err))
	}
	res.State = StateUpsertConfirmed

	if err := d.Store.DeleteByFilter(ctx, d.TenantID, n.UID, len(chunks)); err != nil {
		return res.failed(fmt.Errorf(
			"the %d point(s) were written and confirmed, but pruning chunk_index >= %d failed, so a "+
				"stale tail may remain: %w", len(points), len(chunks), err))
	}
	res.State = StatePruned
	return res
}

// embedPoints embeds every chunk and joins each embedding to the payload already computed for it.
//
// It asserts the count the backend returned rather than zipping optimistically: a response with
// fewer embeddings than texts would otherwise pair chunk i's payload with chunk j's vector, which
// is a corruption no hash catches — point_hash covers the text, not the vector (PRD-contrato §2.4).
func embedPoints(ctx context.Context, d Deps, expected []ExpectedPoint, chunks []chunk.Chunk) ([]Point, error) {
	if len(chunks) == 0 {
		return nil, nil
	}
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Text
	}

	vectors, err := d.Embedder.EmbedDocuments(ctx, texts)
	if err != nil {
		return nil, fmt.Errorf("embedding %d chunk(s): %w", len(texts), err)
	}
	if len(vectors) != len(texts) {
		return nil, fmt.Errorf("the embedder returned %d embedding(s) for %d chunk(s); pairing them "+
			"would attach one chunk's vector to another chunk's payload", len(vectors), len(texts))
	}

	points := make([]Point, len(expected))
	for i, e := range expected {
		points[i] = Point{
			ChunkIndex: e.ChunkIndex,
			PointHash:  e.PointHash,
			Fields:     e.Fields,
			Vector:     vectors[i],
		}
	}
	return points, nil
}

// upsertWithRetry attempts the write up to d.UpsertAttempts times and returns the last outcome.
//
// Retrying is safe because the point ID is deterministic (schema.PointID), so a retry after an
// ambiguous result rewrites the same points rather than duplicating them. The budget is bounded so
// an unreachable Qdrant fails the run instead of hanging it (T16).
//
// A note that produced zero chunks — an empty body, which S03 returns nothing for — skips the call
// entirely and counts as confirmed. There is nothing to write, and the prune that follows with
// fromChunkIndex 0 is what removes the points the note used to have.
func upsertWithRetry(ctx context.Context, d Deps, n vault.Note, points []Point) (UpsertOutcome, error) {
	if len(points) == 0 {
		return UpsertConfirmed, nil
	}

	attempts := max(d.UpsertAttempts, 1)
	var outcome UpsertOutcome
	var err error
	for range attempts {
		outcome, err = d.Store.UpsertPoints(ctx, d.TenantID, n.UID, points)
		if outcome == UpsertConfirmed {
			return outcome, nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	if err == nil {
		err = fmt.Errorf("the store did not confirm the write after %d attempt(s)", attempts)
	}
	return outcome, err
}

func (r NoteResult) failed(err error) NoteResult {
	r.State = StateFailed
	r.Err = err
	return r
}
