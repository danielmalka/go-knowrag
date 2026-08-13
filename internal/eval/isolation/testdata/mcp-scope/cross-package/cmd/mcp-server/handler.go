// The escalation MCPScopeBindingCase does NOT catch, kept as a fixture so the limitation cannot be
// forgotten and so a later claim of full coverage fails a test.
//
// `search` below hands the caller's tenant to another package, which builds the query. There is no
// retrieval.Query literal here to check, so every rule about how a scope is set has nothing to
// apply to — and the decoy underneath is not even needed for that: the real cmd/mcp-server already
// contains a second, correctly scoped literal (explainEmptyArea's probe), which satisfies the "did
// this scan see any query at all" guard on its own. The decoy is here to reproduce that exactly.
//
// Catching this means following a data-flow question across the import graph, which is a different
// tool from a one-directory scan. The report says the MCP binding is not proven by this suite, and
// says this is why.
//
// Not built or linted: testdata is ignored by the go tool.
package main

import (
	"github.com/danielmalka/go-knowrag/internal/escalate"
	"github.com/danielmalka/go-knowrag/internal/retrieval"
)

type toolInput struct {
	Query    string `json:"query"`
	TenantID string `json:"tenant_id"`
	Area     string `json:"area,omitempty"`
}

type config struct {
	Collection string
	TenantID   string
}

// search is the escalation. The scan sees a call it is not asked about, in a function that returns
// no literal.
func search(cfg config, in toolInput) retrieval.Query {
	return escalate.Build(cfg.Collection, in.TenantID)
}

// probe is the correctly scoped literal every real handler also has, and it is what makes the scan
// report that it looked at something.
func probe(cfg config) retrieval.Query {
	return retrieval.Query{Collection: cfg.Collection, TenantID: cfg.TenantID}
}
