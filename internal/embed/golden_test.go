package embed

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"

	"github.com/danielmalka/go-knowrag/internal/schema"
)

// endpointEnv names the live service for the tests in this file. Unset means skip, never fail: CI
// has no GPU and no sidecar, and a suite that goes red on a machine that was never expected to run
// it teaches people to ignore red.
const endpointEnv = "KNOWRAG_EMBEDDER_ENDPOINT"

// The tolerance, declared before the measurement rather than fitted to it (S04 T8/T10).
//
// The numbers come from what the runtime actually varies by, measured on 2026-08-09 against the
// live service: embedding the same text alone and inside batches of 2 and 32 gives cosine
// 0.99999901, max per-element dense delta 1.83e-4, identical sparse indices, max sparse weight
// delta 2.44e-4. That variance is fp16 rounding over batch padding, not semantics.
//
// So the thresholds sit two orders of magnitude above the observed noise and far below anything
// retrieval ranking can feel — RRF fusion over top-k results does not reorder on a 1e-3 perturbation
// of a unit vector. Sparse *indices* get no tolerance at all: they are token ids, so a different
// index is a different word, never rounding.
const (
	goldenMinCosine    = 0.9999
	goldenMaxDenseAbs  = 1e-3
	goldenMaxSparseAbs = 1e-3
)

type goldenFixture struct {
	InputText             string    `json:"input_text"`
	Kind                  Kind      `json:"kind"`
	ModelRevision         string    `json:"model_revision"`
	ExpectedDense         []float32 `json:"expected_dense"`
	ExpectedSparseIndices []uint32  `json:"expected_sparse_indices"`
	ExpectedSparseValues  []float32 `json:"expected_sparse_values"`
	ExpectedTokens        int       `json:"expected_tokens"`
}

func loadGolden(t testing.TB) goldenFixture {
	t.Helper()
	data, err := os.ReadFile("testdata/golden/bge_m3_query_fixture.json")
	if err != nil {
		t.Fatalf("reading golden fixture: %v", err)
	}
	var f goldenFixture
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("decoding golden fixture: %v", err)
	}
	return f
}

// liveEmbedder builds a client against the running service, or skips.
func liveEmbedder(t testing.TB) *ServiceEmbedder {
	t.Helper()
	endpoint := os.Getenv(endpointEnv)
	if endpoint == "" {
		t.Skipf("%s is unset; skipping the live-service test", endpointEnv)
	}
	tr, err := NewHTTPTransport(endpoint)
	if err != nil {
		t.Fatalf("NewHTTPTransport: %v", err)
	}
	e, err := NewServiceEmbedder(Profile{
		Endpoint: endpoint, Timeout: 30 * time.Second, VerifyTimeout: 30 * time.Second, BatchSize: 32,
		// The service serializes inference behind a lock (one CUDA model is not concurrency-safe),
		// so more than one request in flight buys queueing, not throughput.
		MaxConcurrent: 1, MaxRetries: 3,
	}, tr)
	if err != nil {
		t.Fatalf("NewServiceEmbedder: %v", err)
	}
	return e
}

// TestGoldenFixture_IsPinnedToTheCurrentRevision needs no service: it catches a fixture left behind
// by a revision bump, which would otherwise look like a passing test against stale vectors.
func TestGoldenFixture_IsPinnedToTheCurrentRevision(t *testing.T) {
	f := loadGolden(t)
	if f.ModelRevision != schema.BGEM3Revision {
		t.Errorf("fixture was generated at revision %s, but this build pins %s — regenerate it, and "+
			"note that doing so means reindexing all three collections (PRD-contrato §2.4)",
			f.ModelRevision, schema.BGEM3Revision)
	}
	if len(f.ExpectedDense) != DenseDim {
		t.Errorf("fixture dense vector has %d elements, want %d", len(f.ExpectedDense), DenseDim)
	}
	if err := validateEmbedding(Embedding{
		Dense:  f.ExpectedDense,
		Sparse: Sparse{Indices: f.ExpectedSparseIndices, Values: f.ExpectedSparseValues},
	}); err != nil {
		t.Errorf("the golden fixture itself violates an invariant: %v", err)
	}
}

// TestServiceEmbedder_EmbedQuery_GoldenFixture is the only test that can falsify the whole package:
// everything else proves this code is self-consistent, and this proves it agrees with what BGE-M3
// actually emits. Changing the model revision breaks it on purpose.
func TestServiceEmbedder_EmbedQuery_GoldenFixture(t *testing.T) {
	f := loadGolden(t)
	e := liveEmbedder(t)

	got, err := e.EmbedQuery(context.Background(), f.InputText)
	if err != nil {
		t.Fatalf("EmbedQuery against the live service: %v", err)
	}

	if c := cosine(got.Dense, f.ExpectedDense); c < goldenMinCosine {
		t.Errorf("dense cosine to the fixture = %.8f, want >= %.4f", c, goldenMinCosine)
	}
	for i := range f.ExpectedDense {
		if d := math.Abs(float64(got.Dense[i] - f.ExpectedDense[i])); d > goldenMaxDenseAbs {
			t.Fatalf("dense[%d] differs by %.2e, tolerance %.0e", i, d, goldenMaxDenseAbs)
		}
	}

	if len(got.Sparse.Indices) != len(f.ExpectedSparseIndices) {
		t.Fatalf("sparse has %d entries, fixture has %d", len(got.Sparse.Indices), len(f.ExpectedSparseIndices))
	}
	for i, idx := range f.ExpectedSparseIndices {
		if got.Sparse.Indices[i] != idx {
			t.Fatalf("sparse index %d = %d, fixture has %d — a different token id is not rounding",
				i, got.Sparse.Indices[i], idx)
		}
		if d := math.Abs(float64(got.Sparse.Values[i] - f.ExpectedSparseValues[i])); d > goldenMaxSparseAbs {
			t.Errorf("sparse weight for token %d differs by %.2e, tolerance %.0e", idx, d, goldenMaxSparseAbs)
		}
	}
}

// TestServiceEmbedder_Handshake_LiveService is the proof the seven pins describe the running
// process, not a document. Without it, Handshake is only tested against fixtures this repo wrote.
func TestServiceEmbedder_Handshake_LiveService(t *testing.T) {
	e := liveEmbedder(t)

	info, err := e.Handshake(context.Background())
	if err != nil {
		t.Fatalf("the live service diverges from this build's pins: %v", err)
	}
	if info.ModelRevision != schema.BGEM3Revision {
		t.Errorf("ModelRevision = %q, want %q", info.ModelRevision, schema.BGEM3Revision)
	}
}

// TestServiceEmbedder_LiveService_SparseContractHolds verifies from the client side the three
// properties the service promises about its sparse output. They are a contract between the two
// sides — nothing in Go re-imposes them — so this is where a server-side regression surfaces.
func TestServiceEmbedder_LiveService_SparseContractHolds(t *testing.T) {
	e := liveEmbedder(t)

	texts := []string{
		"como configurar o cron do n8n para rodar toda segunda",
		"a",
		"documento com repetição repetição repetição de termos repetidos",
	}
	got, err := e.EmbedDocuments(context.Background(), texts)
	if err != nil {
		t.Fatalf("EmbedDocuments against the live service: %v", err)
	}
	for i, emb := range got {
		// validateEmbedding already enforces ascending, non-zero and non-empty; asserting it here
		// against real output is what makes those checks a statement about the service rather than
		// about hand-written fixtures.
		if err := validateEmbedding(emb); err != nil {
			t.Errorf("text %d (%q): live output violates the contract: %v", i, texts[i], err)
		}
	}
}

// BenchmarkServiceEmbedder_EmbedQuery is S04 T11's number: EmbedQuery latency end to end from Go,
// against the live service, including the loopback hop the ADR's own measurement excluded.
func BenchmarkServiceEmbedder_EmbedQuery(b *testing.B) {
	e := liveEmbedder(b)
	ctx := context.Background()
	text := loadGolden(b).InputText

	for b.Loop() {
		if _, err := e.EmbedQuery(ctx, text); err != nil {
			b.Fatalf("EmbedQuery: %v", err)
		}
	}
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
