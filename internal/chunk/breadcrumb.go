package chunk

import (
	"context"
	"strings"
)

// breadcrumbSeparator joins the heading path, and breadcrumbGap separates it from the body. Both
// are part of the embedded text and therefore part of point_hash (PRD-contrato §2.4): changing
// either changes every chunk's text and forces a full reindex, which is the same cost as bumping
// Version — so it is a contract change, not a formatting preference.
const (
	breadcrumbSeparator = " > "
	breadcrumbGap       = "\n\n"
)

// PrefixBreadcrumb renders the exact text that gets embedded and hashed: the `H1 > H2 [> H3]` path,
// a blank line, then the body verbatim.
//
// The body is not rewritten in any way — not trimmed, not re-wrapped, not re-normalized. It arrives
// LF-only from S02 (PRD-contrato §2.4) and this package adds no second normalization pass: text
// this function quietly rewrote would stop matching the text S02 delivered.
//
// An empty breadcrumb returns the body unchanged rather than a leading blank line. A note with no
// headings still produces a chunk, and the two extra bytes would be content invented here that the
// model reads and the hash covers.
func PrefixBreadcrumb(breadcrumb []string, body string) string {
	if len(breadcrumb) == 0 {
		return body
	}
	return strings.Join(breadcrumb, breadcrumbSeparator) + breadcrumbGap + body
}

// measureTokens is the only place this package counts tokens, and it always counts the
// breadcrumb-prefixed text. The breadcrumb is embedded together with the body, so it occupies the
// model's window: a clamp decided on the bare body would authorize chunks that overflow it
// (PRD-stories-fundacao §3, S03, "O breadcrumb entra no orçamento de tokens do chunk").
//
// The counter's error is propagated, never absorbed. A tokenizer that cannot be reached is not a
// chunk of zero tokens — it is a run that must stop.
func measureTokens(ctx context.Context, tc TokenCounter, breadcrumb []string, body string) (int, error) {
	return tc.CountTokens(ctx, PrefixBreadcrumb(breadcrumb, body))
}
