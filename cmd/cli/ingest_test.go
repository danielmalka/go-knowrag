package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/chunk"
	"github.com/danielmalka/go-knowrag/internal/config"
	"github.com/danielmalka/go-knowrag/internal/embed"
	"github.com/danielmalka/go-knowrag/internal/ingest/lock"
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
		if want := cfg.VaultNames(); !slices.Equal(got, want) {
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
		got, err := selectVaults(cfg, "vault-c")
		if err == nil {
			t.Fatalf("selectVaults on an unregistered vault = %v, want an error", got)
		}
		if !strings.Contains(err.Error(), "vault-c") {
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

// TestRunIngest_MalformedArea_RefusedBeforeAnythingIsScanned is where the slug rule has to hold now
// that it no longer runs in config.Load: RequireVaults is the last thing between a malformed name
// and point_hash, and runIngest is what has to call it.
//
// The area here has a space, deliberately, and not an uppercase letter. Case is the one malformation
// vault.deriveArea would catch on its own, by lowercasing the folder before comparing — so a test
// written with `Research` would pass even if this check were deleted, and prove nothing. `my area`
// matches a folder of that exact name and is written verbatim into the payload and the hash.
//
// Refused *before* scanning matters too: a run that scanned first would report note counts, then
// fail, and the counts would read like progress.
func TestRunIngest_MalformedArea_RefusedBeforeAnythingIsScanned(t *testing.T) {
	cfg := &config.Config{
		QdrantEndpoint:    deadQdrant,
		QdrantAPIKey:      "not-a-real-key",
		DefaultCollection: "knowrag_test",
		EmbedderEndpoint:  tokenizeStub(t),
		Vaults: map[string]config.VaultSettings{
			"trabalho": {Path: writeVault(t, map[string]string{
				"my area/uma.md": note("0198a7f2-4b31-7c42-9e15-3d8a92c47b03", "Uma"),
			}), Areas: "my area"},
		},
	}

	var out bytes.Buffer
	err := runIngest(context.Background(), &out, io.Discard, cfg, ingestOptions{
		vaultFlag: "trabalho",
		dryRun:    true,
		tenantID:  defaultTenantID,
		chunkCfg:  chunk.Config{FloorTokens: defaultFloorTokens, CeilingTokens: defaultCeilingTokens},
	})
	if err == nil {
		t.Fatalf("runIngest with area %q = nil, want an error; output was %q", "my area", out.String())
	}
	if !strings.Contains(err.Error(), "my area") {
		t.Errorf("error %q does not name the offending area", err)
	}
	if out.Len() != 0 {
		t.Errorf("runIngest wrote %q before refusing; nothing should be reported for a run that never starts", out.String())
	}
}

// TestScanVaults_TwoVaults_EachScannedWithItsOwnSettings closes a gap that only existed after D-26.
//
// While the roster was a compile-time enum, settings came from cfg.VaultOf(v) — a switch with one
// case per vault, where using the wrong vault's settings meant writing the wrong case and reading
// it back wrong. The map-and-loop version cannot be written wrong that visibly: `cfg.Vaults[name]`
// and `cfg.Vaults[names[0]]` differ by three characters and the compiler is happy with either.
//
// Two vaults with different areas is the smallest input where that shows: `um` accepts only `alfa`
// and `dois` only `beta`, so any mix-up makes one of the notes an unknown area and the scan fails
// instead of quietly returning a count.
//
// It asserts on scanVaults rather than on a whole run, because scanning is the subject and every
// mode now reaches Qdrant afterwards — routing this through runIngest would make it a test of the
// index connection instead.
func TestScanVaults_TwoVaults_EachScannedWithItsOwnSettings(t *testing.T) {
	cfg := &config.Config{
		Vaults: map[string]config.VaultSettings{
			"um": {Path: writeVault(t, map[string]string{
				"alfa/uma.md": note("0198a7f2-4b31-7c42-9e15-3d8a92c47b04", "Uma"),
			}), Areas: "alfa"},
			"dois": {Path: writeVault(t, map[string]string{
				"beta/outra.md": note("0198a7f2-4b31-7c42-9e15-3d8a92c47b05", "Outra"),
			}), Areas: "beta"},
		},
	}

	scans, err := scanVaults(cfg, []string{"um", "dois"})
	if err != nil {
		t.Fatalf("scanVaults over two vaults: %v — a vault scanned with another's settings makes its "+
			"own area unknown, which is what this failure looks like", err)
	}
	for _, scan := range scans {
		if len(scan.Notes) != 1 {
			t.Errorf("vault %s returned %d note(s), want 1", scan.Vault, len(scan.Notes))
		}
	}
}

// TestIngestCmd_RegistersItsFlags proves the flags were actually registered rather than only
// documented: cobra generates the help text from the registered set, so a flag missing from it is
// a flag the operator cannot pass.
func TestIngestCmd_RegistersItsFlags(t *testing.T) {
	cmd := newIngestCmd(&config.Config{})

	for _, name := range []string{
		"vault", "dry-run", "tenant", "floor-tokens", "ceiling-tokens", "prune", "yes", "json",
	} {
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
//
// It also answers /handshake with this build's own pins, which is what lets a real (non-dry) run
// reach the end of runIngest without a service anywhere: the handshake is fail-closed, so a stub
// that did not answer it would turn every such test into a test of the handshake.
func tokenizeStub(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/handshake" {
			writeHandshake(t, w)
			return
		}
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

// writeHandshake answers GET /handshake with this build's own pins, read from embed.Expected()
// instead of written out by hand: a stub carrying literal revisions would go red the day a pin
// moves, for a reason that has nothing to do with the command under test. `precision` travels in
// the contract's spelling rather than torch's, which the transport passes through untranslated.
func writeHandshake(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	expected := embed.Expected()
	err := json.NewEncoder(w).Encode(map[string]any{
		"model_revision":     expected.ModelRevision,
		"tokenizer_revision": expected.TokenizerRevision,
		"dense_dim":          expected.Dim,
		"normalized":         expected.Normalization == embed.ExpectedNormalization,
		"pooling":            expected.Pooling,
		"precision":          expected.Precision,
		"sparse":             expected.SparseParams,
	})
	if err != nil {
		t.Errorf("encoding /handshake response: %v", err)
	}
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

// TestRunIngest_DryRun_ReportsThroughTheSameReport is the wiring, and its name now says only what it
// checks: a dry run goes through the unified orchestration and produces a machine report labelled
// dry-run.
//
// It used to be called "...WithoutWriting" and asserted nothing about any store — the vault is empty
// and Qdrant is dead, so the run had nothing to write with. "It wrote nothing" is proven where a
// store can be watched: internal/ingest's TestRunBatch_DryRun_TouchesNothingAndReportsWhatItWould
// against the fake's call log, and the before/after point count in integration_test.go.
func TestRunIngest_DryRun_ReportsThroughTheSameReport(t *testing.T) {
	lockedCache(t)
	cfg := emptyVaultCfg(t)

	var out, errOut bytes.Buffer
	err := runIngest(t.Context(), &out, &errOut, cfg, ingestOptions{
		vaultFlag: "trabalho",
		dryRun:    true,
		json:      true,
		tenantID:  defaultTenantID,
		chunkCfg:  chunk.Config{FloorTokens: defaultFloorTokens, CeilingTokens: defaultCeilingTokens},
	})
	if err != nil {
		t.Fatalf("runIngest --dry-run: %v — stdout %q, stderr %q", err, &out, &errOut)
	}

	var report map[string]any
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("stdout does not parse as JSON: %v\n%s", err, out.String())
	}
	// --dry-run --json used to be refused outright, because the dry run produced no report at all.
	if got := report["mode"]; got != "dry-run" {
		t.Errorf("report mode is %v, want %q", got, "dry-run")
	}
	if !strings.Contains(errOut.String(), "tokenizer:") {
		t.Errorf("stderr %q does not carry the tokenizer summary", errOut.String())
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
			wantNamed:  []string{"QDRANT_ENDPOINT", "KNOWRAG_ADMIN_QDRANT_API_KEY", "DEFAULT_COLLECTION"},
			wantAbsent: []string{"KNOWRAG_VAULT_PESSOAL_PATH"},
		},
		// A dry run reads the index now, so it needs the same settings the write path does. What it
		// still must not name is a vault it was not pointed at.
		"a dry run needs it too": {
			dryRun:     true,
			vaultFlag:  "trabalho",
			wantNamed:  []string{"EMBEDDER_ENDPOINT", "QDRANT_ENDPOINT", "KNOWRAG_VAULT_TRABALHO_PATH"},
			wantAbsent: []string{"KNOWRAG_VAULT_PESSOAL_PATH"},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := &config.Config{Vaults: baseVaults}
			err := runIngest(context.Background(), io.Discard, io.Discard, cfg, ingestOptions{
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

	err := runIngest(context.Background(), io.Discard, io.Discard, cfg, ingestOptions{
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
		QdrantEndpoint:    deadQdrant,
		QdrantAPIKey:      "not-a-real-key",
		DefaultCollection: "knowrag_test",
		EmbedderEndpoint:  tokenizeStub(t),
		Vaults: map[string]config.VaultSettings{
			"trabalho": {Path: filepath.Join(t.TempDir(), "absent"), Areas: "00-inbox"},
		},
	}

	err := runIngest(context.Background(), io.Discard, io.Discard, cfg, ingestOptions{
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

// deadQdrant is where the lock tests below point QDRANT_ENDPOINT: a loopback address with nothing
// behind it, so the settings are present — a non-dry run demands them, and the lock is keyed on
// them — while nothing is ever reached. 127.0.0.2 rather than 127.0.0.1 on purpose: the machine
// running this suite may well have a real Qdrant on the usual loopback, and a hermetic test that
// quietly starts talking to it is not hermetic.
const deadQdrant = "127.0.0.2:6334"

// lockedCache points os.UserCacheDir — where internal/ingest/lock puts its files — at a directory
// this test owns. XDG_CACHE_HOME is the hook os.UserCacheDir documents on Linux, so the path
// derivation exercised here is production's, not a test-only branch.
func lockedCache(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
}

// holdLock takes the ingestion lock for a scope and keeps it until the test ends, on its own
// FileLock value — the same thing a second `knowrag ingest` process would be holding.
func holdLock(t *testing.T, cfg *config.Config, tenant string) {
	t.Helper()
	held, err := lock.New(t.Context(), cfg.QdrantEndpoint, cfg.DefaultCollection, tenant)
	if err != nil {
		t.Fatalf("lock.New for the scope under test: %v", err)
	}
	if err := held.TryAcquire(); err != nil {
		t.Fatalf("pre-acquiring the ingestion lock: %v", err)
	}
	t.Cleanup(func() { _ = held.Release() })
}

// TestRunIngest_LockHeld_RefusedBeforeAnythingIsScanned is the fail-fast half of D-31, and the
// "before anything is scanned" half is the one carrying weight.
//
// The vault path does not exist, so scanning it fails with "scanning vault" — which means an
// implementation that scanned first and checked the lock afterwards cannot produce ErrHeld here at
// all. A test that only asserted "an error came back" would pass that implementation happily, and
// the whole point of taking the lock before the scan is that the refused run does no work.
func TestRunIngest_LockHeld_RefusedBeforeAnythingIsScanned(t *testing.T) {
	lockedCache(t)
	cfg := &config.Config{
		QdrantEndpoint:    deadQdrant,
		QdrantAPIKey:      "not-a-real-key",
		DefaultCollection: "knowrag_test",
		EmbedderEndpoint:  tokenizeStub(t),
		Vaults: map[string]config.VaultSettings{
			"trabalho": {Path: filepath.Join(t.TempDir(), "absent"), Areas: "00-inbox"},
		},
	}
	holdLock(t, cfg, defaultTenantID)

	var out bytes.Buffer
	err := runIngest(t.Context(), &out, io.Discard, cfg, ingestOptions{
		vaultFlag: "trabalho",
		tenantID:  defaultTenantID,
		chunkCfg:  chunk.Config{FloorTokens: defaultFloorTokens, CeilingTokens: defaultCeilingTokens},
	})
	if !errors.Is(err, lock.ErrHeld) {
		t.Fatalf("runIngest while the lock is held = %v, want lock.ErrHeld — a %q error means the "+
			"vault was scanned before the lock was checked", err, "scanning vault")
	}
	// The operator's half of the message: exit code 3 says "refused", the text has to say by what.
	for _, want := range []string{"another ingestion is already running", cfg.DefaultCollection, defaultTenantID} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if out.Len() != 0 {
		t.Errorf("runIngest reported %q for a run that never started", out.String())
	}
}

// TestRunIngest_SuccessfulRun_ReleasesTheLock covers the other end: the lock a run took has to be
// gone once the run returns, or the next ingestion — the nightly one, most likely — is refused for
// a run that finished hours ago.
//
// The vault holds an area folder and no notes, which is what makes a complete, successful run
// hermetic: the command goes all the way through the handshake and the orchestration and finds
// nothing to embed or write, so no Qdrant has to exist behind the endpoint the lock is keyed on.
func TestRunIngest_SuccessfulRun_ReleasesTheLock(t *testing.T) {
	lockedCache(t)
	root := writeVault(t, nil)
	if err := os.MkdirAll(filepath.Join(root, "00-inbox"), 0o750); err != nil {
		t.Fatalf("creating the empty area folder: %v", err)
	}
	cfg := &config.Config{
		QdrantEndpoint:    deadQdrant,
		QdrantAPIKey:      "not-a-real-key",
		DefaultCollection: "knowrag_test",
		EmbedderEndpoint:  tokenizeStub(t),
		Vaults:            map[string]config.VaultSettings{"trabalho": {Path: root, Areas: "00-inbox"}},
	}

	var out bytes.Buffer
	err := runIngest(t.Context(), &out, io.Discard, cfg, ingestOptions{
		vaultFlag: "trabalho",
		tenantID:  defaultTenantID,
		chunkCfg:  chunk.Config{FloorTokens: defaultFloorTokens, CeilingTokens: defaultCeilingTokens},
	})
	if err != nil {
		t.Fatalf("runIngest over a vault with no notes: %v — output was %q", err, out.String())
	}

	next, err := lock.New(t.Context(), cfg.QdrantEndpoint, cfg.DefaultCollection, defaultTenantID)
	if err != nil {
		t.Fatalf("lock.New: %v", err)
	}
	if err := next.TryAcquire(); err != nil {
		t.Fatalf("TryAcquire after a successful run = %v, want success: the run kept its lock", err)
	}
	t.Cleanup(func() { _ = next.Release() })
}

// TestRunIngest_DryRun_ProceedsWhileTheLockIsHeld pins the one path that deliberately ignores the
// lock. A dry run writes nothing, so there is nothing for a concurrent run to tread on — and now
// that it reads the index, that is a choice rather than a consequence of never connecting.
//
// The vault is empty so the run completes hermetically; that costs the test nothing, because a
// missing dry-run guard refuses the run at the lock, before the note count matters at all.
func TestRunIngest_DryRun_ProceedsWhileTheLockIsHeld(t *testing.T) {
	lockedCache(t)
	cfg := emptyVaultCfg(t)
	holdLock(t, cfg, defaultTenantID)

	var out, errOut bytes.Buffer
	err := runIngest(t.Context(), &out, &errOut, cfg, ingestOptions{
		vaultFlag: "trabalho",
		dryRun:    true,
		tenantID:  defaultTenantID,
		chunkCfg:  chunk.Config{FloorTokens: defaultFloorTokens, CeilingTokens: defaultCeilingTokens},
	})
	if err != nil {
		t.Fatalf("--dry-run while another run holds the lock: %v — a dry run takes no lock", err)
	}
	if !strings.Contains(out.String(), "note(s)") {
		t.Errorf("dry-run output %q does not carry a report", out.String())
	}
}

// emptyVaultCfg is a config a full non-dry run can complete against with no equipment: one vault
// holding an area folder and no notes, so the run goes through the handshake, the lock and the
// orchestration and finds nothing to embed. Qdrant is the dead endpoint, which is reachable enough
// to build a client and key a lock on, and never answers — so the orphan snapshot always fails here.
func emptyVaultCfg(t *testing.T) *config.Config {
	t.Helper()
	root := writeVault(t, nil)
	if err := os.MkdirAll(filepath.Join(root, "00-inbox"), 0o750); err != nil {
		t.Fatalf("creating the empty area folder: %v", err)
	}
	return &config.Config{
		QdrantEndpoint:    deadQdrant,
		QdrantAPIKey:      "not-a-real-key",
		DefaultCollection: "knowrag_test",
		EmbedderEndpoint:  tokenizeStub(t),
		Vaults:            map[string]config.VaultSettings{"trabalho": {Path: root, Areas: "00-inbox"}},
	}
}

// TestRunIngest_JSON_StdoutCarriesOnlyTheReport is the machine-output contract and nothing else:
// stdout parses, stderr carries the human line. No --prune, deliberately — a destructive flag in
// this fixture would tie the stream assertions to the prune's outcome, and the refusal has its own
// test below.
//
// The snapshot still fails against the dead endpoint, which is worth having: the report has to be
// valid JSON on a degraded run, and `orphans_scanned` is how a consumer learns the run could not
// look.
func TestRunIngest_JSON_StdoutCarriesOnlyTheReport(t *testing.T) {
	lockedCache(t)
	cfg := emptyVaultCfg(t)

	var out, errOut bytes.Buffer
	err := runIngest(t.Context(), &out, &errOut, cfg, ingestOptions{
		vaultFlag: "trabalho",
		tenantID:  defaultTenantID,
		json:      true,
		chunkCfg:  chunk.Config{FloorTokens: defaultFloorTokens, CeilingTokens: defaultCeilingTokens},
	})
	if err != nil {
		t.Fatalf("runIngest --json over a vault with no notes: %v — stdout %q, stderr %q",
			err, &out, &errOut)
	}

	var report map[string]any
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("stdout does not parse as JSON: %v\nstdout was:\n%s", err, out.String())
	}
	if got := report["mode"]; got != "incremental" {
		t.Errorf("report mode is %v, want %q — a stored report that does not say which mode "+
			"produced it cannot be compared to another", got, "incremental")
	}
	if got := report["orphans_scanned"]; got != false {
		t.Errorf("orphans_scanned is %v on a run whose snapshot failed, want false", got)
	}
	// The D-25 instrument is still printed, on the stream a parser is not reading. Both halves are
	// asserted: dropping it entirely would pass a test that only checked stdout.
	if !strings.Contains(errOut.String(), "tokenizer:") {
		t.Errorf("stderr %q does not carry the tokenizer summary", errOut.String())
	}
	if strings.Contains(out.String(), "tokenizer:") {
		t.Errorf("stdout %q carries a human line; --json output has to be parseable on its own",
			out.String())
	}
}

// TestRunIngest_PruneWithoutSnapshot_PrintsTheReportThenFails is the ordering, which is the one
// property the unit test of pruneOrphans cannot see: ingestScans prints before it returns the
// failure, so an operator whose prune was refused still gets the run report for the notes it did
// process. An implementation that returned the error first would pass any assertion written only
// about the error.
func TestRunIngest_PruneWithoutSnapshot_PrintsTheReportThenFails(t *testing.T) {
	lockedCache(t)
	cfg := emptyVaultCfg(t)

	var out, errOut bytes.Buffer
	err := runIngest(t.Context(), &out, &errOut, cfg, ingestOptions{
		vaultFlag: "trabalho",
		tenantID:  defaultTenantID,
		prune:     true,
		yes:       true,
		chunkCfg:  chunk.Config{FloorTokens: defaultFloorTokens, CeilingTokens: defaultCeilingTokens},
	})
	if err == nil {
		t.Fatalf("runIngest --prune --yes against an unreadable index = nil; the run asked to delete "+
			"notes it could not identify and has to say so. stdout was %q", &out)
	}
	if !strings.Contains(err.Error(), "refusing to prune") {
		t.Errorf("error %q is not the prune refusal; the run failed for some other reason", err)
	}
	for _, want := range []string{"0 note(s)", "not scanned"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout %q does not contain %q: the report has to be printed before the "+
				"failure is returned, not swallowed by it", out.String(), want)
		}
	}
}
