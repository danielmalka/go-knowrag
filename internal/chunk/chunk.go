package chunk

import (
	"context"
	"fmt"
	"slices"

	"github.com/danielmalka/go-knowrag/internal/vault"
)

// Chunk is one embeddable unit of a note.
//
// Text is the exact string that gets embedded and that S06a hashes into point_hash
// (PRD-contrato §2.4): the breadcrumb, a blank line, then the note's own bytes verbatim. Breadcrumb
// is the same path in list form, which is the payload's `headings` field. Oversize is the payload's
// `oversize` boolean — a plain value, not an internal marker: it says the chunk is above the
// configured ceiling and could not be cut legally.
//
// There is deliberately no hash field. The four-hash design collapsed into one point_hash assembled
// by S06a (ADR-004); a fingerprint computed here would be the second one that decision removed.
type Chunk struct {
	Text       string
	Index      int
	Breadcrumb []string
	Oversize   bool
}

// ChunkNote splits one note into ordered, embeddable chunks.
//
// The pipeline is: find the atomic units (fences, then tables inside no fence), split at `##`
// boundaries that fall outside them, clamp — H3 split above the ceiling, merge below the floor —
// then classify each result against the ceiling and the model's hard window.
//
// Determinism is structural rather than tested-in. Every chunk's body is a byte range of note.Body,
// carried as offsets and sliced exactly once at the end; nothing is assembled in a shared buffer,
// no map is iterated, and no step rewrites the text. Two runs over the same note therefore produce
// the same indices and byte-identical text, which is what keeps S06a's point_hash stable on
// unchanged input — and editing one line can only change the one range containing it.
//
// note.Body arrives LF-only from S02 (PRD-contrato §2.4). Nothing here normalizes it again: a
// second normalization site is a second rule, free to drift from the first and to make the text
// S03 hands over stop matching the text S02 delivered.
//
// It returns no chunks at all for a note whose body is empty or whitespace — there is nothing to
// embed, and a point holding "\n\n" is noise in the index. A note with content always yields at
// least one chunk, even with no heading of any level.
func ChunkNote(ctx context.Context, note vault.Note, cfg Config, tc TokenCounter) ([]Chunk, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", note.Path, err)
	}

	body := note.Body
	fences := scanFences(body)
	units := mergeUnits(fences, scanTables(body, fences))

	pieces, err := clamp(ctx, body, splitSections(body, units), cfg, tc)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", note.Path, err)
	}

	chunks := make([]Chunk, 0, len(pieces))
	for i, p := range pieces {
		// p.Tokens is the count the clamp already took of this exact text — same range, same
		// breadcrumb, same string. Asking again would be a second network round trip per chunk under
		// ADR-001's counter, for an answer that cannot differ.
		//
		// The hard-limit refusal aborts the whole note rather than dropping one chunk: a note
		// indexed with a hole in it is indistinguishable from a note that never had that content.
		oversize, err := classifyOversize(note.Path, p.Breadcrumb, p.Tokens, cfg)
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, Chunk{
			Text:       PrefixBreadcrumb(p.Breadcrumb, body[p.Start:p.End]),
			Index:      i,
			Breadcrumb: slices.Clone(p.Breadcrumb),
			Oversize:   oversize,
		})
	}
	if len(chunks) == 0 {
		return nil, nil
	}
	return chunks, nil
}
