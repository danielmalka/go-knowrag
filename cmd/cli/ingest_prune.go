package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/danielmalka/go-knowrag/internal/config"
	"github.com/danielmalka/go-knowrag/internal/ingest"
	"github.com/danielmalka/go-knowrag/internal/vault"
)

// pruneStep runs the optional prune of a run and returns what it needs to hand back: the prune error and
// the state of the vault-root records, which the caller writes after the report is printed and only if
// the whole run succeeded.
//
// The scope it builds is where the scheduled prune's two limits come from: the note counts and the
// ceiling, and the identity verdict of each vault root against the records of the last successful run.
// A problem READING the records is not allowed to open the gate: it becomes a reason for every vault.
type pruneStep struct {
	err      error
	scope    *pruneScope
	current  map[string]vaultRoot
	verdicts map[string]rootVerdict
	scopeKey string
	recs     rootRecords
	statePth string
}

func newPruneScope(
	ctx context.Context,
	cfg *config.Config,
	scans []vault.ScanResult,
	opts ingestOptions,
	errOut io.Writer,
) *pruneStep {
	st := &pruneStep{
		current:  make(map[string]vaultRoot, len(scans)),
		verdicts: make(map[string]rootVerdict, len(scans)),
		scopeKey: cfg.QdrantEndpoint + "|" + cfg.DefaultCollection + "|" + opts.tenantID,
		recs:     rootRecords{Version: rootRecordsVersion, Roots: map[string]vaultRoot{}},
	}
	counts := make(map[string]int, len(scans))
	for _, s := range scans {
		counts[s.Vault] = len(s.Notes)
	}
	st.scope = &pruneScope{NoteCounts: counts, Ceiling: opts.ceiling, Roots: st.verdicts}
	if isTerminal(os.Stdin) && !opts.yes {
		st.scope.Confirm = func(reasons []string) bool {
			_, _ = fmt.Fprintln(errOut, "this prune is outside the limits of an unattended one:")
			for _, r := range reasons {
				_, _ = fmt.Fprintln(errOut, "  - "+r)
			}
			return askYesNo(os.Stdin, errOut, "Prune anyway, and accept the current vault root(s) as the right one?")
		}
	}

	var err error
	if st.statePth, err = vaultStateFile(); err == nil {
		st.recs, err = loadRootRecords(st.statePth)
	}
	for _, s := range scans {
		root, ierr := identifyRoot(ctx, cfg.Vaults[s.Vault].Path)
		if ierr != nil && err == nil {
			err = ierr
		}
		st.current[s.Vault] = root
	}
	if err != nil {
		// Unreadable state or an unresolvable root proves nothing, so it counts against every vault.
		for _, s := range scans {
			st.verdicts[s.Vault] = rootVerdict{Mismatch: true, Reason: fmt.Sprintf("the vault-root records could not be checked: %v", err)}
		}
		st.statePth = ""
		return st
	}
	for v, verdict := range checkRoots(st.recs, st.scopeKey, st.current) {
		st.verdicts[v] = verdict
	}
	return st
}

// run is the prune itself, when the run asked for one.
func (st *pruneStep) run(ctx context.Context, points ingest.Store, roots map[string]string, opts ingestOptions, report *ingest.Report) {
	if opts.prune {
		st.err = pruneOrphans(ctx, points, opts.tenantID, roots, st.scope, opts, report)
	}
}

// record writes the vault roots of a run that completed, so the next unattended prune has something to
// compare against. It never runs for a dry run, a run with failed notes or an interrupted one, and it
// does not overwrite a root that differs from the recorded one unless a person accepted it (the manual
// prune confirmation, or --accept-vault-root).
func (st *pruneStep) record(opts ingestOptions, report ingest.Report, errOut io.Writer) {
	if opts.dryRun || st.statePth == "" || runOutcome(report, nil) != nil {
		return
	}
	accepted := opts.acceptVaultRoot || st.scope.accepted
	if err := recordRoots(st.statePth, st.scopeKey, st.current, accepted, time.Now()); err != nil {
		_, _ = fmt.Fprintf(errOut, "warning: the vault-root records were not saved (%v); the next scheduled prune will be refused until they are\n", err)
	}
}

// addPruneGuardFlags registers the flags of the two limits on an unattended prune.
func addPruneGuardFlags(cmd *cobra.Command, opts *ingestOptions) {
	cmd.Flags().IntVar(&opts.ceiling.MaxNotes, "prune-max-notes", opts.ceiling.MaxNotes,
		"with --yes: refuse to prune more than this many notes of one vault in one run (and never more "+
			"than --prune-max-percent of it). A manual prune in a terminal asks instead")
	cmd.Flags().Float64Var(&opts.ceiling.MaxPercent, "prune-max-percent", opts.ceiling.MaxPercent,
		"with --yes: refuse to prune more than this percentage of one vault's indexed notes in one run")
	cmd.Flags().BoolVar(&opts.acceptVaultRoot, "accept-vault-root", false,
		"record the vault roots of this run as the right ones even if they differ from the last "+
			"successful run; a scheduled prune refuses a root it does not recognize until someone does this")
}
