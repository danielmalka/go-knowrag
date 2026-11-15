package schema

import (
	"fmt"
	"regexp"
	"slices"
)

// ModelProfile is everything about one embedding model that the rest of the build used to spell as a
// literal: which model, at which immutable revision, how wide its dense vectors are, what precision
// the loaded weights must report, and which collections hold its vectors.
//
// It exists so that moving to another embedding model is "register a profile and make it the default",
// not a hunt through the module for vector widths and revision hashes. One profile is registered today,
// EmbeddingGemma 2; BGE-M3, the model before it, was removed after the cutover (no way back, owner's
// decision of 2026-11-15) together with everything that existed only for its sparse head.
//
// The profile is a reviewed code value, NOT free-form per-deployment fields. The revision stays in code:
// a config file or env var that disagreed with the pinned revision would point the runtime at a
// different model with nothing catching it before ingestion, and the symptom would be falling recall
// weeks later (internal/embed/config.go).
type ModelProfile struct {
	// ID is the short stable name used to look the profile up and in logs ("embeddinggemma-2").
	ID string
	// Model is the HuggingFace repository id, informational and used in error messages.
	Model string
	// Revision is the pinned, immutable HuggingFace commit of Model (40 hex characters). Changing it
	// means every stored vector came from a different model, so it is drift and requires reindexing
	// (PRD-contrato §2.3b).
	Revision string
	// DenseDim is the dense output width. uint64 because that is the width the Qdrant wire type
	// wants; converting at each call site would only move the conversion somewhere less visible.
	DenseDim uint64
	// Precisions are the dtypes the loaded weights may report in the handshake, in preference order
	// (the first is PrimaryPrecision). EmbeddingGemma lists bf16 and fp32, both of which run on the
	// GPU, and does NOT list fp16, so a backend that loads it in fp16 fails the handshake: its
	// activations overflow fp16 and it returns NaN or silently degraded vectors.
	Precisions []string
	// Pooling and Normalization are the pooling mode and the vector normalization the model card
	// fixes ("mean", "l2"); the handshake confirms the backend reports exactly these.
	Pooling       string
	Normalization string
	// PromptVersion names the task-prefix scheme the backend applies to a query and to a document
	// ("" = the model uses no prefix, which no registered profile does today). The service applies the
	// prefix, not this client; the handshake
	// confirms the scheme so a service that forgot or changed it fails loudly instead of embedding
	// queries and documents into subtly different spaces.
	PromptVersion string
	// MaxInputTokens is the model's input window in tokens, as its tokenizer counts them
	// (EmbeddingGemma 2: "8K" on the model card). A chunk above it cannot be embedded and is never
	// truncated (internal/chunk).
	MaxInputTokens int
	// PromptReserveTokens is the part of that window the service's task prefix and the note title take
	// (EmbeddingGemma: "title: <title> | text: " in front of every document). The chunker is given
	// MaxInputTokens minus this, so a chunk that fills its ceiling still fits once the prefix is added.
	// It is an upper bound chosen with margin, not a measurement of every title: a note title has no
	// length limit of its own, so ingest cuts the title the service is sent to TitleMaxTokens (below),
	// which is what makes this reserve a guarantee and not a hope.
	PromptReserveTokens int
	// Collections are the Qdrant collection names that hold this profile's vectors, in provisioning
	// order. They are deliberately identical in shape (design brief §2.1).
	Collections []string
}

// DefaultProfileID is the profile every entry point serves: EmbeddingGemma 2, the only one registered.
const DefaultProfileID = "embeddinggemma-2"

// HasCollection reports whether name is one of this profile's collections.
func (p ModelProfile) HasCollection(name string) bool { return slices.Contains(p.Collections, name) }

// Precision spellings used across the build (the wire dtype names are translated to these at
// internal/embed/http.go).
const (
	PrecisionFP16 = "fp16"
	PrecisionBF16 = "bf16"
	PrecisionFP32 = "fp32"
)

var knownPrecisions = []string{PrecisionFP16, PrecisionBF16, PrecisionFP32}

var (
	revisionPattern   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	profileIDPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)
	collectionPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_]*$`)
)

// profiles is the registry. Unexported: an exported slice's backing array is writable from any
// package in the module, and one stray write would redefine "correct" for every later handshake.
var profiles = []ModelProfile{
	{
		ID:    "embeddinggemma-2",
		Model: "google/embeddinggemma-2",
		// Head of google/embeddinggemma-2 on Hugging Face as read on 2026-11-15 (last commit
		// 2026-10-28, before the trial). The trial itself did not pin a revision; this is the
		// reviewable pin the migration PRs use, and it is re-confirmed when the service is built.
		Revision:   "f9972ac22e696a105c482202646979e33e4b5a1e",
		DenseDim:   768,
		Precisions: []string{PrecisionBF16, PrecisionFP32}, // NEVER fp16: model card, "Do not use float16"
		// Model card: mean pooling, an L2 Normalize module, text-only dense 768d.
		Pooling:       "mean",
		Normalization: "l2",
		// Query: "task: search result | query: " + text. Document: "title: " + (title or "none") +
		// " | text: " + text. Applied by the embedding service (model card, prompts).
		PromptVersion: "gemma-search-v1",
		// The card says "8K token context window"; the trial's longest document was 3869 tokens, nothing
		// near it. 128 covers "title: ... | text: " plus a long note title with margin.
		MaxInputTokens:      8192,
		PromptReserveTokens: 128,
		// The names keep the trust-boundary name and add the model as a suffix: the suffix is the only
		// place an operator, a Qdrant dashboard or a log line shows which vector space a collection holds
		// (the model revision lives in the applied-state record, not in Qdrant), a later model switch
		// repeats the pattern with its own suffix instead of reusing a name for a different width, and a
		// generation number such as "_v2" says nothing and collides with the golden-set "v2". Profiles
		// never share a collection name (TestProfiles_CollectionNamesAreDisjoint). The collections of the
		// retired BGE-M3 profile (`interno`, `clientes`, `base_paga`) are not managed by this build.
		Collections: []string{"interno_eg2", "clientes_eg2", "base_paga_eg2"},
	},
}

// Profiles returns a fresh deep copy of the registry.
func Profiles() []ModelProfile {
	out := make([]ModelProfile, len(profiles))
	for i, p := range profiles {
		out[i] = p.clone()
	}
	return out
}

// ProfileByID returns a copy of the registered profile with that ID.
func ProfileByID(id string) (ModelProfile, bool) {
	for _, p := range profiles {
		if p.ID == id {
			return p.clone(), true
		}
	}
	return ModelProfile{}, false
}

// DefaultProfile returns a copy of the profile named by DefaultProfileID.
func DefaultProfile() ModelProfile {
	p, ok := ProfileByID(DefaultProfileID)
	if !ok {
		// The registry test pins this; reaching it means someone edited one half of the pair.
		panic(fmt.Sprintf("schema: default model profile %q is not registered", DefaultProfileID))
	}
	return p
}

// maxDenseDim bounds DenseDim: no embedding model this project could host is anywhere near it, and it
// is what makes DenseWidth's conversion safe.
const maxDenseDim = 1 << 16

// DenseWidth is DenseDim as an int, for callers that index or size Go slices (Qdrant's wire type is
// uint64, hence the field). Validate rejects any profile above maxDenseDim, so the conversion cannot
// overflow for a validated profile.
func (p ModelProfile) DenseWidth() int {
	return int(p.DenseDim) // #nosec G115 -- bounded by Validate (DenseDim <= maxDenseDim)
}

// WindowTokens is the token window a chunk may fill: the input window minus the prompt reserve.
func (p ModelProfile) WindowTokens() int { return p.MaxInputTokens - p.PromptReserveTokens }

// promptFramingTokens is what the fixed words of the document prefix ("title: " and " | text: ") take
// out of PromptReserveTokens, with slack for the token boundaries a title can merge into. Measured at
// 7 tokens with the EmbeddingGemma tokenizer (README of the embedder service); 32 leaves a wide margin.
const promptFramingTokens = 32

// TitleMaxTokens is the most tokens a note title may take in the document prefix: the prompt reserve
// minus the framing. Ingest truncates the title it sends to this (never the chunk text), so a chunk
// of WindowTokens plus its prefix never passes MaxInputTokens, whatever the note is titled. Zero for a
// profile with no prompt scheme, whose requests carry no title.
func (p ModelProfile) TitleMaxTokens() int {
	if !p.NeedsTitle() {
		return 0
	}
	return p.PromptReserveTokens - promptFramingTokens
}

// NeedsTitle reports whether the service builds a document prefix that includes the note title, which
// is the case for every profile that pins a prompt scheme.
func (p ModelProfile) NeedsTitle() bool { return p.PromptVersion != "" }

// PrimaryPrecision is the first (preferred) allowed precision, or "" for a profile with none.
func (p ModelProfile) PrimaryPrecision() string {
	if len(p.Precisions) == 0 {
		return ""
	}
	return p.Precisions[0]
}

// AllowsPrecision reports whether the backend may report this dtype for the profile.
func (p ModelProfile) AllowsPrecision(precision string) bool {
	return slices.Contains(p.Precisions, precision)
}

func (p ModelProfile) clone() ModelProfile {
	p.Collections = slices.Clone(p.Collections)
	p.Precisions = slices.Clone(p.Precisions)
	return p
}

// Validate rejects a profile that cannot describe a working model, naming the offending field and
// value. A half-built profile must fail where it is constructed, not on the first embedding.
func (p ModelProfile) Validate() error {
	if !profileIDPattern.MatchString(p.ID) {
		return fmt.Errorf("model profile: ID %q is not a lower-case identifier (^[a-z0-9][a-z0-9.-]*$)", p.ID)
	}
	if p.Model == "" {
		return fmt.Errorf("model profile %q: Model is empty", p.ID)
	}
	if !revisionPattern.MatchString(p.Revision) {
		return fmt.Errorf("model profile %q: Revision %q is not a 40-character commit hash; a branch or tag name is mutable and cannot pin a model", p.ID, p.Revision)
	}
	if p.DenseDim == 0 || p.DenseDim > maxDenseDim {
		return fmt.Errorf("model profile %q: DenseDim %d is outside 1..%d", p.ID, p.DenseDim, maxDenseDim)
	}
	if err := p.validatePrecisions(); err != nil {
		return err
	}
	if p.Pooling == "" || p.Normalization == "" {
		return fmt.Errorf("model profile %q: Pooling %q and Normalization %q must both be set", p.ID, p.Pooling, p.Normalization)
	}
	if p.MaxInputTokens <= 0 || p.PromptReserveTokens < 0 || p.PromptReserveTokens >= p.MaxInputTokens {
		return fmt.Errorf("model profile %q: MaxInputTokens %d and PromptReserveTokens %d must satisfy 0 <= reserve < max",
			p.ID, p.MaxInputTokens, p.PromptReserveTokens)
	}
	if p.NeedsTitle() && p.TitleMaxTokens() <= 0 {
		return fmt.Errorf("model profile %q: PromptReserveTokens %d leaves no room for a title (framing takes %d)",
			p.ID, p.PromptReserveTokens, promptFramingTokens)
	}
	if p.PromptVersion != "" && !profileIDPattern.MatchString(p.PromptVersion) {
		return fmt.Errorf("model profile %q: PromptVersion %q is not a lower-case identifier", p.ID, p.PromptVersion)
	}
	if len(p.Collections) == 0 {
		return fmt.Errorf("model profile %q: no collections", p.ID)
	}
	seen := map[string]bool{}
	for _, c := range p.Collections {
		if !collectionPattern.MatchString(c) {
			return fmt.Errorf("model profile %q: collection name %q is not ^[a-z0-9][a-z0-9_]*$", p.ID, c)
		}
		if seen[c] {
			return fmt.Errorf("model profile %q: collection %q is listed twice", p.ID, c)
		}
		seen[c] = true
	}
	return nil
}

func (p ModelProfile) validatePrecisions() error {
	if len(p.Precisions) == 0 {
		return fmt.Errorf("model profile %q: Precisions is empty", p.ID)
	}
	seen := map[string]bool{}
	for _, pr := range p.Precisions {
		if !slices.Contains(knownPrecisions, pr) {
			return fmt.Errorf("model profile %q: precision %q is not one of %v", p.ID, pr, knownPrecisions)
		}
		if seen[pr] {
			return fmt.Errorf("model profile %q: precision %q is listed twice", p.ID, pr)
		}
		seen[pr] = true
	}
	return nil
}
