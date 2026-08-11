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
)

// rosterCfg builds a config with two rostered vaults, neither pointed at a real directory: tests
// that need a scannable vault overwrite Path (and usually Areas) on the one they use.
func rosterCfg() *config.Config {
	return &config.Config{
		Vaults: map[string]config.VaultSettings{
			"pessoal":  {Path: "/vaults/pessoal", Areas: "00-inbox"},
			"trabalho": {Path: "/vaults/trabalho", Areas: "00-inbox"},
		},
	}
}

func TestSelectVaults(t *testing.T) {
	cfg := rosterCfg()

	t.Run("both resolves the whole roster", func(t *testing.T) {
		got, err := selectVaults(cfg, bothVaults)
		if err != nil {
			t.Fatalf("selectVaults(%q): %v", bothVaults, err)
		}
		if want := cfg.VaultNames(); !slicesEqual(got, want) {
			t.Errorf("selectVaults(%q) = %v, want %v", bothVaults, got, want)
		}
	})

	for name := range cfg.Vaults {
		t.Run(name, func(t *testing.T) {
			got, err := selectVaults(cfg, name)
			if err != nil {
				t.Fatalf("selectVaults(%q): %v", name, err)
			}
			if len(got) != 1 || got[0] != name {
				t.Errorf("selectVaults(%q) = %v, want exactly [%s]", name, got, name)
			}
		})
	}

	t.Run("a name outside the roster is refused and the valid names are listed", func(t *testing.T) {
		got, err := selectVaults(cfg, "malkawhat")
		if err == nil {
			t.Fatalf("selectVaults on an unregistered vault = %v, want an error", got)
		}
		if !strings.Contains(err.Error(), "malkawhat") {
			t.Errorf("error %q does not name the offending value", err)
		}
		for _, want := range []string{"pessoal", "trabalho", bothVaults} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not list %q", err, want)
			}
		}
	})

	t.Run("an empty roster is refused rather than resolving to zero vaults", func(t *testing.T) {
		got, err := selectVaults(&config.Config{}, "anything")
		if err == nil {
			t.Fatalf("selectVaults against an empty roster = %v, want an error", got)
		}
	})
}

// TestSelectVaults_VaultNamedBoth_RefusesEveryRun covers the collision D-26 created: `both` passes
// the slug rule, so an operator may put it in KNOWRAG_VAULTS, and then --vault has two meanings.
//
// The failure this prevents is not an error message — it is the absence of one. Resolving the
// sentinel first ingests every vault while the operator asked for the one named `both`; that run
// exits 0 and reports a clean count of the wrong corpus. So the check refuses regardless of what
// --vault was given, including the vault's own name: while the roster carries it, no value of the
// flag has an unambiguous meaning.
func TestSelectVaults_VaultNamedBoth_RefusesEveryRun(t *testing.T) {
	cfg := &config.Config{Vaults: map[string]config.VaultSettings{
		bothVaults: {Path: "/vaults/both", Areas: "infra"},
		"trabalho": {Path: "/vaults/trabalho", Areas: "infra"},
	}}

	for _, flag := range []string{bothVaults, "trabalho", "unknown"} {
		t.Run(flag, func(t *testing.T) {
			got, err := selectVaults(cfg, flag)
			if err == nil {
				t.Fatalf("selectVaults(%q) with a vault named %q = %v, want an error",
					flag, bothVaults, got)
			}
			// The collision error, not merely *an* error: with the check removed, `unknown` still
			// fails — for the ordinary reason — and its message happens to contain the word `both`
			// while listing the valid names. Asserting on the phrase only the collision path writes
			// is what makes this subtest discriminate instead of decorate.
			if !strings.Contains(err.Error(), "rename that vault") {
				t.Errorf("error %q is not the collision error; the run failed for some other reason", err)
			}
		})
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
	// The --vault help is built before cfg is read (see newIngestCmd), so it cannot name the
	// configured vaults — only point at where they come from.
	usage := cmd.Flags().Lookup("vault").Usage
	for _, want := range []string{"KNOWRAG_VAULTS", bothVaults} {
		if !strings.Contains(usage, want) {
			t.Errorf("--vault usage %q does not mention %q", usage, want)
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

// writeVault builds a minimal vault: notes under an area folder, plus one root file that only the
// exclusion list makes acceptable.
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
		Vaults: map[string]config.VaultSettings{
			"trabalho": {
				Path:             root,
				Areas:            "00-inbox",
				ExcludeRootFiles: "CLAUDE.md",
			},
		},
	}

	var out bytes.Buffer
	err := runIngest(context.Background(), &out, cfg, ingestOptions{
		vaultFlag: "trabalho",
		dryRun:    true,
		tenantID:  defaultTenantID,
		chunkCfg:  chunk.Config{FloorTokens: defaultFloorTokens, CeilingTokens: defaultCeilingTokens},
	})
	if err != nil {
		t.Fatalf("runIngest --dry-run with no Qdrant settings: %v", err)
	}

	got := out.String()
	// "tokenizer:" is the D-25 instrument's line. It is asserted here and not only in the counter's
	// own unit test because the counter can keep counting perfectly while nobody prints the number,
	// and a measurement nobody reads is the same as no measurement.
	for _, want := range []string{"2 note(s)", "2 chunk(s) to embed", "nothing was embedded or written", "tokenizer:"} {
		if !strings.Contains(got, want) {
			t.Errorf("dry-run output %q does not contain %q", got, want)
		}
	}
}

// TestRunIngest_MissingSettings_NamesOnlyWhatTheRunNeeds pins the per-command requirement in the
// direction that matters: a real run asks for Qdrant, a dry run does not, and neither asks about a
// vault it was not pointed at.
func TestRunIngest_MissingSettings_NamesOnlyWhatTheRunNeeds(t *testing.T) {
	baseVaults := map[string]config.VaultSettings{
		"trabalho": {},
		"pessoal":  {},
	}
	tests := map[string]struct {
		dryRun     bool
		wantNamed  []string
		wantAbsent []string
		vaultFlag  string
	}{
		"a real run needs Qdrant": {
			dryRun:     false,
			vaultFlag:  "trabalho",
			wantNamed:  []string{"QDRANT_ENDPOINT", "QDRANT_API_KEY", "DEFAULT_COLLECTION"},
			wantAbsent: []string{"KNOWRAG_VAULT_PESSOAL_PATH"},
		},
		"a dry run does not": {
			dryRun:     true,
			vaultFlag:  "trabalho",
			wantNamed:  []string{"EMBEDDER_ENDPOINT", "KNOWRAG_VAULT_TRABALHO_PATH"},
			wantAbsent: []string{"QDRANT_ENDPOINT", "KNOWRAG_VAULT_PESSOAL_PATH"},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := &config.Config{Vaults: baseVaults}
			err := runIngest(context.Background(), io.Discard, cfg, ingestOptions{
				vaultFlag: tc.vaultFlag,
				dryRun:    tc.dryRun,
			})
			if err == nil {
				t.Fatal("runIngest with an otherwise-empty config returned no error")
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

// TestRunIngest_EmptyRoster_FailsAsConfigurationNotAnEmptyRun pins the failure mode an operator
// needs: a `both` run against a roster nobody configured must refuse to start, not report a clean
// zero-note run that looks like success.
func TestRunIngest_EmptyRoster_FailsAsConfigurationNotAnEmptyRun(t *testing.T) {
	cfg := &config.Config{EmbedderEndpoint: tokenizeStub(t)}

	err := runIngest(context.Background(), io.Discard, cfg, ingestOptions{
		vaultFlag: bothVaults,
		dryRun:    true,
	})
	if err == nil {
		t.Fatal("runIngest against an empty roster returned no error")
	}
	if !strings.Contains(err.Error(), "no vaults are configured") {
		t.Errorf("error %q does not say the roster is empty", err)
	}
}

// TestRunIngest_UnreadableVault_FailsBeforeTouchingAnything keeps a misconfigured path from
// reporting an empty but successful run.
func TestRunIngest_UnreadableVault_FailsBeforeTouchingAnything(t *testing.T) {
	cfg := &config.Config{
		EmbedderEndpoint: tokenizeStub(t),
		Vaults: map[string]config.VaultSettings{
			"trabalho": {Path: filepath.Join(t.TempDir(), "absent"), Areas: "00-inbox"},
		},
	}

	err := runIngest(context.Background(), io.Discard, cfg, ingestOptions{
		vaultFlag: "trabalho",
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
