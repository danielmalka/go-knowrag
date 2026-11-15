package embed

import (
	"context"
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

// TestServiceEmbedder_Handshake_LiveService is the proof the pins describe the running process, not a
// document. Without it, Handshake is only tested against fixtures this repo wrote.
func TestServiceEmbedder_Handshake_LiveService(t *testing.T) {
	e := liveEmbedder(t)

	info, err := e.Handshake(context.Background())
	if err != nil {
		t.Fatalf("the live service diverges from this build's pins: %v", err)
	}
	if info.ModelRevision != schema.DefaultProfile().Revision {
		t.Errorf("ModelRevision = %q, want %q", info.ModelRevision, schema.DefaultProfile().Revision)
	}
}

// TestServiceEmbedder_LiveService_VectorsMeetTheContract checks real output against the invariants
// validateEmbedding enforces: the profile's width and finite elements, and a unit norm (the model's
// Normalize module). Against real output that is a statement about the service, not about fixtures
// this repository wrote. There is no golden vector here: the previous one was BGE-M3's, and an
// EmbeddingGemma 2 one has to be generated from the pinned revision on the GPU host.
func TestServiceEmbedder_LiveService_VectorsMeetTheContract(t *testing.T) {
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
		if err := validateDefault(emb); err != nil {
			t.Errorf("text %d (%q): live output violates the contract: %v", i, texts[i], err)
		}
		var sq float64
		for _, v := range emb.Dense {
			sq += float64(v) * float64(v)
		}
		if math.Abs(sq-1) > 1e-2 {
			t.Errorf("text %d: the vector's squared norm is %.4f, want 1 (L2-normalized)", i, sq)
		}
	}
}

// BenchmarkServiceEmbedder_EmbedQuery is EmbedQuery latency end to end from Go, against the live
// service, including the loopback hop.
func BenchmarkServiceEmbedder_EmbedQuery(b *testing.B) {
	e := liveEmbedder(b)
	ctx := context.Background()
	const text = "como configurar o cron do n8n para rodar toda segunda"

	for b.Loop() {
		if _, err := e.EmbedQuery(ctx, text); err != nil {
			b.Fatalf("EmbedQuery: %v", err)
		}
	}
}
