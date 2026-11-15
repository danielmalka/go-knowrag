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
	if !slices.Equal(p.Precisions, []string{"fp16"}) || p.PrimaryPrecision() != "fp16" {
		t.Errorf("Precisions = %v, want [fp16]", p.Precisions)
	}
	if p.Pooling != "cls" || p.Normalization != "l2" || p.PromptVersion != "" {
		t.Errorf("Pooling/Normalization/PromptVersion = %q/%q/%q, want cls/l2/none", p.Pooling, p.Normalization, p.PromptVersion)
	}
	if !p.HasSparse() || p.SparseParams["kind"] != "lexical_weights" || p.SparseParams["id_space"] != "tokenizer_vocab" {
		t.Errorf("SparseParams = %v, want the BGE-M3 lexical_weights head", p.SparseParams)
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
		"no precision":         func(p *ModelProfile) { p.Precisions = nil },
		"unknown precision":    func(p *ModelProfile) { p.Precisions = []string{"int8"} },
		"duplicate precision":  func(p *ModelProfile) { p.Precisions = []string{"fp16", "fp16"} },
		"no pooling":           func(p *ModelProfile) { p.Pooling = "" },
		"no normalization":     func(p *ModelProfile) { p.Normalization = "" },
		"bad prompt version":   func(p *ModelProfile) { p.PromptVersion = "Has Space" },
		"no input window":      func(p *ModelProfile) { p.MaxInputTokens = 0 },
		"negative reserve":     func(p *ModelProfile) { p.PromptReserveTokens = -1 },
		"reserve eats window":  func(p *ModelProfile) { p.PromptReserveTokens = p.MaxInputTokens },
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
		DenseDim: 768, Precisions: []string{"bf16"}, Pooling: "mean", Normalization: "l2", Collections: []string{"nova"},
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

// EmbeddingGemma 2 is registered but is NOT the default: the switch is a later PR. Its pins are
// spelled out in literals from the model card, and the one that matters most is what it refuses.
func TestEmbeddingGemmaProfile_PinsFromTheModelCard(t *testing.T) {
	p, ok := ProfileByID("embeddinggemma-2")
	if !ok {
		t.Fatal("the embeddinggemma-2 profile is not registered")
	}
	if p.ID == DefaultProfileID {
		t.Fatal("EmbeddingGemma must not be the default yet")
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if p.Model != "google/embeddinggemma-2" || p.DenseDim != 768 || p.Pooling != "mean" || p.Normalization != "l2" {
		t.Errorf("model/dim/pooling/normalization = %q/%d/%q/%q", p.Model, p.DenseDim, p.Pooling, p.Normalization)
	}
	if p.HasSparse() {
		t.Errorf("EmbeddingGemma is dense-only, got SparseParams %v", p.SparseParams)
	}
	if !p.AllowsPrecision("bf16") || !p.AllowsPrecision("fp32") {
		t.Errorf("bf16 (GPU) and fp32 (CPU) must be allowed: %v", p.Precisions)
	}
	if p.AllowsPrecision("fp16") {
		t.Fatal("fp16 must be refused: EmbeddingGemma returns NaN or silently degraded vectors in fp16")
	}
	if p.PromptVersion == "" {
		t.Error("EmbeddingGemma needs a task-prefix scheme in the handshake")
	}
	m := ManifestFor(p)
	if len(m) != len(p.Collections) || m[0].DenseDim != 768 || m[0].ModelRevision != p.Revision {
		t.Fatalf("ManifestFor(EG2) = %+v", m)
	}
	for _, c := range m {
		for _, old := range DefaultProfile().Collections {
			if c.Name == old {
				t.Errorf("the new collection %q collides with the BGE-M3 one", c.Name)
			}
		}
	}
}

func TestProfile_AllowsPrecision(t *testing.T) {
	bge := DefaultProfile()
	if !bge.AllowsPrecision("fp16") || bge.AllowsPrecision("bf16") {
		t.Errorf("BGE-M3 allows only fp16, got %v", bge.Precisions)
	}
}

// Two profiles must never share a collection name: it would put two models' vectors, of possibly
// different widths, into one collection. This is also the guard for the final EG2 names.
func TestProfiles_CollectionNamesAreDisjoint(t *testing.T) {
	owner := map[string]string{}
	for _, p := range Profiles() {
		for _, c := range p.Collections {
			if other, dup := owner[c]; dup {
				t.Errorf("collection %q is claimed by both %q and %q", c, other, p.ID)
			}
			owner[c] = p.ID
		}
	}
}

func TestEmbeddingGemmaProfile_FinalCollectionNames(t *testing.T) {
	p, _ := ProfileByID("embeddinggemma-2")
	want := []string{"interno_eg2", "clientes_eg2", "base_paga_eg2"}
	if !slices.Equal(p.Collections, want) {
		t.Errorf("Collections = %v, want %v", p.Collections, want)
	}
}

// A dense-only manifest entry names no sparse vector; a hybrid one still does.
func TestManifestFor_SparseVectorNameFollowsTheProfile(t *testing.T) {
	eg2, _ := ProfileByID("embeddinggemma-2")
	for _, c := range ManifestFor(eg2) {
		if c.SparseVectorName != "" {
			t.Errorf("%s: dense-only entry names sparse vector %q", c.Name, c.SparseVectorName)
		}
	}
	for _, c := range Manifest() {
		if c.SparseVectorName != SparseVectorName {
			t.Errorf("%s: default entry lost its sparse vector: %q", c.Name, c.SparseVectorName)
		}
	}
}

// The window the chunker may fill is the model's input window minus what the prompt prefix takes.
func TestProfiles_TokenWindows(t *testing.T) {
	bge := DefaultProfile()
	if bge.MaxInputTokens != 8192 || bge.PromptReserveTokens != 0 || bge.WindowTokens() != 8192 || bge.NeedsTitle() {
		t.Errorf("BGE-M3 window = %d (max %d, reserve %d, needs title %v), want 8192/0 and no title",
			bge.WindowTokens(), bge.MaxInputTokens, bge.PromptReserveTokens, bge.NeedsTitle())
	}
	eg2, _ := ProfileByID("embeddinggemma-2")
	if eg2.MaxInputTokens != 8192 || eg2.PromptReserveTokens != 128 || eg2.WindowTokens() != 8064 || !eg2.NeedsTitle() {
		t.Errorf("EmbeddingGemma window = %d (max %d, reserve %d, needs title %v), want 8064 from 8192-128 and a title",
			eg2.WindowTokens(), eg2.MaxInputTokens, eg2.PromptReserveTokens, eg2.NeedsTitle())
	}
}
