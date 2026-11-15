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
// It exists so that moving to another embedding model is "register a profile and select it", not a
// hunt through the module for 1024s and revision hashes (PR 1 of the embedding migration; the
// default profile is still BGE-M3 and nothing about runtime behavior changed).
//
// "Configurable" here means selected by ID from the registry below, NOT free-form per-deployment
// fields. The revision stays a reviewed code value: a config file or env var that disagreed with the
// pinned revision would point the runtime at a different model with nothing catching it before
// ingestion, and the symptom would be falling recall weeks later (internal/embed/config.go).
type ModelProfile struct {
	// ID is the short stable name used to select the profile and in logs ("bge-m3").
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
	// Precision is the dtype the loaded weights must report in the handshake ("fp16", "bf16", "fp32").
	Precision string
	// Collections are the Qdrant collection names that hold this profile's vectors, in provisioning
	// order. They are deliberately identical in shape (design brief §2.1).
	Collections []string
}

// DefaultProfileID is the profile every entry point uses until a later PR of the migration selects
// another one.
const DefaultProfileID = "bge-m3"

var (
	revisionPattern   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	profileIDPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)
	collectionPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_]*$`)
)

// profiles is the registry. Unexported: an exported slice's backing array is writable from any
// package in the module, and one stray write would redefine "correct" for every later handshake.
var profiles = []ModelProfile{
	{
		ID:    "bge-m3",
		Model: "BAAI/bge-m3",
		// Confirmed 2026-08-08: HEAD of BAAI/bge-m3 on Hugging Face; the weights repo had no new
		// commit in about two years. Moved here from the former schema.DefaultProfile().Revision constant.
		Revision:  "5617a9f61b028005a4858fdac845db406aefb181",
		DenseDim:  1024,
		Precision: "fp16", // measured on the loaded weights (ADR-001 §6.2, round 2)
		// interno: the operator's own vaults, the only collection populated in this build. clientes
		// and base_paga are provisioned empty so nothing ever grows a special case for the one that
		// has data in it.
		Collections: []string{"interno", "clientes", "base_paga"},
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

func (p ModelProfile) clone() ModelProfile {
	p.Collections = slices.Clone(p.Collections)
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
	if p.Precision == "" {
		return fmt.Errorf("model profile %q: Precision is empty", p.ID)
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
