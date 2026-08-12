package clicmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/danielmalka/go-knowrag/internal/config"
	"github.com/danielmalka/go-knowrag/internal/retrieval"
)

// defaultTopK is how many chunks `search` asks for when the operator does not say.
//
// The number has to be the MCP tool's default, because a parity claim that only holds when both
// sides are told the same top_k explicitly is a parity claim about the case nobody runs. That
// default is decided in cmd/mcp-server/handler.go and not here, so this comment cannot be checked
// by reading this file — what checks it is TestSearchParity_CLIAndMCP_BuildEquivalentQueryAndSameResults
// in cmd/mcp-server/search_parity_test.go, which runs both surfaces with no top_k given at all and
// compares the retrieval.Query each one built.
const defaultTopK = 5

// noMatches is what an empty answer prints, and the reason it exists is the alternative. A search
// that reached the index and got nothing back would otherwise print a blank line, which is
// indistinguishable in a terminal from a command that printed nothing because it fell over. The
// exit status is 0 either way, so the text is the only place the difference can live.
const noMatches = "no chunk matched: the query reached the index and the index answered with nothing."

// noBreadcrumb stands in for a chunk that sits above the first heading of its note. That is
// ordinary rather than broken (internal/retrieval/result.go treats headings as the one optional
// payload field), and an indented empty line would read as a rendering fault.
const noBreadcrumb = "(no heading)"

// Searcher is the one thing the search command needs from internal/retrieval.
//
// It is an interface for the same reason cmd/mcp-server's is: the command's tests, and the parity
// test that compares it against the MCP adapter, run with no Qdrant and no embedder. It carries the
// single method that answers a query — there is nothing here through which a search could be
// assembled, which is NFR-8 written as a type instead of as a rule to remember.
type Searcher interface {
	Search(ctx context.Context, q retrieval.Query) ([]retrieval.Result, error)
}

// Connect opens the searcher a run will use and hands back the function that releases it.
//
// The command takes this rather than a Searcher because the real one owns a Qdrant connection: it
// must not be built while the command tree is being assembled (that would dial on `--help`), and it
// must be closed when the run ends. cmd/cli supplies the real implementation; a test supplies a
// fake and a no-op close.
type Connect func(ctx context.Context) (Searcher, func(), error)

// searchOptions is the parsed flag set, in one value so the query builder takes what the command
// collected rather than eight arguments in an order nobody can check.
type searchOptions struct {
	text            string
	tenantID        string
	collection      string
	area            string
	topK            int
	includeArchived bool
	includePrivate  bool
	json            bool
}

// NewSearchCmd builds `search`.
//
// cfg is read here, at build time, for one value only: the default collection printed in the help
// text. Everything else it holds is read by the Connect the caller passes in, at run time, so the
// command tree still assembles and prints its help on a host with no configuration — the same
// property newSchemaCmd and newIngestCmd are built around (cmd/cli).
func NewSearchCmd(cfg *config.Config, connect Connect) *cobra.Command {
	opts := searchOptions{topK: defaultTopK}

	cmd := &cobra.Command{
		Use:   "search <text>",
		Short: "Search the indexed chunks and print what comes back",
		Long: "search runs one hybrid query against a collection and prints the matching chunks,\n" +
			"most relevant first. It is the same search the MCP server answers with, through the same\n" +
			"package, with one difference: this side is privileged and may be pointed at any tenant\n" +
			"and at content an MCP client can never ask for.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Silenced for everything below, including the missing --tenant: cobra writes the usage
			// block to stdout, and with --json stdout is the envelope, so a refusal would hand a
			// parser a wall of flag descriptions where it expected a document. The refusals name the
			// flag they are about. Cobra's own parse errors never reach this function and keep their
			// usage block. Same rule as `ingest` (cmd/cli/ingest.go).
			cmd.SilenceUsage = true
			opts.text = args[0]

			out := cmd.OutOrStdout()
			hits, err := runSearch(cmd.Context(), connect, opts)
			if err != nil {
				if opts.json {
					// The emit error is dropped, and only here: this path already has a failure to
					// report, and replacing it with "the pipe closed" would lose the cause the
					// operator needs. main prints the same failure as a human line on stderr
					// (cmd/cli/main.go), so nothing is silent even if stdout is gone.
					_ = Emit(out, Failed(err))
				}
				return err
			}
			return writeHits(out, opts.json, hits)
		},
	}

	cmd.Flags().StringVar(&opts.tenantID, "tenant", "",
		"tenant_id every result must carry. Required: there is no value that means every tenant")
	cmd.Flags().StringVar(&opts.collection, "collection", cfg.DefaultCollection,
		"collection to search")
	cmd.Flags().StringVar(&opts.area, "area", "",
		"restrict the search to one vault area; empty searches every area")
	cmd.Flags().IntVar(&opts.topK, "top-k", opts.topK,
		"how many chunks to return")
	cmd.Flags().BoolVar(&opts.includeArchived, "include-archived", false,
		"include notes whose status is archived, which are excluded by default")
	cmd.Flags().BoolVar(&opts.includePrivate, "include-private", false,
		"include notes whose visibility is private. This is the privileged path: it is the only "+
			"place in this system where private content can be asked for, and the MCP tool has no "+
			"equivalent input")
	cmd.Flags().BoolVar(&opts.json, "json", false,
		"print the answer as a JSON envelope on stdout and nothing else")

	return cmd
}

// runSearch turns the flags into a query, opens the searcher and runs it.
//
// The order is the content: the query is built — and refused, if it is one this command will not
// make — before connect is called at all. A refusal that happened after would still exit non-zero,
// having first dialed Qdrant and the embedding service for a search it was never going to run.
func runSearch(ctx context.Context, connect Connect, opts searchOptions) ([]retrieval.Result, error) {
	q, err := searchQuery(opts)
	if err != nil {
		return nil, err
	}

	searcher, release, err := connect(ctx)
	if err != nil {
		return nil, Backend(err)
	}
	defer release()

	hits, err := searcher.Search(ctx, q)
	if err != nil {
		return nil, categorize(err)
	}
	return hits, nil
}

// searchQuery builds the query, and holds the one refusal this command makes on its own.
//
// The refusal lives here rather than in the RunE that calls connect, because this is the function
// nobody can get a Query without: a second entry point, a future daemon, a flag added later, none
// of them can assemble a search that skips the tenant check, because there is nothing to assemble
// it out of. S07 enforces the same rule inside retrieval.Search — but that one fires after the
// connection is open, and "the CLI must not even attempt the call" is the half that has to be here.
func searchQuery(opts searchOptions) (retrieval.Query, error) {
	if strings.TrimSpace(opts.tenantID) == "" {
		return retrieval.Query{}, Usage(
			"--tenant is required and was not given: a search with no tenant is not a search with " +
				"weaker isolation, it is a search of somebody else's notes as well as yours. Pass " +
				"--tenant with the tenant this collection was ingested under")
	}

	return retrieval.Query{
		Collection: opts.collection,
		TenantID:   opts.tenantID,
		Text:       opts.text,
		TopK:       opts.topK,
		Area:       opts.area,

		IncludeArchived: opts.includeArchived,
		IncludePrivate:  opts.includePrivate,
	}, nil
}

// structuralRejections are the errors internal/retrieval returns for a query it can prove
// unanswerable before any I/O happens (internal/retrieval/query.go). They are the operator's
// command line, not a backend that failed, and telling the two apart is the whole reason that
// package exports them as sentinels instead of formatting message text.
var structuralRejections = []error{
	retrieval.ErrEmptyCollection,
	retrieval.ErrEmptyTenant,
	retrieval.ErrEmptyQuery,
	retrieval.ErrInvalidTopK,
	retrieval.ErrInvalidOffset,
	retrieval.ErrPrefetchLimitTooLow,
}

func categorize(err error) error {
	for _, sentinel := range structuralRejections {
		if errors.Is(err, sentinel) {
			return &Error{Category: CategoryUsage, Err: err}
		}
	}
	return Backend(err)
}

// searchHitJSON is one hit as a script reads it, and it is a separate type from retrieval.Result on
// purpose — the reasoning is written out in result.go and in internal/ingest/reportjson.go. The
// fields are spelled out rather than converted so that renaming a field on retrieval.Result stays a
// compile error here instead of silently renaming a key somebody's script reads.
type searchHitJSON struct {
	UID        string  `json:"uid"`
	ChunkIndex int     `json:"chunk_index"`
	Text       string  `json:"text"`
	Breadcrumb string  `json:"breadcrumb"`
	Path       string  `json:"path"`
	Score      float32 `json:"score"`
	// Untrusted is carried rather than assumed. It is true on every result internal/retrieval
	// produces, and a consumer that never sees the key has no way to notice the day one arrives
	// without it.
	Untrusted bool `json:"untrusted"`
}

// hitsJSON always returns an array, never nil: a script iterating the payload should not have to
// special-case the empty answer, and `null` is what an empty Go slice marshals to.
func hitsJSON(hits []retrieval.Result) []searchHitJSON {
	out := make([]searchHitJSON, 0, len(hits))
	for _, h := range hits {
		out = append(out, searchHitJSON{
			UID:        h.UID,
			ChunkIndex: h.ChunkIndex,
			Text:       h.Text,
			Breadcrumb: h.Breadcrumb,
			Path:       h.Path,
			Score:      h.Score,
			Untrusted:  h.Untrusted,
		})
	}
	return out
}

// writeHits renders the answer, and the two modes share no output: with --json stdout carries the
// envelope and nothing else, because a caller piping stdout into a parser must not have to strip a
// human line off the front first.
func writeHits(w io.Writer, jsonMode bool, hits []retrieval.Result) error {
	if jsonMode {
		return Emit(w, Succeeded(hitsJSON(hits)))
	}
	if len(hits) == 0 {
		if _, err := fmt.Fprintln(w, noMatches); err != nil {
			return fmt.Errorf("writing the result: %w", err)
		}
		return nil
	}
	for _, h := range hits {
		breadcrumb := h.Breadcrumb
		if breadcrumb == "" {
			breadcrumb = noBreadcrumb
		}
		if _, err := fmt.Fprintf(w, "%.4f  %s\n        %s\n        uid: %s  chunk_index: %d\n\n",
			h.Score, h.Path, breadcrumb, h.UID, h.ChunkIndex); err != nil {
			return fmt.Errorf("writing the results: %w", err)
		}
	}
	return nil
}
