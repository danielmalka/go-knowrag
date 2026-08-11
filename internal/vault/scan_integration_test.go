//go:build integration

package vault

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/danielmalka/go-knowrag/internal/config"
)

// TestScanVault_RealVaults_Integration runs the scanner against the actual corpus.
//
// The roster comes from config.Load(), never from a table in this file. An earlier version carried
// one — names, area lists, exclusion lists, and a per-vault environment variable spelled out — and
// it was the same defect D-26 was paid to remove, just relocated into a test. The failure mode is
// what makes it worth a comment: a hard-coded `KNOWRAG_VAULT_<NAME>_PATH` whose <NAME> no longer
// matches the operator's roster does not fail, it **skips**, and a skipped integration test reads
// exactly like a passing one in the run output.
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
	cfg, err := config.Load()
	if err != nil {
		t.Skipf("no usable configuration: %v", err)
	}
	names := cfg.VaultNames()
	if len(names) == 0 {
		t.Skip("KNOWRAG_VAULTS names no vault")
	}

	scanned := make([]ScanResult, 0, len(names))

	for _, name := range names {
		settings := cfg.Vaults[name]
		t.Run(name, func(t *testing.T) {
			root := settings.Path
			if root == "" {
				t.Skipf("vault %s has no configured path", name)
			}
			// #nosec G703 -- root is the operator-configured vault path this test exists to read
			if _, err := os.Stat(root); err != nil {
				t.Skipf("vault %s at %s is not readable: %v", name, root, err)
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
			result, err := ScanVault(root, name, settings.AreaNames(), Exclusions{
				Folders:   settings.Folders(),
				RootFiles: settings.RootFiles(),
			})
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
			scanned = append(scanned, result)
		})
	}

	// Every pair, not one named pair: the point ID is uuid5(tenant_id + uid + chunk_index) and does
	// not include `vault`, so a uid repeated across *any* two of them silently overwrites. With the
	// roster now open-ended, checking only the first two would leave a third vault unguarded.
	if len(scanned) < 2 {
		t.Skip("cross-vault uid check needs at least two vaults to have scanned cleanly")
	}
	total := 0
	for i := range scanned {
		total += len(scanned[i].Notes)
		for j := i + 1; j < len(scanned); j++ {
			if err := CheckCrossVaultDuplicateUIDs(scanned[i], scanned[j]); err != nil {
				t.Errorf("cross-vault duplicate uid: %v", err)
			}
		}
	}
	t.Logf("total indexable notes across %d vault(s): %d", len(scanned), total)
}

func logAreaHistogram(t *testing.T, result ScanResult) {
	t.Helper()
	counts := map[string]int{}
	for _, n := range result.Notes {
		counts[n.Area]++
	}
	for area, n := range counts {
		t.Logf("  area %-20s %4d", area, n)
	}
}
