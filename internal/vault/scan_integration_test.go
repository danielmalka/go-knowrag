//go:build integration

package vault

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/danielmalka/go-knowrag/internal/schema"
)

// Real-vault exclusion configuration, PRD-contrato §2.4b as of 2026-08-08. It lives here rather
// than in the production package on purpose: the lists are per-vault operator config (S01's
// loader), and a copy compiled into internal/vault would make re-including PowerAI a code change.
var realVaults = []struct {
	vault      schema.Vault
	env        string
	exclusions Exclusions
}{
	{
		vault: schema.VaultMalkaLife(),
		env:   "KNOWRAG_VAULT_MALKALIFE_PATH",
		exclusions: Exclusions{
			Folders:   []string{"PowerAI", "resources", "templates"},
			RootFiles: []string{"AGENTS.md", "CLAUDE.md", "CLEITON.md", "GEMINI.md"},
		},
	},
	{
		vault: schema.VaultMalkaWay(),
		env:   "KNOWRAG_VAULT_MALKAWAY_PATH",
		exclusions: Exclusions{
			RootFiles: []string{"CLAUDE.md", "de-para-projetos.md"},
		},
	},
}

// TestScanVault_RealVaults_Integration runs the scanner against the actual corpus.
//
// Behind the `integration` build tag *and* skipped when a vault path is unset or absent: the tag
// keeps it out of `make test`, the skip keeps `make test-integration` usable on a host that has
// Qdrant but not the vaults.
//
// The timings it logs are the NFR-4/NFR-5 evidence artifact. Walk+parse is timed separately from
// gitUpdatedMap because the vaults live on /mnt/c and the ingestion binary runs inside WSL, so the
// read phase crosses a filesystem boundary; if that number blows its budget, the named plan B is
// to sync the vault into the WSL filesystem before ingesting. Logged, not gated — NFR-4 owns the
// budget.
func TestScanVault_RealVaults_Integration(t *testing.T) {
	results := make(map[schema.Vault]ScanResult, len(realVaults))

	for _, rv := range realVaults {
		t.Run(rv.vault.String(), func(t *testing.T) {
			root := os.Getenv(rv.env)
			if root == "" {
				t.Skipf("%s is not set", rv.env)
			}
			// #nosec G703 -- root is the operator-configured vault path this test exists to read
			if _, err := os.Stat(root); err != nil {
				t.Skipf("%s=%s is not readable: %v", rv.env, root, err)
			}

			// gitUpdatedMap on its own first, so its cost is attributable. ScanVault below runs it
			// once more internally; the two numbers are reported separately on purpose.
			gitStart := time.Now()
			gitUpdated, err := gitUpdatedMap(root)
			gitElapsed := time.Since(gitStart)
			if err != nil {
				t.Fatalf("gitUpdatedMap: %v", err)
			}
			t.Logf("gitUpdatedMap: %d paths in %.3fs", len(gitUpdated), gitElapsed.Seconds())

			scanStart := time.Now()
			result, err := ScanVault(root, rv.vault, rv.exclusions)
			scanElapsed := time.Since(scanStart)
			t.Logf("ScanVault (walk + read + parse + derive + git): %.3fs", scanElapsed.Seconds())
			t.Logf("read phase alone (scan minus git): %.3fs",
				(scanElapsed - gitElapsed).Seconds())

			if err != nil {
				var scanErrs *ScanErrors
				if errors.As(err, &scanErrs) {
					t.Errorf("%d note(s) failed the contract:", len(scanErrs.Errs))
					for _, e := range scanErrs.Errs {
						t.Errorf("  %v", e)
					}
				} else {
					t.Errorf("ScanVault: %v", err)
				}
				return
			}

			t.Logf("notes: %d, skipped: %d", len(result.Notes), len(result.Skipped))
			for _, s := range result.Skipped {
				t.Logf("  skipped %s: %s", s.Path, s.Reason)
			}
			logAreaHistogram(t, result)

			// Every note must carry a usable `updated`: both vaults are git repositories, so the
			// git path is the happy path here and a zero timestamp means neither it nor mtime fired.
			for _, n := range result.Notes {
				if n.Updated.IsZero() {
					t.Errorf("%s: Updated is the zero time", n.Path)
				}
			}
			results[rv.vault] = result
		})
	}

	life, haveLife := results[schema.VaultMalkaLife()]
	way, haveWay := results[schema.VaultMalkaWay()]
	if !haveLife || !haveWay {
		t.Skip("cross-vault uid check needs both vaults to have scanned cleanly")
	}
	if err := CheckCrossVaultDuplicateUIDs(life, way); err != nil {
		t.Errorf("cross-vault duplicate uid: %v", err)
	}
	t.Logf("total indexable notes across both vaults: %d", len(life.Notes)+len(way.Notes))
}

func logAreaHistogram(t *testing.T, result ScanResult) {
	t.Helper()
	counts := map[string]int{}
	for _, n := range result.Notes {
		counts[n.Area.String()]++
	}
	for area, n := range counts {
		t.Logf("  area %-20s %4d", area, n)
	}
}
