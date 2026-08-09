package chunk

import (
	"fmt"

	"github.com/danielmalka/go-knowrag/internal/schema"
)

// HardTokenLimit is BGE-M3's context window (PRD-contrato §2.3b), in tokens. It is not a tuning
// knob: a chunk above it cannot be embedded at all, so the pipeline refuses it by name instead of
// handing the embedder something it would silently truncate.
const HardTokenLimit = 8192

// Version identifies the chunking algorithm, and is one of the "pipeline config" inputs S06a hashes
// into point_hash (PRD-contrato §2.4) — bumping it must change point_hash for every point.
//
// It is an alias of schema.ChunkerVersion rather than its own literal. S00 already declared that
// constant for exactly this purpose, and two independent literals spelling "v1" are two places to
// bump: the day only one of them moves, half the pipeline believes the chunker changed and half
// does not, which is the same silent-drift failure the single hash exists to prevent.
const Version = schema.ChunkerVersion

// Config is the calibrated clamp (PRD-contrato §2.8's 256–1024 starting range; the real values come
// from S03 T10's calibration against the corpus). Both bounds are counted on the
// breadcrumb-prefixed text, never the bare body — the breadcrumb is embedded too, so it occupies
// the window.
//
// There is deliberately no DefaultConfig(): the floor and the ceiling are outputs of the calibration
// report, and a default compiled in before that report exists would be a number nobody measured
// that every caller inherits.
type Config struct {
	FloorTokens   int
	CeilingTokens int
}

// Validate rejects a config that cannot describe a legal chunk. Every rule here fails loudly at the
// top of ChunkNote rather than producing surprising chunks halfway through a 731-note run.
func (c Config) Validate() error {
	if c.FloorTokens <= 0 {
		return fmt.Errorf("chunk config: FloorTokens = %d, must be positive", c.FloorTokens)
	}
	if c.CeilingTokens <= 0 {
		return fmt.Errorf("chunk config: CeilingTokens = %d, must be positive", c.CeilingTokens)
	}
	if c.FloorTokens > c.CeilingTokens {
		return fmt.Errorf(
			"chunk config: FloorTokens = %d is above CeilingTokens = %d; the merge pass would have to "+
				"cross the ceiling to reach the floor", c.FloorTokens, c.CeilingTokens)
	}
	if c.CeilingTokens > HardTokenLimit {
		return fmt.Errorf(
			"chunk config: CeilingTokens = %d is above the model's %d-token window (PRD-contrato §2.3b)",
			c.CeilingTokens, HardTokenLimit)
	}
	return nil
}
