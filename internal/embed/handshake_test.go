package embed

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/schema"
)

// fullyPinned is a complete set of expectations, used as the `want` side of the tests below. It is
// spelled out rather than delegating to Expected() so a test that blanks one field is visibly
// varying one thing, and so an accidental edit to the pins shows up as a failure here too.
func fullyPinned() BackendHandshakeInfo {
	return BackendHandshakeInfo{
		ModelRevision:     schema.DefaultProfile().Revision,
		TokenizerRevision: schema.DefaultProfile().Revision,
		Dim:               DefaultDenseDim(),
		Normalization:     schema.DefaultProfile().Normalization,
		Pooling:           schema.DefaultProfile().Pooling,
		Precision:         schema.DefaultProfile().PrimaryPrecision(),
		PromptVersion:     schema.DefaultProfile().PromptVersion,
	}
}

func TestValidateHandshake_MatchingBackend_ReturnsNil(t *testing.T) {
	if err := validateHandshake(fullyPinned(), fullyPinned()); err != nil {
		t.Fatalf("a backend matching every pin was rejected: %v", err)
	}
}

// TestValidateHandshake_DivergingField is the story's "um teste por campo": every field of the
// "config efetiva do embedder" (PRD-contrato §2.4), each diverging on its own.
func TestValidateHandshake_DivergingField(t *testing.T) {
	cases := map[string]struct {
		mutate    func(*BackendHandshakeInfo)
		wantField string
		wantValue string // the reported value must appear, so the operator can act without a debugger
	}{
		"ModelRevision": {
			mutate:    func(i *BackendHandshakeInfo) { i.ModelRevision = "0000000000000000000000000000000000000000" },
			wantField: "ModelRevision",
			wantValue: "0000000000000000000000000000000000000000",
		},
		"TokenizerRevision": {
			mutate:    func(i *BackendHandshakeInfo) { i.TokenizerRevision = "1111111111111111111111111111111111111111" },
			wantField: "TokenizerRevision",
			wantValue: "1111111111111111111111111111111111111111",
		},
		"Dim": {
			mutate:    func(i *BackendHandshakeInfo) { i.Dim = 1024 },
			wantField: "Dim",
			wantValue: "1024",
		},
		"Normalization": {
			mutate:    func(i *BackendHandshakeInfo) { i.Normalization = "none" },
			wantField: "Normalization",
			wantValue: "none",
		},
		"Pooling": {
			mutate:    func(i *BackendHandshakeInfo) { i.Pooling = "cls" },
			wantField: "Pooling",
			wantValue: "cls",
		},
		"Precision": {
			mutate:    func(i *BackendHandshakeInfo) { i.Precision = "int8" },
			wantField: "Precision",
			wantValue: "int8",
		},
		"PromptVersion": {
			mutate:    func(i *BackendHandshakeInfo) { i.PromptVersion = "gemma-search-v2" },
			wantField: "PromptVersion",
			wantValue: "gemma-search-v2",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := fullyPinned()
			tc.mutate(&got)

			err := validateHandshake(got, fullyPinned())
			if err == nil {
				t.Fatalf("a backend diverging on %s was accepted", name)
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Errorf("error %q does not name the field %s", err, tc.wantField)
			}
			if !strings.Contains(err.Error(), tc.wantValue) {
				t.Errorf("error %q does not report the backend's value %q", err, tc.wantValue)
			}
			if !errors.Is(err, ErrHandshake) {
				t.Errorf("error %q does not match ErrHandshake", err)
			}
		})
	}
}

// TestValidateHandshake_UnpinnedExpectedField_FailsClosed encodes ADR-001 §4's policy: an absent pin
// is not a skipped check. Guessing a plausible-looking value would be worse — the wrong test would
// fail for the wrong reason. (PromptVersion is the one field where "" is a legal pin; it has its own
// test below.)
func TestValidateHandshake_UnpinnedExpectedField_FailsClosed(t *testing.T) {
	blanks := map[string]func(*BackendHandshakeInfo){
		"ModelRevision":     func(i *BackendHandshakeInfo) { i.ModelRevision = "" },
		"TokenizerRevision": func(i *BackendHandshakeInfo) { i.TokenizerRevision = "" },
		"Dim":               func(i *BackendHandshakeInfo) { i.Dim = 0 },
		"Normalization":     func(i *BackendHandshakeInfo) { i.Normalization = "" },
		"Pooling":           func(i *BackendHandshakeInfo) { i.Pooling = "" },
		"Precision":         func(i *BackendHandshakeInfo) { i.Precision = "" },
	}
	for field, blank := range blanks {
		t.Run(field, func(t *testing.T) {
			want := fullyPinned()
			blank(&want)

			err := validateHandshake(fullyPinned(), want)
			if err == nil {
				t.Fatalf("an unpinned %s was silently accepted from the backend", field)
			}
			if !strings.Contains(err.Error(), field+" is not pinned") {
				t.Errorf("error %q does not say %s itself is not pinned; it reads as a divergence", err, field)
			}
		})
	}
}

func TestValidateHandshake_BackendOmitsField_FailsNamingIt(t *testing.T) {
	omissions := map[string]func(*BackendHandshakeInfo){
		"ModelRevision":     func(i *BackendHandshakeInfo) { i.ModelRevision = "" },
		"TokenizerRevision": func(i *BackendHandshakeInfo) { i.TokenizerRevision = "" },
		"Dim":               func(i *BackendHandshakeInfo) { i.Dim = 0 },
		"Normalization":     func(i *BackendHandshakeInfo) { i.Normalization = "" },
		"Pooling":           func(i *BackendHandshakeInfo) { i.Pooling = "" },
		"Precision":         func(i *BackendHandshakeInfo) { i.Precision = "" },
		"PromptVersion":     func(i *BackendHandshakeInfo) { i.PromptVersion = "" },
	}
	for field, omit := range omissions {
		t.Run(field, func(t *testing.T) {
			got := fullyPinned()
			omit(&got)

			err := validateHandshake(got, fullyPinned())
			if err == nil {
				t.Fatalf("a backend that did not report %s was accepted", field)
			}
			if !strings.Contains(err.Error(), "did not report "+field) {
				t.Errorf("error %q does not say the backend failed to report %s", err, field)
			}
		})
	}
}

// A backend that carries a task-prefix scheme the profile does not pin is a different system: the
// "" pin means "this model takes no prefix", and hearing one is a divergence.
func TestValidateHandshake_PromptVersionOnAPrefixlessProfileIsRefused(t *testing.T) {
	want := fullyPinned()
	want.PromptVersion = ""
	err := validateHandshake(fullyPinned(), want)
	if err == nil || !strings.Contains(err.Error(), "takes no prefix") {
		t.Fatalf("validateHandshake = %v, want a refusal saying the profile takes no prefix", err)
	}
}

// TestExpected_MatchesTheModelCard pins the values the model card and the pinned repository fix, so a
// silent edit to one of them shows up as a failing test rather than as a different point_hash months
// later.
func TestExpected_MatchesTheModelCard(t *testing.T) {
	e := Expected()
	if e.ModelRevision != "f9972ac22e696a105c482202646979e33e4b5a1e" || e.TokenizerRevision != e.ModelRevision {
		t.Errorf("revisions = %q/%q, want the pinned EmbeddingGemma 2 commit for both", e.ModelRevision, e.TokenizerRevision)
	}
	if e.Dim != 768 || e.Dim != DefaultDenseDim() {
		t.Errorf("Dim = %d, want 768", e.Dim)
	}
	if e.Normalization != "l2" || e.Pooling != "mean" {
		t.Errorf("Normalization/Pooling = %q/%q, want l2/mean (model card)", e.Normalization, e.Pooling)
	}
	if e.Precision != "bf16" || e.PromptVersion != "gemma-search-v1" {
		t.Errorf("Precision/PromptVersion = %q/%q, want bf16/gemma-search-v1", e.Precision, e.PromptVersion)
	}
}

// TestExpected_IsFullyPinned: no field is left without a pin, so the handshake can actually pass
// instead of failing closed on an unknown. A future field added to BackendHandshakeInfo without a pin
// trips this immediately.
func TestExpected_IsFullyPinned(t *testing.T) {
	if err := validateHandshake(Expected(), Expected()); err != nil {
		t.Fatalf("this build's own pins do not satisfy the handshake: %v", err)
	}
}

func TestServiceEmbedder_Handshake_UsesTransportAndReportsBackendFailure(t *testing.T) {
	wantErr := errors.New("connection refused")
	e := newTestEmbedder(t, &stubTransport{
		info: func(context.Context) (BackendHandshakeInfo, error) { return BackendHandshakeInfo{}, wantErr },
	})

	_, err := e.Handshake(context.Background())
	if !errors.Is(err, ErrBackend) {
		t.Errorf("a backend that cannot be reached gave %v, want an ErrBackend-class error", err)
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("error %v does not wrap the transport's own error", err)
	}
}

// TestServiceEmbedder_Handshake_ReturnsWhatTheBackendReported matters because PRD-contrato §2.4 says
// the *confirmed* values, not the pins, are what S06a hashes into point_hash.
func TestServiceEmbedder_Handshake_ReturnsWhatTheBackendReported(t *testing.T) {
	reported := fullyPinned()
	e := newTestEmbedder(t, &stubTransport{
		info: func(context.Context) (BackendHandshakeInfo, error) { return reported, nil },
	})
	e.expected = fullyPinned()

	got, err := e.Handshake(context.Background())
	if err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	if got.ModelRevision != reported.ModelRevision || got.Precision != reported.Precision {
		t.Errorf("Handshake returned %+v, want the backend's own report %+v", got, reported)
	}
}

// PR 1 of the embedding migration: the pins come from a model profile, not from package constants.
func TestExpectedFor_DefaultProfileEqualsExpected(t *testing.T) {
	got := ExpectedFor(schema.DefaultProfile())
	want := Expected()
	if got.ModelRevision != want.ModelRevision || got.Dim != want.Dim || got.Precision != want.Precision ||
		got.PromptVersion != want.PromptVersion {
		t.Fatalf("ExpectedFor(default) = %+v, Expected() = %+v", got, want)
	}
}

func TestExpectedFor_TakesRevisionDimAndPrecisionFromTheProfile(t *testing.T) {
	other := schema.ModelProfile{
		ID: "other", Model: "org/other", Revision: strings.Repeat("b", 40),
		DenseDim: 768, Precisions: []string{"bf16"}, Pooling: "mean", Normalization: "l2", Collections: []string{"nova"},
	}
	e := ExpectedFor(other)
	if e.ModelRevision != other.Revision || e.TokenizerRevision != other.Revision {
		t.Errorf("revisions = %q/%q, want the profile's %q", e.ModelRevision, e.TokenizerRevision, other.Revision)
	}
	if e.Dim != 768 || e.Precision != "bf16" {
		t.Errorf("Dim/Precision = %d/%q, want 768/bf16", e.Dim, e.Precision)
	}
}

// A backend that serves the default model is refused by an embedder built for another profile, and
// the error names the field: the handshake validates against the profile, not against a constant.
func TestHandshake_ValidatesAgainstTheEmbeddersProfile(t *testing.T) {
	other := schema.DefaultProfile()
	other.Revision = strings.Repeat("c", 40)
	e, err := NewServiceEmbedderFor(testProfile(), &stubTransport{info: func(context.Context) (BackendHandshakeInfo, error) { return fullyPinned(), nil }}, other)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Handshake(context.Background())
	if !errors.Is(err, ErrHandshake) || !strings.Contains(err.Error(), "ModelRevision") {
		t.Fatalf("Handshake = %v, want an ErrHandshake naming ModelRevision", err)
	}
	if e.ModelID() != other.Revision {
		t.Errorf("ModelID() = %q, want the profile's revision", e.ModelID())
	}
}

func TestNewServiceEmbedderFor_RejectsAnInvalidProfile(t *testing.T) {
	bad := schema.DefaultProfile()
	bad.Revision = "main"
	if _, err := NewServiceEmbedderFor(testProfile(), &stubTransport{}, bad); err == nil {
		t.Fatal("an invalid model profile was accepted")
	}
}
