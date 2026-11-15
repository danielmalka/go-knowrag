package retrieval

import (
	"errors"
	"fmt"
	"strings"
)

// The structural rejections. They are sentinels rather than ad-hoc strings because two callers
// downstream (the MCP adapter in S08, the CLI in S09) have to tell "you asked for something
// impossible" apart from "Qdrant is down", and errors.Is is the only way to do that without
// matching on message text.
var (
	// ErrEmptyCollection is a query that names no collection. There is deliberately no default:
	// a package that could fall back to one collection would be a package that names `interno`.
	ErrEmptyCollection = errors.New("retrieval: no collection named")

	// ErrEmptyTenant is the reason this package exists. tenant_id is not a filter a caller opts
	// into; a Query without one is not a query with weaker isolation, it is not a query at all.
	ErrEmptyTenant = errors.New("retrieval: tenant_id is empty")

	ErrEmptyQuery = errors.New("retrieval: query text is empty")

	ErrInvalidTopK   = errors.New("retrieval: top_k out of range")
	ErrInvalidOffset = errors.New("retrieval: offset out of range")

	ErrEmptyUID   = errors.New("retrieval: uid is empty")
	ErrInvalidUID = errors.New("retrieval: uid is not a UUID")
	// ErrNoteTooLarge is a lookup that hit noteLookupLimit: we cannot tell a 64-chunk note from a
	// 65-chunk one, so returning the page would be a silent truncation.
	ErrNoteTooLarge = errors.New("retrieval: the note has more chunks than a single lookup will return")
)

// maxTopK and maxOffset bound the result window. They are not a policy about how much a caller may
// read — they are what keeps topK+offset and the over-fetch window (fetchWindow) inside int, so an
// absurd input is rejected at the boundary instead of wrapping into a huge unsigned wire limit.
const (
	maxTopK   = 1_000
	maxOffset = 100_000
)

// Query is one search, expressed in this package's own vocabulary rather than Qdrant's.
//
// TenantID has no "unset means all tenants" reading and no override: it is the one field whose
// absence is an error rather than a default (see Validate). Everything else is optional, and every
// optional field narrows the answer — none widens it.
type Query struct {
	Collection string
	TenantID   string
	Text       string
	TopK       int
	Offset     int

	// Optional facets. Empty means "do not restrict on this field", never "restrict to empty".
	Area  string
	Type  string
	Vault string
	Tags  []string

	// IncludeArchived lifts the default `status != archived` exclusion (PRD-contrato §2.4:
	// archived notes are indexed and filtered at query time, not skipped at ingestion).
	IncludeArchived bool

	// IncludePrivate lifts the default `visibility != private` exclusion. It is a privileged path:
	// the administrative CLI (S09) may set it, and the MCP adapter (S08) never may — no MCP tool
	// input reaches this field. The rule is keyed here, on an explicit flag, and nowhere else; in
	// particular buildFilter must never read Collection to decide it (S07 T5).
	IncludePrivate bool

	// UID names one note for GetByUID. Search ignores it: a search is a question, and a caller that
	// already knows the uid should open the note rather than filter a ranked list down to it.
	UID string
}

// Validate rejects every query this package can prove is unanswerable before any I/O happens.
//
// There is exactly one gate: Search runs Validate first, and everything after it may assume a Query
// that cleared it.
func (q Query) Validate() error {
	switch {
	case q.Collection == "":
		return ErrEmptyCollection
	case q.TenantID == "":
		return ErrEmptyTenant
	case strings.TrimSpace(q.Text) == "":
		return ErrEmptyQuery
	case q.TopK <= 0 || q.TopK > maxTopK:
		return fmt.Errorf("%w: top_k = %d, want 1..%d", ErrInvalidTopK, q.TopK, maxTopK)
	case q.Offset < 0 || q.Offset > maxOffset:
		return fmt.Errorf("%w: offset = %d, want 0..%d", ErrInvalidOffset, q.Offset, maxOffset)
	}
	return nil
}
