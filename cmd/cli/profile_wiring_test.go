package main

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/chunk"
	"github.com/danielmalka/go-knowrag/internal/config"
	"github.com/danielmalka/go-knowrag/internal/schema"
)

// The cutover (PR 6): every command serves the model profile its configuration names, EmbeddingGemma 2
// by default, and none of them reads the library's fallback profile (BGE-M3 until it is deleted).

func TestSchemaApply_ProvisionsTheDeployedProfilesCollections(t *testing.T) {
	profile, err := schema.ResolveProfile("")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code, err := runSchemaApply(t.Context(), &out, cleanQdrant{}, schema.ManifestFor(profile), &memState{})
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
	profile, _ := schema.ResolveProfile("")
	if got := profile.WindowTokens(); got != 8064 {
		t.Errorf("deployed profile window = %d, want 8064", got)
	}
}

// TestNoEntryPointReadsTheLibraryDefaultProfile is what keeps "the cutover is complete" true after the
// next edit. schema.DefaultProfile (BGE-M3) stays as the library fallback until the cleanup PR deletes
// that profile, so a command that called it, or built an embedder or a manifest without a profile,
// would still compile and would quietly serve the old model. The entry points are scanned, not the
// library: the fallback is legitimate there and in tests.
func TestNoEntryPointReadsTheLibraryDefaultProfile(t *testing.T) {
	forbidden := map[string][]string{
		"schema": {"DefaultProfile", "Manifest"},
		"embed":  {"Expected", "DefaultDenseDim", "NewServiceEmbedder"},
	}
	var hits []string
	for _, root := range []string{"..", "../../internal/clicmd", "../../internal/eval"} {
		hits = append(hits, scanForbidden(t, root, forbidden)...)
	}
	if len(hits) > 0 {
		t.Errorf("entry points must resolve their profile (schema.ResolveProfile / cfg.Profile) and pass it "+
			"explicitly; these read the library fallback:\n  %s", strings.Join(hits, "\n  "))
	}
}

// TestEveryEntryPointSearchConfigNamesItsProfile covers the one fallback the selector scan cannot: a
// retrieval.Config left at its zero value or DefaultConfig() silently queries the default profile.
func TestEveryEntryPointSearchConfigNamesItsProfile(t *testing.T) {
	for _, dir := range []string{"..", "../../internal/clicmd"} {
		for _, file := range goFiles(t, dir) {
			src := readFile(t, file)
			if strings.Contains(src, "retrieval.DefaultConfig()") && !strings.Contains(src, ".Profile = ") {
				t.Errorf("%s builds a searcher from retrieval.DefaultConfig() without setting .Profile", file)
			}
		}
	}
}

func scanForbidden(t *testing.T, root string, forbidden map[string][]string) []string {
	t.Helper()
	var hits []string
	for _, file := range goFiles(t, root) {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && slices.Contains(forbidden[pkg.Name], sel.Sel.Name) {
				hits = append(hits, file+": "+pkg.Name+"."+sel.Sel.Name)
			}
			return true
		})
	}
	return hits
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

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) // #nosec G304 -- a path this test just walked under the repository
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
