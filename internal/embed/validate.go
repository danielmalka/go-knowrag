package embed

import (
	"fmt"
	"math"
)

// validateEmbedding reports the first invariant the embedding breaks, evaluated on the backend's
// response exactly as it arrived. Every check names the field and the offending position, because
// "invalid embedding" over a 3000-chunk run is not actionable.
//
// The invariants are PRD-contrato §2.3b's, and they are not negotiable here: 1024 dense elements,
// all finite; sparse indices and values the same length, non-empty, strictly ascending, all
// weights non-zero and finite. The two degenerate-sparse classes additionally wrap a sentinel
// (see errors.go) so the caller can attribute them to one note.
//
// Ordering is checked, never imposed. scripts/embedder-service/server.py sorts by token id and
// drops non-positive weights before it answers, so all three sparse rules are a contract between
// the two sides; re-sorting here would swallow the day the server stops keeping it, and an unstable
// sparse order makes an unchanged note look changed to point_hash.
//
// It does not mutate its input on either path.
func validateEmbedding(e Embedding) error {
	if len(e.Dense) != DenseDim {
		return fmt.Errorf("dense vector has %d elements, want %d", len(e.Dense), DenseDim)
	}
	for i, v := range e.Dense {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return fmt.Errorf("element Dense[%d] is not a finite number (%v)", i, v)
		}
	}

	s := e.Sparse
	if len(s.Indices) != len(s.Values) {
		return fmt.Errorf(
			"sparse vector length mismatch: %d indices, %d values", len(s.Indices), len(s.Values))
	}
	// An empty sparse vector is what a backend that silently dropped its sparse head returns. It
	// breaks no per-element rule, which is exactly why it needs its own check: hybrid search with
	// RRF fusion (PRD-contrato §2.3b) would degrade to dense-only and nothing would say so.
	if len(s.Indices) == 0 {
		return fmt.Errorf("sparse vector is empty; BGE-M3 produces dense and sparse in one pass, " +
			"so an empty sparse half means the backend is not serving the sparse head")
	}

	for i := 1; i < len(s.Indices); i++ {
		if s.Indices[i] == s.Indices[i-1] {
			return fmt.Errorf("Sparse.Indices[%d]: token index %d is repeated: %w",
				i, s.Indices[i], ErrSparseRepeatedIndex)
		}
		if s.Indices[i] < s.Indices[i-1] {
			return fmt.Errorf(
				"Sparse.Indices[%d] = %d is below Sparse.Indices[%d] = %d; indices must be strictly "+
					"ascending after sorting, which they cannot be unless the response was malformed",
				i, s.Indices[i], i-1, s.Indices[i-1])
		}
	}
	for i, v := range s.Values {
		if v == 0 {
			return fmt.Errorf("Sparse.Values[%d] (token index %d) is zero: %w",
				i, s.Indices[i], ErrSparseZeroWeight)
		}
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return fmt.Errorf("Sparse.Values[%d] is not a finite number (%v)", i, v)
		}
	}
	return nil
}
