// Command measure-search is the two-minute NFR-1b command: it runs a batch of real hybrid searches
// against the deployed Qdrant and embedding service, decomposes each one into the embedding-query,
// Qdrant and overhead legs (internal/retrieval.SearchTimed), and reports p50/p95/p99 for every
// column together with the pass/fail verdict against NFR-1's own gate (PRD.md: p95 <= 3s, p99 <=
// 5s end-to-end; NFR-1b has no gate of its own, it only has to decompose that number — PRD.md's
// NFR-1b row target column is "—").
//
// It dials the same components cmd/cli's `search` command does (cmd/cli/search.go's dialSearcher)
// and reads the same environment variables (internal/config); the connection code here is a small,
// deliberate duplicate rather than an import from cmd/cli, so this tool never has to change when
// cmd/cli does and vice versa. If cmd/cli/search.go's dial sequence changes (a new client option, a
// different embed profile), update it here too.
//
// It never runs against real infrastructure on its own — an operator runs it, pointed at their own
// deployment via the usual KNOWRAG_* / QDRANT_* / EMBEDDER_ENDPOINT environment variables.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/danielmalka/go-knowrag/internal/config"
	"github.com/danielmalka/go-knowrag/internal/embed"
	"github.com/danielmalka/go-knowrag/internal/measure"
	"github.com/danielmalka/go-knowrag/internal/retrieval"
	"github.com/danielmalka/go-knowrag/internal/store"
)

// nfr1P95Gate and nfr1P99Gate are NFR-1's end-to-end budget. The number lives in PRD.md (the NFR-1
// table row); it is repeated here as a literal because this binary has no dependency on docs/ (which
// is gitignored, CLAUDE.md), so if PRD.md's gate ever changes, this line is where to update it too.
const (
	nfr1P95Gate = 3 * time.Second
	nfr1P99Gate = 5 * time.Second
)

// queries is a stringSlice flag.Value so --query can repeat.
type queries []string

func (q *queries) String() string     { return strings.Join(*q, ",") }
func (q *queries) Set(v string) error { *q = append(*q, v); return nil }

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("measure-search", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var qs queries
	fs.Var(&qs, "query", "a query text to search with; repeat for more than one. Required at least once")
	tenant := fs.String("tenant", "", "tenant_id to search under. Required")
	collection := fs.String("collection", "", "collection to search; defaults to DEFAULT_COLLECTION")
	n := fs.Int("n", 30, "how many queries to run in total, cycling through --query in order "+
		"(T5's task doc asks for at least 30)")
	out := fs.String("out", "", "path to write the durable JSON report to; defaults to "+
		"./out/measure-search-<unix-timestamp>.json")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "usage: measure-search --tenant <id> --query <text> [--query <text> ...] "+
			"[--n 30] [--collection name] [--out path]\n\n"+
			"Measures NFR-1b: the search latency decomposition, against the real deployment named by\n"+
			"QDRANT_ENDPOINT / KNOWRAG_ADMIN_QDRANT_API_KEY / EMBEDDER_ENDPOINT / DEFAULT_COLLECTION.\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *tenant == "" {
		_, _ = fmt.Fprintln(stderr, "measure-search: --tenant is required")
		return 2
	}
	if len(qs) == 0 {
		_, _ = fmt.Fprintln(stderr, "measure-search: at least one --query is required")
		return 2
	}
	if *n < len(qs) {
		*n = len(qs)
	}

	cfg, err := config.Load()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "measure-search: loading config: %v\n", err)
		return 1
	}
	if *collection == "" {
		*collection = cfg.DefaultCollection
	}
	if err := cfg.Require(config.NeedQdrant | config.NeedEmbedder | config.NeedCollection); err != nil {
		_, _ = fmt.Fprintf(stderr, "measure-search: %v\n", err)
		return 1
	}

	searcher, release, err := dialSearcher(cfg)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "measure-search: %v\n", err)
		return 1
	}
	defer release()

	ctx := context.Background()
	legs := make([]retrieval.Timing, 0, *n)
	for i := 0; i < *n; i++ {
		q := retrieval.Query{Collection: *collection, TenantID: *tenant, Text: qs[i%len(qs)], TopK: 5}
		// Concurrency 1, sequential: NFR-1's own measurement condition (PRD.md) is "concorrência 1,
		// sem swap", and a benchmark that fired queries in parallel would be measuring a different
		// system than the one the gate describes.
		_, timing, err := searcher.SearchTimed(ctx, q)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "measure-search: query %d (%q) failed: %v\n", i, qs[i%len(qs)], err)
			return 1
		}
		legs = append(legs, timing)
	}

	report := measure.EvaluateSearch(legs, nfr1P95Gate, nfr1P99Gate)
	_, _ = fmt.Fprintln(stdout, report.String())

	path := *out
	if path == "" {
		path = fmt.Sprintf("out/measure-search-%d.json", time.Now().Unix())
	}
	if err := writeJSONReport(path, report); err != nil {
		_, _ = fmt.Fprintf(stderr, "measure-search: writing report to %s: %v\n", path, err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "report written to %s\n", path)

	if !report.Pass {
		return 1
	}
	return 0
}

// dialSearcher mirrors cmd/cli/search.go's dialSearcher — same client, same embed profile shape —
// so a p95/p99 measured here describes the connection an operator's own `cli search` makes, not a
// differently-tuned one.
func dialSearcher(cfg *config.Config) (*retrieval.Searcher, func(), error) {
	client, err := store.NewQdrantClient(store.Config{Endpoint: cfg.QdrantEndpoint, APIKey: cfg.QdrantAPIKey})
	if err != nil {
		return nil, nil, err
	}
	release := func() { _ = client.Close() }

	querier, err := store.NewQuerier(client)
	if err != nil {
		release()
		return nil, nil, err
	}

	transport, err := embed.NewHTTPTransport(cfg.EmbedderEndpoint)
	if err != nil {
		release()
		return nil, nil, err
	}
	// Same shape as cmd/cli/ingest.go's embedProfile: a one-shot operator command with nobody
	// waiting on a p99 of its own connection setup, so the generous timeouts there are appropriate
	// here too — this tool's own p95/p99 are what it measures, not what it is bound by.
	embedder, err := embed.NewServiceEmbedder(embed.OperatorProfile(cfg.EmbedderEndpoint), transport)
	if err != nil {
		release()
		return nil, nil, err
	}

	return retrieval.NewSearcher(embedder, querier, retrieval.DefaultConfig()), release, nil
}

func writeJSONReport(path string, report measure.SearchReport) error {
	if dir := dirOf(path); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(reportJSON{
		RequirementGate: "NFR-1 (PRD.md): p95 <= 3s, p99 <= 5s end-to-end; NFR-1b decomposes it into embed/qdrant/overhead",
		N:               report.N,
		Embed:           newStatsJSON(report.Embed),
		Qdrant:          newStatsJSON(report.Qdrant),
		Overhead:        newStatsJSON(report.Overhead),
		Total:           newStatsJSON(report.Total),
		P95Gate:         report.P95Gate.String(),
		P99Gate:         report.P99Gate.String(),
		Pass:            report.Pass,
		MeasuredAt:      time.Now().UTC().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

type statsJSON struct {
	P50 string `json:"p50"`
	P95 string `json:"p95"`
	P99 string `json:"p99"`
}

func newStatsJSON(s measure.Stats) statsJSON {
	return statsJSON{P50: s.P50.String(), P95: s.P95.String(), P99: s.P99.String()}
}

type reportJSON struct {
	RequirementGate string    `json:"requirement_gate"`
	N               int       `json:"n"`
	Embed           statsJSON `json:"embed"`
	Qdrant          statsJSON `json:"qdrant"`
	Overhead        statsJSON `json:"overhead"`
	Total           statsJSON `json:"total"`
	P95Gate         string    `json:"p95_gate"`
	P99Gate         string    `json:"p99_gate"`
	Pass            bool      `json:"pass"`
	MeasuredAt      string    `json:"measured_at"`
}

func dirOf(path string) string {
	i := strings.LastIndexByte(path, '/')
	if i < 0 {
		return ""
	}
	return path[:i]
}
