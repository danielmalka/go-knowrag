package main

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/danielmalka/go-knowrag/internal/clicmd"
	"github.com/danielmalka/go-knowrag/internal/config"
	"github.com/danielmalka/go-knowrag/internal/eval"
)

// evalMode is one gate. Both have the same shape so the command picks one and then knows nothing
// else about which it picked; injected as function fields so the tests below drive every outcome —
// passed, failed, broken — without either harness existing.
type evalMode func(context.Context, eval.Options) (eval.Outcome, error)

type evalModes struct {
	golden    evalMode
	isolation evalMode
}

// newEvalCmd builds `eval`.
//
// The mode flags are enforced by cobra's own validators rather than by a check in RunE. That is not
// only less code: MarkFlagsOneRequired puts the requirement in the generated help, so `eval --help`
// says a mode is mandatory instead of the operator finding out by running it.
func newEvalCmd(cfg *config.Config, modes evalModes) *cobra.Command {
	var golden, isolation, jsonOut bool

	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Run a quality gate and report whether it passed",
		Long: "eval runs one of the two gates and exits non-zero if it did not pass.\n\n" +
			"--golden measures retrieval recall against the golden set. --isolation runs the\n" +
			"tenant-isolation suite, which has no score: a single failing case fails the suite.\n\n" +
			"A gate that fails exits with its own code, distinct from a broken backend — see the\n" +
			"exit-code list in `knowrag --help`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Silenced from here on for the same reason `search` and `ingest` silence it: with
			// --json stdout carries the envelope, and a usage block printed there is a document a
			// parser cannot read. Cobra's own flag validation fails before this line and keeps it.
			cmd.SilenceUsage = true

			mode, run := modes.pick(golden, isolation)
			out := cmd.OutOrStdout()

			outcome, err := run(cmd.Context(), eval.Options{
				Collection: cfg.DefaultCollection,
				TenantID:   defaultTenantID,
			})
			if err != nil {
				if jsonOut {
					// The emit error is dropped here alone: this path already has a failure to
					// report, and replacing it with "the pipe closed" would lose the cause. main
					// prints the same failure to stderr.
					_ = clicmd.Emit(out, clicmd.Failed(err))
				}
				return err
			}

			// Printed before the verdict is returned, exactly as the ingestion prints its report
			// before failing the run: a gate that came back negative did the measuring, and the
			// numbers are the only thing anyone can act on.
			if werr := writeOutcome(out, jsonOut, mode, outcome); werr != nil {
				return werr
			}
			if !outcome.Passed {
				return clicmd.Assertion("the %s gate did not pass", mode)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&golden, "golden", false, "measure retrieval recall against the golden set")
	cmd.Flags().BoolVar(&isolation, "isolation", false, "run the tenant-isolation suite")
	cmd.Flags().BoolVar(&jsonOut, "json", false,
		"print the outcome as a JSON envelope on stdout and nothing else")
	cmd.MarkFlagsMutuallyExclusive("golden", "isolation")
	cmd.MarkFlagsOneRequired("golden", "isolation")

	return cmd
}

// pick resolves the two booleans to the one mode cobra has already guaranteed. Cobra refuses the
// command before RunE runs if neither or both flags are set, so the fallback is unreachable; it
// answers with the golden mode rather than a nil function, because a nil here would turn a
// validator that stopped working into a panic instead of a wrong-but-visible run.
func (m evalModes) pick(golden, isolation bool) (string, evalMode) {
	if isolation && !golden {
		return "isolation", m.isolation
	}
	return "golden", m.golden
}

// evalJSON is the wire shape of `eval --json`, separate from eval.Outcome for the reason
// internal/clicmd/result.go and internal/ingest/reportjson.go both spell out: renaming a Go field
// must not silently rename a key a script reads.
type evalJSON struct {
	Mode   string `json:"mode"`
	Passed bool   `json:"passed"`
	// Score has no omitempty, so the isolation gate reports `"score": null` rather than no key at
	// all. The absence is the statement — that suite has no number and must not be reported as a
	// percentage — and a missing key would read to a consumer as a field it forgot to look for.
	Score   *float64 `json:"score"`
	Summary string   `json:"summary"`
}

func writeOutcome(w io.Writer, jsonOut bool, mode string, o eval.Outcome) error {
	if jsonOut {
		data := evalJSON{Mode: mode, Passed: o.Passed, Score: o.Score, Summary: o.Summary}
		// A failed gate carries both keys: `data` with what was measured and `error` with the
		// verdict. It is the one command where a consumer needs both, and the envelope's `ok`
		// mirrors the gate rather than the process having reached the end.
		if !o.Passed {
			return clicmd.Emit(w, clicmd.Result{
				Data: data,
				Error: &clicmd.ErrorInfo{
					Category: string(clicmd.CategoryAssertion),
					Message:  fmt.Sprintf("the %s gate did not pass", mode),
				},
			})
		}
		return clicmd.Emit(w, clicmd.Succeeded(data))
	}

	if o.Summary != "" {
		if _, err := fmt.Fprintln(w, o.Summary); err != nil {
			return fmt.Errorf("writing the evaluation report: %w", err)
		}
	}
	verdict := "FAILED"
	if o.Passed {
		verdict = "passed"
	}
	line := fmt.Sprintf("%s: %s", mode, verdict)
	if o.Score != nil {
		line += fmt.Sprintf(" (score %.4f)", *o.Score)
	}
	if _, err := fmt.Fprintln(w, line); err != nil {
		return fmt.Errorf("writing the evaluation verdict: %w", err)
	}
	return nil
}
