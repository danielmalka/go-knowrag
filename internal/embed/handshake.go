package embed

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/danielmalka/go-knowrag/internal/schema"
)

// The pins of the "config efetiva do embedder" are properties of the MODEL, established from the model
// card and the pinned repository — not settings someone picked (ADR-001 §4) — and they live in the
// model profile (schema.ModelProfile): revision, dense width, allowed precisions, pooling,
// normalization, sparse parameters (none for a dense-only model) and the task-prefix scheme. This
// file only compares a backend's report against them.

// ErrHandshake marks any failure to confirm the backend's effective config. A caller that cannot
// distinguish "the service is down" from "the service is serving a different model" would treat
// both as retryable; only one of them is.
var ErrHandshake = errors.New("embedder handshake failed")

// BackendHandshakeInfo is what the service reports about the config it actually loaded: the fields
// PRD-contrato §2.4 enumerates — ModelRevision, TokenizerRevision, Dim, Normalization, Pooling,
// Precision, SparseParams — plus PromptVersion, the task-prefix scheme the service applies (added
// with the dense-only EmbeddingGemma profile, where forgetting a prefix is the failure that matters).
// The list is closed, not "etc.": every one of them is confirmed against the model profile, so a field
// nobody confirms is a field declared rather than verified.
//
// A field the backend does not report must arrive at its zero value. Handshake reads a zero value
// as "not reported" and fails on it; that is the fail-closed policy of ADR-001 §4, and it is why
// this struct has no "unknown" or "optional" representation to hide behind. The two profile-driven
// exceptions are explicit and symmetric: a dense-only profile expects NO SparseParams and a model
// with no task prefix expects NO PromptVersion, and in both cases the backend reporting one is the
// divergence.
type BackendHandshakeInfo struct {
	ModelRevision     string
	TokenizerRevision string
	Dim               int
	Normalization     string
	Pooling           string
	Precision         string
	SparseParams      map[string]string
	PromptVersion     string
}

// Expected returns the pins of the default model profile.
func Expected() BackendHandshakeInfo { return ExpectedFor(schema.DefaultProfile()) }

// ExpectedFor returns the pins for one model profile. Precision is the profile's PRIMARY precision,
// for display and for fixtures: validation accepts any of the profile's allowed precisions
// (validateAgainstProfile), which is why a CPU host reporting fp32 and a GPU host reporting bf16 both
// pass for EmbeddingGemma while fp16 never does.
//
// It returns a fresh value each call: SparseParams is a map, and a shared one lets any caller
// rewrite the pins for the whole process.
func ExpectedFor(p schema.ModelProfile) BackendHandshakeInfo {
	return BackendHandshakeInfo{
		// The tokenizer ships inside the model repository at the same commit as the weights, so
		// "the tokenizer revision matching the model revision" (PRD-contrato §2.3b, ADR-001 §4 row 2)
		// is that same commit. If a future runtime sources the tokenizer from a separate repository,
		// this stops being true and the profile needs its own field.
		ModelRevision:     p.Revision,
		TokenizerRevision: p.Revision,
		Dim:               p.DenseWidth(),
		Normalization:     p.Normalization,
		Pooling:           p.Pooling,
		Precision:         p.PrimaryPrecision(),
		SparseParams:      maps.Clone(p.SparseParams),
		PromptVersion:     p.PromptVersion,
	}
}

// Handshake asks the backend what it actually loaded and fails unless every field of the
// "config efetiva do embedder" (PRD-contrato §2.4) — ModelRevision, TokenizerRevision, Dim,
// Normalization, Pooling, Precision, SparseParams, PromptVersion — match the embedder's model profile. It fails, naming the field,
// when the backend does not report it, when this build has no pin for it, and when the two differ.
//
// It should be called once, before the first EmbedDocuments/EmbedQuery, by whatever process
// constructs the embedder; a non-nil result must abort startup. ModelID() is self-attested and
// proves nothing about the backend — this is the only thing that does.
//
// "Should" and not "must", since D-33: a success here latches, and an embedder whose latch is unset
// runs this itself before it embeds anything (ensureVerified). Calling it explicitly is still the
// better shape — it fails at startup instead of inside the first request, and cmd/cli needs the
// returned report for point_hash regardless — but forgetting it no longer means embedding through a
// backend nobody confirmed.
//
// The returned BackendHandshakeInfo is the backend's own report, and it — not the pins — is what
// S06a hashes into point_hash and writes to the embedding_model payload field.
//
// Handshake is a method here rather than a member of the Embedder interface because only a
// network-backed embedder has anything to confirm; FakeEmbedder has no backend to diverge from.
func (e *ServiceEmbedder) Handshake(ctx context.Context) (BackendHandshakeInfo, error) {
	// VerifyTimeout, not Timeout. The distinction is the whole reason that field exists: this call
	// is bounded by what the caller can afford to spend confirming, which is not the same number as
	// what it can afford to spend embedding (Profile.VerifyTimeout).
	// Bound in a separate variable rather than shadowing ctx: the error path below has to be able to
	// ask the CALLER's context whether it was the one that ended, and a shadowed ctx cannot answer
	// that — it is cancelled by this method's own budget as well.
	callCtx, cancel := context.WithTimeout(ctx, e.profile.VerifyTimeout)
	defer cancel()

	info, err := e.transport.Info(callCtx)
	if err != nil {
		// The caller stopped, rather than the backend failing to answer. This package draws that
		// line everywhere else (outerCtxErr) — a client that disconnected or a process shutting down
		// is not an outage, and reporting it as one turns every cancelled request into a false
		// report that the embedding service is down.
		//
		// It has to be checked against the caller's context and not callCtx, which is cancelled by
		// this method's own budget too: VerifyTimeout expiring genuinely IS "the backend did not
		// answer", and outerCtxErr keeps that an ErrBackend while letting a cancellation through.
		//
		// The check is new with D-33, and it was harmless to omit before only because nothing called
		// Handshake inside a request. Now the first search of every process does.
		if cErr := ctx.Err(); cErr != nil {
			return BackendHandshakeInfo{}, outerCtxErr(e.profile.Endpoint, cErr)
		}
		return BackendHandshakeInfo{}, fmt.Errorf(
			"%w: could not read the backend's effective config from %s: %w",
			ErrBackend, e.profile.Endpoint, err)
	}
	if err := validateAgainstProfile(info, e.model); err != nil {
		return BackendHandshakeInfo{}, err
	}
	// The latch, set only here: this is the one place in the package that has actually compared a
	// backend's report against the pins. Everything else that wants to know whether that happened
	// reads e.verified rather than deciding for itself (D-33).
	e.verified.Store(true)
	return info, nil
}

// validateAgainstProfile is the policy for one model profile: the single-valued pins are compared
// as before (validateHandshake); Precision is checked against the profile's ALLOWED set; and a
// dense-only profile or a prefix-less model is handled by the symmetric rules documented on
// BackendHandshakeInfo. It is separate from Handshake so the policy is provable without a backend.
func validateAgainstProfile(got BackendHandshakeInfo, m schema.ModelProfile) error {
	want := ExpectedFor(m)
	// An unreported Precision is left to validateHandshake so it is named in field order, like every
	// other missing field; only a REPORTED dtype is checked against the allowed set here.
	if got.Precision != "" && !m.AllowsPrecision(got.Precision) {
		hint := ""
		if got.Precision == schema.PrecisionFP16 {
			hint = "; fp16 is refused because this model's activations overflow it, returning NaN or silently degraded vectors"
		}
		return fmt.Errorf("%w: Precision: backend reports %q, which profile %q does not allow (allowed: %v)%s",
			ErrHandshake, got.Precision, m.ID, m.Precisions, hint)
	}
	if got.Precision != "" {
		want.Precision = got.Precision // any allowed dtype is the pin; the check above is the real one
	}
	return validateHandshake(got, want)
}

// validateHandshake compares one report against one set of pins. It is separate from Handshake so
// the policy is provable without a backend, and so the pins are a parameter rather than a
// package-level variable any caller could reassign.
func validateHandshake(got, want BackendHandshakeInfo) error {
	checks := []struct {
		field string
		got   string
		want  string
	}{
		{"ModelRevision", got.ModelRevision, want.ModelRevision},
		{"TokenizerRevision", got.TokenizerRevision, want.TokenizerRevision},
		{"Dim", intField(got.Dim), intField(want.Dim)},
		{"Normalization", got.Normalization, want.Normalization},
		{"Pooling", got.Pooling, want.Pooling},
		{"Precision", got.Precision, want.Precision},
	}
	for _, c := range checks {
		if err := compareField(c.field, c.got, c.want); err != nil {
			return err
		}
	}
	if err := comparePromptVersion(got.PromptVersion, want.PromptVersion); err != nil {
		return err
	}
	return compareSparseParams(got.SparseParams, want.SparseParams)
}

// comparePromptVersion is the one field where "" is a legal pin: a model with no task prefix pins
// "" and must hear "" back. Any other disagreement — a prefix scheme the service did not report, one
// it reports when the model takes none, or a different one — means queries and documents are being
// embedded under a scheme this build did not review.
func comparePromptVersion(got, want string) error {
	if got == want {
		return nil
	}
	switch {
	case got == "":
		return fmt.Errorf("%w: the backend did not report PromptVersion; this profile pins task-prefix scheme %q and an unreported one is a divergence (ADR-001 §4)", ErrHandshake, want)
	case want == "":
		return fmt.Errorf("%w: PromptVersion: the backend applies task-prefix scheme %q, but this profile takes no prefix", ErrHandshake, got)
	default:
		return fmt.Errorf("%w: PromptVersion: backend reports %q, pinned value is %q", ErrHandshake, got, want)
	}
}

// intField renders an int field as a string so the seven checks share one comparison, and renders
// the zero value as "" so "the backend did not report Dim" and "Dim is not pinned" stay
// distinguishable from a reported 0 — which is itself not a legal dimension.
func intField(v int) string {
	if v == 0 {
		return ""
	}
	return fmt.Sprint(v)
}

func compareField(field, got, want string) error {
	if got == "" {
		return fmt.Errorf(
			"%w: the backend did not report %s; an unreported field is a divergence, not consent "+
				"(ADR-001 §4) — it feeds point_hash and cannot be assumed", ErrHandshake, field)
	}
	if want == "" {
		return fmt.Errorf(
			"%w: %s is not pinned in this build, and the backend reports %q; an absent pin fails "+
				"closed rather than accepting whatever the backend says (ADR-001 §4)",
			ErrHandshake, field, got)
	}
	if got != want {
		return fmt.Errorf("%w: %s: backend reports %q, pinned value is %q",
			ErrHandshake, field, got, want)
	}
	return nil
}

// compareSparseParams is the one field that is a map, so it names the specific differing key: a
// caller told only that "SparseParams differs" has to diff two maps by hand.
func compareSparseParams(got, want map[string]string) error {
	if len(want) == 0 && len(got) == 0 {
		return nil // dense-only profile, dense-only backend
	}
	if len(want) == 0 {
		return fmt.Errorf(
			"%w: SparseParams is not pinned for this profile (it is dense-only), and the backend reports %s; "+
				"a backend that carries a sparse head is serving a different model", ErrHandshake, renderParams(got))
	}
	if len(got) == 0 {
		return fmt.Errorf(
			"%w: the backend did not report SparseParams; without it the sparse half of every "+
				"vector is generated by parameters nobody confirmed (ADR-001 §4)", ErrHandshake)
	}
	if maps.Equal(got, want) {
		return nil
	}
	for _, k := range slices.Sorted(maps.Keys(want)) {
		g, ok := got[k]
		if !ok {
			return fmt.Errorf("%w: SparseParams: the backend did not report key %q (pinned to %q)",
				ErrHandshake, k, want[k])
		}
		if g != want[k] {
			return fmt.Errorf("%w: SparseParams: key %q: backend reports %q, pinned value is %q",
				ErrHandshake, k, g, want[k])
		}
	}
	for _, k := range slices.Sorted(maps.Keys(got)) {
		if _, ok := want[k]; !ok {
			return fmt.Errorf("%w: SparseParams: the backend reports key %q = %q, which is not pinned",
				ErrHandshake, k, got[k])
		}
	}
	return nil
}

func renderParams(m map[string]string) string {
	parts := make([]string, 0, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, fmt.Sprintf("%s=%q", k, m[k]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}
