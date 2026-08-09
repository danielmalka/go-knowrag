package chunk

import (
	"context"
	"slices"
	"strings"
)

// piece is a candidate chunk before it gets an index and an oversize flag: a byte range of the body
// plus the breadcrumb that will be prefixed to it. Merging two pieces is a range union, never a
// string concatenation — the body is never rebuilt, so no separator is invented and adjacent chunks
// cannot contaminate each other.
//
// Tokens is the breadcrumb-inclusive count of exactly this piece's text, carried forward so the
// number the clamp already paid for is the number the oversize flag reads. Under ADR-001 the
// counter is a network hop; re-measuring identical text downstream would double the tokenization
// round trips of a full ingest, against NFR-4's ≤30 min budget. Zero means "not measured yet" —
// mergePieces guarantees every piece it emits carries a real count, so clamp's callers can rely on
// it. A counter that genuinely answers 0 costs a redundant call, never a wrong decision.
type piece struct {
	Breadcrumb []string
	Start, End int
	Tokens     int
}

// clamp turns sections into the final ordered pieces: split first at H3 what exceeds the ceiling,
// then merge what falls below the floor.
//
// Split-then-merge, in that order, and the order is load-bearing. Splitting an H2 at its H3
// children leaves the H2's own heading and preamble as a first, usually tiny child; running the
// merge afterwards folds it into the H3 that follows (under their deepest common breadcrumb, which
// is the H2's) instead of emitting a chunk containing one heading line. The merge cannot undo the
// split it just made, because a merge that would cross the ceiling is refused — and the two halves
// of an over-ceiling section always do.
func clamp(ctx context.Context, body string, sections []section, cfg Config, tc TokenCounter) ([]piece, error) {
	pieces, err := splitOverCeiling(ctx, body, sections, cfg, tc)
	if err != nil {
		return nil, err
	}
	return mergePieces(ctx, body, pieces, cfg, tc)
}

// splitOverCeiling subdivides at H3 every section whose breadcrumb-inclusive count is above the
// ceiling, and drops sections that hold nothing but whitespace.
//
// A section above the ceiling with no H3 child is emitted whole. That is the contract's only
// remaining option: the boundary set is `##` primary, `###` secondary (PRD-stories-fundacao §3,
// S03) and nothing else — cutting mid-paragraph, or mid-fence, is not a boundary this pipeline is
// allowed to invent. Such a piece reaches oversize classification, which is where the contract says
// it belongs.
func splitOverCeiling(ctx context.Context, body string, sections []section, cfg Config, tc TokenCounter) ([]piece, error) {
	var out []piece
	for _, s := range sections {
		if blank(body[s.Start:s.End]) {
			continue
		}
		n, err := measureTokens(ctx, tc, s.Breadcrumb, body[s.Start:s.End])
		if err != nil {
			return nil, err
		}
		if n <= cfg.CeilingTokens || len(s.Children) == 0 {
			out = append(out, piece{Breadcrumb: s.Breadcrumb, Start: s.Start, End: s.End, Tokens: n})
			continue
		}
		// The children are left unmeasured: the merge pass counts a run of them once, as a union,
		// not one by one. Measuring them here would buy nothing and cost a call per child.
		for _, c := range s.Children {
			if blank(body[c.Start:c.End]) {
				continue
			}
			out = append(out, piece{Breadcrumb: c.Breadcrumb, Start: c.Start, End: c.End})
		}
	}
	return out, nil
}

// mergePieces folds consecutive pieces below the floor into one, stopping before the running
// breadcrumb-inclusive total would cross the ceiling.
//
// The merged breadcrumb is the deepest common prefix of the merged pieces' breadcrumbs
// (PRD-stories-fundacao §3, S03), not the first one's: a chunk that opens with `H1 > H2 > H3a` but
// also contains H3b would claim a position it does not hold, and the breadcrumb is embedded text —
// the model would read the lie.
//
// The candidate is re-counted at every step rather than summed from the parts. Tokenizers are not
// additive across a join, and the breadcrumb itself shrinks as pieces merge, so a running sum would
// drift away from what the model actually sees — which is the number the ceiling is about.
//
// Every piece this pass emits carries the count that decided its fate, in piece.Tokens. That is the
// same text ChunkNote classifies against the ceiling and the hard window, so it asks the counter
// nothing further: one measurement per chunk, not two.
//
// ponytail: re-counting makes this O(n²) counter calls on a note of many tiny sections. Real notes
// have tens of sections, and the calibration (T10) is what would show it hurting; memoize by range
// if it ever does.
func mergePieces(ctx context.Context, body string, pieces []piece, cfg Config, tc TokenCounter) ([]piece, error) {
	var out []piece
	for i := 0; i < len(pieces); {
		cur := pieces[i]
		if cur.Tokens == 0 {
			n, err := measureTokens(ctx, tc, cur.Breadcrumb, body[cur.Start:cur.End])
			if err != nil {
				return nil, err
			}
			cur.Tokens = n
		}

		j := i + 1
		for cur.Tokens < cfg.FloorTokens && j < len(pieces) {
			cand := piece{
				Breadcrumb: commonBreadcrumbPrefix([][]string{cur.Breadcrumb, pieces[j].Breadcrumb}),
				Start:      cur.Start,
				End:        pieces[j].End,
			}
			cn, err := measureTokens(ctx, tc, cand.Breadcrumb, body[cand.Start:cand.End])
			if err != nil {
				return nil, err
			}
			if cn > cfg.CeilingTokens {
				break
			}
			cand.Tokens = cn
			cur = cand
			j++
		}

		out = append(out, cur)
		i = j
	}
	return out, nil
}

// commonBreadcrumbPrefix returns the longest element-wise common prefix of the breadcrumbs.
//
// It never fabricates a multi-branch path: breadcrumbs sharing no first element collapse to nothing
// at all, which is the honest answer for sections with no common ancestor. The result is a fresh
// slice — returning a sub-slice of an input would let a later append rewrite a breadcrumb already
// handed to an emitted chunk.
func commonBreadcrumbPrefix(breadcrumbs [][]string) []string {
	if len(breadcrumbs) == 0 {
		return nil
	}
	prefix := slices.Clone(breadcrumbs[0])
	for _, bc := range breadcrumbs[1:] {
		if len(bc) < len(prefix) {
			prefix = prefix[:len(bc)]
		}
		for i := range prefix {
			if bc[i] != prefix[i] {
				prefix = prefix[:i]
				break
			}
		}
	}
	if len(prefix) == 0 {
		return nil
	}
	return prefix
}

// blank reports whether a range carries no content. Whitespace-only ranges — the gap before a note's
// first `##`, a trailing blank line — are dropped rather than embedded: a vector for "\n\n" is noise
// in the index and a point in Qdrant that means nothing.
func blank(s string) bool { return strings.TrimSpace(s) == "" }
