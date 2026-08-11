package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/danielmalka/go-knowrag/internal/ingest"
)

// errUsage marks a refusal about how the command was invoked rather than about what it found —
// a flag combination this build cannot honour, or a destructive run nobody authorized. main maps it
// to its own exit code (exitUsage) so a scheduler can tell "your command line is wrong" from "the
// run broke", the same split exitLockHeld exists for.
var errUsage = errors.New("refused as invoked")

// validateModes refuses the flag combinations this build cannot honour, before any of them costs
// anything.
//
// Both refusals are the same fact stated twice: --dry-run returns from runIngest before
// ingest.Orchestrate runs, so there is no index snapshot and no Report on that path. --prune would
// therefore delete nothing while looking like it did, and --json would have nothing to print, so
// stdout would carry the dry run's human lines to a caller that asked for machine output. Silently
// honouring either is worse than refusing: one hides a destructive no-op, the other hands a parser
// prose. Part A-ii unifies the paths and both refusals go away then.
func validateModes(opts ingestOptions) error {
	if !opts.dryRun {
		return nil
	}
	for flag, set := range map[string]bool{"--prune": opts.prune, "--json": opts.json} {
		if set {
			return fmt.Errorf("%w: %s cannot be combined with --dry-run, because a dry run never "+
				"reads the index — it would have nothing to prune and nothing to report", errUsage, flag)
		}
	}
	return nil
}

// confirmPrune is the gate in front of the only destructive thing this command does.
//
// The non-interactive case is the one that had to be written down: a cron job that ran --prune and
// hit a prompt would hang until someone noticed, which is a worse outcome than either answer. So a
// stdin that is not a character device is refused outright rather than asked. ingest.Prune checks
// the same authorization one layer down (prune.go) for callers that never come through here.
//
// prompt is stderr, not stdout, and that is not tidiness: with --json the run report goes to stdout,
// and a question printed there lands in front of the JSON and stops it parsing. Same rule the
// tokenizer summary follows (printReport, ingest.go) — stdout carries only what a machine reads.
func confirmPrune(stdin *os.File, prompt io.Writer, opts ingestOptions) error {
	if !opts.prune || opts.yes {
		return nil
	}
	if !isTerminal(stdin) {
		return fmt.Errorf("%w: --prune deletes the points of every note missing from the vaults and "+
			"stdin is not a terminal, so there is nobody to confirm it; pass --yes to authorize the "+
			"deletion, or drop --prune to have the orphans only listed", errUsage)
	}
	return askConfirmation(stdin, prompt)
}

// askConfirmation is the prompt itself, split from the gate above so a test can answer it. The
// terminal check needs an *os.File and a terminal cannot be faked without a pty; the question and
// the answer need neither.
func askConfirmation(in io.Reader, prompt io.Writer) error {
	_, _ = fmt.Fprint(prompt, "--prune will delete the points of every note missing from the "+
		"scanned vaults. Continue? [y/N]: ")
	var answer string
	// The error is deliberately ignored and not distinguished from a "no": every way this read can
	// fail — a closed stdin, an interrupted terminal — leaves the answer unknown, and unknown is
	// exactly the case the refusal is for. Only a bare `y` or `Y` is a yes: Fscanln reads the first
	// whitespace-separated field, so "yes" and "y please" are both refusals.
	_, _ = fmt.Fscanln(in, &answer)
	if answer != "y" && answer != "Y" {
		return fmt.Errorf("%w: the prune was not confirmed, so nothing was deleted", errUsage)
	}
	return nil
}

// isTerminal reports whether a human could be typing into f, via os.ModeCharDevice — stdlib, which
// is why there is no dependency here.
//
// It is a proxy, not the truth: a tty sets that bit and so does /dev/null, which is a character
// device like any other. Being wrong about /dev/null costs a prompt that immediately reads EOF and
// refuses, which is the same answer as not prompting — so the proxy is good enough for the one
// decision it feeds. What it is right about is the case that matters: a pipe leaves the bit clear,
// so a cron job never reaches the prompt. The non-interactive test uses an os.Pipe for exactly that
// reason, and the prompt test uses /dev/null to get the other branch deterministically.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// splitStillOnDisk moves every candidate whose file is still on disk out of the prune list.
//
// This is the check that separates the two things ScanOrphans cannot tell apart. Its answer is
// `snapshot \ liveUIDs`, and "the scan did not return this uid" is not the same fact as "the note
// was deleted" — vault.ScanVault drops an emptied file into Skipped, and SkippedNote carries no uid,
// while adding a folder to KNOWRAG_VAULT_*_EXCLUDE_FOLDERS drops every note under it at once. Both
// leave the files on disk, and pruning on that evidence turns a config edit into a mass deletion.
//
// It lives here rather than in ScanOrphans because this is the layer that knows where the vaults
// are: the ingest package takes notes, not roots, and giving it disk access to answer one question
// would be a worse trade than one os.Stat per candidate.
//
// The path comes from the stored payload, which is what the note was last indexed under. That is the
// right question: a note moved to a location this run *does* scan is in the live set and never
// becomes a candidate, so the only paths reaching here are ones no scanned note claims.
//
// **Only a proven absence authorizes the prune**, which is why the test is fs.ErrNotExist and not
// `err == nil`. A stat can fail for reasons that say nothing about whether the note exists —
// permission denied on a parent directory, ENOTDIR, an I/O error, a vault path that stopped
// resolving mid-run. Treating those as deletion is the same mistake as pruning an unscanned vault,
// and the vaults here sit behind a /mnt mount that can fail partially without ever returning ENOENT.
func splitStillOnDisk(roots map[string]string, report *ingest.Report) {
	gone := report.Orphans[:0:0]
	for _, o := range report.Orphans {
		root, known := roots[o.Vault]
		// A vault outside this run's roots cannot be checked, and unchecked is not evidence of
		// deletion — ScanOrphans already scoped by vault, so reaching this is a caller bug, and
		// keeping the note is the answer that cannot destroy anything.
		if !known {
			report.OnDisk = append(report.OnDisk, o)
			continue
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(o.Path))); !errors.Is(err, fs.ErrNotExist) {
			report.OnDisk = append(report.OnDisk, o)
			continue
		}
		gone = append(gone, o)
	}
	report.Orphans = gone
}

// pruneOrphans is the destructive step, and it is a function taking an ingest.Store rather than ten
// lines inline so a test can hand it a store and watch whether it was touched at all. That is the
// only assertion that separates a refusal from a prune that ran and then complained — both return an
// error, and over an empty orphan list both also delete nothing.
func pruneOrphans(
	ctx context.Context,
	s ingest.Store,
	tenantID string,
	roots map[string]string,
	report *ingest.Report,
) error {
	// Here and not only at the call site: this is the function that deletes, so the check that says
	// which candidates are safe to delete belongs in front of it, where no caller can forget it.
	// ingestScans runs it too, for the report of a run that is not pruning at all — splitStillOnDisk
	// is idempotent, so the second call re-stats a list that already passed and changes nothing.
	splitStillOnDisk(roots, report)

	if !report.OrphansScanned {
		return errors.New(
			"refusing to prune: this run could not read the index snapshot, so it does not know " +
				"which notes were deleted — nothing was removed; fix the connection to Qdrant and " +
				"run it again")
	}
	var err error
	report.PointsPruned, err = ingest.Prune(
		ctx, s, tenantID, report.Orphans, ingest.PruneOptions{Confirmed: true})
	return err
}
