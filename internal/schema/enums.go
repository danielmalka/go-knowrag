package schema

// Canonical enum values for frontmatter fields, per docs/PRD-contrato.md §2.4 (type, status,
// visibility, vault) and §2.4b (area, scoped per vault). Each type below is an opaque struct with
// one unexported string field: no package outside internal/schema can construct a valid instance
// directly (there is no exported field to set and no exported zero-value constructor), so the only
// way to obtain one is through a Parse* function or one of the exported vars declared here. That,
// combined with the single register* call site per value, is what makes "one declaration, one
// source of truth" a property of the code rather than a convention a drift test has to police.

// NoteType is the frontmatter `type` field.
type NoteType struct{ s string }

func (t NoteType) String() string { return t.s }
func (t NoteType) Valid() bool    { _, ok := noteTypeSet[t.s]; return ok }

// ParseNoteType looks up s in the registered NoteType set. ok is false for the zero value and for
// any string that was never passed to registerNoteType.
func ParseNoteType(s string) (NoteType, bool) { t, ok := noteTypeSet[s]; return t, ok }

var noteTypeSet = map[string]NoteType{}

// registerNoteType builds a NoteType and inserts it into noteTypeSet in the same line, so a value
// that exists as a var necessarily exists in the set Valid()/ParseNoteType read, and vice versa.
func registerNoteType(s string) NoteType {
	t := NoteType{s}
	noteTypeSet[s] = t
	return t
}

var (
	NoteTypeConcept   = registerNoteType("concept")
	NoteTypeMOC       = registerNoteType("moc")
	NoteTypeProject   = registerNoteType("project")
	NoteTypePost      = registerNoteType("post")
	NoteTypeLesson    = registerNoteType("lesson")
	NoteTypeReference = registerNoteType("reference")
	NoteTypeTemplate  = registerNoteType("template")
	NoteTypeLog       = registerNoteType("log")
	NoteTypeLore      = registerNoteType("lore")
	NoteTypeCharacter = registerNoteType("character")
	NoteTypeScript    = registerNoteType("script")
	NoteTypePrompt    = registerNoteType("prompt")
	NoteTypeIndex     = registerNoteType("index")
	NoteTypeAgent     = registerNoteType("agent")
	NoteTypeSkill     = registerNoteType("skill")
)

// Status is the frontmatter `status` field.
type Status struct{ s string }

func (t Status) String() string           { return t.s }
func (t Status) Valid() bool              { _, ok := statusSet[t.s]; return ok }
func ParseStatus(s string) (Status, bool) { t, ok := statusSet[s]; return t, ok }

var statusSet = map[string]Status{}

func registerStatus(s string) Status {
	t := Status{s}
	statusSet[s] = t
	return t
}

var (
	StatusDraft      = registerStatus("draft")
	StatusInProgress = registerStatus("in-progress")
	StatusStable     = registerStatus("stable")
	StatusArchived   = registerStatus("archived")
)

// Visibility is the frontmatter `visibility` field.
type Visibility struct{ s string }

func (t Visibility) String() string { return t.s }
func (t Visibility) Valid() bool    { _, ok := visibilitySet[t.s]; return ok }
func ParseVisibility(s string) (Visibility, bool) {
	t, ok := visibilitySet[s]
	return t, ok
}

var visibilitySet = map[string]Visibility{}

func registerVisibility(s string) Visibility {
	t := Visibility{s}
	visibilitySet[s] = t
	return t
}

var (
	VisibilityPrivate   = registerVisibility("private")
	VisibilityInternal  = registerVisibility("internal")
	VisibilityShareable = registerVisibility("shareable")
)

// Vault is the frontmatter `vault` field. Slugs are the lowercase of the literal on-disk folder
// name (PRD-contrato §2.4b) — malkalife/malkaway, settled 2026-08-08, nothing left to confirm.
type Vault struct{ s string }

func (t Vault) String() string          { return t.s }
func (t Vault) Valid() bool             { _, ok := vaultSet[t.s]; return ok }
func ParseVault(s string) (Vault, bool) { t, ok := vaultSet[s]; return t, ok }

var vaultSet = map[string]Vault{}

func registerVault(s string) Vault {
	t := Vault{s}
	vaultSet[s] = t
	return t
}

var (
	VaultMalkaLife = registerVault("malkalife")
	VaultMalkaWay  = registerVault("malkaway")
)

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
	// AreaInbox holds the literal "00-inbox": PRD-contrato §2.4b derives area from the lowercase
	// on-disk folder name with no normalization, and the folder keeps its sort-order prefix on disk
	// in both vaults. The Go identifier can't start with a digit, so the var name and the string
	// value deliberately differ — this is the one exception to "identifier mirrors value" in this
	// file, and it exists for that reason, not by oversight.
	AreaInbox = registerArea("00-inbox", VaultMalkaLife, VaultMalkaWay)

	// MalkaLife-only areas.
	AreaMOCs     = registerArea("mocs", VaultMalkaLife)
	AreaPersonal = registerArea("personal", VaultMalkaLife)
	AreaResearch = registerArea("research", VaultMalkaLife)

	// MalkaWay-only areas.
	AreaArcanto          = registerArea("arcanto", VaultMalkaWay)
	AreaCarreira         = registerArea("carreira", VaultMalkaWay)
	AreaCooperativa      = registerArea("cooperativa", VaultMalkaWay)
	AreaInfra            = registerArea("infra", VaultMalkaWay)
	AreaProjetosPessoais = registerArea("projetos-pessoais", VaultMalkaWay)
)
