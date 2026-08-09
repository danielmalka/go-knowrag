package vault

import (
	"time"

	"github.com/google/uuid"

	"github.com/danielmalka/go-knowrag/internal/schema"
)

// Note is one validated vault note: the frontmatter contract of PRD-contrato §2.4, plus the
// fields S02 derives (Vault, Path, Area, Sub, Updated) and the raw body S03 chunks.
//
// Type, Status and Visibility are the internal/schema enum types, resolved at the boundary by
// parseFrontmatter. An earlier revision of this comment kept them as plain strings and justified it
// by pointing at "the frontmatter validator that owns that job" — no such validator existed
// anywhere in the module, and S00 T7 had already pointed the same decision back at S02, so the
// three fields ended up validated by nobody. The real corpus shows what that cost: `status: maduro`,
// `status: todo` and `status: "#em-progresso"` all parsed clean, reached NoteMetadataFields, the
// point_hash and the Qdrant payload, and the query-time default filter `status != archived` did not
// recognize any of them. The correction is recorded rather than the history rewritten, because the
// argument that replaces it was already sitting in the next sentence: Area is schema.Area because
// deriving it means looking it up in schema's per-vault map, so the typed value comes for free and
// there is no second copy of the map to drift — and ParseNoteType/ParseStatus/ParseVisibility read
// that same single registry, so the same is true of all four.
//
// Body has already been normalized to LF (PRD-contrato §2.4). Nothing downstream normalizes again.
type Note struct {
	Vault schema.Vault
	Path  string // relative to the vault root, slash-separated

	UID        uuid.UUID
	Type       schema.NoteType
	Status     schema.Status
	Visibility schema.Visibility
	Tags       []string
	Created    time.Time

	// Title and Lang are the only two of PRD-contrato §2.4's optional fields carried here: they are
	// the two that reach the payload and the hash. `description`, `source`, `related` and
	// `aliases` are deliberately not parsed — no story reads them, none is a payload field, and
	// the contract fixes their presence but not their shape. The real corpus proves the cost of
	// parsing them anyway: 18 notes write `related` as a scalar string and one writes `source` as
	// a map, so decoding them into fixed Go types rejects 19 notes that are valid under §2.4 over
	// data nothing downstream would have looked at. Adding one back means giving it a type and a
	// consumer, in that order.
	Title string
	Lang  string

	Area schema.Area
	Sub  string

	// Updated is derived from git (or mtime), carried for S06a to write to the payload, and
	// deliberately absent from NoteMetadataFields — see that function's doc comment.
	Updated time.Time

	Body string
}

// SkippedNote records a file the scan chose not to turn into a Note, with the reason. Skips are
// recorded rather than dropped so "731 files, 730 notes" is always explainable without re-running
// the scan: an unexplained gap between the two counts is how silent data loss hides.
type SkippedNote struct {
	Path   string
	Reason string
}

// reasonEmptyFile is the one skip reason S02 defines today. An empty file has no frontmatter, so
// under the contract it would be a hard error — but it is also indistinguishable from "a note the
// owner started and never wrote", produces no chunk, and one exists in the real corpus
// (MalkaLife/research/curadoria/2026-07-04-curadoria-tech.md, 0 bytes). Failing the whole ingestion
// over a file with nothing in it trades a real outage for a phantom problem; recording it keeps the
// decision visible.
const reasonEmptyFile = "file is empty"

// ScanResult is one vault's completed scan.
type ScanResult struct {
	Vault   schema.Vault
	Notes   []Note
	Skipped []SkippedNote
}
