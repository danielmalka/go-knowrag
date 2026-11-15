package embed

import (
	"fmt"
	"math"

	"github.com/danielmalka/go-knowrag/internal/schema"
)

// validateEmbedding reports the first invariant the embedding breaks, evaluated on the backend's
// response exactly as it arrived. Every check names the field and the offending position, because
// "invalid embedding" over a 3000-chunk run is not actionable.
//
// The invariants are PRD-contrato §2.3b's, and they are not negotiable here: the dense vector has the
// model profile's width (768 for EmbeddingGemma 2) and every element is finite. It does not mutate its
// input.
func validateEmbedding(e Embedding, m schema.ModelProfile) error {
	if len(e.Dense) != m.DenseWidth() {
		return fmt.Errorf("dense vector has %d elements, want %d (profile %q)", len(e.Dense), m.DenseWidth(), m.ID)
	}
	for i, v := range e.Dense {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return fmt.Errorf("element Dense[%d] is not a finite number (%v)", i, v)
		}
	}
	return nil
}
