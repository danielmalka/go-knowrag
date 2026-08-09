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

// The pinned values of the "config efetiva do embedder". Each cites where its pin comes from,
// because ADR-001 §4's whole point is that these are properties of the MODEL, established from the
// model card and the pinned repository — not settings someone picked.
//
// ExpectedNormalization and ExpectedPooling come from the BGE-M3 model card and were re-confirmed
// against the pinned revision's own files (ADR-001 §6.2: 1_Pooling/config.json has
// pooling_mode_cls_token: true; modules.json declares a 2_Normalize module). ExpectedPrecision was
// the last field left open and closed on 2026-08-09 by measurement, not by choice: the loaded
// weights report torch.float16 (ADR-001 §6.2, round 2). It is one value, for one runtime — the
// earlier "may differ between the query host and the batch host" reading died with the single-host
// topology of ADR-001 §7.1.
const (
	ExpectedNormalization = "l2"
	ExpectedPooling       = "cls"
	ExpectedPrecision     = "fp16"
)

// ExpectedSparseParams pins the sparse-generation parameters, verified against the running service
// on 2026-08-09. `kind` says the weights are BGE-M3's lexical_weights head (not SPLADE, not a
// bag-of-words fallback) and `id_space` says the indices are tokenizer vocabulary ids, which is
// what makes them comparable to a query embedded later. Both feed point_hash, so a backend that
// changed either would be writing into a different lexical space with no other symptom than
// falling recall.
func ExpectedSparseParams() map[string]string {
	return map[string]string{"kind": "lexical_weights", "id_space": "tokenizer_vocab"}
}

// ErrHandshake marks any failure to confirm the backend's effective config. A caller that cannot
// distinguish "the service is down" from "the service is serving a different model" would treat
// both as retryable; only one of them is.
var ErrHandshake = errors.New("embedder handshake failed")

// BackendHandshakeInfo is what the service reports about the config it actually loaded: the seven
// fields PRD-contrato §2.4 enumerates — ModelRevision, TokenizerRevision, Dim, Normalization,
// Pooling, Precision, SparseParams — and no others. The list is closed, not "etc.": every one of
// them feeds S06a's point_hash, so a field nobody confirms is a field of the hash that is declared
// rather than verified.
//
// A field the backend does not report must arrive at its zero value. Handshake reads a zero value
// as "not reported" and fails on it; that is the fail-closed policy of ADR-001 §4, and it is why
// this struct has no "unknown" or "optional" representation to hide behind.
type BackendHandshakeInfo struct {
	ModelRevision     string
	TokenizerRevision string
	Dim               int
	Normalization     string
	Pooling           string
	Precision         string
	SparseParams      map[string]string
}

// Expected returns this build's pinned expectations for all seven fields. All seven are pinned:
// ADR-001 §7.2 closed on 2026-08-09 and SparseParams, the last one open, was read off the chosen
// service rather than guessed.
//
// It returns a fresh value each call: SparseParams is a map, and a shared one lets any caller
// rewrite the pins for the whole process.
func Expected() BackendHandshakeInfo {
	return BackendHandshakeInfo{
		// The tokenizer ships inside BAAI/bge-m3 at the same commit as the weights, so "the
		// tokenizer revision matching the model revision" (PRD-contrato §2.3b, ADR-001 §4 row 2) is
		// that same commit. If a future runtime sources the tokenizer from a separate repository,
		// this stops being true and the pin needs its own constant in internal/schema.
		ModelRevision:     schema.BGEM3Revision,
		TokenizerRevision: schema.BGEM3Revision,
		Dim:               DenseDim,
		Normalization:     ExpectedNormalization,
		Pooling:           ExpectedPooling,
		Precision:         ExpectedPrecision,
		SparseParams:      ExpectedSparseParams(),
	}
}

// Handshake asks the backend what it actually loaded and fails unless all seven fields of the
// "config efetiva do embedder" (PRD-contrato §2.4) — ModelRevision, TokenizerRevision, Dim,
// Normalization, Pooling, Precision, SparseParams — match their pins. It fails, naming the field,
// when the backend does not report it, when this build has no pin for it, and when the two differ.
//
// It must be called once, before the first EmbedDocuments/EmbedQuery, by whatever process
// constructs the embedder; a non-nil result must abort startup. ModelID() is self-attested and
// proves nothing about the backend — this is the only thing that does.
//
// The returned BackendHandshakeInfo is the backend's own report, and it — not the pins — is what
// S06a hashes into point_hash and writes to the embedding_model payload field.
//
// Handshake is a method here rather than a member of the Embedder interface because only a
// network-backed embedder has anything to confirm; FakeEmbedder has no backend to diverge from.
func (e *ServiceEmbedder) Handshake(ctx context.Context) (BackendHandshakeInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, e.profile.Timeout)
	defer cancel()

	info, err := e.transport.Info(ctx)
	if err != nil {
		return BackendHandshakeInfo{}, fmt.Errorf(
			"%w: could not read the backend's effective config from %s: %w",
			ErrBackend, e.profile.Endpoint, err)
	}
	if err := validateHandshake(info, e.expected); err != nil {
		return BackendHandshakeInfo{}, err
	}
	return info, nil
}

// validateHandshake compares one report against one set of pins. It is separate from Handshake so
// the seven-field policy is provable without a backend, and so the pins are a parameter rather than
// a package-level variable any caller could reassign.
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
	return compareSparseParams(got.SparseParams, want.SparseParams)
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
	if len(got) == 0 {
		return fmt.Errorf(
			"%w: the backend did not report SparseParams; without it the sparse half of every "+
				"vector is generated by parameters nobody confirmed (ADR-001 §4)", ErrHandshake)
	}
	if len(want) == 0 {
		return fmt.Errorf(
			"%w: SparseParams is not pinned in this build, and the backend reports %s; an absent pin "+
				"fails closed (ADR-001 §4)", ErrHandshake, renderParams(got))
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
