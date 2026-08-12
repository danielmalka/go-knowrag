package isolation

import "context"

// Case is one adversarial scenario. It answers with an empty string when isolation held, and with
// what went wrong when it did not — so a case cannot report failure without saying why.
type Case struct {
	Name string
	Run  func(ctx context.Context) string
}

// Suite is a plain slice of cases. A registry and nothing more: a new case is one entry in
// DefaultSuite, with no runner to touch.
type Suite struct {
	Cases []Case
}

// Run executes every case and folds the results into one verdict.
//
// Every case runs even after one has failed. A suite that stopped at the first failure would hide
// the shape of the breakage, and the shape is the useful part: one dropped condition failing four
// cases reads very differently from four unrelated failures.
func (s Suite) Run(ctx context.Context) Report {
	report := Report{Pass: true, Cases: make([]CaseResult, 0, len(s.Cases))}
	for _, c := range s.Cases {
		detail := c.Run(ctx)
		if detail != "" {
			report.Pass = false
		}
		report.Cases = append(report.Cases, CaseResult{Name: c.Name, Pass: detail == "", Detail: detail})
	}
	// An empty suite is not a passing suite. Nothing was attacked, so nothing was proven, and the
	// zero value of Pass would otherwise report the strongest possible claim from zero evidence —
	// the same rule internal/eval applies to an evaluation that did not run.
	if len(s.Cases) == 0 {
		report.Pass = false
		report.Cases = append(report.Cases, CaseResult{
			Name:   "suite is empty",
			Detail: "no isolation case ran, so nothing was proven; an empty suite must never report a pass",
		})
	}
	return report
}

// DefaultSuite is every case this build ships. cmd/cli runs exactly this.
func DefaultSuite(root string) Suite {
	return Suite{Cases: []Case{
		CrossTenantCase(),
		AdversarialTenantEmptyCase(),
		AdversarialTenantPayloadCase(),
		QueryTextInjectionCase(),
		PrivateVisibilityCase(),
		ArchivedExclusionCase(),
		PrivilegedPathCase(),
		ArchitectureBoundaryCase(root),
	}}
}
