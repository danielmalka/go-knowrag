// Command cli is the operator CLI for ingestion, inspection and evaluation.
// The subcommands land in S09; this entrypoint only proves config, logging and the command
// tree wire up.
package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/danielmalka/go-knowrag/internal/clicmd"
	"github.com/danielmalka/go-knowrag/internal/config"
	"github.com/danielmalka/go-knowrag/internal/ingest/lock"
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
// Nothing in this build produces it yet — `eval` is S10/S11's, and until it lands the only route to
// this number is a command returning clicmd.CategoryAssertion, which none does. It is declared with
// the others so the mapping below is the whole contract rather than two thirds of it.
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

	if err := newRootCmd(cfg).Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "knowrag:", err)
		if errors.Is(err, lock.ErrHeld) {
			os.Exit(exitLockHeld)
		}
		if errors.Is(err, errUsage) {
			os.Exit(exitUsage)
		}
		if errors.Is(err, errInterrupted) {
			os.Exit(exitInterrupted)
		}
		// Everything else answers for its own category, defaulting to "the run broke" for an error
		// nobody classified (clicmd.CategoryOf). The three sentinels above are checked first because
		// they predate the categories and carry codes no category maps to.
		os.Exit(exitFor(clicmd.CategoryOf(err)))
	}
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
		Long: "knowrag ingests Obsidian vaults into Qdrant, searches what was indexed, and evaluates\n" +
			"what comes back out. Schema provisioning, ingestion and search are wired up; evaluation\n" +
			"arrives with its story.\n\n" +
			"This is a privileged administrative tool. It accepts any --tenant and any --collection,\n" +
			"and `search --include-private` reaches notes the MCP server structurally cannot return.\n" +
			"It therefore uses its own administrative Qdrant credential, " + config.AdminQdrantAPIKeyEnv + ",\n" +
			"which is a different variable from the scoped runtime key the MCP server reads: neither\n" +
			"falls back to the other, and having this one is having every tenant in the collection.",
		Args: cobra.NoArgs,
		// Runnable with no subcommand given, so a bare invocation prints usage instead of an
		// empty Usage block.
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	root.AddCommand(newSchemaCmd(cfg))
	root.AddCommand(newIngestCmd(cfg))
	root.AddCommand(newSearchCmd(cfg))
	// Cobra prints its own message for a bad flag or unknown subcommand; the extra copy Execute
	// would return here is noise.
	root.SilenceErrors = true
	return root
}
