package embed

import (
	"errors"
	"fmt"
)

// ErrSparseZeroWeight and ErrSparseRepeatedIndex mark the two *degenerate-sparse* violation
// classes. Both are hard errors here — nothing in this package repairs a vector, and either one
// fails the whole batch (S04 Escopo: "Erro invalida o batch inteiro — nunca retorna resultado
// parcial").
//
// They are sentinels, and no other violation class matches them, because the caller has to tell two
// situations apart that a bare error cannot distinguish:
//
//   - a degenerate vector for one chunk — S06a moves that chunk's *note* to `failed`, logs it, and
//     carries on with the run;
//   - a broken backend (wrong dimension, NaN, count mismatch) — that is not a note-level problem
//     and must not be skipped past.
//
// The unit S06a skips is the note, never a lone chunk: a note missing chunk 3 fails S06a's
// structural-integrity predicate (points must cover chunk_index 0..N-1, count exactly N) on every
// later run, so it would never converge.
var (
	ErrSparseZeroWeight    = errors.New("sparse vector contains a zero weight")
	ErrSparseRepeatedIndex = errors.New("sparse vector contains a repeated token index")
)

// ErrBackend marks a failure to get an answer out of the embedding service at all — connection
// refused, a server error that survived every retry, a malformed response. It is deliberately
// distinct from the validation errors above: those mean the service answered and the answer was
// wrong, this means there is no answer.
//
// S06a and S07 both need that distinction. A retrieval query that cannot reach the embedder is an
// availability problem to report to the caller; a query whose vector failed validation is a
// correctness problem in the backend. Neither is ever a zero vector.
var ErrBackend = errors.New("embedding backend unavailable")

// ItemError locates a per-item failure inside a batch. Without it a caller learns only that "the
// batch failed", which S06a cannot resolve to a note and a chunk_index — and resolving it is the
// whole point of the sentinels above.
//
// Index is the position in the *original* EmbedDocuments input, not in whatever sub-batch the
// request was split into; the caller never sees sub-batches and cannot map them back.
type ItemError struct {
	Index int
	Err   error
}

func (e *ItemError) Error() string {
	return fmt.Sprintf("embedding for input item %d: %v", e.Index, e.Err)
}

func (e *ItemError) Unwrap() error { return e.Err }
