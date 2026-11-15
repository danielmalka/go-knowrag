package embed

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/schema"
)

// PR 2 of the embedding migration: the embed contract accepts a dense-only profile (EmbeddingGemma 2),
// refuses fp16 for it, and confirms the task-prefix scheme in the handshake. The default profile
// (BGE-M3) is unchanged and keeps its own tests.

func eg2(t *testing.T) schema.ModelProfile {
	t.Helper()
	p, ok := schema.ProfileByID("embeddinggemma-2")
	if !ok {
		t.Fatal("embeddinggemma-2 profile is not registered")
	}
	return p
}

// reportFor is what a healthy backend for the profile reports, with the dtype of the host.
func reportFor(m schema.ModelProfile, precision string) BackendHandshakeInfo {
	r := ExpectedFor(m)
	r.Precision = precision
	return r
}

func TestAgainstProfile_EG2_AcceptsBF16AndFP32(t *testing.T) {
	m := eg2(t)
	for _, precision := range []string{"bf16", "fp32"} {
		if err := validateAgainstProfile(reportFor(m, precision), m); err != nil {
			t.Errorf("%s was refused for EmbeddingGemma: %v", precision, err)
		}
	}
}

// R3 of the plan: fp16 gives NaN in silence. The handshake is the second line (the service is the
// first), and the refusal must say why.
func TestAgainstProfile_EG2_RefusesFP16(t *testing.T) {
	m := eg2(t)
	err := validateAgainstProfile(reportFor(m, "fp16"), m)
	if !errors.Is(err, ErrHandshake) {
		t.Fatalf("fp16 was not refused with ErrHandshake: %v", err)
	}
	for _, want := range []string{"Precision", "fp16", "NaN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestAgainstProfile_Precision_UnknownAndMissing(t *testing.T) {
	m := eg2(t)
	if err := validateAgainstProfile(reportFor(m, "int8"), m); !errors.Is(err, ErrHandshake) {
		t.Errorf("int8 accepted: %v", err)
	}
	err := validateAgainstProfile(reportFor(m, ""), m)
	if !errors.Is(err, ErrHandshake) || !strings.Contains(err.Error(), "did not report Precision") {
		t.Errorf("a missing Precision must be named as unreported: %v", err)
	}
}

// The BGE-M3 profile keeps its single dtype: bf16 is refused there, as the old pin did.
func TestAgainstProfile_Default_StillPinsFP16(t *testing.T) {
	m := schema.DefaultProfile()
	if err := validateAgainstProfile(reportFor(m, "fp16"), m); err != nil {
		t.Fatalf("the default profile refused its own fp16: %v", err)
	}
	if err := validateAgainstProfile(reportFor(m, "bf16"), m); !errors.Is(err, ErrHandshake) {
		t.Fatalf("the default profile accepted bf16: %v", err)
	}
}

func TestAgainstProfile_EG2_DenseOnly(t *testing.T) {
	m := eg2(t)
	ok := reportFor(m, "bf16")
	if len(ok.SparseParams) != 0 {
		t.Fatalf("a dense-only profile expects no SparseParams, got %v", ok.SparseParams)
	}
	if err := validateAgainstProfile(ok, m); err != nil {
		t.Fatalf("a dense-only backend was refused: %v", err)
	}
	withSparse := ok
	withSparse.SparseParams = map[string]string{"kind": "lexical_weights", "id_space": "tokenizer_vocab"}
	err := validateAgainstProfile(withSparse, m)
	if !errors.Is(err, ErrHandshake) || !strings.Contains(err.Error(), "SparseParams") {
		t.Fatalf("a backend that carries a sparse head was accepted for a dense-only profile: %v", err)
	}
	// and the other direction is unchanged: BGE-M3 still must report its sparse head
	bge := schema.DefaultProfile()
	noSparse := reportFor(bge, "fp16")
	noSparse.SparseParams = nil
	if err := validateAgainstProfile(noSparse, bge); !errors.Is(err, ErrHandshake) {
		t.Fatalf("a BGE-M3 backend without its sparse head was accepted: %v", err)
	}
}

func TestAgainstProfile_PromptVersion(t *testing.T) {
	m := eg2(t)
	if m.PromptVersion == "" {
		t.Fatal("EG2 must pin a prompt version")
	}
	missing := reportFor(m, "bf16")
	missing.PromptVersion = ""
	err := validateAgainstProfile(missing, m)
	if !errors.Is(err, ErrHandshake) || !strings.Contains(err.Error(), "did not report PromptVersion") {
		t.Errorf("an unreported prefix scheme was accepted: %v", err)
	}
	other := reportFor(m, "bf16")
	other.PromptVersion = "another-scheme"
	if err := validateAgainstProfile(other, m); !errors.Is(err, ErrHandshake) || !strings.Contains(err.Error(), "another-scheme") {
		t.Errorf("a different prefix scheme was accepted: %v", err)
	}
	// a model with no prefix expects "" back, and a backend that applies one diverges
	bge := schema.DefaultProfile()
	if err := validateAgainstProfile(reportFor(bge, "fp16"), bge); err != nil {
		t.Errorf("BGE-M3 (no prefix) refused: %v", err)
	}
	withPrefix := reportFor(bge, "fp16")
	withPrefix.PromptVersion = m.PromptVersion
	if err := validateAgainstProfile(withPrefix, bge); !errors.Is(err, ErrHandshake) {
		t.Errorf("a backend applying a prefix to a no-prefix model was accepted: %v", err)
	}
}

func TestAgainstProfile_EG2_OtherPinsStillCompared(t *testing.T) {
	m := eg2(t)
	for name, mutate := range map[string]func(*BackendHandshakeInfo){
		"ModelRevision": func(i *BackendHandshakeInfo) { i.ModelRevision = strings.Repeat("0", 40) },
		"Dim":           func(i *BackendHandshakeInfo) { i.Dim = 1024 },
		"Pooling":       func(i *BackendHandshakeInfo) { i.Pooling = "cls" },
		"Normalization": func(i *BackendHandshakeInfo) { i.Normalization = "none" },
	} {
		r := reportFor(m, "bf16")
		mutate(&r)
		if err := validateAgainstProfile(r, m); !errors.Is(err, ErrHandshake) || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestValidateEmbedding_DenseOnlyProfile(t *testing.T) {
	m := eg2(t)
	dense := make([]float32, 768)
	dense[0] = 1
	if err := validateEmbedding(Embedding{Dense: dense}, m); err != nil {
		t.Fatalf("a 768-wide dense-only vector was refused: %v", err)
	}
	withSparse := Embedding{Dense: dense, Sparse: Sparse{Indices: []uint32{1}, Values: []float32{0.5}}}
	if err := validateEmbedding(withSparse, m); err == nil || !strings.Contains(err.Error(), "dense-only") {
		t.Errorf("a sparse half on a dense-only profile was accepted: %v", err)
	}
	if err := validateEmbedding(Embedding{Dense: dense[:512]}, m); err == nil {
		t.Error("a 512-wide vector was accepted for the 768-wide profile")
	}
	nan := append([]float32(nil), dense...)
	nan[3] = float32(math.NaN())
	if err := validateEmbedding(Embedding{Dense: nan}, m); err == nil {
		t.Error("a NaN dense element was accepted (the fp16 failure mode)")
	}
	// the BGE-M3 profile still requires its sparse half
	bge := schema.DefaultProfile()
	if err := validateEmbedding(Embedding{Dense: make([]float32, 1024)}, bge); err == nil {
		t.Error("a BGE-M3 vector without a sparse half was accepted")
	}
}

func TestFakeEmbedder_ForADenseOnlyProfile(t *testing.T) {
	m := eg2(t)
	f, err := NewFakeEmbedderFor(m)
	if err != nil {
		t.Fatal(err)
	}
	out, err := f.EmbedDocuments(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	q, err := f.EmbedQuery(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range append(out, q) {
		if len(e.Dense) != 768 || len(e.Sparse.Indices) != 0 {
			t.Errorf("embedding %d: %d dense, %d sparse; want 768 and none", i, len(e.Dense), len(e.Sparse.Indices))
		}
		if err := validateEmbedding(e, m); err != nil {
			t.Errorf("embedding %d fails validation for its own profile: %v", i, err)
		}
		var norm float64
		for _, v := range e.Dense {
			norm += float64(v) * float64(v)
		}
		if math.Abs(norm-1) > 1e-3 {
			t.Errorf("embedding %d is not L2-normalized (|v|^2 = %v)", i, norm)
		}
	}
	// deterministic, and the zero value still fakes the default profile
	again, _ := f.EmbedQuery(context.Background(), "a")
	if again.Dense[0] != q.Dense[0] {
		t.Error("the fake is not deterministic")
	}
	zero, _ := FakeEmbedder{}.EmbedQuery(context.Background(), "a")
	if len(zero.Dense) != 1024 || len(zero.Sparse.Indices) == 0 {
		t.Errorf("the zero-value fake changed: %d dense, %d sparse", len(zero.Dense), len(zero.Sparse.Indices))
	}
	bad := m
	bad.Revision = "main"
	if _, err := NewFakeEmbedderFor(bad); err == nil {
		t.Error("an invalid profile was accepted by NewFakeEmbedderFor")
	}
}

// End to end through the service embedder: a dense-only backend is accepted, fp16 stops embedding.
func TestServiceEmbedder_EG2_EndToEnd(t *testing.T) {
	m := eg2(t)
	dense := make([]float32, 768)
	dense[0] = 1
	tr := &stubTransport{
		embed: func(_ context.Context, texts []string) ([]Embedding, error) {
			out := make([]Embedding, len(texts))
			for i := range out {
				out[i] = Embedding{Dense: dense}
			}
			return out, nil
		},
		info: func(context.Context) (BackendHandshakeInfo, error) { return reportFor(m, "bf16"), nil },
	}
	e, err := NewServiceEmbedderFor(testProfile(), tr, m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Handshake(context.Background()); err != nil {
		t.Fatalf("the dense-only handshake failed: %v", err)
	}
	if got, err := e.EmbedDocuments(context.Background(), []string{"x", "y"}); err != nil || len(got) != 2 {
		t.Fatalf("EmbedDocuments = %v, %v", got, err)
	}

	fp16 := &stubTransport{info: func(context.Context) (BackendHandshakeInfo, error) { return reportFor(m, "fp16"), nil }}
	e2, _ := NewServiceEmbedderFor(testProfile(), fp16, m)
	if _, err := e2.Handshake(context.Background()); !errors.Is(err, ErrHandshake) {
		t.Fatalf("fp16 handshake: %v", err)
	}
	if _, err := e2.EmbedQuery(context.Background(), "x"); err == nil {
		t.Fatal("embedding went through a backend that reported fp16")
	}
}
