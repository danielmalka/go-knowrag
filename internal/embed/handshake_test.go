package embed

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/schema"
)

// fullyPinned is a complete set of expectations, used as the `want` side of the tests below. It is
// spelled out rather than delegating to Expected() so a test that blanks one field is visibly
// varying one thing, and so an accidental edit to the pins shows up as a failure here too.
func fullyPinned() BackendHandshakeInfo {
	return BackendHandshakeInfo{
		ModelRevision:     schema.BGEM3Revision,
		TokenizerRevision: schema.BGEM3Revision,
		Dim:               DenseDim,
		Normalization:     ExpectedNormalization,
		Pooling:           ExpectedPooling,
		Precision:         ExpectedPrecision,
		SparseParams:      map[string]string{"kind": "lexical_weights", "id_space": "tokenizer_vocab"},
	}
}

func TestValidateHandshake_MatchingBackend_ReturnsNil(t *testing.T) {
	if err := validateHandshake(fullyPinned(), fullyPinned()); err != nil {
		t.Fatalf("a backend matching every pin was rejected: %v", err)
	}
}

// TestValidateHandshake_DivergingField is the story's "um teste por campo": all seven fields of the
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
			mutate:    func(i *BackendHandshakeInfo) { i.Dim = 768 },
			wantField: "Dim",
			wantValue: "768",
		},
		"Normalization": {
			mutate:    func(i *BackendHandshakeInfo) { i.Normalization = "none" },
			wantField: "Normalization",
			wantValue: "none",
		},
		"Pooling": {
			mutate:    func(i *BackendHandshakeInfo) { i.Pooling = "mean" },
			wantField: "Pooling",
			wantValue: "mean",
		},
		"Precision": {
			mutate:    func(i *BackendHandshakeInfo) { i.Precision = "int8" },
			wantField: "Precision",
			wantValue: "int8",
		},
		"SparseParams": {
			mutate: func(i *BackendHandshakeInfo) {
				i.SparseParams = map[string]string{"kind": "splade", "id_space": "tokenizer_vocab"}
			},
			wantField: "SparseParams",
			wantValue: "splade",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := fullyPinned()
			got.SparseParams = maps.Clone(got.SparseParams)
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
// fail for the wrong reason.
func TestValidateHandshake_UnpinnedExpectedField_FailsClosed(t *testing.T) {
	blanks := map[string]func(*BackendHandshakeInfo){
		"ModelRevision":     func(i *BackendHandshakeInfo) { i.ModelRevision = "" },
		"TokenizerRevision": func(i *BackendHandshakeInfo) { i.TokenizerRevision = "" },
		"Dim":               func(i *BackendHandshakeInfo) { i.Dim = 0 },
		"Normalization":     func(i *BackendHandshakeInfo) { i.Normalization = "" },
		"Pooling":           func(i *BackendHandshakeInfo) { i.Pooling = "" },
		"Precision":         func(i *BackendHandshakeInfo) { i.Precision = "" },
		"SparseParams":      func(i *BackendHandshakeInfo) { i.SparseParams = nil },
	}
	for field, blank := range blanks {
		t.Run(field, func(t *testing.T) {
			want := fullyPinned()
			blank(&want)

			err := validateHandshake(fullyPinned(), want)
			if err == nil {
				t.Fatalf("an unpinned %s was silently accepted from the backend", field)
			}
			if !strings.Contains(err.Error(), field) {
				t.Errorf("error %q does not name %s", err, field)
			}
			// The message must blame the field itself, not one key inside it: "the backend reports
			// key X, which is not pinned" would send the reader looking for a key-level problem
			// when the whole field has no pin.
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
		"SparseParams":      func(i *BackendHandshakeInfo) { i.SparseParams = nil },
	}
	for field, omit := range omissions {
		t.Run(field, func(t *testing.T) {
			got := fullyPinned()
			omit(&got)

			err := validateHandshake(got, fullyPinned())
			if err == nil {
				t.Fatalf("a backend that did not report %s was accepted", field)
			}
			if !strings.Contains(err.Error(), field) {
				t.Errorf("error %q does not name %s", err, field)
			}
			// Same reasoning as the unpinned case: a field that is absent entirely must be named as
			// absent, not reduced to a complaint about one key that happens to be missing with it.
			if !strings.Contains(err.Error(), "did not report "+field) {
				t.Errorf("error %q does not say the backend failed to report %s", err, field)
			}
		})
	}
}

// TestExpected_MatchesADR001 pins the values ADR-001 §4/§6.2 measured, so a silent edit to one of
// them shows up as a failing test rather than as a different point_hash months later.
func TestExpected_MatchesADR001(t *testing.T) {
	e := Expected()
	if e.ModelRevision != schema.BGEM3Revision {
		t.Errorf("ModelRevision = %q, want schema.BGEM3Revision", e.ModelRevision)
	}
	if e.TokenizerRevision != schema.BGEM3Revision {
		t.Errorf("TokenizerRevision = %q, want schema.BGEM3Revision", e.TokenizerRevision)
	}
	if e.Dim != DenseDim {
		t.Errorf("Dim = %d, want %d", e.Dim, DenseDim)
	}
	if e.Normalization != "l2" {
		t.Errorf("Normalization = %q, want %q (BGE-M3 model card, ADR-001 §4)", e.Normalization, "l2")
	}
	if e.Pooling != "cls" {
		t.Errorf("Pooling = %q, want %q (BGE-M3 model card, ADR-001 §4)", e.Pooling, "cls")
	}
	if e.Precision != "fp16" {
		t.Errorf("Precision = %q, want %q (measured, ADR-001 §6.2 round 2)", e.Precision, "fp16")
	}
}

// TestExpected_IsACopy stops one caller's mutation from poisoning the pins for the whole process —
// the map makes that possible in a way the string fields do not.
func TestExpected_IsACopy(t *testing.T) {
	e := Expected()
	e.SparseParams = map[string]string{"poisoned": "yes"}
	if _, bad := Expected().SparseParams["poisoned"]; bad {
		t.Fatal("Expected() hands out a shared map; a caller can rewrite the pins")
	}
}

// TestExpected_IsFullyPinned is what unblocked S04: with ADR-001 §7.2 closed, no field is left
// without a pin, so the handshake can actually pass instead of failing closed on the last unknown.
// A future field added to BackendHandshakeInfo without a pin trips this immediately.
func TestExpected_IsFullyPinned(t *testing.T) {
	if err := validateHandshake(Expected(), Expected()); err != nil {
		t.Fatalf("this build's own pins do not satisfy the handshake: %v", err)
	}
	if !maps.Equal(Expected().SparseParams, ExpectedSparseParams()) {
		t.Error("Expected() and ExpectedSparseParams() disagree")
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

	// On the success path every field of the report equals its pin, so field comparison alone
	// cannot tell "returned the report" from "returned the pins". Identity can: handing back the
	// embedder's own SparseParams map would let the caller rewrite the pins for the process.
	got.SparseParams["poisoned"] = "yes"
	if _, bad := e.expected.SparseParams["poisoned"]; bad {
		t.Error("Handshake returned the embedder's own pins, not the backend's report")
	}
}
