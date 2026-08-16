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

	// ErrPrefetchLimitTooLow is the calibration floor of PRD-contrato §2.3b: each prefetch leg has
	// to return at least as many candidates as the fused answer needs, or RRF fuses two lists that
	// were already truncated below the answer size.
	ErrPrefetchLimitTooLow = errors.New("retrieval: prefetch limit is below top_k + offset")

	// ErrInvalidSearchMode is a Mode that names no mode. It is rejected rather than folded into the
	// hybrid default, because folding it there would make a garbage int and a deliberate hybrid
	// query indistinguishable — and the whole point of the toggle is that the eval harness can
	// prove which one it measured (internal/eval/hybrid_compare.go).
	ErrInvalidSearchMode = errors.New("retrieval: search mode is not one of the defined modes")

	ErrEmptyUID   = errors.New("retrieval: uid is empty")
	ErrInvalidUID = errors.New("retrieval: uid is not a UUID")
	// ErrNoteTooLarge is a lookup that hit noteLookupLimit: we cannot tell a 64-chunk note from a
	// 65-chunk one, so returning the page would be a silent truncation.
	ErrNoteTooLarge = errors.New("retrieval: the note has more chunks than a single lookup will return")
)

// SearchMode selects the query shape: the §2.3b hybrid (two prefetch legs fused with RRF) or a
// single dense vector search.
//
// The zero value is the hybrid, so every Query written before this field existed keeps the shape it
// had. Dense-only exists for one reason: S07 could not answer whether the sparse leg earns its
// place because there was no golden set to measure it with, and S10 is where that measurement
// happens. Which one this build ships as the default is not decided in this file — S10 T11 adds
// DefaultSearchMode from the measured comparison, and until then no caller here reads a default.
type SearchMode int

const (
	SearchModeHybrid SearchMode = iota
	SearchModeDenseOnly
)

func (m SearchMode) String() string {
	switch m {
	case SearchModeHybrid:
		return "hybrid"
	case SearchModeDenseOnly:
		return "dense-only"
	default:
		return fmt.Sprintf("SearchMode(%d)", int(m))
	}
}

// Valid reports whether m names a defined mode.
func (m SearchMode) Valid() bool { return m == SearchModeHybrid || m == SearchModeDenseOnly }

// maxTopK and maxOffset bound the result window. They are not a policy about how much a caller may
// read — they are what keeps topK+offset and the multiplication in prefetchLimit inside int, so an
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

	// Mode selects hybrid (zero value) or dense-only. It narrows nothing about *which* points may
	// come back — the filter is built the same way in both modes, tenant condition included — it
	// only changes how they are ranked.
	Mode SearchMode

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
// The prefetch floor is checked here rather than at request-construction time so that there is
// exactly one gate: Search runs Validate first, and everything after it may assume a Query that
// already cleared the floor. That is what lets calibratedPrefetchLimit return a plain int.
func (q Query) Validate(prefetchMultiplier int) error {
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
	case !q.Mode.Valid():
		return fmt.Errorf("%w: mode = %d", ErrInvalidSearchMode, int(q.Mode))
	}

	// Checked in both modes even though dense-only builds no prefetch leg. The floor is a property
	// of the configured multiplier, not of the mode, and one gate that always runs is worth more
	// than a branch here that a third mode would have to remember to update.
	_, err := prefetchLimit(q.TopK, q.Offset, prefetchMultiplier)
	return err
}

// prefetchLimit is the per-leg candidate limit: the answer window scaled by the configured
// multiplier.
//
// It takes a plain int rather than a Config so this file stays self-contained — build.go calls the
// same function with the caller's configured multiplier and does not re-derive the floor.
func prefetchLimit(topK, offset, multiplier int) (int, error) {
	floor := topK + offset
	if multiplier <= 0 {
		return 0, fmt.Errorf("%w: multiplier = %d, so the limit would be %d and the floor is %d",
			ErrPrefetchLimitTooLow, multiplier, floor*multiplier, floor)
	}

	limit := floor * multiplier
	// limit/multiplier != floor catches the wrap exactly; limit < floor catches the rest. Both are
	// unreachable through Validate (maxTopK/maxOffset bound the inputs), and both stay because
	// prefetchLimit is also reachable from build.go with whatever multiplier a Config carries.
	if limit/multiplier != floor || limit < floor {
		return 0, fmt.Errorf("%w: top_k+offset = %d times multiplier %d does not fit in an int",
			ErrPrefetchLimitTooLow, floor, multiplier)
	}
	return limit, nil
}
