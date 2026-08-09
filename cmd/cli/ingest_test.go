package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/chunk"
	"github.com/danielmalka/go-knowrag/internal/config"
	"github.com/danielmalka/go-knowrag/internal/schema"
)

func TestSelectVaults(t *testing.T) {
	t.Run("both selects every registered vault", func(t *testing.T) {
		got, err := selectVaults(bothVaults)
		if err != nil {
			t.Fatalf("selectVaults(%q): %v", bothVaults, err)
		}
		if len(got) != len(schema.AllVaults()) {
			t.Errorf("selectVaults(%q) = %v, want all %d vaults", bothVaults, got, len(schema.AllVaults()))
		}
	})

	for _, v := range schema.AllVaults() {
		t.Run(v.String(), func(t *testing.T) {
			got, err := selectVaults(v.String())
			if err != nil {
				t.Fatalf("selectVaults(%q): %v", v, err)
			}
			if len(got) != 1 || got[0] != v {
				t.Errorf("selectVaults(%q) = %v, want exactly %v", v, got, v)
			}
		})
	}

	t.Run("an unknown name is refused by name", func(t *testing.T) {
		got, err := selectVaults("malkawhat")
		if err == nil {
			t.Fatalf("selectVaults on an unregistered vault = %v, want an error", got)
		}
		if !strings.Contains(err.Error(), "malkawhat") {
			t.Errorf("error %q does not name the offending value", err)
		}
	})
}

// TestIngestCmd_RegistersItsFlags proves the flags were actually registered rather than only
// documented: cobra generates the help text from the registered set, so a flag missing from it is
// a flag the operator cannot pass.
func TestIngestCmd_RegistersItsFlags(t *testing.T) {
	cmd := newIngestCmd(&config.Config{})

	for _, name := range []string{"vault", "dry-run", "tenant", "floor-tokens", "ceiling-tokens"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("ingest does not register --%s", name)
		}
	}
	// The --vault help must name the real vaults, which is what keeps it correct the day a third
	// one is registered in internal/schema.
	usage := cmd.Flags().Lookup("vault").Usage
	for _, v := range schema.AllVaults() {
		if !strings.Contains(usage, v.String()) {
			t.Errorf("--vault usage %q does not mention the registered vault %s", usage, v)
		}
	}
}

// tokenizeStub answers /tokenize the way the real service does, counting whitespace-separated
// fields. The count rule does not have to match BGE-M3 here — what is under test is the command's
// wiring, and internal/chunk owns what the counter does with the answer.
func tokenizeStub(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Inputs []string `json:"inputs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding /tokenize request: %v", err)
		}
		counts := make([]string, len(req.Inputs))
		for i, s := range req.Inputs {
			counts[i] = fmt.Sprint(len(strings.Fields(s)))
		}
		_, _ = io.WriteString(w, `{"counts":[`+strings.Join(counts, ",")+`]}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// writeVault builds a minimal MalkaWay vault: notes under an area folder, plus one root file that
// only the exclusion list makes acceptable.
func writeVault(t *testing.T, notes map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range notes {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		// #nosec G703 -- rel is a test-local literal under a t.TempDir() root
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("writing %s: %v", rel, err)
		}
	}
	return root
}

func note(uid, title string) string {
	return fmt.Sprintf(`---
uid: %s
type: log
status: draft
created: 2026-08-07
tags: [test]
title: %s
---

# %s

Body of %s, long enough to be a chunk on its own.
`, uid, title, title, title)
}

// TestRunIngest_DryRun_ReportsCountsAndNeedsNoQdrant is the dry run's whole contract in one test:
// it reports what a real run would embed, it writes nothing, and — the part that is easy to lose in
// a refactor — it does not demand Qdrant settings it never uses.
func TestRunIngest_DryRun_ReportsCountsAndNeedsNoQdrant(t *testing.T) {
	root := writeVault(t, map[string]string{
		"00-inbox/uma.md":   note("0198a7f2-4b31-7c42-9e15-3d8a92c47b01", "Uma"),
		"00-inbox/outra.md": note("0198a7f2-4b31-7c42-9e15-3d8a92c47b02", "Outra"),
		"CLAUDE.md":         "# agent instructions, excluded by configuration\n",
	})
	cfg := &config.Config{
		EmbedderEndpoint: tokenizeStub(t),
		VaultMalkaWay: config.VaultSettings{
			Path:             root,
			ExcludeRootFiles: "CLAUDE.md",
		},
	}

	var out bytes.Buffer
	err := runIngest(context.Background(), &out, cfg, ingestOptions{
		vaultFlag: schema.VaultMalkaWay().String(),
		dryRun:    true,
		tenantID:  defaultTenantID,
		chunkCfg:  chunk.Config{FloorTokens: defaultFloorTokens, CeilingTokens: defaultCeilingTokens},
	})
	if err != nil {
		t.Fatalf("runIngest --dry-run with no Qdrant settings: %v", err)
	}

	got := out.String()
	for _, want := range []string{"2 note(s)", "2 chunk(s) to embed", "nothing was embedded or written"} {
		if !strings.Contains(got, want) {
			t.Errorf("dry-run output %q does not contain %q", got, want)
		}
	}
}

// TestRunIngest_MissingSettings_NamesOnlyWhatTheRunNeeds pins the per-command requirement in the
// direction that matters: a real run asks for Qdrant, a dry run does not, and neither asks for the
// vault it was not pointed at.
func TestRunIngest_MissingSettings_NamesOnlyWhatTheRunNeeds(t *testing.T) {
	tests := map[string]struct {
		dryRun     bool
		wantNamed  []string
		wantAbsent []string
		vaultFlag  string
	}{
		"a real run needs Qdrant": {
			dryRun:     false,
			vaultFlag:  schema.VaultMalkaWay().String(),
			wantNamed:  []string{"QDRANT_ENDPOINT", "QDRANT_API_KEY", "DEFAULT_COLLECTION"},
			wantAbsent: []string{"KNOWRAG_VAULT_MALKALIFE_PATH"},
		},
		"a dry run does not": {
			dryRun:     true,
			vaultFlag:  schema.VaultMalkaWay().String(),
			wantNamed:  []string{"EMBEDDER_ENDPOINT", "KNOWRAG_VAULT_MALKAWAY_PATH"},
			wantAbsent: []string{"QDRANT_ENDPOINT", "KNOWRAG_VAULT_MALKALIFE_PATH"},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			err := runIngest(context.Background(), io.Discard, &config.Config{}, ingestOptions{
				vaultFlag: tc.vaultFlag,
				dryRun:    tc.dryRun,
			})
			if err == nil {
				t.Fatal("runIngest with an empty config returned no error")
			}
			for _, want := range tc.wantNamed {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %s", err, want)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(err.Error(), absent) {
					t.Errorf("error %q names %s, which this run does not read", err, absent)
				}
			}
		})
	}
}

// TestRunIngest_UnreadableVault_FailsBeforeTouchingAnything keeps a misconfigured path from
// reporting an empty but successful run.
func TestRunIngest_UnreadableVault_FailsBeforeTouchingAnything(t *testing.T) {
	cfg := &config.Config{
		EmbedderEndpoint: tokenizeStub(t),
		VaultMalkaWay:    config.VaultSettings{Path: filepath.Join(t.TempDir(), "absent")},
	}

	err := runIngest(context.Background(), io.Discard, cfg, ingestOptions{
		vaultFlag: schema.VaultMalkaWay().String(),
		dryRun:    true,
		chunkCfg:  chunk.Config{FloorTokens: defaultFloorTokens, CeilingTokens: defaultCeilingTokens},
	})
	if err == nil {
		t.Fatal("runIngest against a vault path that does not exist returned no error")
	}
	if !strings.Contains(err.Error(), "scanning vault") {
		t.Errorf("error %q does not say which stage failed", err)
	}
}
