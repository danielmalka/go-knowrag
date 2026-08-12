// Command cli is the operator CLI for ingestion, inspection and evaluation.
//
// This file is the wiring: it loads configuration, installs the logger, assembles the command tree
// and turns whatever a command returns into a process exit code. Every subcommand's own logic lives
// beside it, or — for `search`, which one test outside this package has to drive — in
// internal/clicmd.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/danielmalka/go-knowrag/internal/clicmd"
	"github.com/danielmalka/go-knowrag/internal/config"
	"github.com/danielmalka/go-knowrag/internal/eval"
	"github.com/danielmalka/go-knowrag/internal/ingest/lock"
	"github.com/danielmalka/go-knowrag/internal/store"
)

// exitLockHeld is what a run refused by the ingestion lock exits with. Every other failure exits 1;
// this one has its own code because it is not the same event: a scheduler that fires while the
// previous ingestion is still running got an orderly refusal, not a broken system, and telling the
// two apart is the difference between a retry and a page.
const exitLockHeld = 3

// exitUsage is what a run refused for how it was invoked exits with — an unconfirmed --prune, a
// flag combination this build cannot honour (cmd/cli/ingest.go, errUsage), or a `search` with no
// --tenant. It is separate from 1 for the same reason exitLockHeld is: retrying it verbatim will
// fail identically, so a caller that sees it has to change the command line rather than wait. 2 is
// what most tools spend on a usage error.
const exitUsage = 2

// exitAssertion is what a command that ran perfectly and answered no exits with: an evaluation
// below its threshold. It is not 1, because 1 means the run broke, and a scheduler that cannot tell
// "the golden set regressed" from "Qdrant is unreachable" will page somebody for the wrong one.
//
// `eval` is what produces it: a gate that ran and came back negative returns
// clicmd.CategoryAssertion, and lands here. A gate whose harness does not exist yet is a different
// event and does not land here — nothing was measured, so there is no verdict to report (see
// eval.ErrNotImplemented).
const exitAssertion = 4

// exitFor turns a failure category into the process exit code for it. The categories are
// internal/clicmd's, because a subcommand has to carry one through cobra's single-error Execute;
// the numbers are here, because this is the file where all of them are read at once.
func exitFor(category clicmd.Category) int {
	switch category {
	case clicmd.CategoryUsage:
		return exitUsage
	case clicmd.CategoryAssertion:
		return exitAssertion
	case clicmd.CategoryBackend:
		return exitFailure
	}
	// Unreachable through clicmd.CategoryOf, which answers with one of the three. A category added
	// there and forgotten here lands on the code that means "the run broke", which is the reading
	// that costs least: it never tells a scheduler to stop retrying something worth retrying.
	return exitFailure
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		// The error goes to stderr rather than the logger: the log level lives in the config
		// that just failed to load.
		fmt.Fprintln(os.Stderr, "knowrag:", err)
		os.Exit(1)
	}

	log := config.NewLogger(cfg.LogLevel)
	// Installed as the default so a command that logs in passing — the ingestion lock's release
	// error, for one — writes at the configured level and in the same JSON as everything else,
	// instead of falling back to slog's plain-text logger at info.
	slog.SetDefault(log)
	log.Debug("cli starting", "config", cfg)

	// ExecuteC rather than Execute, for the command it hands back — exitCodeFor needs it.
	cmd, err := newRootCmd(cfg).ExecuteC()
	if err != nil {
		fmt.Fprintln(os.Stderr, "knowrag:", err)
		os.Exit(exitCodeFor(cmd, err))
	}
}

// exitCodeFor decides what the process exits with. It is a function so the whole mapping is
// assertable without running the binary.
//
// The SilenceUsage case is the one worth explaining. Cobra validates flags and arguments before any
// RunE runs and returns those failures raw — an unknown flag, a missing argument, a mode flag group
// nobody satisfied — so they arrive with no category on them and would fall through to "the run
// broke", which tells a scheduler to retry a command line that will fail identically forever. Every
// RunE in this tree sets SilenceUsage as its first statement, so the flag is still false exactly
// when cobra refused the command line before reaching one, and that is the signal: cobra printing a
// usage block and this exiting on the usage code are the same event, decided by the same field.
//
// "Every RunE sets it" is correctness that lives in one line of every command — the shape this
// series has paid for four times. It cannot be moved to a single place: a PersistentPreRunE runs
// *before* ValidateRequiredFlags and ValidateFlagGroups in cobra v1.10.2's execute(), so setting the
// flag there would mark a refused flag group as a command that ran, and setting it on the command
// struct at construction would suppress the usage block for the parse errors that should print one.
// So the call site stays, and TestEveryRunE_SilencesUsageFirst (cmd/cli/main_test.go) walks the
// source of both command packages and fails on the first RunE that forgets — which is the same
// answer, mechanically checked, rather than remembered.
func exitCodeFor(cmd *cobra.Command, err error) int {
	switch {
	// The three sentinels come first because they predate the categories and carry codes no
	// category maps to. All three are returned from inside a RunE, where SilenceUsage is already
	// set, so the case below can never take one of them.
	case errors.Is(err, lock.ErrHeld):
		return exitLockHeld
	case errors.Is(err, errUsage):
		return exitUsage
	case errors.Is(err, errInterrupted):
		return exitInterrupted
	case cmd != nil && !cmd.SilenceUsage:
		return exitUsage
	}
	return exitFor(clicmd.CategoryOf(err))
}

// newRootCmd assembles the command tree. It is a function rather than ten lines inside main so a
// test can run the tree — and read the help text below — without a process exit.
func newRootCmd(cfg *config.Config) *cobra.Command {
	root := &cobra.Command{
		Use:   "knowrag",
		Short: "Operator CLI for the knowledge layer — a privileged, administrative tool",
		// The privilege is declared here because it is the first thing anyone reading `--help`
		// should know, and because it is not visible from any single command. This tool takes
		// --tenant and --collection as ordinary flags and searches whatever it is given, including
		// content marked private, which no MCP client can ask for. Whoever can run this binary with
		// this configuration can read every tenant in the collection.
		Long: "knowrag ingests Obsidian vaults into Qdrant, searches what was indexed, counts what is\n" +
			"in there, and runs the quality gates over it. The gates are the one part not built yet:\n" +
			"`eval` exists, names the story that builds each harness, and refuses rather than\n" +
			"reporting a pass it did not measure.\n\n" +
			"This is a privileged administrative tool. It accepts any --tenant and any --collection,\n" +
			"and `search --include-private` reaches notes the MCP server structurally cannot return.\n" +
			"It therefore uses its own administrative Qdrant credential, " + config.AdminQdrantAPIKeyEnv + ",\n" +
			"which is a different variable from the scoped runtime key the MCP server reads: neither\n" +
			"falls back to the other, and having this one is having every tenant in the collection.",
		Args: cobra.NoArgs,
		// Runnable with no subcommand given, so a bare invocation prints usage instead of an
		// empty Usage block.
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Set here for the same reason every other RunE sets it first, and it is not cosmetic
			// even though this body only prints help: exitCodeFor reads this flag to tell a command
			// line cobra refused from a command that ran and failed. Left unset, a Help() that fails
			// on a closed pipe would be reported as a usage error — and, worse, the invariant the
			// mapping rests on would have an exception, which is how the next one gets added.
			cmd.SilenceUsage = true
			return cmd.Help()
		},
	}
	root.AddCommand(newSchemaCmd(cfg))
	root.AddCommand(newIngestCmd(cfg))
	root.AddCommand(newSearchCmd(cfg))
	root.AddCommand(newStatsCmd(func(ctx context.Context, tenantID string) (map[string]store.Stats, error) {
		return readStats(ctx, cfg, tenantID)
	}))
	// The two gates, pointed at internal/eval's entry points. Both refuse with eval.ErrNotImplemented
	// until S10 and S11 build the harnesses behind them; what is fixed here is the call shape and the
	// exit code, so neither story has to invent one.
	root.AddCommand(newEvalCmd(cfg, evalModes{golden: eval.GoldenGate, isolation: eval.IsolationGate},
		func(ctx context.Context) (clicmd.Searcher, func(), error) { return dialSearcher(ctx, cfg) }))
	// Cobra prints its own message for a bad flag or unknown subcommand; the extra copy Execute
	// would return here is noise.
	root.SilenceErrors = true
	return root
}
