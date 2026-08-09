package ingest

import (
	"fmt"
	"strings"
)

// Report is what a run did, note by note. It holds the results rather than only the counts because
// a failed note is useless without its path and its reason — "3 failed" sends an operator back to
// the logs to find out which three.
type Report struct {
	Results []NoteResult
}

// Count returns how many notes ended in state.
func (r Report) Count(state NoteState) int {
	n := 0
	for _, res := range r.Results {
		if res.State == state {
			n++
		}
	}
	return n
}

// Counts returns one entry per state observed in the run. States that did not occur are absent
// rather than present with a zero, so the map reads as "what happened" instead of "what could
// have".
func (r Report) Counts() map[NoteState]int {
	out := map[NoteState]int{}
	for _, res := range r.Results {
		out[res.State]++
	}
	return out
}

// Failed reports whether any note ended StateFailed. This is what decides the process exit code:
// PRD-stories-pipeline §3 S06a requires a run with any failed note to exit non-zero, so a partial
// ingestion cannot be mistaken for a clean one by a cron job reading only the status.
func (r Report) Failed() bool { return r.Count(StateFailed) > 0 }

// ExitCode is Failed() as a process exit code.
func (r Report) ExitCode() int {
	if r.Failed() {
		return 1
	}
	return 0
}

// Failures returns the failed results, in run order, for the end-of-run summary.
func (r Report) Failures() []NoteResult {
	var out []NoteResult
	for _, res := range r.Results {
		if res.State == StateFailed {
			out = append(out, res)
		}
	}
	return out
}

// String renders the per-state counts in a fixed order — the order of the state machine, not map
// order — followed by one line per failure.
func (r Report) String() string {
	counts := r.Counts()
	parts := make([]string, 0, 5)
	for _, s := range []NoteState{
		StateSkipped, StateEmbedded, StateUpsertConfirmed, StatePruned, StateFailed,
	} {
		if n, ok := counts[s]; ok {
			parts = append(parts, fmt.Sprintf("%s=%d", s, n))
		}
	}

	lines := []string{fmt.Sprintf("%d note(s): %s", len(r.Results), strings.Join(parts, " "))}
	for _, f := range r.Failures() {
		lines = append(lines, fmt.Sprintf("  - %s: %v", f.Path, f.Err))
	}
	return strings.Join(lines, "\n")
}
