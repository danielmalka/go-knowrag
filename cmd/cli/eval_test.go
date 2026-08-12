package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/clicmd"
	"github.com/danielmalka/go-knowrag/internal/config"
	"github.com/danielmalka/go-knowrag/internal/eval"
	"github.com/danielmalka/go-knowrag/internal/retrieval"
)

// recordingModes is both gates as call-counting closures, so every assertion below is about which
// one ran rather than about what was printed.
type recordingModes struct {
	goldenCalls    int
	isolationCalls int
	scopes         []eval.Options

	outcome eval.Outcome
	err     error
}

func (r *recordingModes) modes() evalModes {
	return evalModes{
		golden: func(_ context.Context, o eval.Options) (eval.Outcome, error) {
			r.goldenCalls++
			r.scopes = append(r.scopes, o)
			return r.outcome, r.err
		},
		isolation: func(_ context.Context, o eval.Options) (eval.Outcome, error) {
			r.isolationCalls++
			r.scopes = append(r.scopes, o)
			return r.outcome, r.err
		},
	}
}

// stubConnect stands in for the Qdrant connection cmd/cli builds for real. Every test in this file
// runs with it, so none of them opens a socket — and a golden run that reached the real dialler
// would fail here rather than hang against a host that is not there.
func stubConnect(context.Context) (clicmd.Searcher, func(), error) {
	return stubSearcher{}, func() {}, nil
}

type stubSearcher struct{}

func (stubSearcher) Search(context.Context, retrieval.Query) ([]retrieval.Result, error) {
	return nil, nil
}

func runEval(t *testing.T, r *recordingModes, args ...string) (stdout string, err error) {
	t.Helper()

	cmd := newEvalCmd(&config.Config{DefaultCollection: "interno"}, r.modes(), stubConnect)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), err
}

func score(v float64) *float64 { return &v }

// TestEvalCmd_EachFlagRunsItsOwnGate is the routing contract, asserted on the call log: a command
// that printed the right words while running the other gate would pass any assertion on output.
func TestEvalCmd_EachFlagRunsItsOwnGate(t *testing.T) {
	tests := map[string]struct {
		flag                      string
		wantGolden, wantIsolation int
	}{
		"--golden":    {flag: "--golden", wantGolden: 1},
		"--isolation": {flag: "--isolation", wantIsolation: 1},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := &recordingModes{outcome: eval.Outcome{Passed: true, Summary: "recall 0.91"}}
			if _, err := runEval(t, r, tc.flag); err != nil {
				t.Fatalf("eval %s: %v", tc.flag, err)
			}
			if r.goldenCalls != tc.wantGolden || r.isolationCalls != tc.wantIsolation {
				t.Errorf("eval %s ran golden %d time(s) and isolation %d; want %d and %d",
					tc.flag, r.goldenCalls, r.isolationCalls, tc.wantGolden, tc.wantIsolation)
			}
		})
	}
}

// TestEvalCmd_ModeFlagsAreExactlyOne covers both refusals cobra's own validators own. It asserts
// the gate never ran, not the message: the wording is cobra's and will change with cobra, while
// "neither gate was started" is the property that has to hold.
func TestEvalCmd_ModeFlagsAreExactlyOne(t *testing.T) {
	for _, args := range [][]string{{}, {"--golden", "--isolation"}} {
		r := &recordingModes{outcome: eval.Outcome{Passed: true}}
		_, err := runEval(t, r, args...)
		if err == nil {
			t.Fatalf("eval %v was accepted; exactly one mode is required", args)
		}
		if r.goldenCalls+r.isolationCalls != 0 {
			t.Errorf("eval %v started a gate anyway (%d call(s))", args, r.goldenCalls+r.isolationCalls)
		}
		for _, want := range []string{"golden", "isolation"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal %q does not name --%s as one of the choices", err, want)
			}
		}
	}
}

// TestEvalCmd_FailedGate_IsAnAssertionNotABrokenBackend is the exit-code contract, and the reason
// the assertion category exists at all: recall that regressed is a true measurement of a worse
// system, and a scheduler that read it as a backend failure would retry it until it gave up.
func TestEvalCmd_FailedGate_IsAnAssertionNotABrokenBackend(t *testing.T) {
	r := &recordingModes{outcome: eval.Outcome{Passed: false, Score: score(0.60), Summary: "recall 0.60"}}

	out, err := runEval(t, r, "--golden")
	if err == nil {
		t.Fatal("a gate that did not pass exited clean")
	}
	if got := clicmd.CategoryOf(err); got != clicmd.CategoryAssertion {
		t.Errorf("CategoryOf(%v) = %q, want %q", err, got, clicmd.CategoryAssertion)
	}
	// Printed before the verdict is returned, the way the ingestion prints its report before
	// failing the run: the numbers are the only thing anybody can act on.
	if !strings.Contains(out, "recall 0.60") {
		t.Errorf("a failed gate swallowed the report it produced:\n%s", out)
	}
}

// TestEvalCmd_MissingHarness_IsABackendFailure is the distinction the whole not-implemented path
// exists for. A gate with no harness measured nothing, so reporting it in the same category as a
// gate that ran and failed would put "the suite regressed" and "the suite does not exist yet" on
// the same exit code.
//
// It asserts the category the failure *has*, not only the one it must not have. This test used to
// say `!= CategoryAssertion`, which is satisfied by every wrong answer as well as the right one: a
// category invented later, or a usage error, would have kept it green while the exit code moved.
//
// The gate here is a stub, and that is the change: no real gate can answer this way any more, since
// S10 wired the golden harness and S11 the isolation suite. It is the same reason
// scripts/ci/eval-gate.sh keeps its pending branch over an empty list — the mapping has to be right
// on the day a third gate is added before its harness is written, and there is no live gate left to
// learn it from.
func TestEvalCmd_MissingHarness_IsABackendFailure(t *testing.T) {
	r := &recordingModes{err: eval.ErrNotImplemented}

	_, err := runEval(t, r, "--golden")
	if err == nil {
		t.Fatal("a gate that answered \"no harness\" exited clean")
	}
	if !errors.Is(err, eval.ErrNotImplemented) {
		t.Errorf("the sentinel was lost on its way through cobra: %v", err)
	}
	if got := clicmd.CategoryOf(err); got != clicmd.CategoryBackend {
		t.Errorf("a missing harness reports as %q, want %q. Assertion would claim a measurement "+
			"nobody made; usage would tell the operator to fix a command line that is already "+
			"correct", got, clicmd.CategoryBackend)
	}
}

// TestEvalCmd_GoldenNeverReportsAPendingHarness is the CLI-level half of the guard
// internal/eval's TestGoldenGate_RefusalIsNotAPendingHarness holds on the package.
//
// scripts/ci/eval-gate.sh greps the command's combined output for eval.ErrNotImplemented's message
// and exits 0 when it finds it. So it is not enough that GoldenGate returns a different error — the
// string must not reach stdout or stderr on any golden failure at all, or the CI gate goes green
// over a golden run that measured nothing.
func TestEvalCmd_GoldenNeverReportsAPendingHarness(t *testing.T) {
	cases := map[string][]string{
		"golden set that is not there": {"--golden", "--file", filepath.Join(t.TempDir(), "nope.yaml")},
		"corpus that is not there":     {"--golden", "--corpus", filepath.Join(t.TempDir(), "nope.yaml")},
	}

	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			cmd := newEvalCmd(&config.Config{DefaultCollection: "interno"},
				evalModes{golden: eval.GoldenGate, isolation: eval.IsolationGate}, stubConnect)
			var out, errOut bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&errOut)
			cmd.SetArgs(args)

			err := cmd.Execute()
			if err == nil {
				t.Fatalf("eval %v passed", args)
			}
			printed := out.String() + errOut.String() + err.Error()
			if strings.Contains(printed, eval.ErrNotImplemented.Error()) {
				t.Errorf("a golden failure printed %q, which scripts/ci/eval-gate.sh reads as a "+
					"pending harness and exits 0 on:\n%s", eval.ErrNotImplemented, printed)
			}
			// A file the operator named and that is not there is a usage failure in both cases: the
			// far side will not change, so a scheduler retrying the identical command line retries
			// it forever.
			if got := clicmd.CategoryOf(err); got != clicmd.CategoryUsage {
				t.Errorf("%s reports as %q, want %q", name, got, clicmd.CategoryUsage)
			}
		})
	}
}

// TestEvalCmd_MissingGoldenSet_IsAUsageFailure keeps "you have not authored a golden set" off the
// exit code that means the system broke. A scheduler retrying the identical command line will never
// find the file, so the honest answer is the one that says to change the command line.
func TestEvalCmd_MissingGoldenSet_IsAUsageFailure(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "golden-set.yaml")
	cmd := newEvalCmd(&config.Config{DefaultCollection: "interno"},
		evalModes{golden: eval.GoldenGate, isolation: eval.IsolationGate}, stubConnect)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--golden", "--file", missing})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("a missing golden set produced a passing run")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("the error does not name the path it looked at: %v", err)
	}
	if got := clicmd.CategoryOf(err); got != clicmd.CategoryUsage {
		t.Errorf("a missing golden set reports as %q, want %q", got, clicmd.CategoryUsage)
	}
	// And it is not reported as a measurement. "Recall 0" over a file that does not exist is the
	// same lie as "orphans: none found" over a corpus nobody scanned.
	if got := clicmd.CategoryOf(err); got == clicmd.CategoryAssertion {
		t.Error("a missing golden set reports as an assertion, which claims a measurement")
	}
}

// TestEvalCmd_CorpusRunNeverDials is the hermetic guarantee, asserted as a call count rather than
// inferred from the job's name: with --corpus, the connection function must not be reached at all.
func TestEvalCmd_CorpusRunNeverDials(t *testing.T) {
	dials := 0
	connect := func(context.Context) (clicmd.Searcher, func(), error) {
		dials++
		return stubSearcher{}, func() {}, nil
	}

	// No configuration at all, which is what a CI runner has. Setting DefaultCollection here is what
	// hid a real defect: a corpus run took the configured collection, and with none configured every
	// question failed retrieval.Query.Validate while this test stayed green.
	cmd := newEvalCmd(&config.Config{},
		evalModes{golden: eval.GoldenGate, isolation: eval.IsolationGate}, connect)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{
		"--golden",
		"--file", "../../testdata/eval/hermetic/golden-set.yaml",
		"--corpus", "../../testdata/eval/hermetic/corpus.yaml",
		"--min-recall", "0.75",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("the hermetic fixture did not pass its own --min-recall: %v\n%s", err, out.String())
	}
	if dials != 0 {
		t.Errorf("a --corpus run opened %d connection(s); the hermetic guarantee has to be a "+
			"property of the code path, not of the runner's environment", dials)
	}
}

// TestEvalCmd_CorpusRun_ThresholdDecidesTheExitCode is the end-to-end T10 contract on the fixture:
// the same command passes below its recall and fails above it, and the failure is an assertion —
// exit 4, "the gate ran and said no" — not a backend failure.
func TestEvalCmd_CorpusRun_ThresholdDecidesTheExitCode(t *testing.T) {
	cases := map[string]struct {
		minRecall string
		wantPass  bool
	}{
		"at the measured recall":    {"0.75", true},
		"below the measured recall": {"0.5", true},
		"above the measured recall": {"0.8", false},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cmd := newEvalCmd(&config.Config{},
				evalModes{golden: eval.GoldenGate, isolation: eval.IsolationGate}, stubConnect)
			var out, errOut bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&errOut)
			cmd.SetArgs([]string{
				"--golden",
				"--file", "../../testdata/eval/hermetic/golden-set.yaml",
				"--corpus", "../../testdata/eval/hermetic/corpus.yaml",
				"--min-recall", tc.minRecall,
			})

			err := cmd.Execute()
			if tc.wantPass {
				if err != nil {
					t.Fatalf("--min-recall %s failed: %v", tc.minRecall, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("--min-recall %s passed on a fixture that measures 0.75", tc.minRecall)
			}
			if got := clicmd.CategoryOf(err); got != clicmd.CategoryAssertion {
				t.Errorf("recall below the threshold reports as %q, want %q — a scheduler cannot "+
					"tell a regression from a broken backend otherwise", got, clicmd.CategoryAssertion)
			}
			// The report is printed before the verdict is returned: the numbers are the only thing
			// anyone can act on.
			if !strings.Contains(out.String(), "Recall@5") {
				t.Errorf("a failing gate printed no report:\n%s", out.String())
			}
		})
	}
}

// TestEvalCmd_IsolationNeverReportsAPendingHarness is the CLI-level half of the guard
// internal/eval's TestIsolationGate_NeverReportsAPendingHarness holds on the package.
//
// scripts/ci/eval-gate.sh greps the command's combined output for eval.ErrNotImplemented's message
// and exits 0 when it finds it. So it is not enough that the gate returns a different error — the
// string must not reach stdout or stderr on any isolation run at all, or the security gate goes
// green over a suite that proved nothing.
func TestEvalCmd_IsolationNeverReportsAPendingHarness(t *testing.T) {
	cmd := newEvalCmd(&config.Config{}, evalModes{golden: eval.GoldenGate, isolation: eval.IsolationGate},
		stubConnect)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--isolation"})

	err := cmd.Execute()
	printed := out.String() + errOut.String()
	if err != nil {
		printed += err.Error()
	}
	if strings.Contains(printed, eval.ErrNotImplemented.Error()) {
		t.Errorf("an isolation run printed %q, which scripts/ci/eval-gate.sh reads as a pending "+
			"harness and exits 0 on:\n%s", eval.ErrNotImplemented, printed)
	}
	// And it really ran: the summary names the cases, so this is not passing because the command
	// failed before reaching the suite.
	if !strings.Contains(out.String(), "cross-tenant") {
		t.Errorf("the isolation run printed no case list:\n%s", out.String())
	}
}

// TestEvalCmd_JSON_CarriesBothTheReportAndTheVerdict covers the one envelope in this CLI that has
// `data` and `error` at once, and the reason it does: a script gating on a recall number needs the
// number and the verdict from the same document.
func TestEvalCmd_JSON_CarriesBothTheReportAndTheVerdict(t *testing.T) {
	r := &recordingModes{outcome: eval.Outcome{Passed: false, Score: score(0.60), Summary: "recall 0.60"}}

	out, err := runEval(t, r, "--golden", "--json")
	if err == nil {
		t.Fatal("a gate that did not pass exited clean")
	}

	var envelope struct {
		OK    bool              `json:"ok"`
		Data  evalJSON          `json:"data"`
		Error *clicmd.ErrorInfo `json:"error"`
	}
	if uerr := json.Unmarshal([]byte(out), &envelope); uerr != nil {
		t.Fatalf("stdout does not parse as JSON on its own: %v\n%s", uerr, out)
	}
	if envelope.OK {
		t.Errorf("the envelope reports ok=true for a gate that failed: %s", out)
	}
	if envelope.Error == nil || envelope.Error.Category != string(clicmd.CategoryAssertion) {
		t.Errorf("the envelope does not carry the assertion category: %s", out)
	}
	if envelope.Data.Score == nil || *envelope.Data.Score != 0.60 {
		t.Errorf("the envelope lost the score the gate measured: %s", out)
	}
	if envelope.Data.Summary != "recall 0.60" {
		t.Errorf("the envelope lost the report: %s", out)
	}
}

// TestEvalCmd_JSON_ScoreIsAbsentNotZero is the distinction a gate script would read wrong, asserted
// on the bytes the command actually writes.
//
// `"score": null` means the gate has no number by design — S11's isolation suite, which must never
// be reportable as a percentage. `"score": 0` means it measured and got zero, which for the golden
// gate is total retrieval failure. A consumer that saw one where the other was meant would either
// page somebody over a healthy isolation run or ignore a recall of zero.
//
// Both directions, because either alone is satisfied by an encoder that always emits the other. It
// replaces a version of this claim that lived in internal/eval and could not fail at run time —
// see the note there.
func TestEvalCmd_JSON_ScoreIsAbsentNotZero(t *testing.T) {
	zero := 0.0

	cases := map[string]struct {
		mode         string
		outcome      eval.Outcome
		want, absent string
	}{
		"isolation has no number":        {"--isolation", eval.Outcome{Passed: true, Summary: "8 cases, all passed"}, `"score":null`, `"score":0`},
		"golden measured and got zero":   {"--golden", eval.Outcome{Passed: false, Score: &zero, Summary: "recall 0.0000"}, `"score":0`, `"score":null`},
		"golden measured something else": {"--golden", eval.Outcome{Passed: true, Score: score(0.75), Summary: "recall 0.7500"}, `"score":0.75`, `"score":null`},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := &recordingModes{outcome: tc.outcome}
			out, _ := runEval(t, r, tc.mode, "--json")

			if !strings.Contains(out, tc.want) {
				t.Errorf("the envelope does not carry %s:\n%s", tc.want, out)
			}
			// The absent half is what makes this a distinction rather than a spelling check: an
			// encoder emitting `null` for everything satisfies the first assertion alone.
			if strings.Contains(out, tc.absent) {
				t.Errorf("the envelope carries %s, which a consumer reads as the other case:\n%s", tc.absent, out)
			}
		})
	}
}

// TestEvalCmd_RunsAgainstTheConfiguredScope keeps what Options carries honest. A gate run against a
// collection nobody ingested measures an empty index and reports it as a real result.
//
// Field by field rather than one struct comparison: Options now carries a Searcher, which is an
// interface with no useful equality, and a `!=` over the whole struct would compare it too.
func TestEvalCmd_RunsAgainstTheConfiguredScope(t *testing.T) {
	r := &recordingModes{outcome: eval.Outcome{Passed: true}}
	if _, err := runEval(t, r, "--golden", "--min-recall", "0.8"); err != nil {
		t.Fatalf("eval --golden: %v", err)
	}
	if len(r.scopes) != 1 {
		t.Fatalf("the gate ran %d time(s), want 1", len(r.scopes))
	}

	got := r.scopes[0]
	if got.Collection != "interno" || got.TenantID != defaultTenantID {
		t.Errorf("the gate ran against %s/%s, want interno/%s", got.Collection, got.TenantID, defaultTenantID)
	}
	// The literal, not the constant. `got.GoldenSetPath != DefaultGoldenSetPath` compares the
	// constant against itself: editing it moves both sides and the assertion never fails. The path
	// is a documented default (README, `eval --help`), so it is worth spelling out here.
	if got.GoldenSetPath != "docs/eval/golden-set.yaml" {
		t.Errorf("GoldenSetPath = %q, want docs/eval/golden-set.yaml", got.GoldenSetPath)
	}
	if DefaultGoldenSetPath != "docs/eval/golden-set.yaml" {
		t.Errorf("DefaultGoldenSetPath = %q; the README and `eval --help` both name "+
			"docs/eval/golden-set.yaml", DefaultGoldenSetPath)
	}
	if got.MinRecall != 0.8 {
		t.Errorf("MinRecall = %v, want the 0.8 the flag carried", got.MinRecall)
	}
	// The searcher has to be there, or the gate would refuse before measuring anything
	// (eval.ErrNoSearcher).
	if got.Searcher == nil {
		t.Error("the golden gate was handed no searcher")
	}
}

// TestEvalCmd_IsolationTakesNoSearcher is the hermetic half of the isolation job: `eval
// --isolation` must not open a connection, so the CI job that runs it stays what its name claims
// while S11 is still pending.
func TestEvalCmd_IsolationTakesNoSearcher(t *testing.T) {
	dials := 0
	connect := func(context.Context) (clicmd.Searcher, func(), error) {
		dials++
		return stubSearcher{}, func() {}, nil
	}

	r := &recordingModes{outcome: eval.Outcome{Passed: true}}
	cmd := newEvalCmd(&config.Config{DefaultCollection: "interno"}, r.modes(), connect)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--isolation"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("eval --isolation: %v", err)
	}
	if dials != 0 {
		t.Errorf("eval --isolation opened %d connection(s)", dials)
	}
	if len(r.scopes) != 1 || r.scopes[0].Searcher != nil {
		t.Errorf("the isolation gate was handed a searcher: %+v", r.scopes)
	}
}

// evalGateScript and ciWorkflow are the two files the CI gate is made of.
const (
	evalGateScript = "../../scripts/ci/eval-gate.sh"
	ciWorkflow     = "../../.github/workflows/ci.yml"
)

// TestCIWorkflow_PendingSentinelMatchesTheErrorItLooksFor is the guard on a string that lives in a
// shell script and describes a Go value.
//
// scripts/ci/eval-gate.sh decides whether a failing gate is a pending harness or a real failure by
// grepping for eval.ErrNotImplemented's message. Nothing at run time relates the two: change the
// error and the script stops recognising it, both jobs go red, and the first reading of that will
// be "the gate broke" — which is the opposite of what happened.
//
// The comparison is equality, and the length floor below is not belt-and-braces. This test used to
// ask whether the error *contains* the sentinel, which is the weaker half of a contract whose other
// half — the comment in the script — already said "must equal". Containment passes for the empty
// string against any error, and `grep -qF ""` matches every line, so a sentinel emptied by an edit
// would leave this green while the gate swallowed compile errors, missing modules and panics as
// "harness pending" and exited 0. A one-character sentinel is the same defect with one more
// keystroke. Equality closes the first door and the floor closes the second, which is the one
// equality cannot: it holds just as well between two useless values.
func TestCIWorkflow_PendingSentinelMatchesTheErrorItLooksFor(t *testing.T) {
	script := readRepoFile(t, evalGateScript)

	const assignment = `not_implemented="`
	i := strings.Index(script, assignment)
	if i < 0 {
		t.Fatalf("%s no longer assigns not_implemented, so this test is checking nothing", evalGateScript)
	}
	rest := script[i+len(assignment):]
	sentinel := rest[:strings.Index(rest, `"`)]

	// Long enough to identify something. Nothing computes this bound; it is a floor under "a string
	// short enough to appear in an unrelated failure", and the failures it has to stay out of are
	// Go toolchain messages.
	const minSentinel = 8
	if len(sentinel) < minSentinel {
		t.Fatalf("the CI gate greps for %q, %d character(s) — a sentinel that short matches failures "+
			"it knows nothing about, and the gate would report them as a pending harness and exit 0",
			sentinel, len(sentinel))
	}
	if sentinel != eval.ErrNotImplemented.Error() {
		t.Errorf("the CI gate greps for %q; eval.ErrNotImplemented is %q. They have to be the same "+
			"string — the script's own comment says so, and a mismatch reports either a pending "+
			"harness as a broken gate or a broken gate as pending", sentinel, eval.ErrNotImplemented)
	}
}

// TestCIWorkflow_RunsBothGates is the regression guard for the change this file is part of. Both
// jobs spent this project's whole life switched off behind `if: false` with a TODO, which is a
// check that reports nothing on every push including the one that breaks it.
func TestCIWorkflow_RunsBothGates(t *testing.T) {
	workflow := withoutYAMLComments(readRepoFile(t, ciWorkflow))

	for _, want := range []string{"eval-gate.sh golden", "eval-gate.sh isolation"} {
		if !strings.Contains(workflow, want) {
			t.Errorf("the workflow never runs %q, so the gate exists and nothing calls it", want)
		}
	}
	// The absent half, and the one that matters: a job can name the script and still be switched
	// off above it. `continue-on-error` is refused for the same reason — it would make a real
	// failure of a real harness green, which is the pending path swallowing the failing one.
	for _, unwanted := range []string{"if: false", "continue-on-error"} {
		if strings.Contains(workflow, unwanted) {
			t.Errorf("the workflow carries %q; a gate that cannot fail is a gate that is off", unwanted)
		}
	}
}

// withoutYAMLComments drops every `#` comment, because the assertions below are about what the
// workflow does and a comment is allowed to describe what it stopped doing. Written after the
// comment explaining why these jobs are no longer switched off failed the test that checks they are
// not switched off.
func withoutYAMLComments(yaml string) string {
	var b strings.Builder
	for _, line := range strings.Split(yaml, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

func readRepoFile(t *testing.T, path string) string {
	t.Helper()

	b, err := os.ReadFile(path) // #nosec G304 -- a literal path inside the repository
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

// TestCIGateScript_NoModeIsAllowedToReportPending is the other half of the sentinel contract, and
// the half both harness stories changed.
//
// The script exits 0 with a warning when a mode on its pending list answers with
// eval.ErrNotImplemented. That is the right reading for a story nobody has built and the wrong one
// for both gates this build ships: S10 wired the golden harness (cmd/cli/eval.go) and S11 the
// isolation suite (internal/eval/eval.go). A gate on that list, broken badly enough to print the
// sentinel, would go green with nobody told.
//
// The assertion is that the list is empty, not that two particular names are off it, and the
// difference is what makes it hold for a gate nobody has written yet: a third mode put on the list
// before its harness exists fails here, which is the reminder that the list is what turns a job's
// failures into warnings.
func TestCIGateScript_NoModeIsAllowedToReportPending(t *testing.T) {
	script := readRepoFile(t, evalGateScript)

	const assignment = `pending_modes="`
	i := strings.Index(script, assignment)
	if i < 0 {
		t.Fatalf("%s no longer lists pending_modes, so nothing decides which gates may answer "+
			"\"pending\" and the sentinel branch is reachable for all of them", evalGateScript)
	}
	rest := script[i+len(assignment):]
	modes := strings.Fields(rest[:strings.Index(rest, `"`)])

	if len(modes) != 0 {
		t.Errorf("pending_modes is %v; every gate this build ships has a harness, so a mode on that "+
			"list is one whose failures exit 0 with a warning instead of failing CI", modes)
	}
}

// TestEvalCmd_Isolation_ExitCodeFollowsTheVerdict is S11's exit-code contract: the process exit is
// the release gate, and nothing but the suite's verdict moves it.
func TestEvalCmd_Isolation_ExitCodeFollowsTheVerdict(t *testing.T) {
	cases := map[string]struct {
		outcome  eval.Outcome
		wantFail bool
	}{
		"suite passed": {eval.Outcome{Mode: "isolation", Passed: true, Summary: "**PASS**"}, false},
		"suite failed": {eval.Outcome{Mode: "isolation", Passed: false, Summary: "**FAIL**"}, true},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := &recordingModes{outcome: tc.outcome}
			_, err := runEval(t, r, "--isolation")

			if !tc.wantFail {
				if err != nil {
					t.Fatalf("a passing suite exited non-zero: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("a failing isolation suite exited clean; this is the release gate")
			}
			if got := clicmd.CategoryOf(err); got != clicmd.CategoryAssertion {
				t.Errorf("a failing suite reports as %q, want %q — it ran and answered no, which is "+
					"not the same event as a broken backend", got, clicmd.CategoryAssertion)
			}
		})
	}
}

// The isolation gate's `"score": null` was asserted again here, on its own. It is a strict subset of
// TestEvalCmd_JSON_ScoreIsAbsentNotZero above, which asserts the same bytes for isolation *and* the
// half that makes it a distinction — that `"score": 0` does not appear. One claim, one test.
