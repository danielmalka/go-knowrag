package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/embed"
)

// The server serves EmbeddingGemma 2, the only registered profile. These tests pin that it refuses a
// collection of any other model (a collection holds one model's vectors, so the query would be embedded
// in a different space) and a backend still running the retired BGE-M3 service.

func TestConfig_ProfileIsEmbeddingGemma2(t *testing.T) {
	p, err := testConfig().Profile()
	if err != nil || p.ID != "embeddinggemma-2" {
		t.Fatalf("Profile() = %q, %v; want embeddinggemma-2", p.ID, err)
	}
}

func TestLoadConfig_RefusesTheRetiredBGEM3Collections(t *testing.T) {
	for _, old := range []string{"interno", "clientes", "base_paga"} {
		setInstanceEnv(t)
		t.Setenv(envCollection, old)
		_, err := LoadConfig()
		if err == nil {
			t.Fatalf("LoadConfig accepted MCP_COLLECTION=%s, a retired BGE-M3 collection, for EmbeddingGemma 2", old)
		}
		for _, want := range []string{envCollection, `"` + old + `"`, "embeddinggemma-2", "interno_eg2"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		}
	}
}

func TestLoadConfig_AcceptsTheProfilesOwnCollections(t *testing.T) {
	for _, own := range []string{"interno_eg2", "clientes_eg2", "base_paga_eg2"} {
		setInstanceEnv(t)
		t.Setenv(envCollection, own)
		if _, err := LoadConfig(); err != nil {
			t.Fatalf("LoadConfig() = %v for MCP_COLLECTION=%s", err, own)
		}
	}
}

func TestVerifyEmbedder_AcceptsABackendMatchingEveryPin(t *testing.T) {
	tr := &stubTransport{info: embed.Expected()}
	if err := verifyEmbedder(context.Background(), testConfig(), tr); err != nil {
		t.Fatalf("a backend matching every EmbeddingGemma 2 pin was refused: %v", err)
	}
}

func TestVerifyEmbedder_RefusesTheOldBGEM3Backend(t *testing.T) {
	// A service still running BGE-M3 (1024 wide, cls pooling, fp16, no prompt scheme) behind an
	// instance that was cut over must stop the server, not be queried in the wrong vector space.
	old := embed.BackendHandshakeInfo{
		ModelRevision:     strings.Repeat("5", 40), // another model's commit
		TokenizerRevision: strings.Repeat("5", 40),
		Dim:               1024, Normalization: "l2", Pooling: "cls", Precision: "fp16",
	}
	err := verifyEmbedder(context.Background(), testConfig(), &stubTransport{info: old})
	if !errors.Is(err, embed.ErrHandshake) {
		t.Fatalf("verifyEmbedder = %v, want a handshake refusal", err)
	}
}

func TestVerifyEmbedder_RefusesFP16(t *testing.T) {
	info := embed.Expected()
	info.Precision = "fp16"
	err := verifyEmbedder(context.Background(), testConfig(), &stubTransport{info: info})
	if !errors.Is(err, embed.ErrHandshake) || !strings.Contains(err.Error(), "fp16") {
		t.Fatalf("verifyEmbedder = %v, want a handshake refusal naming fp16", err)
	}
}

func TestNewSearchEmbedder_ChecksTheBackendAgainstTheProfile(t *testing.T) {
	bad := embed.Expected()
	bad.Dim = 1024
	embedder, err := newSearchEmbedder(testConfig(), &stubTransport{info: bad})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := embedder.Handshake(context.Background()); !errors.Is(err, embed.ErrHandshake) {
		t.Fatalf("the search embedder accepted a 1024-wide backend: %v", err)
	}
}
