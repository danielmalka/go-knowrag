package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/clicmd"
	"github.com/danielmalka/go-knowrag/internal/config"
	"github.com/danielmalka/go-knowrag/internal/retrieval"
)

// This file lives in cmd/mcp-server rather than in a package of its own because there is nowhere
// else it can live. The MCP adapter is unexported inside this package main, so no other package can
// reach it; the CLI adapter is in internal/clicmd, which any package may import. Here is the one
// directory where both are in scope at once.
//
// It runs in the ordinary suite. No build tag, no subprocess, no network, no Qdrant: both adapters
// are driven in this test binary over one fake searcher. A parity test behind `//go:build
// integration` would be a parity test nobody runs, and the failure it is meant to catch — two
// adapters drifting apart — is exactly the kind that arrives while nobody is looking.

// parityHits is what the fake searcher answers on both sides.
//
// The scores are deliberately not in descending order. RRF fusion decides the order upstream, in
// internal/retrieval, and both adapters must pass that order through untouched; a fixture already
// sorted by score would let an adapter that re-sorts by score look identical to one that does not.
func parityHits() []retrieval.Result {
	return []retrieval.Result{
		{UID: "uid-b", ChunkIndex: 2, Text: "The certificate rotation runbook.",
			Breadcrumb: "infra > certs", Path: "infra/certs.md", Score: 0.42, Untrusted: true},
		{UID: "uid-a", ChunkIndex: 0, Text: "Kubernetes ingress notes.",
			Breadcrumb: "infra > kubernetes > ingress", Path: "infra/kubernetes.md",
			Score: 0.87, Untrusted: true},
		{UID: "uid-c", ChunkIndex: 1, Text: "How the cluster resolves internal names.",
			Breadcrumb: "infra > dns", Path: "infra/dns.md", Score: 0.61, Untrusted: true},
	}
}

// locator is one result as both surfaces render it: the fields a reader on either side uses to find
// the chunk again. The text itself is not compared — the MCP surface frames and sanitizes it for a
// language model and the CLI does not, which is a difference in presentation rather than in what
// was found.
type locator struct {
	Path       string
	Breadcrumb string
	UID        string
	ChunkIndex int
	Score      string
}

// TestSearchParity_CLIAndMCP_BuildEquivalentQueryAndSameResults is the acceptance criterion: the
// same question, asked through either surface with the same scope, is the same search.
//
// Neither side is told a top_k, and that is the point of the test rather than an omission. Each
// adapter's default lives in its own file — clicmd's defaultTopK and this package's defaultTopK —
// so nothing in either file can tell a reader whether the two still agree. This is what tells them.
//
// Parity is defined with the CLI's --include-archived and --include-private both off, which are its
// defaults and what the MCP path always runs with: the tool has no input that reaches either field,
// so a comparison with one of them on would be a comparison against a search the MCP surface cannot
// make. TestSearchCmd_IncludePrivate_OnlyAddsPrivateResults (internal/clicmd) covers the other half
// — that turning the privileged one on widens the answer and changes nothing else.
func TestSearchParity_CLIAndMCP_BuildEquivalentQueryAndSameResults(t *testing.T) {
	const text = "como rotacionar certificados"
	const area = "infra"

	cfg := testConfig()
	// One fake for both, so a difference in what came back cannot be a difference between two
	// fixtures that drifted.
	shared := &fakeSearcher{results: parityHits()}

	cliOut := runCLISearch(t, cfg, shared, text, area)
	cliQuery := shared.lastQuery()

	mcpOut := resultText(t, callRaw(t, connect(t, cfg, shared),
		fmt.Sprintf(`{"query":%q,"area":%q}`, text, area)))
	mcpQuery := shared.lastQuery()

	if shared.calls() != 2 {
		t.Fatalf("the shared searcher was called %d time(s), want one per surface", shared.calls())
	}

	if !reflect.DeepEqual(cliQuery, mcpQuery) {
		t.Errorf("the two surfaces built different queries from the same question:\ncli: %+v\nmcp: %+v",
			cliQuery, mcpQuery)
	}
	// Asserted on the value both sides carried, not on the CLI's alone: parity between two queries
	// that are equal and both privileged would be parity over the wrong profile.
	if cliQuery.IncludeArchived || cliQuery.IncludePrivate {
		t.Errorf("the CLI's defaults reach privileged content: %+v", cliQuery)
	}

	cliLocators := cliLocators(t, cliOut)
	mcpLocators := mcpLocators(t, mcpOut)
	if !reflect.DeepEqual(cliLocators, mcpLocators) {
		t.Errorf("the two surfaces reported different results, or reported them in a different "+
			"order — the fusion order is part of the answer:\ncli: %+v\nmcp: %+v",
			cliLocators, mcpLocators)
	}
	// Both renderings agreeing on nothing at all would satisfy the comparison above.
	if len(cliLocators) != len(parityHits()) {
		t.Fatalf("both surfaces reported %d result(s) for %d hit(s) — the comparison above passed "+
			"over an answer neither side rendered", len(cliLocators), len(parityHits()))
	}
}

// runCLISearch drives the CLI's search command over the shared fake and returns its --json output.
//
// It runs the subcommand directly rather than under cmd/cli's root, which is the one thing this
// test cannot reach from here: cmd/cli is a package main too. What the root adds is the exit-code
// mapping and the help text, neither of which is what parity is about — the query and the answer
// are built entirely inside this command.
func runCLISearch(t *testing.T, cfg Config, s clicmd.Searcher, text, area string) string {
	t.Helper()

	cmd := clicmd.NewSearchCmd(&config.Config{DefaultCollection: cfg.Collection},
		func(context.Context) (clicmd.Searcher, func(), error) { return s, func() {}, nil })

	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	// No --top-k and no --collection beyond the configured default: the MCP side names neither, so
	// naming them here would compare two explicit values instead of two defaults.
	cmd.SetArgs([]string{text, "--tenant", cfg.TenantID, "--area", area, "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cli search: %v\n%s", err, errOut.String())
	}
	return out.String()
}

// cliLocators reads the CLI's JSON envelope.
func cliLocators(t *testing.T, out string) []locator {
	t.Helper()

	var envelope struct {
		OK   bool `json:"ok"`
		Data []struct {
			UID        string  `json:"uid"`
			ChunkIndex int     `json:"chunk_index"`
			Breadcrumb string  `json:"breadcrumb"`
			Path       string  `json:"path"`
			Score      float32 `json:"score"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("parsing the CLI envelope: %v\n%s", err, out)
	}
	if !envelope.OK {
		t.Fatalf("the CLI search reported ok=false: %s", out)
	}

	locators := make([]locator, 0, len(envelope.Data))
	for _, h := range envelope.Data {
		locators = append(locators, locator{
			Path: h.Path, Breadcrumb: h.Breadcrumb, UID: h.UID, ChunkIndex: h.ChunkIndex,
			Score: fmt.Sprintf("%.4f", h.Score),
		})
	}
	return locators
}

// mcpBlock matches one rendered result in the MCP response (format.go assembles it).
//
// Reading the other surface's text is what makes the comparison about what a consumer receives
// rather than about what a fake returned to two callers. It is also a coupling, so it fails loudly:
// mcpLocators refuses an answer this pattern did not fully account for rather than comparing the
// part it managed to read.
var mcpBlock = regexp.MustCompile(`path: (.+)\nbreadcrumb: (.*)\nuid: (\S+) \| chunk_index: (\d+) \| score: ([0-9.]+)`)

func mcpLocators(t *testing.T, out string) []locator {
	t.Helper()

	matches := mcpBlock.FindAllStringSubmatch(out, -1)
	if len(matches) == 0 {
		t.Fatalf("no result block was recognised in the MCP response. Either the search returned "+
			"nothing, or cmd/mcp-server/format.go changed shape and mcpBlock has to follow it:\n%s", out)
	}

	locators := make([]locator, 0, len(matches))
	for _, m := range matches {
		index, err := strconv.Atoi(m[4])
		if err != nil {
			t.Fatalf("chunk_index %q in the MCP response is not a number: %v", m[4], err)
		}
		locators = append(locators, locator{
			Path: m[1], Breadcrumb: m[2], UID: m[3], ChunkIndex: index, Score: m[5],
		})
	}
	return locators
}
