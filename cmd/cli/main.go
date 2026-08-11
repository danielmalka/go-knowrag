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

	"github.com/danielmalka/go-knowrag/internal/config"
	"github.com/danielmalka/go-knowrag/internal/ingest/lock"
)

// exitLockHeld is what a run refused by the ingestion lock exits with. Every other failure exits 1;
// this one has its own code because it is not the same event: a scheduler that fires while the
// previous ingestion is still running got an orderly refusal, not a broken system, and telling the
// two apart is the difference between a retry and a page.
const exitLockHeld = 3

// exitUsage is what a run refused for how it was invoked exits with — an unconfirmed --prune, or a
// flag combination this build cannot honour (cmd/cli/ingest.go, errUsage). It is separate from 1 for
// the same reason exitLockHeld is: retrying it verbatim will fail identically, so a caller that sees
// it has to change the command line rather than wait. 2 is what most tools spend on a usage error.
const exitUsage = 2

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

	root := &cobra.Command{
		Use:   "knowrag",
		Short: "Operator CLI for the knowledge layer",
		Long:  "knowrag ingests Obsidian vaults into Qdrant and evaluates what comes back out.\nSchema provisioning and ingestion are wired up; evaluation arrives with its story.",
		Args:  cobra.NoArgs,
		// Runnable with no subcommand given, so a bare invocation prints usage instead of an
		// empty Usage block.
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	root.AddCommand(newSchemaCmd(cfg))
	root.AddCommand(newIngestCmd(cfg))
	// Cobra prints its own message for a bad flag or unknown subcommand; the extra copy Execute
	// would return here is noise.
	root.SilenceErrors = true

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "knowrag:", err)
		if errors.Is(err, lock.ErrHeld) {
			os.Exit(exitLockHeld)
		}
		if errors.Is(err, errUsage) {
			os.Exit(exitUsage)
		}
		os.Exit(1)
	}
}
