package retrieval

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
)

// noteLookupLimit is how many chunks one GetByUID will ask for.
//
// The 2026-08-11 calibration (docs/eval/chunking-calibration.md) measured a maximum of 35 chunks
// on the real corpus. 64 is that ceiling with room; hitting it is treated as an error rather than
// a page, because a truncated note looks exactly like a short one.
const noteLookupLimit = 64

// GetByUID returns every visible chunk of one note, in document order.
//
// It is a filtered read, not a search: no embedding, no prefetch, no fusion. The tenant condition
// and the archived/private guards come from the same buildFilter Search uses, so a lookup cannot
// see a note a search under the same flags would have hidden. Area, type, vault and tags are
// ignored — the uid already names the note.
func (s *Searcher) GetByUID(ctx context.Context, q Query) ([]Result, error) {
	if s == nil || s.executor == nil {
		return nil, errors.New("retrieval: Searcher has no executor")
	}

	uid, err := parseUID(q.UID)
	if err != nil {
		return nil, err
	}
	switch {
	case q.Collection == "":
		return nil, ErrEmptyCollection
	case q.TenantID == "":
		return nil, ErrEmptyTenant
	}

	lookup := Query{
		Collection:      q.Collection,
		TenantID:        q.TenantID,
		IncludeArchived: q.IncludeArchived,
		IncludePrivate:  q.IncludePrivate,
	}
	f := buildFilter(lookup)
	f.Must = append(f.Must, Condition{Field: fieldUID, Value: uid.String()})

	points, err := s.executor.ExecuteQuery(ctx, SearchRequest{
		Collection:    q.Collection,
		Filter:        f,
		Limit:         noteLookupLimit,
		PayloadFields: resultPayloadFields(),
	})
	if err != nil {
		return nil, fmt.Errorf("retrieval: looking up uid %s on %s: %w", uid, q.Collection, err)
	}
	if len(points) >= noteLookupLimit {
		return nil, fmt.Errorf("%w: uid %s returned %d chunks", ErrNoteTooLarge, uid, len(points))
	}

	results, err := formatResults(points)
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(results, func(a, b Result) int {
		return a.ChunkIndex - b.ChunkIndex
	})
	return results, nil
}

func parseUID(raw string) (uuid.UUID, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return uuid.UUID{}, ErrEmptyUID
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("%w: %q", ErrInvalidUID, raw)
	}
	return id, nil
}
