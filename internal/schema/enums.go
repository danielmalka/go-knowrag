package schema

import (
	"maps"
	"slices"
)

// Canonical enum values for frontmatter fields, per docs/PRD-contrato.md §2.4 (type, status,
// visibility). `vault` and `area` used to live here too; D-26 moved them to installation
// configuration (internal/config) because which vaults and areas exist is a fact about a deployment,
// not a fact about the contract. Each type below is an opaque struct with one unexported string
// field, and every canonical value is reached through an exported accessor
// function (schema.StatusDraft(), not a bare schema.StatusDraft) over an unexported var. Two properties
// follow, and both are properties of the code rather than conventions a drift test has to police:
//
//   - Unconstructible from outside: there is no exported field to set and no exported zero-value
//     constructor, so the only ways in are a Parse* function or an accessor.
//   - Immutable from outside: an exported var can be reassigned by any package in the module
//     (`schema.StatusDraft = ...` would poison canonical identity at run time); a function has no
//     assignable symbol.
//
// Known limit, stated honestly because the alternative is a comment that overclaims: inside this
// package nothing stops someone writing `Status{"whatever"}` directly and skipping registerStatus,
// which would produce a value that is not Valid() and that Parse* cannot return. Go has no way to
// forbid that within a package short of moving each type to its own package, which is more
// machinery than the risk earns. The registered maps stay the single source of truth: Valid(),
// Parse*, All*() and the architecture test all read them, so an unregistered value is inert
// everywhere that matters rather than silently canonical.

// NoteType is the frontmatter `type` field.
type NoteType struct{ s string }

func (t NoteType) String() string { return t.s }
func (t NoteType) Valid() bool    { _, ok := noteTypeSet[t.s]; return ok }

// ParseNoteType looks up s in the registered NoteType set. ok is false for the zero value and for
// any string that was never passed to registerNoteType.
func ParseNoteType(s string) (NoteType, bool) { t, ok := noteTypeSet[s]; return t, ok }

// AllNoteTypes returns every registered NoteType, ordered by string value. The slice is a fresh
// copy: mutating it cannot reach the registry.
func AllNoteTypes() []NoteType { return sortedValues(noteTypeSet) }

var noteTypeSet = map[string]NoteType{}

// registerNoteType builds a NoteType and inserts it into noteTypeSet in the same line, so a value
// that exists as a var necessarily exists in the set Valid()/ParseNoteType read, and vice versa.
func registerNoteType(s string) NoteType {
	t := NoteType{s}
	noteTypeSet[s] = t
	return t
}

var (
	noteTypeConcept   = registerNoteType("concept")
	noteTypeMOC       = registerNoteType("moc")
	noteTypeProject   = registerNoteType("project")
	noteTypePost      = registerNoteType("post")
	noteTypeLesson    = registerNoteType("lesson")
	noteTypeReference = registerNoteType("reference")
	noteTypeTemplate  = registerNoteType("template")
	noteTypeLog       = registerNoteType("log")
	noteTypeLore      = registerNoteType("lore")
	noteTypeCharacter = registerNoteType("character")
	noteTypeScript    = registerNoteType("script")
	noteTypePrompt    = registerNoteType("prompt")
	noteTypeIndex     = registerNoteType("index")
	noteTypeAgent     = registerNoteType("agent")
	noteTypeSkill     = registerNoteType("skill")
)

func NoteTypeConcept() NoteType   { return noteTypeConcept }
func NoteTypeMOC() NoteType       { return noteTypeMOC }
func NoteTypeProject() NoteType   { return noteTypeProject }
func NoteTypePost() NoteType      { return noteTypePost }
func NoteTypeLesson() NoteType    { return noteTypeLesson }
func NoteTypeReference() NoteType { return noteTypeReference }
func NoteTypeTemplate() NoteType  { return noteTypeTemplate }
func NoteTypeLog() NoteType       { return noteTypeLog }
func NoteTypeLore() NoteType      { return noteTypeLore }
func NoteTypeCharacter() NoteType { return noteTypeCharacter }
func NoteTypeScript() NoteType    { return noteTypeScript }
func NoteTypePrompt() NoteType    { return noteTypePrompt }
func NoteTypeIndex() NoteType     { return noteTypeIndex }
func NoteTypeAgent() NoteType     { return noteTypeAgent }
func NoteTypeSkill() NoteType     { return noteTypeSkill }

// Status is the frontmatter `status` field.
type Status struct{ s string }

func (t Status) String() string           { return t.s }
func (t Status) Valid() bool              { _, ok := statusSet[t.s]; return ok }
func ParseStatus(s string) (Status, bool) { t, ok := statusSet[s]; return t, ok }

// AllStatuses returns every registered Status, ordered by string value, as a fresh copy.
func AllStatuses() []Status { return sortedValues(statusSet) }

var statusSet = map[string]Status{}

func registerStatus(s string) Status {
	t := Status{s}
	statusSet[s] = t
	return t
}

var (
	statusDraft      = registerStatus("draft")
	statusInProgress = registerStatus("in-progress")
	statusStable     = registerStatus("stable")
	statusArchived   = registerStatus("archived")
)

func StatusDraft() Status      { return statusDraft }
func StatusInProgress() Status { return statusInProgress }
func StatusStable() Status     { return statusStable }
func StatusArchived() Status   { return statusArchived }

// Visibility is the frontmatter `visibility` field.
type Visibility struct{ s string }

func (t Visibility) String() string { return t.s }
func (t Visibility) Valid() bool    { _, ok := visibilitySet[t.s]; return ok }
func ParseVisibility(s string) (Visibility, bool) {
	t, ok := visibilitySet[s]
	return t, ok
}

// AllVisibilities returns every registered Visibility, ordered by string value, as a fresh copy.
func AllVisibilities() []Visibility { return sortedValues(visibilitySet) }

var visibilitySet = map[string]Visibility{}

func registerVisibility(s string) Visibility {
	t := Visibility{s}
	visibilitySet[s] = t
	return t
}

var (
	visibilityPrivate   = registerVisibility("private")
	visibilityInternal  = registerVisibility("internal")
	visibilityShareable = registerVisibility("shareable")
)

func VisibilityPrivate() Visibility   { return visibilityPrivate }
func VisibilityInternal() Visibility  { return visibilityInternal }
func VisibilityShareable() Visibility { return visibilityShareable }

// sortedValues returns m's values ordered by map key. The result is freshly allocated on every
// call, which is what keeps All*() from handing a caller a handle on the registry.
func sortedValues[T any](m map[string]T) []T {
	out := make([]T, 0, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		out = append(out, m[k])
	}
	return out
}
