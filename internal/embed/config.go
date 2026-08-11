package embed

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"time"

	"gopkg.in/yaml.v3"
)

// Profile configures *where* the embedding service is and *how* this client talks to it. Nothing
// more: there is deliberately no model-revision field.
//
// PRD-contrato §2.3b pins BGE-M3 "by immutable revision (commit hash)" — a project-level constant
// (schema.BGEM3Revision), not a per-deployment setting. A ModelRevision field here would be exactly
// the seam that rule forbids: a config file or env var disagreeing with the constant would point
// the runtime at a different revision with nothing catching it before ingestion, and the symptom
// would be falling recall weeks later, not an error. Changing the revision means reindexing all
// three collections, so it has to be a deliberate code change, not a config edit. The field's
// absence closes that by construction rather than by a runtime check —
// TestProfile_HasNoModelRevisionField keeps it absent.
type Profile struct {
	Endpoint string `yaml:"endpoint"`
	// Timeout bounds ONE attempt, not the whole call: with MaxRetries attempts the wall-clock
	// ceiling is roughly Timeout*MaxRetries plus backoff. A caller who needs a hard overall bound
	// passes a context with a deadline, which is honoured on top of this.
	Timeout       time.Duration `yaml:"timeout"`
	BatchSize     int           `yaml:"batch_size"`
	MaxConcurrent int           `yaml:"max_concurrent"`
	// MaxRetries is the total number of attempts, not the number of extra ones: MaxRetries=1 means
	// one try and no retry. Named as the story names it (S04 T5) rather than renamed here, where
	// the mismatch with its own acceptance criterion would be worse than the awkward word.
	MaxRetries int `yaml:"max_retries"`
	// VerifyTimeout bounds the handshake, and it is a separate number from Timeout because the two
	// answer to different budgets.
	//
	// Timeout is per embedding attempt and is sized against how long inference takes. The handshake
	// does no inference — it reads static configuration off an already-loaded model — so on a healthy
	// service it is the cheapest call this client makes. What it has to survive is the opposite of
	// slow work: a backend that accepts the connection and then does not answer. A caller verifying
	// at startup can wait that out; a caller verifying inside a request someone is waiting on cannot,
	// and what it can afford is whatever its own deadline has left after the embedding it still owes.
	//
	// One field, two callers, two answers: cmd/mcp-server's boot check sets 30 s and its search path
	// sets 1,5 s. Before this existed, Handshake used Timeout and the search path's first verification
	// could spend 4 s of a 10 s deadline budgeted for an 8,25 s embedding leg — 12,25 s of work inside
	// a 10 s bound (D-33, found by review).
	VerifyTimeout time.Duration `yaml:"verify_timeout"`
}

// Validate rejects a profile that cannot describe a working client, at construction time rather
// than on the first embedding of a 3000-chunk run. Every zero value here is a mistake, not a
// "use the default": there is no default endpoint, and a zero timeout or batch size would either
// hang or spin.
func (p Profile) Validate() error {
	if p.Endpoint == "" {
		return fmt.Errorf("embed profile: Endpoint is empty")
	}
	if p.Timeout <= 0 {
		return fmt.Errorf("embed profile: Timeout = %v, must be positive", p.Timeout)
	}
	if p.BatchSize <= 0 {
		return fmt.Errorf("embed profile: BatchSize = %d, must be positive", p.BatchSize)
	}
	if p.MaxConcurrent <= 0 {
		return fmt.Errorf("embed profile: MaxConcurrent = %d, must be positive", p.MaxConcurrent)
	}
	if p.MaxRetries <= 0 {
		return fmt.Errorf("embed profile: MaxRetries = %d, must be at least 1 (it counts attempts, "+
			"not extra attempts)", p.MaxRetries)
	}
	if p.VerifyTimeout <= 0 {
		return fmt.Errorf("embed profile: VerifyTimeout = %v, must be positive; it bounds the "+
			"handshake, which every embedder now runs before its first embedding, and a zero here "+
			"would be an unbounded call inside whatever request happened to trigger it",
			p.VerifyTimeout)
	}
	return nil
}

// ParseProfile decodes one embedder profile from YAML in strict mode, so an unknown key is an error
// naming the key rather than a setting that silently never took effect.
//
// This is the mechanism behind the acceptance criterion "a config file declaring a model revision
// is rejected at parse time, naming the offending key": model_revision is not a field of Profile,
// so strict decoding rejects it by name. It is the same strict decoder internal/config already uses
// (S01 T4), reused rather than reinvented.
func ParseProfile(data []byte) (Profile, error) {
	// yaml.Unmarshal ignores unknown keys; only a Decoder can be told to reject them.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var p Profile
	if err := dec.Decode(&p); err != nil && !errors.Is(err, io.EOF) {
		return Profile{}, fmt.Errorf("embed profile: %w", err)
	}
	return p, nil
}
