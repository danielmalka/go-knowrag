package embed

import (
	"errors"
	"fmt"
)

// ErrBackend marks a failure to get an answer out of the embedding service at all — connection
// refused, a server error that survived every retry, a malformed response. It is deliberately
// distinct from the validation errors (validate.go): those mean the service answered and the answer was
// wrong, this means there is no answer.
//
// S06a and S07 both need that distinction. A retrieval query that cannot reach the embedder is an
// availability problem to report to the caller; a query whose vector failed validation is a
// correctness problem in the backend. Neither is ever a zero vector.
var ErrBackend = errors.New("embedding backend unavailable")

// ItemError locates a per-item failure inside a batch. Without it a caller learns only that "the
// batch failed", which S06a cannot resolve to a note and a chunk_index.
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
