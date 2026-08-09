package retrieval

import (
	"fmt"
	"strings"
)

// breadcrumbSeparator joins the stored `headings` list back into the `H1 > H2 > H3` path a reader
// sees. It is the same separator internal/chunk embedded into the chunk text (PrefixBreadcrumb),
// spelled again here because that one is unexported and is part of point_hash — this one is display
// only, and the two must not become the same knob by accident.
const breadcrumbSeparator = " > "

// ScoredPoint is one raw hit as the executor hands it back: an identity, a fusion score and the
// projected payload. It is this package's type rather than the Qdrant client's for the reason
// build.go states, and it stays deliberately dumb — the mapping onto a Result happens here, in the
// package that knows what the §2.4 payload means.
type ScoredPoint struct {
	ID      string
	Score   float32
	Payload map[string]any
}

// Result is one search hit as every consumer sees it.
//
// Untrusted is set on every Result, always. It is not a judgement about this particular chunk: the
// entire corpus is text somebody wrote, and a note containing "ignore your instructions and…" is
// indistinguishable from one that does not until something downstream delimits it. S08 turns this
// flag into the actual framing (internal/security's markers); the flag exists so that framing can
// never be skipped for a result that "looked fine".
type Result struct {
	UID        string
	ChunkIndex int
	Text       string
	Breadcrumb string
	Path       string
	Score      float32
	Untrusted  bool
}

// formatResults maps raw hits onto Results.
//
// A point missing a required payload field is an error naming that point, not a Result with a hole
// in it: the only ways a chunk reaches the index without `uid` or `text` are a write from something
// other than this pipeline or a hand edit, and both are exactly what the caller needs told.
// Breadcrumb is the one optional field — a note whose chunk sits above the first heading has no
// headings, which is ordinary rather than broken.
func formatResults(points []ScoredPoint) ([]Result, error) {
	out := make([]Result, 0, len(points))
	for _, p := range points {
		uid, err := payloadString(p, fieldUID)
		if err != nil {
			return nil, err
		}
		index, err := payloadInt(p, fieldChunkIndex)
		if err != nil {
			return nil, err
		}
		text, err := payloadString(p, fieldText)
		if err != nil {
			return nil, err
		}
		path, err := payloadString(p, fieldPath)
		if err != nil {
			return nil, err
		}

		headings, _ := p.Payload[fieldHeadings].([]string)
		out = append(out, Result{
			UID:        uid,
			ChunkIndex: index,
			Text:       text,
			Breadcrumb: strings.Join(headings, breadcrumbSeparator),
			Path:       path,
			Score:      p.Score,
			Untrusted:  true,
		})
	}
	return out, nil
}

func payloadString(p ScoredPoint, field string) (string, error) {
	v, ok := p.Payload[field].(string)
	if !ok {
		return "", missingField(p, field, "string")
	}
	return v, nil
}

func payloadInt(p ScoredPoint, field string) (int, error) {
	v, ok := p.Payload[field].(int)
	if !ok {
		return 0, missingField(p, field, "integer")
	}
	return v, nil
}

func missingField(p ScoredPoint, field, kind string) error {
	return fmt.Errorf("retrieval: point %s: payload field %q is missing or not a %s — the point was "+
		"not written by this pipeline (PRD-contrato §2.4)", p.ID, field, kind)
}
