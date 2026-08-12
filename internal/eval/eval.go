package eval

import (
	"context"
	"errors"
	"fmt"

	"github.com/danielmalka/go-knowrag/internal/retrieval"
)

// ErrNotImplemented is what both modes answer with until their harness exists.
//
// It is a sentinel rather than a message because two callers have to recognise it without matching
// prose: cmd/cli maps it to an exit code, and the CI jobs that run these gates recognise a
// still-missing harness by it. That second caller is a shell script, scripts/ci/eval-gate.sh, which
// carries this message as a literal because it cannot import a Go value; the two are held equal by
// TestCIWorkflow_PendingSentinelMatchesTheErrorItLooksFor in cmd/cli, which reads that script.
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

	// The golden gate's own inputs, added by S10 as the type comment above anticipated. All four
	// stay zero-valued until S10 T10 wires cmd/cli/eval.go, and GoldenGate refuses while they are —
	// see the sentinel below.
	//
	// Searcher is the seam that keeps this package free of a Qdrant client and a GPU: the harness
	// needs one search function and the CLI is what builds one.
	Searcher      Searcher
	GoldenSetPath string
	// MinRecall is the threshold the run is judged against. Zero is a legitimate value — "record
	// the number, do not gate on it" is what S10 T15 does — so it is not what tells the gate it is
	// unwired; Searcher and GoldenSetPath are.
	MinRecall float64
	// K is the cut-off. Zero means the default below.
	K int
}

// DefaultK is the cut-off every acceptance criterion in this project is written against: Recall@5.
// It is a constant here because the number is asserted in prose in the PRD and would otherwise be
// re-typed at each call site.
const DefaultK = 5

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

// The two gates. One function each rather than one function with a mode argument, so that a caller
// cannot pass a mode that does not exist and so each keeps its own signature as S10 and S11 grow
// them apart.
//
// They are named for the gate rather than for the run, and that is to leave S10 and S11 the names
// their own documents already spend. S10's task document specifies
// `RunGolden(ctx, Searcher, []GoldenQuestion, RunConfig) ([]QuestionResult, error)` in this package
// — a harness that measures and reports per question. Had this stub been called RunGolden, S10's
// first commit would have had to delete it before it could add its own, and the collision would
// have surfaced as a build failure in somebody else's story. Two names, two jobs: `*Gate` is the
// CLI-facing seam that answers "did it pass", `Run*` is the harness that does the measuring, and
// the gate calls the harness.

// GoldenGate loads the golden set, measures recall against it, and answers whether it cleared the
// threshold.
//
// The refusal above it is still reachable, and the reason is scope rather than a missing harness:
// the harness is in this package (goldenset.go, runner.go, report.go), but nothing builds a
// Searcher or names a golden-set file until S10 T10 wires cmd/cli/eval.go. Until then the CLI calls
// this with neither, and a gate with no searcher must say so rather than report a zero.
//
// Passed requires Complete. A run where some question could not be asked is not a run that measured
// the golden set, and its recall — computed over the questions that answered — must never clear a
// threshold on the strength of the ones it skipped.
func GoldenGate(ctx context.Context, opts Options) (Outcome, error) {
	if opts.Searcher == nil || opts.GoldenSetPath == "" {
		return Outcome{}, notImplemented("golden", "S10",
			"the CLI wiring that builds a searcher and names a golden-set file (T10); the harness "+
				"itself is built, but nothing handed this gate either of them")
	}

	set, err := LoadGoldenSet(opts.GoldenSetPath)
	if err != nil {
		return Outcome{}, err
	}

	k := opts.K
	if k <= 0 {
		k = DefaultK
	}
	results, err := RunGolden(ctx, opts.Searcher, set.Questions, RunConfig{
		Collection: opts.Collection,
		TenantID:   opts.TenantID,
		K:          k,
	})
	if err != nil {
		return Outcome{}, err
	}

	report := Aggregate(results)
	report.K = k
	report.Mode = retrieval.SearchModeHybrid.String()
	if cerr := ValidateCoverage(set.Questions, set.Coverage); cerr != nil {
		// Warn, do not fail: coverage is an authoring gate, not a run-time one (S10 open question 4,
		// decided). The warning rides in the report so it reaches the operator who ran the eval.
		report.CoverageWarning = cerr.Error()
	}
	attachProvenance(ctx, &report, opts.GoldenSetPath, set.Questions)

	recall := report.Global.Recall()
	return Outcome{
		Mode:    "golden",
		Passed:  report.Complete && recall >= opts.MinRecall,
		Score:   &recall,
		Summary: RenderReport(report),
	}, nil
}

// attachProvenance records which version of the golden set was measured.
//
// A git failure leaves GoldenSetCommit empty and does not fail the gate: not knowing the commit is
// a gap in the report, not a reason to throw away a measured run. The renderer prints "not
// resolved" for it rather than nothing, so the gap is visible instead of looking like a clean
// provenance section (report.go, renderProvenance).
//
// The baseline instant is the golden-set file's own last commit, so what gets flagged is every
// entry introduced after the file was last committed — that is, entries that exist only in the
// working tree. S10 T15 passes the real baseline run's time instead, once one exists.
func attachProvenance(ctx context.Context, report *Report, path string, questions []GoldenQuestion) {
	file, perEntry, err := GoldenSetCommit(ctx, path, questions)
	if err != nil {
		return
	}
	report.GoldenSetCommit = file.Hash
	report.Stale = ResolveStale(FlagStaleEntries(perEntry, file.Time), perEntry, questions)
}

func IsolationGate(_ context.Context, _ Options) (Outcome, error) {
	return Outcome{}, notImplemented("isolation", "S11", "the multi-tenant isolation suite")
}

// notImplemented says which gate is missing, which story builds it, and — the part that matters —
// that nothing was measured. A caller who reads only the first clause must not be able to come away
// thinking the gate ran.
func notImplemented(mode, story, what string) error {
	return fmt.Errorf("%w: `eval --%s` has no harness yet — %s builds %s. Nothing was measured, "+
		"so this is not a passing evaluation and must not be read as one", ErrNotImplemented, mode, story, what)
}
