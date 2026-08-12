// Package isolation is the adversarial proof that no tenant reaches another tenant's data.
//
// What it proves, and what it does not, stated here because the difference decides how much a green
// run is worth:
//
// Every case drives the real internal/retrieval code path — the real Query validation, the real
// buildFilter, the real request builder — against a deliberately hostile store (probe.go). It
// proves the request that leaves this system carries the tenant condition, and that the store
// cannot answer with a foreign point unless that condition is missing. It does not prove Qdrant
// honours a `must` clause: that is Qdrant's contract, and internal/retrieval's integration-tagged
// tests (TestSearch_Integration_TenantIsolation) check it against a real instance on the private
// runner. A green suite here means "this system cannot ask a wrong question", not "the database was
// interrogated".
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
		"and a store that ignored it would be caught. Does not prove Qdrant honours that condition — " +
		"internal/retrieval's integration-tagged tests do that against a real instance.\n")
	return b.String()
}
