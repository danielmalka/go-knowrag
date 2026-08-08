package main

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/danielmalka/go-knowrag/internal/config"
	"github.com/danielmalka/go-knowrag/internal/schema"
	"github.com/danielmalka/go-knowrag/internal/store"
)

// Process exit codes. Two are enough: an apply either brought Qdrant to the manifest or it did
// not, and every way of not doing so — bad config, unreachable server, drift — needs the same
// thing from the operator, which is to read the message.
const (
	exitOK      = 0
	exitFailure = 1
)

// newSchemaCmd builds `schema` and its `apply` subcommand.
//
// cfg is read at run time rather than at build time so the command tree can be assembled (and its
// help printed) without a valid configuration.
func newSchemaCmd(cfg *config.Config) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schema",
		Short: "Inspect and provision the Qdrant collections",
		Args:  cobra.NoArgs,
	}

	statePath := schema.DefaultAppliedStatePath
	apply := &cobra.Command{
		Use:   "apply",
		Short: "Make Qdrant match the collection manifest, idempotently",
		Long: "apply creates whatever the manifest describes and is missing, verifies whatever\n" +
			"already exists, and fails without writing when the two disagree. Running it twice\n" +
			"performs no writes the second time.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// From here on, any error is a runtime failure — bad config, unreachable server,
			// drift — and printing the usage block after it buries the message that matters.
			// Flag and argument errors happen before this line and still print usage.
			cmd.SilenceUsage = true

			client, err := store.NewQdrantClient(store.Config{
				Endpoint: cfg.QdrantEndpoint,
				APIKey:   cfg.QdrantAPIKey,
			})
			if err != nil {
				return err
			}
			// Closed on every path out, including the drift path: the CLI is short-lived, but
			// S06a/S07's tests build a client per test against the same code.
			defer func() { _ = client.Close() }()

			code, err := runSchemaApply(cmd.Context(), cmd.OutOrStdout(), client,
				&schema.FileAppliedStateStore{Path: statePath})
			if code == exitOK {
				return nil
			}
			// Returned, not printed: main prints it once, prefixed, and exits non-zero. Printing
			// here too would show the operator the same failure twice.
			return err
		},
	}
	apply.Flags().StringVar(&statePath, "state-file", statePath,
		"path to the committed applied-schema record (relative paths resolve from the working directory)")

	cmd.AddCommand(apply)
	return cmd
}

// runSchemaApply is the command with cobra, the network and os.Exit taken out: it returns the exit
// code the process should use, which is what makes both outcomes assertable in a plain test.
func runSchemaApply(
	ctx context.Context,
	out io.Writer,
	api schema.QdrantAPI,
	state schema.AppliedStateStore,
) (int, error) {
	report, err := schema.Apply(ctx, api, schema.Manifest(), state)

	// The report prints before the error check because a failed apply still did whatever it did to
	// the collections it got through, and the operator needs to see that half.
	for _, c := range report.Collections {
		// A failed write to the report stream is not worth failing an apply that succeeded, and
		// there is nowhere left to report it to anyway.
		_, _ = fmt.Fprintln(out, c)
	}
	if err != nil {
		return exitFailure, err
	}
	return exitOK, nil
}
