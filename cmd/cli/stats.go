package main

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/danielmalka/go-knowrag/internal/clicmd"
	"github.com/danielmalka/go-knowrag/internal/config"
	"github.com/danielmalka/go-knowrag/internal/schema"
	"github.com/danielmalka/go-knowrag/internal/store"
)

// statsReader answers the counts for every collection the manifest describes, optionally narrowed
// to one tenant. It is one function rather than a per-collection reader plus a loop because the
// loop is the part that must not be reinvented: the collections come from schema.Manifest(), so no
// caller here ever names `interno`.
type statsReader func(ctx context.Context, tenantID string) (map[string]store.Stats, error)

// newStatsCmd builds `stats`.
//
// The scope is deliberately two numbers and one optional flag. Per the PRD this command exists to
// check an ingestion and to notice orphans, and explicitly not to be observability — anything more
// belongs to a metrics pipeline that this project does not have and is not pretending to.
func newStatsCmd(read statsReader) *cobra.Command {
	var tenantID string
	var jsonOut bool

	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Count the points and the distinct notes in each collection",
		Long: "stats reports, per collection, how many points it holds and how many distinct notes\n" +
			"those points came from. The gap between the two numbers is where an orphan shows up:\n" +
			"a note that shrank leaves points behind that no longer belong to any live chunk.\n\n" +
			"It reads every point's uid to count them, so it costs one full pass over the\n" +
			"collection and is a command to run deliberately, not on a schedule.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Same reason as every other command here: with --json stdout is the envelope, and a
			// usage block printed onto it is a document no parser can read.
			cmd.SilenceUsage = true

			// A tenant of whitespace is neither of the two things --tenant can mean. It is not
			// absent, so it does not mean "every tenant", and it is not a tenant, so it matches no
			// point: the run reports zero points and zero uids over a healthy corpus, which reads
			// exactly like an ingestion that never happened. `search` refuses the same value for
			// the same reason (internal/clicmd/search.go).
			//
			// Refused rather than trimmed. Trimming would accept `--tenant " malka "` and search a
			// value the operator did not type, which is the silent normalisation config.errNotSlug
			// exists to avoid — and the same value is written into point_hash on the write path.
			if tenantID != "" && strings.TrimSpace(tenantID) == "" {
				return clicmd.Usage("--tenant is whitespace: it matches no point, so the counts " +
					"would come back zero over a healthy collection. Pass a tenant, or drop the " +
					"flag to count every tenant")
			}

			out := cmd.OutOrStdout()
			counts, err := read(cmd.Context(), tenantID)
			if err != nil {
				if jsonOut {
					// Dropped only on this path: the failure being reported is the one worth
					// keeping, and main prints it to stderr regardless.
					_ = clicmd.Emit(out, clicmd.Failed(err))
				}
				return err
			}
			return writeStats(out, jsonOut, tenantID, counts)
		},
	}

	cmd.Flags().StringVar(&tenantID, "tenant", "",
		"count only this tenant's points; omitted, the counts cover every tenant in the collection")
	cmd.Flags().BoolVar(&jsonOut, "json", false,
		"print the counts as a JSON envelope on stdout and nothing else")

	// No configuration is read here: the collections come from the manifest and the scope from the
	// flag. What needs configuration is the reader, and it is built in main.
	return cmd
}

// readStats is the real reader: one connection, one Client per collection in the manifest.
func readStats(ctx context.Context, cfg *config.Config, tenantID string) (map[string]store.Stats, error) {
	if err := cfg.Require(config.NeedQdrant); err != nil {
		return nil, err
	}
	client, err := store.NewQdrantClient(store.Config{
		Endpoint: cfg.QdrantEndpoint,
		APIKey:   cfg.QdrantAPIKey,
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()

	out := map[string]store.Stats{}
	for _, c := range schema.Manifest() {
		points, err := store.NewClient(client, c.Name)
		if err != nil {
			return nil, err
		}
		stats, err := points.Stats(ctx, tenantID)
		if err != nil {
			return nil, err
		}
		out[c.Name] = stats
	}
	return out, nil
}

// statsJSON is the wire shape, separate from store.Stats for the reason result.go spells out.
type statsJSON struct {
	// Tenant says what the numbers are about. It is always present and empty means every tenant —
	// a consumer must never have to infer the scope from whether a key is there.
	Tenant      string                     `json:"tenant"`
	Collections map[string]collectionStats `json:"collections"`
}

type collectionStats struct {
	Points int `json:"points"`
	UIDs   int `json:"uids"`
}

func writeStats(w io.Writer, jsonOut bool, tenantID string, counts map[string]store.Stats) error {
	if jsonOut {
		data := statsJSON{Tenant: tenantID, Collections: map[string]collectionStats{}}
		for name, s := range counts {
			data.Collections[name] = collectionStats{Points: s.Points, UIDs: s.UIDs}
		}
		return clicmd.Emit(w, clicmd.Succeeded(data))
	}

	// Sorted, because map order is random and two runs of the same command have to produce the
	// same lines — an operator comparing before and after an ingestion is diffing this output.
	for _, name := range slices.Sorted(maps.Keys(counts)) {
		s := counts[name]
		if _, err := fmt.Fprintf(w, "%-12s points: %-8d uids: %d\n", name, s.Points, s.UIDs); err != nil {
			return fmt.Errorf("writing the counts: %w", err)
		}
	}
	return nil
}
