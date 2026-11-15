package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/embed"
	"github.com/danielmalka/go-knowrag/internal/schema"
)

// The deployed profile is EmbeddingGemma 2 (the cutover). These tests pin that this server serves it
// with no extra configuration, and that it refuses a collection of any other model, since a collection
// holds one model's vectors and the query would be embedded in a different space.

func eg2Config() Config {
	cfg := testConfig()
	cfg.Collection = "interno_eg2"
	cfg.ModelProfile = "" // the default is the point
	return cfg
}

func TestConfig_DefaultsToEmbeddingGemma2(t *testing.T) {
	p, err := eg2Config().Profile()
	if err != nil || p.ID != "embeddinggemma-2" {
		t.Fatalf("Profile() = %q, %v; want embeddinggemma-2 with no MCP_MODEL_PROFILE set", p.ID, err)
	}
}

func TestLoadConfig_RefusesACollectionOfAnotherModel(t *testing.T) {
	setInstanceEnv(t)
	t.Setenv(envModelProfile, "") // deployed profile: EmbeddingGemma 2, whose collections are *_eg2
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("LoadConfig accepted MCP_COLLECTION=interno (BGE-M3's) for EmbeddingGemma 2")
	}
	for _, want := range []string{envCollection, `"interno"`, "embeddinggemma-2", "interno_eg2", envModelProfile} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestLoadConfig_AcceptsTheProfilesOwnCollection(t *testing.T) {
	setInstanceEnv(t)
	t.Setenv(envModelProfile, "")
	t.Setenv(envCollection, "interno_eg2")
	if _, err := LoadConfig(); err != nil {
		t.Fatalf("LoadConfig() = %v for MCP_COLLECTION=interno_eg2", err)
	}
}

func TestVerifyEmbedder_AcceptsAnEmbeddingGemmaBackendByDefault(t *testing.T) {
	eg2, _ := schema.ProfileByID("embeddinggemma-2")
	tr := &stubTransport{info: embed.ExpectedFor(eg2)}
	if err := verifyEmbedder(context.Background(), eg2Config(), tr); err != nil {
		t.Fatalf("a backend matching every EmbeddingGemma 2 pin was refused: %v", err)
	}
}

func TestVerifyEmbedder_RefusesTheOldBackendOnceTheDeployedProfileIsEmbeddingGemma(t *testing.T) {
	// A service still running BGE-M3 (1024 wide, cls pooling, no prompt scheme) behind an instance that
	// was cut over must stop the server, not be queried in the wrong vector space.
	tr := &stubTransport{info: embed.Expected()}
	err := verifyEmbedder(context.Background(), eg2Config(), tr)
	if !errors.Is(err, embed.ErrHandshake) {
		t.Fatalf("verifyEmbedder = %v, want a handshake refusal", err)
	}
}

func TestVerifyEmbedder_RefusesFP16ForEmbeddingGemma(t *testing.T) {
	eg2, _ := schema.ProfileByID("embeddinggemma-2")
	info := embed.ExpectedFor(eg2)
	info.Precision = "fp16"
	err := verifyEmbedder(context.Background(), eg2Config(), &stubTransport{info: info})
	if !errors.Is(err, embed.ErrHandshake) || !strings.Contains(err.Error(), "fp16") {
		t.Fatalf("verifyEmbedder = %v, want a handshake refusal naming fp16", err)
	}
}

func TestNewSearchEmbedder_IsBuiltForTheConfiguredProfile(t *testing.T) {
	eg2, _ := schema.ProfileByID("embeddinggemma-2")
	tr := &stubTransport{info: embed.Expected()} // BGE-M3's report
	embedder, err := newSearchEmbedder(eg2Config(), tr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := embedder.Handshake(context.Background()); !errors.Is(err, embed.ErrHandshake) {
		t.Fatalf("the search embedder accepted BGE-M3's report under %s: %v", eg2.ID, err)
	}
}
