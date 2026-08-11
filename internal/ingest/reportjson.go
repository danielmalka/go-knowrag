package ingest

import (
	"encoding/json"

	"github.com/google/uuid"
)

// The JSON report is a separate set of types from Report itself, and that separation is the whole
// mechanism behind "parseável e estável" (PRD-stories-pipeline §3 S06b).
//
// Report is a Go type that later stories will keep changing — a field renamed for readability, a
// helper promoted to a field, an error type swapped. Any of those would silently rewrite the wire
// format if the wire format were `json.Marshal(Report)`, and the only consumer that would notice is
// a script in the runbook, at 3am, by breaking. With the shape declared here, renaming a Report
// field leaves the wire format alone, and changing the wire format takes editing this file and the
// golden fixture together.
//
// Be honest about what that buys: **visibility, not prevention**. Producer and fixture live in one
// repo, so a patch that renames a key here and regenerates the golden passes green — nothing stops
// it. What the arrangement guarantees is that such a patch cannot be an accident and cannot be
// invisible: it shows up in the diff as a deliberate edit to a file whose whole job is the contract.
// A real guarantee would need the fixture outside this repo, or a version in the payload, and this
// story has neither.
//
// It also solves a plain mechanical problem: NoteResult.Err is an `error`, and errors do not
// marshal. json.Marshal renders one as `{}`, so a report of failures would be a list of empty
// objects, which is a shape that parses cleanly and says nothing.
type reportJSON struct {
	Mode           string          `json:"mode"`
	Notes          int             `json:"notes"`
	Counts         map[string]int  `json:"counts"`
	PointsWritten  int             `json:"points_written"`
	PointsPruned   int             `json:"points_pruned"`
	OrphansScanned bool            `json:"orphans_scanned"`
	Orphans        []orphanJSON    `json:"orphans"`
	OnDisk         []orphanJSON    `json:"indexed_not_scanned"`
	Errors         []noteErrorJSON `json:"errors"`
}

type orphanJSON struct {
	UID    uuid.UUID `json:"uid"`
	Vault  string    `json:"vault"`
	Path   string    `json:"path"`
	Points int       `json:"points"`
}

type noteErrorJSON struct {
	Path    string    `json:"path"`
	UID     uuid.UUID `json:"uid"`
	Message string    `json:"message"`
}

// JSON renders the run in the stable shape above.
//
// Every list is always an array, never null: a consumer that iterates `.orphans` should not have to
// special-case the clean run, and `null` is what an empty Go slice marshals to.
func (r Report) JSON() ([]byte, error) {
	counts := map[string]int{}
	for state, n := range r.Counts() {
		counts[string(state)] = n
	}

	orphans := orphansJSON(r.Orphans)
	onDisk := orphansJSON(r.OnDisk)

	failures := r.Failures()
	errs := make([]noteErrorJSON, 0, len(failures))
	for _, f := range failures {
		errs = append(errs, noteErrorJSON{Path: f.Path, UID: f.UID, Message: f.Err.Error()})
	}

	return json.Marshal(reportJSON{
		Mode:           r.Mode,
		Notes:          len(r.Results),
		Counts:         counts,
		PointsWritten:  r.PointsWritten(),
		PointsPruned:   r.PointsPruned,
		OrphansScanned: r.OrphansScanned,
		Orphans:        orphans,
		OnDisk:         onDisk,
		Errors:         errs,
	})
}

// orphansJSON converts a list and always returns an array, never nil: a consumer iterating these
// keys should not have to special-case the clean run.
func orphansJSON(in []Orphan) []orphanJSON {
	out := make([]orphanJSON, 0, len(in))
	for _, o := range in {
		// nolint:staticcheck // S1016 suggests orphanJSON(o), and that conversion is the coupling this
		// file exists to avoid: it compiles only while the two structs stay field-for-field identical,
		// which is a promise the wire format must not make to a Go type that later stories will keep
		// changing. Spelling the fields out is what lets Orphan grow without the shape following it.
		out = append(out, orphanJSON{UID: o.UID, Vault: o.Vault, Path: o.Path, Points: o.Points})
	}
	return out
}
