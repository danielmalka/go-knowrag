// This tree exists to be scanned, never to be built. It holds one of each escalation route
// MCPScopeBindingCase refuses, so the case can be shown to fire rather than only to stay green —
// the same role internal/archtest/testdata/violation.go plays for the architecture case.
//
// Four of the six were written by hand. Two — the `:=` alias and the embedded input — were found by
// review, against the earlier version of this case that read the source's shape instead of its
// resolved types, and both passed it clean. They are here because a route that once escaped is the
// one worth keeping a fixture for.
//
// It lives under testdata/, which the go tool ignores, so nothing here is compiled or linted.
package main

import "github.com/danielmalka/go-knowrag/internal/retrieval"

// toolInput is the tool's input surface, found by its JSON tags exactly as the real one is.
type toolInput struct {
	Query    string `json:"query"`
	TenantID string `json:"tenant_id"`
	Area     string `json:"area,omitempty"`
}

// config is the instance scope. No JSON tags, so the scan must not mistake it for input.
type config struct {
	Collection string
	TenantID   string
}

// fromInput is the escalation itself: the caller names the tenant and the server obeys.
func fromInput(cfg config, in toolInput) retrieval.Query {
	return retrieval.Query{Collection: cfg.Collection, TenantID: in.TenantID, Text: in.Query}
}

// laundered is the same escalation with the two names kept apart, which is what a rule looking for
// `in.TenantID` next to `TenantID:` would miss.
func laundered(cfg config, in toolInput) retrieval.Query {
	return retrieval.Query{Collection: scopeFor(in), TenantID: cfg.TenantID}
}

func scopeFor(in toolInput) string { return in.Area }

// aliased renames the input on the way in. The declaration carries no type, so nothing about its
// shape says `toolInput`; only the resolved type does.
func aliased(cfg config, in toolInput) retrieval.Query {
	proxy := in
	return retrieval.Query{Collection: cfg.Collection, TenantID: proxy.TenantID}
}

// requestCtx embeds the input, so `c.TenantID` is a promoted field and requestCtx declares no JSON
// tag of its own — invisible to any rule that reads declarations rather than types.
type requestCtx struct {
	toolInput
	deadline int
}

func embedded(cfg config, c requestCtx) retrieval.Query {
	return retrieval.Query{Collection: cfg.Collection, TenantID: c.TenantID}
}

// omitted names no tenant at all, which is a scope this package never decided.
func omitted(cfg config) retrieval.Query {
	return retrieval.Query{Collection: cfg.Collection}
}

// overwritten builds the query correctly and then moves the scope, which is the shape no rule about
// the literal can see.
func overwritten(cfg config, in toolInput) retrieval.Query {
	q := retrieval.Query{Collection: cfg.Collection, TenantID: cfg.TenantID}
	q.TenantID = in.TenantID
	return q
}
