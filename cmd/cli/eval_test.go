package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/clicmd"
	"github.com/danielmalka/go-knowrag/internal/config"
	"github.com/danielmalka/go-knowrag/internal/eval"
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

func runEval(t *testing.T, r *recordingModes, args ...string) (stdout string, err error) {
	t.Helper()

	cmd := newEvalCmd(&config.Config{DefaultCollection: "interno"}, r.modes())
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
// gate that ran and failed would put "the golden set regressed" and "the golden set does not exist
// yet" on the same exit code.
//
// It asserts the category the failure *has*, not only the one it must not have. This test used to
// say `!= CategoryAssertion`, which is satisfied by every wrong answer as well as the right one: a
// category invented later, or a usage error, would have kept it green while the exit code moved.
func TestEvalCmd_MissingHarness_IsABackendFailure(t *testing.T) {
	for _, mode := range []string{"--golden", "--isolation"} {
		// The real entry points, not a fake: what is under test is the category the command gives
		// the error internal/eval actually returns today.
		cmd := newEvalCmd(&config.Config{}, evalModes{golden: eval.GoldenGate, isolation: eval.IsolationGate})
		var out, errOut bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&errOut)
		cmd.SetArgs([]string{mode})

		err := cmd.Execute()
		if err == nil {
			t.Fatalf("eval %s reported a pass with no harness behind it", mode)
		}
		if !errors.Is(err, eval.ErrNotImplemented) {
			t.Errorf("eval %s lost the sentinel on its way through cobra: %v", mode, err)
		}
		if got := clicmd.CategoryOf(err); got != clicmd.CategoryBackend {
			t.Errorf("eval %s reports a missing harness as %q, want %q. Assertion would claim a "+
				"measurement nobody made; usage would tell the operator to fix a command line that "+
				"is already correct", mode, got, clicmd.CategoryBackend)
		}
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

// TestEvalCmd_JSON_IsolationReportsANullScore is S11's requirement seen from the wire. Its suite has
// no number by design, and a missing key would read to a consumer as a field it forgot to ask for
// rather than as a gate that has nothing to average.
func TestEvalCmd_JSON_IsolationReportsANullScore(t *testing.T) {
	r := &recordingModes{outcome: eval.Outcome{Passed: true, Summary: "8 cases, all passed"}}

	out, err := runEval(t, r, "--isolation", "--json")
	if err != nil {
		t.Fatalf("eval --isolation --json: %v", err)
	}
	if !strings.Contains(out, `"score":null`) {
		t.Errorf("the isolation envelope does not state the absent score:\n%s", out)
	}
}

// TestEvalCmd_RunsAgainstTheConfiguredScope keeps the one thing Options carries honest. A gate run
// against a collection nobody ingested measures an empty index and reports it as a real result.
func TestEvalCmd_RunsAgainstTheConfiguredScope(t *testing.T) {
	r := &recordingModes{outcome: eval.Outcome{Passed: true}}
	if _, err := runEval(t, r, "--golden"); err != nil {
		t.Fatalf("eval --golden: %v", err)
	}
	if len(r.scopes) != 1 {
		t.Fatalf("the gate ran %d time(s), want 1", len(r.scopes))
	}
	if want := (eval.Options{Collection: "interno", TenantID: defaultTenantID}); r.scopes[0] != want {
		t.Errorf("the gate ran against %+v, want %+v", r.scopes[0], want)
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
