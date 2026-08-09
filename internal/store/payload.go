package store

import (
	"fmt"
	"maps"
	"math"

	"github.com/qdrant/go-client/qdrant"

	"github.com/danielmalka/go-knowrag/internal/ingest"
)

// Payload fields this package has to know by name. Everything else in the §2.4 payload is carried
// opaquely — this package writes what ingest hands it and reads it back — but these two have a
// structural role: point_hash is compared on its own (condition 3) and chunk_index decides which
// chunk a point is (conditions 1 and 2), so both are lifted out of the field map into their own
// struct fields on the way back.
const (
	payloadPointHash  = "point_hash"
	payloadChunkIndex = "chunk_index"
)

// marshalPayload turns one point's fields plus its fingerprint into a Qdrant payload.
//
// The value types are the ones ingest chose, not a re-derivation from field names: an int stays an
// integer (chunk_index has to be, or the prune's range filter is refused), a []string stays a list
// (S07 filters on individual tags), a bool stays a bool. This function knowing nothing about what
// `tags` or `oversize` mean is the point — the payload contract lives in internal/ingest, and a
// second interpretation of it here is a second contract free to drift.
func marshalPayload(fields map[string]any, pointHash string) (map[string]*qdrant.Value, error) {
	out := make(map[string]any, len(fields)+1)
	for k, v := range fields {
		if list, ok := v.([]string); ok {
			// qdrant.NewValue accepts []interface{}, not []string.
			items := make([]any, len(list))
			for i, s := range list {
				items[i] = s
			}
			out[k] = items
			continue
		}
		out[k] = v
	}
	out[payloadPointHash] = pointHash

	values, err := qdrant.TryValueMap(out)
	if err != nil {
		return nil, fmt.Errorf("building the payload: %w", err)
	}
	return values, nil
}

// unmarshalPayload is marshalPayload's inverse: a read-back payload becomes the record the
// integrity predicate compares field by field.
//
// It is strict about types on purpose. A payload value of a shape this pipeline never writes means
// something else wrote it, and that is exactly the manual administrative edit condition 4 exists to
// catch — returning an error names it at the read instead of letting a silently-coerced value
// compare equal later.
func unmarshalPayload(payload map[string]*qdrant.Value) (ingest.PointRecord, error) {
	fields := make(map[string]any, len(payload))
	for k, v := range payload {
		converted, err := payloadValue(v)
		if err != nil {
			return ingest.PointRecord{}, fmt.Errorf("field %q: %w", k, err)
		}
		fields[k] = converted
	}

	hash, ok := fields[payloadPointHash].(string)
	if !ok {
		return ingest.PointRecord{}, fmt.Errorf(
			"no %s in the payload — a point written under the previous contract, or by something "+
				"other than this pipeline; either way it is not integral (PRD-contrato §2.4)",
			payloadPointHash)
	}
	index, ok := fields[payloadChunkIndex].(int)
	if !ok {
		return ingest.PointRecord{}, fmt.Errorf("no integer %s in the payload", payloadChunkIndex)
	}

	// point_hash travels in its own field so it can never reach condition 4's field-by-field
	// comparison, which must stay independent of it. chunk_index stays in the map as well as being
	// lifted out: it is a §2.4 payload field like any other, and dropping it would make it the one
	// field a manual edit could change unnoticed.
	rest := maps.Clone(fields)
	delete(rest, payloadPointHash)

	return ingest.PointRecord{ChunkIndex: index, PointHash: hash, Fields: rest}, nil
}

func payloadValue(v *qdrant.Value) (any, error) {
	switch kind := v.GetKind().(type) {
	case *qdrant.Value_StringValue:
		return kind.StringValue, nil
	case *qdrant.Value_BoolValue:
		return kind.BoolValue, nil
	case *qdrant.Value_IntegerValue:
		n := kind.IntegerValue
		if n > math.MaxInt32 || n < math.MinInt32 {
			return nil, fmt.Errorf("integer %d is outside the range this pipeline writes", n)
		}
		return int(n), nil
	case *qdrant.Value_ListValue:
		items := kind.ListValue.GetValues()
		out := make([]string, len(items))
		for i, item := range items {
			s, ok := item.GetKind().(*qdrant.Value_StringValue)
			if !ok {
				return nil, fmt.Errorf("list element %d is not a string", i)
			}
			out[i] = s.StringValue
		}
		return out, nil
	default:
		return nil, fmt.Errorf("value of type %T; this pipeline writes only strings, integers, "+
			"booleans and string lists", v.GetKind())
	}
}
