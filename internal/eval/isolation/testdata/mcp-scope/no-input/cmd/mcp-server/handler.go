// A server that builds a correctly scoped query and decodes nothing from a tool call. Every rule
// about tool input holds here by having nothing to apply to, which is the green this case must
// refuse to report. Not built or linted: testdata is ignored by the go tool.
package main

import "github.com/danielmalka/go-knowrag/internal/retrieval"

type config struct {
	Collection string
	TenantID   string
}

func search(cfg config) retrieval.Query {
	return retrieval.Query{Collection: cfg.Collection, TenantID: cfg.TenantID}
}
