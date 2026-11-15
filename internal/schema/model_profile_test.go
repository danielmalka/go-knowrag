package schema

import (
	"slices"
	"strings"
	"testing"
)

// The default profile is spelled out in literals on purpose: it is the second, independent statement
// of what the build embeds with, so a silent edit to the registry cannot agree with itself. The
// values come from the model card and the pinned repository.
func TestDefaultProfile_IsEmbeddingGemma2(t *testing.T) {
	p := DefaultProfile()
	if p.ID != "embeddinggemma-2" || p.Model != "google/embeddinggemma-2" {
		t.Errorf("ID/Model = %q/%q, want embeddinggemma-2 / google/embeddinggemma-2", p.ID, p.Model)
	}
	if DefaultProfileID != "embeddinggemma-2" {
		t.Errorf("DefaultProfileID = %q, want embeddinggemma-2", DefaultProfileID)
	}
	if p.Revision != "f9972ac22e696a105c482202646979e33e4b5a1e" {
		t.Errorf("Revision = %q, want the pinned EmbeddingGemma 2 commit", p.Revision)
	}
	if p.DenseDim != 768 || p.DenseWidth() != 768 {
		t.Errorf("DenseDim = %d, want 768", p.DenseDim)
	}
	if p.Pooling != "mean" || p.Normalization != "l2" || p.PromptVersion != "gemma-search-v1" {
		t.Errorf("Pooling/Normalization/PromptVersion = %q/%q/%q, want mean/l2/gemma-search-v1", p.Pooling, p.Normalization, p.PromptVersion)
	}
	if want := []string{"interno_eg2", "clientes_eg2", "base_paga_eg2"}; !slices.Equal(p.Collections, want) {
		t.Errorf("Collections = %v, want %v", p.Collections, want)
	}
}

// The precision rule is the one that matters most: bf16 and fp32 both run on the GPU, and fp16 returns
// NaN or silently degraded vectors, so a backend that loads the weights in fp16 must fail the handshake.
func TestDefaultProfile_AllowsBF16AndFP32ButNeverFP16(t *testing.T) {
	p := DefaultProfile()
	if !p.AllowsPrecision("bf16") || !p.AllowsPrecision("fp32") || p.PrimaryPrecision() != "bf16" {
		t.Errorf("bf16 (primary) and fp32 must be allowed: %v", p.Precisions)
	}
	if p.AllowsPrecision("fp16") {
		t.Fatal("fp16 must be refused: EmbeddingGemma returns NaN or silently degraded vectors in fp16")
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
	if _, ok := ProfileByID("bge-m3"); ok {
		t.Error("the retired BGE-M3 profile is registered again")
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
	p.Precisions[0] = "poisoned"
	again := DefaultProfile()
	if again.Revision == "poisoned" || again.Collections[0] == "poisoned" || again.Precisions[0] == "poisoned" {
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
		"upper-case ID":        func(p *ModelProfile) { p.ID = "EmbeddingGemma-2" },
		"empty model":          func(p *ModelProfile) { p.Model = "" },
		"short revision":       func(p *ModelProfile) { p.Revision = "main" },
		"non-hex revision":     func(p *ModelProfile) { p.Revision = strings.Repeat("z", 40) },
		"zero dimension":       func(p *ModelProfile) { p.DenseDim = 0 },
		"absurd dimension":     func(p *ModelProfile) { p.DenseDim = 1 << 40 },
		"no precision":         func(p *ModelProfile) { p.Precisions = nil },
		"unknown precision":    func(p *ModelProfile) { p.Precisions = []string{"int8"} },
		"duplicate precision":  func(p *ModelProfile) { p.Precisions = []string{"bf16", "bf16"} },
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
			p.Precisions = slices.Clone(good.Precisions)
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
// collection set, with no other edit. That is what keeps a later model a registry entry.
func TestManifestFor_FollowsTheProfile(t *testing.T) {
	p := ModelProfile{
		ID: "other", Model: "org/other", Revision: strings.Repeat("a", 40),
		DenseDim: 512, Precisions: []string{"bf16"}, Pooling: "mean", Normalization: "l2", Collections: []string{"nova"},
	}
	m := ManifestFor(p)
	if len(m) != 1 || m[0].Name != "nova" || m[0].DenseDim != 512 || m[0].ModelRevision != p.Revision {
		t.Fatalf("ManifestFor(other) = %+v", m)
	}
	// Manifest() is the default profile's.
	def := Manifest()
	if len(def) != 3 || def[0].DenseDim != 768 || def[0].Name != "interno_eg2" {
		t.Fatalf("Manifest() = %+v, want EmbeddingGemma 2's three 768-wide collections", def)
	}
}

// Two profiles must never share a collection name: it would put two models' vectors, of possibly
// different widths, into one collection. With one registered profile this is trivially true; it is the
// guard for the day a second one is added.
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
	for _, retired := range []string{"interno", "clientes", "base_paga"} {
		if _, claimed := owner[retired]; claimed {
			t.Errorf("%q is a retired BGE-M3 collection name and must not be managed by a profile", retired)
		}
	}
}

// The window the chunker may fill is the model's input window minus what the prompt prefix takes.
func TestDefaultProfile_TokenWindow(t *testing.T) {
	p := DefaultProfile()
	if p.MaxInputTokens != 8192 || p.PromptReserveTokens != 128 || p.WindowTokens() != 8064 || !p.NeedsTitle() {
		t.Errorf("window = %d (max %d, reserve %d, needs title %v), want 8064 from 8192-128 and a title",
			p.WindowTokens(), p.MaxInputTokens, p.PromptReserveTokens, p.NeedsTitle())
	}
}

func TestProfiles_TitleBudgetKeepsAFullChunkInsideTheModelWindow(t *testing.T) {
	for _, p := range Profiles() {
		if !p.NeedsTitle() {
			if p.TitleMaxTokens() != 0 {
				t.Errorf("%s: TitleMaxTokens = %d, want 0 for a profile with no prompt scheme", p.ID, p.TitleMaxTokens())
			}
			continue
		}
		if got := p.WindowTokens() + p.TitleMaxTokens() + promptFramingTokens; got > p.MaxInputTokens {
			t.Errorf("%s: chunk window %d + title %d + framing %d = %d, over the model window %d",
				p.ID, p.WindowTokens(), p.TitleMaxTokens(), promptFramingTokens, got, p.MaxInputTokens)
		}
	}
	if got := DefaultProfile().TitleMaxTokens(); got != 96 {
		t.Errorf("EmbeddingGemma TitleMaxTokens = %d, want 96 (128 reserve - 32 framing)", got)
	}
}

func TestModelProfile_Validate_RejectsAReserveWithNoRoomForATitle(t *testing.T) {
	p := DefaultProfile()
	p.PromptReserveTokens = promptFramingTokens
	if err := p.Validate(); err == nil {
		t.Fatal("Validate accepted a prompt scheme whose reserve leaves no room for a title")
	}
}

func TestModelProfile_HasCollection(t *testing.T) {
	p := DefaultProfile()
	if !p.HasCollection("interno_eg2") || p.HasCollection("interno") {
		t.Errorf("HasCollection: interno_eg2=%v interno=%v, want true and false", p.HasCollection("interno_eg2"), p.HasCollection("interno"))
	}
}
