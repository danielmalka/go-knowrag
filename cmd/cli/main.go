// Command cli is the operator CLI for ingestion, inspection and evaluation.
// The subcommands land in S09; this entrypoint only proves config, logging and the command
// tree wire up.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/danielmalka/go-knowrag/internal/config"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		// The error goes to stderr rather than the logger: the log level lives in the config
		// that just failed to load.
		fmt.Fprintln(os.Stderr, "knowrag:", err)
		os.Exit(1)
	}

	log := config.NewLogger(cfg.LogLevel)
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
		os.Exit(1)
	}
}
