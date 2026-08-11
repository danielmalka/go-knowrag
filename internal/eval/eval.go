package eval

import (
	"context"
	"errors"
	"fmt"
)

// ErrNotImplemented is what both modes answer with until their harness exists.
//
// It is a sentinel rather than a message because two callers have to recognise it without matching
// prose: cmd/cli maps it to an exit code, and the CI jobs that run these gates recognise a
// still-missing harness by it (.github/workflows/ci.yml, guarded by TestCIWorkflow_GatesOnTheRealSentinel
// in cmd/cli so the string in the YAML cannot drift away from this one).
//
// What it must never be is a zero-valued Outcome and a nil error. An evaluation that did not run is
// not an evaluation that passed — the same rule the ingestion report follows when it refuses to
// render "orphans not scanned" as "no orphans found".
var ErrNotImplemented = errors.New("eval: not implemented")

// Options is the scope an evaluation runs against.
//
// It carries the scope and nothing else, and that is deliberate: S10 and S11 own what their
// harnesses need beyond it — S10's own task document has RunGolden taking a Searcher and a golden
// set, S11's has a Suite that builds its own cases — and a field added here for them now would be a
// guess at an interface neither has written yet. Both stories will add what they need to this struct
// and to cmd/cli/eval.go's one call site.
type Options struct {
	Collection string
	TenantID   string
}

// Outcome is what an evaluation reports to the CLI, and it is the piece of this contract that S10
// and S11 must not diverge from: cmd/cli reads exactly these fields to decide the exit code and
// what to print.
//
// It is deliberately not either story's own report type. S10's task document specifies an
// internal/eval Report of recall statistics per area; S11's specifies an isolation Report and states
// as a requirement that it carry no numeric score anywhere, because a tenant-isolation suite that
// could be reported as 90% passing is a suite somebody can argue down. Those two shapes cannot be
// one struct. This is the narrow thing both can be rendered into on the way out.
type Outcome struct {
	// Mode is "golden" or "isolation" — which gate produced this.
	Mode string

	// Passed is the whole verdict. For isolation it is S11's Report.Pass, which is false if any
	// single case failed; for golden it is the recall against the configured threshold.
	Passed bool

	// Score is the golden set's recall, and it is a pointer so that "this mode has no score" is
	// representable and distinct from "this mode scored zero". Isolation leaves it nil — see the
	// type comment for why that is a requirement rather than an omission.
	Score *float64

	// Summary is what an operator reads: the rendered report, already formatted by whichever
	// harness produced it. The CLI prints it and does not parse it.
	Summary string
}

// The two modes. One function each rather than one function with a mode argument, so that a caller
// cannot pass a mode that does not exist and so each keeps its own signature as S10 and S11 grow
// them apart.
func RunGolden(_ context.Context, _ Options) (Outcome, error) {
	return Outcome{}, notImplemented("golden", "S10", "the golden set and the recall harness")
}

func RunIsolation(_ context.Context, _ Options) (Outcome, error) {
	return Outcome{}, notImplemented("isolation", "S11", "the multi-tenant isolation suite")
}

// notImplemented says which gate is missing, which story builds it, and — the part that matters —
// that nothing was measured. A caller who reads only the first clause must not be able to come away
// thinking the gate ran.
func notImplemented(mode, story, what string) error {
	return fmt.Errorf("%w: `eval --%s` has no harness yet — %s builds %s. Nothing was measured, "+
		"so this is not a passing evaluation and must not be read as one", ErrNotImplemented, mode, story, what)
}
