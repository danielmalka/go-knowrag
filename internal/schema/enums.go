package schema

import (
	"maps"
	"slices"
)

// Canonical enum values for frontmatter fields, per docs/PRD-contrato.md §2.4 (type, status,
// visibility, vault) and §2.4b (area, scoped per vault). Each type below is an opaque struct with
// one unexported string field, and every canonical value is reached through an exported accessor
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

// Vault is the frontmatter `vault` field. Slugs are the lowercase of the literal on-disk folder
// name (PRD-contrato §2.4b) — malkalife/malkaway, settled 2026-08-08, nothing left to confirm.
type Vault struct{ s string }

func (t Vault) String() string          { return t.s }
func (t Vault) Valid() bool             { _, ok := vaultSet[t.s]; return ok }
func ParseVault(s string) (Vault, bool) { t, ok := vaultSet[s]; return t, ok }

// AllVaults returns every registered Vault, ordered by string value, as a fresh copy.
func AllVaults() []Vault { return sortedValues(vaultSet) }

var vaultSet = map[string]Vault{}

func registerVault(s string) Vault {
	t := Vault{s}
	vaultSet[s] = t
	return t
}

var (
	vaultMalkaLife = registerVault("malkalife")
	vaultMalkaWay  = registerVault("malkaway")
)

func VaultMalkaLife() Vault { return vaultMalkaLife }
func VaultMalkaWay() Vault  { return vaultMalkaWay }

// Area is the frontmatter `area` field, scoped per Vault (PRD-contrato §2.4b): the same string can
// be meaningful in one vault and undefined in the other, so validity is a function of (Vault,
// string), not of string alone — hence areaSetByVault is keyed by Vault first.
type Area struct{ s string }

func (a Area) String() string { return a.s }

// ValidFor reports whether a is a registered Area for vault v.
func (a Area) ValidFor(v Vault) bool { _, ok := areaSetByVault[v][a.s]; return ok }

// ParseArea looks up s as an Area registered for vault v. ok is false if s is not registered for v,
// even if it is registered for the other vault.
func ParseArea(v Vault, s string) (Area, bool) { a, ok := areaSetByVault[v][s]; return a, ok }

// AreasFor returns every Area registered for vault v, ordered by string value, as a fresh copy. An
// unregistered Vault yields an empty slice, matching ParseArea's "not registered for v" answer.
func AreasFor(v Vault) []Area { return sortedValues(areaSetByVault[v]) }

var areaSetByVault = map[Vault]map[string]Area{}

// registerArea builds an Area and inserts it into areaSetByVault under every vault in vaults. The
// variadic parameter is what lets a value valid in both vaults (00-inbox) be declared exactly once
// instead of twice, keeping the "one declaration site" property from splitting into "one per vault".
func registerArea(s string, vaults ...Vault) Area {
	a := Area{s}
	for _, v := range vaults {
		if areaSetByVault[v] == nil {
			areaSetByVault[v] = map[string]Area{}
		}
		areaSetByVault[v][s] = a
	}
	return a
}

var (
	// areaInbox holds the literal "00-inbox": PRD-contrato §2.4b derives area from the lowercase
	// on-disk folder name with no normalization, and the folder keeps its sort-order prefix on disk
	// in both vaults. The Go identifier can't start with a digit, so the var name and the string
	// value deliberately differ — this is the one exception to "identifier mirrors value" in this
	// file, and it exists for that reason, not by oversight.
	areaInbox = registerArea("00-inbox", vaultMalkaLife, vaultMalkaWay)

	// MalkaLife-only areas.
	areaMOCs     = registerArea("mocs", vaultMalkaLife)
	areaPersonal = registerArea("personal", vaultMalkaLife)
	areaResearch = registerArea("research", vaultMalkaLife)

	// MalkaWay-only areas.
	areaArcanto          = registerArea("arcanto", vaultMalkaWay)
	areaCarreira         = registerArea("carreira", vaultMalkaWay)
	areaCooperativa      = registerArea("cooperativa", vaultMalkaWay)
	areaInfra            = registerArea("infra", vaultMalkaWay)
	areaProjetosPessoais = registerArea("projetos-pessoais", vaultMalkaWay)
)

func AreaInbox() Area            { return areaInbox }
func AreaMOCs() Area             { return areaMOCs }
func AreaPersonal() Area         { return areaPersonal }
func AreaResearch() Area         { return areaResearch }
func AreaArcanto() Area          { return areaArcanto }
func AreaCarreira() Area         { return areaCarreira }
func AreaCooperativa() Area      { return areaCooperativa }
func AreaInfra() Area            { return areaInfra }
func AreaProjetosPessoais() Area { return areaProjetosPessoais }

// sortedValues returns m's values ordered by map key. The result is freshly allocated on every
// call, which is what keeps All*()/AreasFor() from handing a caller a handle on the registry.
func sortedValues[T any](m map[string]T) []T {
	out := make([]T, 0, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		out = append(out, m[k])
	}
	return out
}
