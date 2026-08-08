package schema

import "testing"

func TestNoteTypeValues(t *testing.T) {
	cases := []struct {
		name string
		want NoteType
	}{
		{"concept", NoteTypeConcept},
		{"moc", NoteTypeMOC},
		{"project", NoteTypeProject},
		{"post", NoteTypePost},
		{"lesson", NoteTypeLesson},
		{"reference", NoteTypeReference},
		{"template", NoteTypeTemplate},
		{"log", NoteTypeLog},
		{"lore", NoteTypeLore},
		{"character", NoteTypeCharacter},
		{"script", NoteTypeScript},
		{"prompt", NoteTypePrompt},
		{"index", NoteTypeIndex},
		{"agent", NoteTypeAgent},
		{"skill", NoteTypeSkill},
	}
	if len(noteTypeSet) != 15 {
		t.Fatalf("noteTypeSet has %d entries, want 15", len(noteTypeSet))
	}
	for _, c := range cases {
		got, ok := ParseNoteType(c.name)
		if !ok {
			t.Errorf("ParseNoteType(%q) failed", c.name)
			continue
		}
		if got != c.want {
			t.Errorf("ParseNoteType(%q) = %v, want %v", c.name, got, c.want)
		}
		if !got.Valid() {
			t.Errorf("NoteType %q not Valid()", c.name)
		}
		if got.String() != c.name {
			t.Errorf("NoteType %q String() = %q", c.name, got.String())
		}
	}
	if _, ok := ParseNoteType("bogus"); ok {
		t.Error("ParseNoteType(\"bogus\") should fail")
	}
	var zero NoteType
	if zero.Valid() {
		t.Error("zero-value NoteType should not be Valid()")
	}
}

func TestStatusValues(t *testing.T) {
	cases := []struct {
		name string
		want Status
	}{
		{"draft", StatusDraft},
		{"in-progress", StatusInProgress},
		{"stable", StatusStable},
		{"archived", StatusArchived},
	}
	if len(statusSet) != 4 {
		t.Fatalf("statusSet has %d entries, want 4", len(statusSet))
	}
	for _, c := range cases {
		got, ok := ParseStatus(c.name)
		if !ok {
			t.Errorf("ParseStatus(%q) failed", c.name)
			continue
		}
		if got != c.want {
			t.Errorf("ParseStatus(%q) = %v, want %v", c.name, got, c.want)
		}
		if !got.Valid() {
			t.Errorf("Status %q not Valid()", c.name)
		}
		if got.String() != c.name {
			t.Errorf("Status %q String() = %q", c.name, got.String())
		}
	}
	if _, ok := ParseStatus("bogus"); ok {
		t.Error("ParseStatus(\"bogus\") should fail")
	}
	var zero Status
	if zero.Valid() {
		t.Error("zero-value Status should not be Valid()")
	}
}

func TestVisibilityValues(t *testing.T) {
	cases := []struct {
		name string
		want Visibility
	}{
		{"private", VisibilityPrivate},
		{"internal", VisibilityInternal},
		{"shareable", VisibilityShareable},
	}
	if len(visibilitySet) != 3 {
		t.Fatalf("visibilitySet has %d entries, want 3", len(visibilitySet))
	}
	for _, c := range cases {
		got, ok := ParseVisibility(c.name)
		if !ok {
			t.Errorf("ParseVisibility(%q) failed", c.name)
			continue
		}
		if got != c.want {
			t.Errorf("ParseVisibility(%q) = %v, want %v", c.name, got, c.want)
		}
		if !got.Valid() {
			t.Errorf("Visibility %q not Valid()", c.name)
		}
		if got.String() != c.name {
			t.Errorf("Visibility %q String() = %q", c.name, got.String())
		}
	}
	if _, ok := ParseVisibility("bogus"); ok {
		t.Error("ParseVisibility(\"bogus\") should fail")
	}
	var zero Visibility
	if zero.Valid() {
		t.Error("zero-value Visibility should not be Valid()")
	}
}

func TestVaultValues(t *testing.T) {
	cases := []struct {
		name string
		want Vault
	}{
		{"malkalife", VaultMalkaLife},
		{"malkaway", VaultMalkaWay},
	}
	if len(vaultSet) != 2 {
		t.Fatalf("vaultSet has %d entries, want 2", len(vaultSet))
	}
	for _, c := range cases {
		got, ok := ParseVault(c.name)
		if !ok {
			t.Errorf("ParseVault(%q) failed", c.name)
			continue
		}
		if got != c.want {
			t.Errorf("ParseVault(%q) = %v, want %v", c.name, got, c.want)
		}
		if !got.Valid() {
			t.Errorf("Vault %q not Valid()", c.name)
		}
		if got.String() != c.name {
			t.Errorf("Vault %q String() = %q", c.name, got.String())
		}
	}
	if _, ok := ParseVault("bogus"); ok {
		t.Error("ParseVault(\"bogus\") should fail")
	}
	var zero Vault
	if zero.Valid() {
		t.Error("zero-value Vault should not be Valid()")
	}
}

func TestAreaValues(t *testing.T) {
	malkaLife := []struct {
		name string
		want Area
	}{
		{"00-inbox", AreaInbox},
		{"mocs", AreaMOCs},
		{"personal", AreaPersonal},
		{"research", AreaResearch},
	}
	malkaWay := []struct {
		name string
		want Area
	}{
		{"00-inbox", AreaInbox},
		{"arcanto", AreaArcanto},
		{"carreira", AreaCarreira},
		{"cooperativa", AreaCooperativa},
		{"infra", AreaInfra},
		{"projetos-pessoais", AreaProjetosPessoais},
	}

	if got := len(areaSetByVault[VaultMalkaLife]); got != 4 {
		t.Fatalf("areaSetByVault[MalkaLife] has %d entries, want 4", got)
	}
	if got := len(areaSetByVault[VaultMalkaWay]); got != 6 {
		t.Fatalf("areaSetByVault[MalkaWay] has %d entries, want 6", got)
	}

	for _, c := range malkaLife {
		got, ok := ParseArea(VaultMalkaLife, c.name)
		if !ok {
			t.Errorf("ParseArea(MalkaLife, %q) failed", c.name)
			continue
		}
		if got != c.want {
			t.Errorf("ParseArea(MalkaLife, %q) = %v, want %v", c.name, got, c.want)
		}
		if !got.ValidFor(VaultMalkaLife) {
			t.Errorf("Area %q not ValidFor(MalkaLife)", c.name)
		}
		if got.String() != c.name {
			t.Errorf("Area %q String() = %q", c.name, got.String())
		}
	}
	for _, c := range malkaWay {
		got, ok := ParseArea(VaultMalkaWay, c.name)
		if !ok {
			t.Errorf("ParseArea(MalkaWay, %q) failed", c.name)
			continue
		}
		if got != c.want {
			t.Errorf("ParseArea(MalkaWay, %q) = %v, want %v", c.name, got, c.want)
		}
		if !got.ValidFor(VaultMalkaWay) {
			t.Errorf("Area %q not ValidFor(MalkaWay)", c.name)
		}
		if got.String() != c.name {
			t.Errorf("Area %q String() = %q", c.name, got.String())
		}
	}

	if _, ok := ParseArea(VaultMalkaLife, "bogus"); ok {
		t.Error("ParseArea(MalkaLife, \"bogus\") should fail")
	}
	if _, ok := ParseArea(VaultMalkaWay, "bogus"); ok {
		t.Error("ParseArea(MalkaWay, \"bogus\") should fail")
	}

	var zero Area
	if zero.ValidFor(VaultMalkaLife) || zero.ValidFor(VaultMalkaWay) {
		t.Error("zero-value Area should not be ValidFor either vault")
	}

	// Cross-vault rejection: values scoped to one vault must not validate for the other.
	if AreaArcanto.ValidFor(VaultMalkaLife) {
		t.Error("AreaArcanto (MalkaWay-only) should not be ValidFor(MalkaLife)")
	}
	if AreaResearch.ValidFor(VaultMalkaWay) {
		t.Error("AreaResearch (MalkaLife-only) should not be ValidFor(MalkaWay)")
	}
	if _, ok := ParseArea(VaultMalkaLife, "arcanto"); ok {
		t.Error("ParseArea(MalkaLife, \"arcanto\") should fail")
	}
	if _, ok := ParseArea(VaultMalkaWay, "research"); ok {
		t.Error("ParseArea(MalkaWay, \"research\") should fail")
	}

	// 00-inbox is the one Area shared across both vaults.
	if !AreaInbox.ValidFor(VaultMalkaLife) || !AreaInbox.ValidFor(VaultMalkaWay) {
		t.Error("AreaInbox should be ValidFor both vaults")
	}
}
