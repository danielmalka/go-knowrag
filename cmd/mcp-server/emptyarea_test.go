package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/danielmalka/go-knowrag/internal/retrieval"
)

// deadFilterHeader is the first line of the dead-filter message, and the marker these tests use to
// say whether the probe's verdict reached the caller.
const deadFilterHeader = "EMPTY BECAUSE OF THE AREA FILTER"

// TestSearchKnowledge_AreaMatchesNothing_SaysTheFilterIsWhy is D-28. MCP_AREAS is maintained by
// hand on a host that never opens a vault, so an area can be advertised here and exist nowhere in
// the index — and the search then answers exactly like a subject nobody ever wrote about.
func TestSearchKnowledge_AreaMatchesNothing_SaysTheFilterIsWhy(t *testing.T) {
	logged := captureLogs(t)
	fake := &fakeSearcher{filterAlive: false}
	cs := connect(t, testConfig(), fake)

	res := callRaw(t, cs, `{"query":"kubernetes ingress","area":"infra"}`)
	if res.IsError {
		t.Fatalf("a search that succeeded and found nothing was reported as a failure: %s", resultText(t, res))
	}
	msg := resultText(t, res)

	// The four things the message has to carry: that the filter caused it, that emptiness proves
	// nothing about the subject, what the caller should do instead, and which variable an operator
	// has to fix.
	for _, want := range []string{
		deadFilterHeader,
		"not evidence",
		"without the area filter",
		"MCP_AREAS",
		`"infra"`,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the dead-filter message omits %q:\n%s", want, msg)
		}
	}
	// The plain empty-result text may not ride along: those are the two readings the message exists
	// to separate, so they may not share a response.
	if strings.Contains(msg, noResults) {
		t.Errorf("the dead-filter message contains the ordinary empty-result text:\n%s", msg)
	}
	// It must claim only what the probe proves. The probe carries the same archived/private guards
	// the search does, so an area whose notes are all archived answers identically to one that was
	// never ingested, and sending an operator to re-run an ingestion that already ran is the failure
	// this wording avoids.
	if strings.Contains(msg, "not indexed") {
		t.Errorf("the message claims the area is not indexed, which the probe does not prove:\n%s", msg)
	}
	// The operator line hedges the same way the body does. It is the line an operator acts on, so
	// an unqualified "nothing in the index" there is the claim that costs an ingestion re-run.
	if !strings.Contains(msg, "nothing visible in the index") {
		t.Errorf("the operator line claims more than the probe proves:\n%s", msg)
	}

	// With no other facet in the call there is nothing to narrow away, so everything the message
	// rests on has to reach the probe unchanged: the area it names, the tenant scope, and the
	// visibility rules that decide what "matches nothing" counts. Text and TopK are not among them —
	// FilterMatchesAnything asks for no ranked answer and never reads either.
	if fake.probeCalls() != 1 {
		t.Fatalf("the probe ran %d time(s), want exactly 1", fake.probeCalls())
	}
	probe, searched := fake.lastProbe(), fake.lastQuery()
	if probe.Area != searched.Area || probe.Collection != searched.Collection ||
		probe.TenantID != searched.TenantID {
		t.Errorf("the probe %+v does not carry the search's area and scope %+v", probe, searched)
	}
	if probe.IncludeArchived != searched.IncludeArchived || probe.IncludePrivate != searched.IncludePrivate {
		t.Errorf("the probe %+v does not run under the search's visibility rules %+v", probe, searched)
	}

	if !strings.Contains(logged.String(), `"area":"infra"`) {
		t.Errorf("no log record names the area that matches nothing:\n%s", logged.String())
	}
	// And the log record hedges too, for the same reason the operator line does: it is read by the
	// same person, looking for the same missing ingestion.
	if !strings.Contains(logged.String(), "matches nothing visible in the index") {
		t.Errorf("the log record claims more than the probe proves:\n%s", logged.String())
	}
}

// TestSearchKnowledge_DeadPairOfFilters_KeepsThePlainMessage is the case that decides what the
// probe is allowed to blame. `infra` is full of notes and holds no `lesson`: the filter the
// search carried is genuinely dead, and the area in it is not. Answering with the dead-area message
// would tell the model to search again without a filter that was never the problem, and tell the
// operator that a correct MCP_AREAS entry is stale.
func TestSearchKnowledge_DeadPairOfFilters_KeepsThePlainMessage(t *testing.T) {
	fake := &fakeSearcher{
		filterAliveFn: func(q retrieval.Query) bool { return q.Area == "infra" && q.Type == "" },
	}
	cs := connect(t, testConfig(), fake)

	res := callRaw(t, cs, `{"query":"kubernetes ingress","area":"infra","type":"lesson"}`)
	if res.IsError {
		t.Fatalf("an empty search was reported as a failure: %s", resultText(t, res))
	}
	if msg := resultText(t, res); msg != noResults {
		t.Errorf("a live area emptied by a second filter was reported as a dead area:\n%s", msg)
	}

	// The narrowing itself, field by field: the facets the message does not talk about are gone,
	// and everything the message *does* rest on — the area, the scope, the visibility defaults —
	// is exactly what the search ran under.
	probe, searched := fake.lastProbe(), fake.lastQuery()
	if probe.Type != "" || probe.Vault != "" || len(probe.Tags) != 0 {
		t.Errorf("the probe still carries facets the message says nothing about: %+v", probe)
	}
	if probe.Area != searched.Area || probe.Collection != searched.Collection ||
		probe.TenantID != searched.TenantID {
		t.Errorf("the probe %+v does not carry the search's area and scope %+v", probe, searched)
	}
	if probe.IncludeArchived != searched.IncludeArchived || probe.IncludePrivate != searched.IncludePrivate {
		t.Errorf("the probe %+v does not run under the search's visibility rules %+v", probe, searched)
	}
}

// TestSearchKnowledge_DeadAreaWithAType_StillBlamesTheArea is the other half of that rule, and the
// half an over-correction erases: a `type` in the call buys the area no alibi. Here `infra` matches
// nothing at all, so the area is the whole reason the pair is dead and the message naming it is the
// true one. A handler guard narrowed to `in.Type == ""`, or an early return inside explainEmptyArea
// whenever a type was asked for, keeps every other test in this file green and drops the diagnosis
// in exactly the case where a stale MCP_AREAS entry is being used.
func TestSearchKnowledge_DeadAreaWithAType_StillBlamesTheArea(t *testing.T) {
	fake := &fakeSearcher{filterAlive: false}
	cs := connect(t, testConfig(), fake)

	res := callRaw(t, cs, `{"query":"kubernetes ingress","area":"infra","type":"lesson"}`)
	if res.IsError {
		t.Fatalf("an empty search was reported as a failure: %s", resultText(t, res))
	}
	msg := resultText(t, res)
	if !strings.Contains(msg, deadFilterHeader) {
		t.Errorf("a dead area was let off because the call also carried a type:\n%s", msg)
	}
	if !strings.Contains(msg, `"infra"`) {
		t.Errorf("the dead-filter message does not name the area it blames:\n%s", msg)
	}
	// And the verdict rests on the narrower question, not on the pair: the type is what the probe
	// dropped, the area is what it kept.
	if probe := fake.lastProbe(); probe.Type != "" || probe.Area != "infra" {
		t.Errorf("the probe did not ask about the area alone: %+v", probe)
	}
}

// TestSearchKnowledge_HungProbe_SpendsTheSearchDeadline pins the probe to the caller's budget. A
// search that answered inside searchDeadline followed by a probe on a fresh context is a call that
// hangs for as long as Qdrant feels like, after the part with a deadline already finished — and the
// caller waits it out for a diagnostic it never asked for.
//
// The fake tells the two apart by which error it returns, so this fails rather than merely runs
// slowly when the probe is handed anything but ctx.
func TestSearchKnowledge_HungProbe_SpendsTheSearchDeadline(t *testing.T) {
	restore := searchDeadline
	searchDeadline = 200 * time.Millisecond
	t.Cleanup(func() { searchDeadline = restore })

	logged := captureLogs(t)
	cs := connect(t, testConfig(), &fakeSearcher{probeHangs: true})

	res := callRaw(t, cs, `{"query":"kubernetes ingress","area":"infra"}`)
	if res.IsError {
		t.Fatalf("a probe that timed out turned a successful search into an error: %s", resultText(t, res))
	}
	if msg := resultText(t, res); msg != noResults {
		t.Errorf("a probe that never answered did not fall back to the ordinary empty answer:\n%s", msg)
	}
	if strings.Contains(logged.String(), "never cancelled") {
		t.Errorf("the probe ran on a context nothing bounds:\n%s", logged.String())
	}
	if !strings.Contains(logged.String(), "probe failed") {
		t.Errorf("the probe timeout was swallowed instead of logged:\n%s", logged.String())
	}
}

// TestSearchKnowledge_GenuinelyEmpty_KeepsThePlainMessage is the half that decides whether the
// probe is worth having. An area that is alive and simply holds nothing on the subject must keep
// the ordinary answer: telling a model its filter is broken every time a search comes back empty
// would teach it to distrust every empty result, which is D-21's silent failure pointed the other
// way.
func TestSearchKnowledge_GenuinelyEmpty_KeepsThePlainMessage(t *testing.T) {
	fake := &fakeSearcher{filterAlive: true}
	cs := connect(t, testConfig(), fake)

	res := callRaw(t, cs, `{"query":"something nobody wrote about","area":"infra"}`)
	if res.IsError {
		t.Fatalf("an empty search was reported as a failure: %s", resultText(t, res))
	}
	if msg := resultText(t, res); msg != noResults {
		t.Errorf("an area that matches points did not get the ordinary empty answer:\n%s", msg)
	}
	// Non-vacuity: the probe really did run, so the assertion above is about its answer and not
	// about a branch that was never entered.
	if fake.probeCalls() != 1 {
		t.Errorf("the probe ran %d time(s), want exactly 1", fake.probeCalls())
	}
}

// TestSearchKnowledge_ProbeFails_KeepsThePlainMessageAndStaysASuccess is the rule that a failed
// diagnosis may not damage what it was diagnosing: the search already succeeded, and losing that
// answer because the follow-up call did not come back would be a worse outcome than the ambiguity
// the probe exists to remove.
func TestSearchKnowledge_ProbeFails_KeepsThePlainMessageAndStaysASuccess(t *testing.T) {
	cfg := testConfig()
	logged := captureLogs(t)
	fake := &fakeSearcher{probeErr: errors.New("qdrant: unauthorized with api key " + cfg.QdrantAPIKey)}
	cs := connect(t, cfg, fake)

	res := callRaw(t, cs, `{"query":"kubernetes ingress","area":"infra"}`)
	if res.IsError {
		t.Fatalf("a failed probe turned a successful search into an error: %s", resultText(t, res))
	}
	msg := resultText(t, res)
	if msg != noResults {
		t.Errorf("a failed probe did not fall back to the ordinary empty answer:\n%s", msg)
	}
	if strings.Contains(msg, deadFilterHeader) {
		t.Errorf("a probe that never answered was reported as a dead filter:\n%s", msg)
	}
	// The probe error reaches the log, because that is where the operator is — and it goes through
	// the same scrubbing the error path uses, since an error message is the classic place a
	// credential escapes to.
	if !strings.Contains(logged.String(), "probe failed") {
		t.Errorf("the probe failure was swallowed instead of logged:\n%s", logged.String())
	}
	if strings.Contains(logged.String(), cfg.QdrantAPIKey) {
		t.Errorf("the probe failure log leaks the credential:\n%s", logged.String())
	}
}

// TestSearchKnowledge_ProbeRunsOnlyWhenItCanExplainSomething pins the cost. The probe is a second
// round trip to Qdrant, and it is worth it exactly when there is an empty result and an area filter
// to blame it on; anywhere else it is latency paid for an answer nobody reads.
func TestSearchKnowledge_ProbeRunsOnlyWhenItCanExplainSomething(t *testing.T) {
	for name, tc := range map[string]struct {
		args string
		fake *fakeSearcher
	}{
		"empty result, no area filter": {`{"query":"anything"}`, &fakeSearcher{}},
		"results found under an area":  {`{"query":"anything","area":"infra"}`, &fakeSearcher{results: sampleResults()}},
	} {
		t.Run(name, func(t *testing.T) {
			cs := connect(t, testConfig(), tc.fake)

			res := callRaw(t, cs, tc.args)
			if res.IsError {
				t.Fatalf("the call failed: %s", resultText(t, res))
			}
			if tc.fake.probeCalls() != 0 {
				t.Errorf("the probe ran %d time(s) on a call it cannot explain anything about",
					tc.fake.probeCalls())
			}
			if strings.Contains(resultText(t, res), deadFilterHeader) {
				t.Errorf("the dead-filter message reached a caller it does not apply to:\n%s", resultText(t, res))
			}
		})
	}
}
