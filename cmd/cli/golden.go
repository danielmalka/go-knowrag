package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/danielmalka/go-knowrag/internal/config"
	"github.com/danielmalka/go-knowrag/internal/goldenauthor"
	"github.com/danielmalka/go-knowrag/internal/vault"
)

// DefaultGoldenSetPath is where both `golden` and `eval --golden` look when --file says nothing.
//
// docs/ is gitignored on purpose (CLAUDE.md), so this file is absent on a fresh checkout and both
// commands say so by name rather than measuring or appending to an empty set. The hermetic CI job
// points --file somewhere else entirely (.github/workflows/ci.yml).
const DefaultGoldenSetPath = "docs/eval/golden-set.yaml"

// newGoldenCmd builds `golden`. Everything it does beyond flags and vault selection is
// internal/goldenauthor's, and that split is the feature's security design rather than tidiness —
// the package doc there is the explanation, and it is worth reading before adding a line to this
// file. In short: a package cannot import internal/retrieval or internal/store, where package main
// cannot stop itself from reaching them.
//
// So the size of this function is a property, not an accident. Everything here is visible at once:
// there is no call to anything that talks to the index, and that is checkable by reading it.
//
// The name is flat rather than `golden add`, matching the rest of this tree (schema, ingest, search,
// stats, eval): a parent command with one child is scaffolding for a second verb that does not
// exist. When one does, this becomes `golden add` and the parent gains a purpose in the same change.
//
// There is no --json. The command is a conversation with a person — it refuses to run without a
// terminal — and an envelope on a stream nobody is parsing is a flag that only ever gets tested.
func newGoldenCmd(cfg *config.Config) *cobra.Command {
	var path string

	cmd := &cobra.Command{
		Use:   "golden",
		Short: "Author golden-set questions, one note at a time",
		Long: "golden draws a note from the vaults, shows you enough of it to remember what it is\n" +
			"about, and asks you for one question that note should answer. It appends what you\n" +
			"write to the golden set and draws the next one, so a session can stop anywhere and\n" +
			"resume later.\n\n" +
			"It draws from the area the coverage table is shortest on, never uniformly, and never\n" +
			"draws a note that already has a question.\n\n" +
			"It never runs a search and never shows a search result, and that is the point of the\n" +
			"instrument rather than a limitation of it: a question written after seeing what the\n" +
			"index returns is a question tuned until it passes, and a golden set of those measures\n" +
			"itself. It reads the vaults, not the index, so it needs no Qdrant and no embedder.\n\n" +
			"Every prompt carries the rule that decides whether a question is worth anything, one\n" +
			"suggested kind of question, and the target mix. The long version — the anti-patterns,\n" +
			"and what to do when a question turns out to be ambiguous — is in docs/eval/, which is\n" +
			"not tracked: every useful example of a good question quotes the vault.\n\n" +
			"The `author` field comes from " + goldenauthor.AuthorEnv + " (falling back to USER) and\n" +
			"the `date` from today. Both are documentation: what proves a question was written\n" +
			"before the result it grades is the git history of the golden set, not a field in it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true

			// The scan is passed unrun. Reading the vaults takes about fifteen seconds and every
			// refusal Run makes — no terminal, no golden set, no author — is knowable before spending
			// them, so Run decides when, and a scheduled invocation hears "there is nobody to answer"
			// immediately instead of after a scan.
			scan := func() ([]vault.ScanResult, error) {
				names, err := selectVaults(cfg, bothVaults)
				if err != nil {
					return nil, err
				}
				if err := cfg.RequireVaults(names...); err != nil {
					return nil, err
				}
				return scanVaults(cfg, names)
			}

			// os.Stdin rather than cmd.InOrStdin(), for the reason `ingest` gives at the same call:
			// whether a human is on the other end is a property of the file descriptor, not of the
			// io.Reader cobra hands over (cmd/cli/ingest_modes.go, isTerminal).
			err := goldenauthor.Run(os.Stdin, cmd.OutOrStdout(), cmd.InOrStdin(), path, scan)
			if errors.Is(err, goldenauthor.ErrRefused) {
				// Re-marked with this package's sentinel so main maps it to the usage exit code
				// alongside every other refusal (cmd/cli/main.go, exitCodeFor).
				return fmt.Errorf("%w: %v", errUsage, err)
			}
			return err
		},
	}

	cmd.Flags().StringVar(&path, "file", DefaultGoldenSetPath,
		"golden set to append to; it must already exist and declare its `coverage:` table")
	return cmd
}
