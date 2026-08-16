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
// The second is a tripwire on one directory's shape and explicitly not a proof — read its doc
// comment before quoting a green run about the MCP server. It reads source because it has no
// choice: cmd/mcp-server is `package main`, so nothing can import it.
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
		"and a store that ignored it would be caught; and that an ingestion (internal/ingest) scopes " +
		"every call to the tenant it was asked for, labels every point it writes with that same " +
		"tenant, and can neither overwrite nor delete another tenant's points. Does not prove Qdrant " +
		"honours those conditions — internal/retrieval's integration-tagged tests do that against a " +
		"real instance.\n")

	// The MCP paragraph is the one this report got wrong once, and the correction is the reason it is
	// this long. An earlier version moved the MCP scope binding from the list below into the sentence
	// above and called it proven; review then escaped the case three ways with running code, two of
	// which are now refused and one of which is not. A suite that declares a gap honestly is worth
	// more than one claiming an invariant that does not hold, so the item stays declared and the
	// paragraph says exactly how far the case reaches. Its shape is fixed by
	// TestMCPScopeCase_DoesNotFollowIndirectionOutOfThePackage, which reads a fixture that escalates
	// past it.
	b.WriteString("\nThe MCP scope binding is not proven here (cmd/mcp-server/config.go fixes it " +
		"from the environment at startup). One case reads that command's source and refuses the " +
		"shapes an escalation takes inside it — a scope built from a tool input, assembled by a " +
		"call, left unset, or assigned after the fact — which is a tripwire on that directory and " +
		"not a proof: a query built in another package and returned from there carries no shape it " +
		"can see. The escalation attempt itself is driven by cmd/mcp-server's own tests, which no " +
		"release gate runs; no package can import a main package, so this suite cannot repeat them.\n")

	// The rest of the system, named here rather than left to whoever reads PASS. The suite drives two
	// entry points, Searcher.Search and ingest.Orchestrate, so a green run is a claim about those two
	// and about nothing else — and the reader who takes it for a claim about the system is the
	// failure this paragraph exists to prevent.
	b.WriteString("\nUntouched by every case above, so a PASS says nothing about them either: " +
		"`stats`, " +
		"which counts every tenant when none is named and does so by design (internal/store/points.go), " +
		"pagination, since every case here queries at offset 0, Searcher.FilterMatchesAnything " +
		"(internal/retrieval/search.go), which no case calls, and Searcher.GetByUID " +
		"(internal/retrieval/get.go), which is a filtered read of one note and is also not called here.\n")
	return b.String()
}
