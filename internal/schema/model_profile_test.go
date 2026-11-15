package schema

import (
	"slices"
	"strings"
	"testing"
)

// The default profile is spelled out in literals on purpose: it is the second, independent statement
// of what the build embeds with, so a silent edit to the registry cannot agree with itself. PR 1 of
// the embedding migration changes no behavior; this test is the proof that the values did not move.
func TestDefaultProfile_IsBGEM3AsBefore(t *testing.T) {
	p := DefaultProfile()
	if p.ID != "bge-m3" || p.Model != "BAAI/bge-m3" {
		t.Errorf("ID/Model = %q/%q, want bge-m3 / BAAI/bge-m3", p.ID, p.Model)
	}
	if p.Revision != "5617a9f61b028005a4858fdac845db406aefb181" {
		t.Errorf("Revision = %q, want the pinned BGE-M3 commit", p.Revision)
	}
	if p.DenseDim != 1024 {
		t.Errorf("DenseDim = %d, want 1024", p.DenseDim)
	}
	if p.Precision != "fp16" {
		t.Errorf("Precision = %q, want fp16", p.Precision)
	}
	if want := []string{"interno", "clientes", "base_paga"}; !slices.Equal(p.Collections, want) {
		t.Errorf("Collections = %v, want %v", p.Collections, want)
	}
	if p.ID != DefaultProfileID {
		t.Errorf("DefaultProfile().ID = %q, DefaultProfileID = %q", p.ID, DefaultProfileID)
	}
}

func TestProfiles_RegistryIsConsistent(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range Profiles() {
		if err := p.Validate(); err != nil {
			t.Errorf("registered profile %q is invalid: %v", p.ID, err)
		}
		if seen[p.ID] {
			t.Errorf("profile id %q registered twice", p.ID)
		}
		seen[p.ID] = true
		got, ok := ProfileByID(p.ID)
		if !ok || got.Revision != p.Revision {
			t.Errorf("ProfileByID(%q) = %+v, %v", p.ID, got, ok)
		}
	}
	if !seen[DefaultProfileID] {
		t.Fatalf("the default profile %q is not registered", DefaultProfileID)
	}
	if _, ok := ProfileByID("no-such-model"); ok {
		t.Error("ProfileByID invented a profile")
	}
}

// The registry hands out copies: a caller that rewrites the revision or a collection name must not
// redefine what "correct" means for the rest of the process (same rule as Manifest()).
func TestProfiles_AreCopies(t *testing.T) {
	p := DefaultProfile()
	p.Revision = "poisoned"
	p.Collections[0] = "poisoned"
	again := DefaultProfile()
	if again.Revision == "poisoned" || again.Collections[0] == "poisoned" {
		t.Fatal("DefaultProfile() shares state with its callers")
	}
	all := Profiles()
	all[0].ID = "poisoned"
	if Profiles()[0].ID == "poisoned" {
		t.Fatal("Profiles() shares its backing array")
	}
}

func TestModelProfile_Validate_RejectsHalfBuiltProfiles(t *testing.T) {
	good := DefaultProfile()
	cases := map[string]func(*ModelProfile){
		"empty ID":             func(p *ModelProfile) { p.ID = "" },
		"upper-case ID":        func(p *ModelProfile) { p.ID = "BGE-M3" },
		"empty model":          func(p *ModelProfile) { p.Model = "" },
		"short revision":       func(p *ModelProfile) { p.Revision = "main" },
		"non-hex revision":     func(p *ModelProfile) { p.Revision = strings.Repeat("z", 40) },
		"zero dimension":       func(p *ModelProfile) { p.DenseDim = 0 },
		"absurd dimension":     func(p *ModelProfile) { p.DenseDim = 1 << 40 },
		"empty precision":      func(p *ModelProfile) { p.Precision = "" },
		"no collections":       func(p *ModelProfile) { p.Collections = nil },
		"duplicate collection": func(p *ModelProfile) { p.Collections = []string{"a", "a"} },
		"bad collection name":  func(p *ModelProfile) { p.Collections = []string{"Has Space"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := good
			p.Collections = slices.Clone(good.Collections)
			mutate(&p)
			err := p.Validate()
			if err == nil {
				t.Fatal("Validate accepted a broken profile")
			}
			if !strings.Contains(err.Error(), "model profile") {
				t.Errorf("error %q does not say it is about the model profile", err)
			}
		})
	}
}

// The manifest is derived from a profile: another profile means another dimension, revision and
// collection set, with no other edit.
func TestManifestFor_FollowsTheProfile(t *testing.T) {
	p := ModelProfile{
		ID: "other", Model: "org/other", Revision: strings.Repeat("a", 40),
		DenseDim: 768, Precision: "bf16", Collections: []string{"nova"},
	}
	m := ManifestFor(p)
	if len(m) != 1 || m[0].Name != "nova" || m[0].DenseDim != 768 || m[0].ModelRevision != p.Revision {
		t.Fatalf("ManifestFor(other) = %+v", m)
	}
	// Manifest() is the default profile's, as before.
	def := Manifest()
	if len(def) != 3 || def[0].DenseDim != 1024 {
		t.Fatalf("Manifest() changed: %+v", def)
	}
}
