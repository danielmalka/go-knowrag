// Package isolation is the adversarial proof that no tenant reaches another tenant's data.
//
// What it proves, and what it does not, stated here because the difference decides how much a green
// run is worth:
//
// Every case drives real production code against a deliberately hostile stand-in for Qdrant. The
// read cases drive internal/retrieval — the real Query validation, the real buildFilter, the real
// request builder — against the store in probe.go, and prove the request that leaves this system
// carries the tenant condition, and that the store cannot answer with a foreign point unless that
// condition is missing. The write case drives internal/ingest's Orchestrate against the store in
// probe_write.go, and proves every point handed over is scoped and labelled with the tenant that
// asked for it — which is the field the read filter has no choice but to trust.
//
// Two cases read source instead of driving code, and say so where they are declared: the
// architecture boundary (cases_architecture.go) and the MCP server's scope binding (cases_mcp.go).
// Both guard invariants about a build rather than about a call, and the second has no choice —
// cmd/mcp-server is `package main`, so nothing can import it.
//
// It does not prove Qdrant honours a `must` clause: that is Qdrant's contract, and
// internal/retrieval's integration-tagged tests (TestSearch_Integration_TenantIsolation) check it
// against a real instance on the private runner. A green suite here means "this system cannot ask a
// wrong question and cannot write a wrongly scoped point", not "the database was interrogated".
//
// That split is what lets the suite run in public CI with no container, no vault and no credential.
package isolation

import "strings"

// CaseResult is one case's verdict.
//
// Detail is written to be read by whoever is woken by the failure: it names the tenant, the query
// and the offending value, because "isolation case failed" without them starts an investigation
// from zero.
type CaseResult struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail,omitempty"`
}

// Report is the suite's whole answer.
//
// There is no score field, no count of passing cases, no percentage, and that is a requirement
// rather than an omission (PRD §3 S11): a security suite reportable as "90% passing" is a suite
// somebody argues down before a release. One failing case fails the suite, and the only number here
// is how many cases there were — which is a fact about coverage, not a grade.
type Report struct {
	Pass  bool         `json:"pass"`
	Cases []CaseResult `json:"cases"`
}

// FailingCases names every case that failed, not the first.
//
// All of them, because isolation failures come in families: one change that drops the tenant
// condition fails four cases at once, and seeing one of the four would send the reader looking for
// a narrower cause than the one that exists.
func (r Report) FailingCases() []string {
	var out []string
	for _, c := range r.Cases {
		if !c.Pass {
			out = append(out, c.Name)
		}
	}
	return out
}

// Summary is what the operator reads.
func (r Report) Summary() string {
	var b strings.Builder
	b.WriteString("# Tenant isolation suite\n\n")
	if r.Pass {
		b.WriteString("**PASS** — every case held.\n\n")
	} else {
		b.WriteString("**FAIL** — isolation is not proven. This blocks release (PRD §2.7 NFR-3).\n\n")
	}

	for _, c := range r.Cases {
		mark := "ok  "
		if !c.Pass {
			mark = "FAIL"
		}
		b.WriteString(mark + "  " + c.Name + "\n")
		if c.Detail != "" {
			b.WriteString("      " + c.Detail + "\n")
		}
	}

	b.WriteString("\nProves: the request this system builds always carries its tenant condition, " +
		"and a store that ignored it would be caught; that an ingestion (internal/ingest) scopes " +
		"every call to the tenant it was asked for, labels every point it writes with that same " +
		"tenant, and can neither overwrite nor delete another tenant's points; and that the scope of " +
		"every search cmd/mcp-server runs is copied from this instance's configuration and can be " +
		"reached by nothing a tool caller sends. Does not prove Qdrant " +
		"honours those conditions — internal/retrieval's integration-tagged tests do that against a " +
		"real instance.\n")

	// The reach of the MCP claim, stated because the case that makes it reads source rather than
	// driving the server. cmd/mcp-server is `package main` and nothing can import it, so the dynamic
	// proof — a real MCP session carrying `tenant_id` in its JSON — lives in that package's own tests
	// and cannot run from here. See MCPScopeBindingCase (cases_mcp.go), which says which is which.
	b.WriteString("\nThe MCP claim is made over that command's source, not by calling it: no package " +
		"can import a main package, so the escalation attempt itself is driven by " +
		"cmd/mcp-server's own tests and what runs here is the rule they would have to break.\n")

	// The rest of the system, named here rather than left to whoever reads PASS. The suite drives
	// three entry points, Searcher.Search, ingest.Orchestrate and cmd/mcp-server's source, so a green
	// run is a claim about those and about nothing else — and the reader who takes it for a claim
	// about the system is the failure this paragraph exists to prevent.
	b.WriteString("\nUntouched by every case above, so a PASS says nothing about them: `stats`, " +
		"which counts every tenant when none is named and does so by design (internal/store/points.go), " +
		"pagination, since every case here queries at offset 0, and Searcher.FilterMatchesAnything " +
		"(internal/retrieval/search.go), which no case calls.\n")
	return b.String()
}
