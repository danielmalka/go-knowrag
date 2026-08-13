// A server that decodes a tool call and builds no retrieval.Query. Every rule about how a scope is
// set holds here over nothing, which is the other green this case must refuse to report. Not built
// or linted: testdata is ignored by the go tool.
package main

type toolInput struct {
	Query    string `json:"query"`
	TenantID string `json:"tenant_id"`
}

func echo(in toolInput) string { return in.Query }
