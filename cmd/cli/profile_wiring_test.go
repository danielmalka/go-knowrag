package main

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/chunk"
	"github.com/danielmalka/go-knowrag/internal/config"
	"github.com/danielmalka/go-knowrag/internal/schema"
)

// Every command serves the one registered model profile, EmbeddingGemma 2, and refuses a collection that
// belongs to another model before it opens a connection.

func TestSchemaApply_ProvisionsTheDeployedProfilesCollections(t *testing.T) {
	var out bytes.Buffer
	code, err := runSchemaApply(t.Context(), &out, cleanQdrant{}, schema.Manifest(), &memState{})
	if err != nil || code != exitOK {
		t.Fatalf("runSchemaApply = %d, %v", code, err)
	}
	for _, name := range []string{"interno_eg2", "clientes_eg2", "base_paga_eg2"} {
		if !strings.Contains(out.String(), name) {
			t.Errorf("the report does not name %s:\n%s", name, out.String())
		}
	}
	if strings.Contains(out.String(), "interno ") || strings.Contains(out.String(), "base_paga ") {
		t.Errorf("the report names a BGE-M3 collection:\n%s", out.String())
	}
}

func TestDialSearcher_RefusesACollectionOfAnotherModelBeforeDialing(t *testing.T) {
	cfg := &config.Config{
		QdrantEndpoint:    deadQdrant,
		QdrantAPIKey:      "not-a-real-key",
		EmbedderEndpoint:  "http://127.0.0.1:1",
		DefaultCollection: "interno", // BGE-M3's
	}
	_, _, err := dialSearcher(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "interno_eg2") {
		t.Fatalf("dialSearcher = %v; want a refusal naming the profile's own collections", err)
	}
}

func TestRunIngest_RefusesACollectionOfAnotherModelBeforeScanning(t *testing.T) {
	cfg := &config.Config{
		QdrantEndpoint:    deadQdrant,
		QdrantAPIKey:      "not-a-real-key",
		EmbedderEndpoint:  tokenizeStub(t),
		DefaultCollection: "interno",
		Vaults: map[string]config.VaultSettings{
			"trabalho": {Path: writeVault(t, map[string]string{
				"00-inbox/uma.md": note("0198a7f2-4b31-7c42-9e15-3d8a92c47b03", "Uma"),
			}), Areas: "00-inbox"},
		},
	}
	var out bytes.Buffer
	err := runIngest(context.Background(), &out, io.Discard, cfg, ingestOptions{
		vaultFlag: "trabalho", dryRun: true, tenantID: defaultTenantID,
		chunkCfg: chunk.Config{FloorTokens: defaultFloorTokens, CeilingTokens: defaultCeilingTokens},
	})
	if err == nil || !strings.Contains(err.Error(), "DEFAULT_COLLECTION") || out.Len() != 0 {
		t.Fatalf("runIngest = %v with output %q; want a refusal naming DEFAULT_COLLECTION and no report", err, out.String())
	}
}

func TestIngestChunkWindowFollowsTheProfile(t *testing.T) {
	// The command tree is built before configuration is read, so it cannot hold the window; runIngest
	// sets it. EmbeddingGemma 2's is 8,192 minus the prompt reserve.
	cmd := newIngestCmd(&config.Config{})
	if cmd.Flags().Lookup("ceiling-tokens") == nil {
		t.Fatal("ingest has no --ceiling-tokens flag")
	}
	if got := schema.DefaultProfile().WindowTokens(); got != 8064 {
		t.Errorf("the profile's chunk window = %d, want 8064", got)
	}
}

// goFiles lists the non-test Go files under root, skipping test fixtures and the isolation suite,
// whose fakes use the library default on purpose (they test tenant filters, not a model).
func goFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "testdata" || d.Name() == "isolation") {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
