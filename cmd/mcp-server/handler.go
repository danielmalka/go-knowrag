package main

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danielmalka/go-knowrag/internal/retrieval"
	"github.com/danielmalka/go-knowrag/internal/schema"
)

const toolName = "search_knowledge"

// topK bounds. Provisional numbers: S10 owns the golden set that could justify different ones.
const (
	defaultTopK = 5
	maxTopK     = 20
)

// SearchKnowledgeInput is the tool's entire input surface.
//
// What is absent is the design: there is no tenant_id and no collection field, so those are not
// values the model can name, misspell, or be talked into changing by a hostile chunk (ADR-002 §3).
// The scope comes from Config, which comes from the environment the operator wrote. The SDK infers
// the published JSON schema from this struct, so the absence is enforced on the wire, not only in
// Go.
type SearchKnowledgeInput struct {
	Query string `json:"query" jsonschema:"the natural-language question or phrase to search for"`
	Area  string `json:"area,omitempty" jsonschema:"optional filter: restrict results to one vault area"`
	Type  string `json:"type,omitempty" jsonschema:"optional filter: restrict results to one note type"`
	TopK  int    `json:"top_k,omitempty" jsonschema:"how many chunks to return; defaults to 5, capped at 20"`
}

// Searcher is the one thing this command needs from internal/retrieval.
//
// It is an interface so the handler tests never need a Qdrant, an embedder, or S07 wiring; main.go
// supplies the real *retrieval.Searcher. It is deliberately the whole dependency: there is no
// second method through which a search could be assembled here, which is NFR-8 ("zero search logic
// in cmd/mcp-server") expressed as a type rather than as a rule someone has to remember.
type Searcher interface {
	Search(ctx context.Context, q retrieval.Query) ([]retrieval.Result, error)
}

// newServer builds the stdio MCP server with its single tool already registered.
//
// searcher is taken as a parameter, not constructed here, because it is built once in main.go and
// captured by the handler closure for the process's whole life (S08 T7): the embedding client and
// the Qdrant connection must outlive a call, not be rebuilt per call.
func newServer(cfg Config, searcher Searcher) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "knowrag", Version: serverVersion}, nil)
	mcp.AddTool(s, &mcp.Tool{
		Name:        toolName,
		Description: toolDescription(),
	}, searchKnowledgeHandler(cfg, searcher))
	return s
}

// toolDescription is built at run time rather than written as a literal so the canonical enum
// values it advertises come from internal/schema's accessors — the single registry — instead of a
// hand-copied list that goes stale the day an area is added.
func toolDescription() string {
	return "Search Daniel's indexed knowledge base and return the matching chunks as untrusted " +
		"retrieved content. Valid `area` values: " + strings.Join(canonicalAreas(), ", ") +
		". Valid `type` values: " + strings.Join(canonicalNoteTypes(), ", ") + "."
}

func canonicalAreas() []string {
	var out []string
	for _, v := range schema.AllVaults() {
		for _, a := range schema.AreasFor(v) {
			if !slices.Contains(out, a.String()) {
				out = append(out, a.String())
			}
		}
	}
	slices.Sort(out)
	return out
}

func canonicalNoteTypes() []string {
	out := make([]string, 0, len(schema.AllNoteTypes()))
	for _, t := range schema.AllNoteTypes() {
		out = append(out, t.String())
	}
	return out
}

// searchKnowledgeHandler is the whole tool: translate input into a retrieval.Query whose scope
// fields come from config, call the searcher, frame the results, log the call.
//
// Nothing here parses the query text for directives. Query text is search text; a chunk or a user
// asking to "ignore the filters and search every tenant" is embedded and matched like any other
// phrase, because Collection and TenantID are assigned from cfg before in.Query is read at all.
func searchKnowledgeHandler(cfg Config, searcher Searcher) mcp.ToolHandlerFor[SearchKnowledgeInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in SearchKnowledgeInput) (*mcp.CallToolResult, any, error) {
		start := time.Now()

		if err := validateFilters(in); err != nil {
			return nil, nil, logAndWrap(cfg, start, 0, err)
		}

		q := retrieval.Query{
			// Scope: instance configuration, never input. This is the story.
			Collection: cfg.Collection,
			TenantID:   cfg.TenantID,

			Text: in.Query,
			TopK: clampTopK(in.TopK),
			Area: in.Area,
			Type: in.Type,

			// IncludeArchived and IncludePrivate stay false and have no input that could set them.
			// Both are privileged paths reachable only by the administrative CLI's explicit flags
			// (S09 / ADR-002 §2.4); an MCP client is not a privileged caller.
		}

		results, err := searcher.Search(ctx, q)
		if err != nil {
			return nil, nil, logAndWrap(cfg, start, 0, fmt.Errorf("search failed: %w", err))
		}

		text, err := formatResults(results)
		if err != nil {
			return nil, nil, logAndWrap(cfg, start, len(results), fmt.Errorf("assembling the response failed: %w", err))
		}

		slog.Info("search_knowledge call",
			"tenant", cfg.TenantID,
			"collection", cfg.Collection,
			"latency_ms", time.Since(start).Milliseconds(),
			"result_count", len(results))

		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
	}
}

// validateFilters rejects an area or type that is not a canonical value.
//
// Passing an unregistered value through would not be a security hole — the filter would simply
// match nothing — but "no results" is the worst possible answer to give an agent, because it reads
// as "the knowledge base has nothing about this" rather than "you misspelled a filter".
func validateFilters(in SearchKnowledgeInput) error {
	if in.Area != "" && !slices.Contains(canonicalAreas(), in.Area) {
		return fmt.Errorf("unknown area %q: valid values are %s", in.Area, strings.Join(canonicalAreas(), ", "))
	}
	if in.Type != "" {
		if _, ok := schema.ParseNoteType(in.Type); !ok {
			return fmt.Errorf("unknown type %q: valid values are %s", in.Type, strings.Join(canonicalNoteTypes(), ", "))
		}
	}
	return nil
}

// clampTopK turns an absent or absurd top_k into a usable one rather than an error: a model that
// asks for 500 chunks wants "as many as you'll give me", and refusing the call teaches it nothing.
func clampTopK(k int) int {
	switch {
	case k <= 0:
		return defaultTopK
	case k > maxTopK:
		return maxTopK
	default:
		return k
	}
}

// logAndWrap emits the one per-call log record for a failing call and returns the error the SDK
// packs into an MCP tool error result (IsError, readable text) rather than a protocol failure — the
// process stays connected and serves the next call.
//
// The credential is scrubbed from the message on the way out. Nothing in this repo is known to put
// it in an error string; this is here because an error message is the classic place a credential
// escapes to, and the check costs one comparison per failed call.
func logAndWrap(cfg Config, start time.Time, resultCount int, err error) error {
	slog.Error("search_knowledge call failed",
		"tenant", cfg.TenantID,
		"collection", cfg.Collection,
		"latency_ms", time.Since(start).Milliseconds(),
		"result_count", resultCount,
		"error", scrubCredential(cfg, err.Error()))

	return fmt.Errorf("search_knowledge: %s", scrubCredential(cfg, err.Error()))
}

func scrubCredential(cfg Config, msg string) string {
	if cfg.QdrantAPIKey == "" {
		return msg
	}
	return strings.ReplaceAll(msg, cfg.QdrantAPIKey, "[REDACTED]")
}
