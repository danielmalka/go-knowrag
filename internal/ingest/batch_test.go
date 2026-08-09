package ingest

import (
	"errors"
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/schema"
	"github.com/danielmalka/go-knowrag/internal/vault"
)

// TestRunBatch_DuplicateUID_FailsBatchNamingBothPaths: two notes with one uid produce one set of
// point IDs, so processing them would overwrite the first note with the second — corrupting data
// rather than failing. The whole batch aborts before a single write, and the error names both paths
// so the operator knows which two files to look at.
func TestRunBatch_DuplicateUID_FailsBatchNamingBothPaths(t *testing.T) {
	h := newHarness()
	a := testNote(t, "research/curadoria/a.md", 2)
	b := testNote(t, "research/curadoria/b.md", 2)
	b.UID = a.UID
	b.Path = "research/curadoria/b.md"

	_, err := RunBatch(t.Context(), h.deps, []vault.Note{a, b})
	if err == nil {
		t.Fatal("RunBatch accepted two notes sharing a uid")
	}
	var dup *vault.DuplicateUIDError
	if !errors.As(err, &dup) {
		t.Fatalf("error %v is not a *vault.DuplicateUIDError", err)
	}
	for _, path := range []string{a.Path, b.Path} {
		if !strings.Contains(err.Error(), path) {
			t.Errorf("error %q does not name %q", err, path)
		}
	}
	if len(h.journal.calls) != 0 {
		t.Errorf("the store/embedder were called %v; the batch must abort before any processing",
			h.journal.calls)
	}
}

// TestOrchestrate_CrossVaultDuplicateUID_FailsBeforeAnyProcessing is the case ScanVault structurally
// cannot see: one uid in MalkaLife and the same uid in MalkaWay. The point ID does not include
// `vault`, so that repeat collides in Qdrant exactly like an in-vault duplicate, and this
// orchestration is the only place that holds both scans at once.
func TestOrchestrate_CrossVaultDuplicateUID_FailsBeforeAnyProcessing(t *testing.T) {
	h := newHarness()

	life := testNote(t, "research/curadoria/nota.md", 2)
	way := testNote(t, "arcanto/nota.md", 2)
	way.Vault = schema.VaultMalkaWay()
	way.Area = schema.AreaArcanto()
	way.UID = life.UID

	_, err := Orchestrate(t.Context(), h.deps,
		vault.ScanResult{Vault: schema.VaultMalkaLife(), Notes: []vault.Note{life}},
		vault.ScanResult{Vault: schema.VaultMalkaWay(), Notes: []vault.Note{way}},
	)
	if err == nil {
		t.Fatal("Orchestrate accepted the same uid in both vaults")
	}
	for _, want := range []string{life.Path, way.Path, schema.VaultMalkaLife().String(), schema.VaultMalkaWay().String()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if len(h.journal.calls) != 0 {
		t.Errorf("the store/embedder were called %v before the cross-vault check finished", h.journal.calls)
	}
}

// TestOrchestrate_UniqueUIDs_PassThrough: the check is silent when there is nothing to report, and
// both vaults' notes reach the batch.
func TestOrchestrate_UniqueUIDs_PassThrough(t *testing.T) {
	h := newHarness()

	life := testNote(t, "research/curadoria/nota.md", 2)
	way := testNote(t, "arcanto/outra.md", 2)
	way.Vault = schema.VaultMalkaWay()
	way.Area = schema.AreaArcanto()

	report, err := Orchestrate(t.Context(), h.deps,
		vault.ScanResult{Vault: schema.VaultMalkaLife(), Notes: []vault.Note{life}},
		vault.ScanResult{Vault: schema.VaultMalkaWay(), Notes: []vault.Note{way}},
	)
	if err != nil {
		t.Fatalf("Orchestrate: %v", err)
	}
	if got, want := report.Count(StatePruned), 2; got != want {
		t.Errorf("%d note(s) reached %s, want %d (%s)", got, StatePruned, want, report)
	}
}

// TestRunBatch_ReingestNoChange is the headline acceptance criterion: running twice over an
// unchanged vault writes nothing the second time, proven by instrumented counters rather than by
// comparing point counts (which would also be identical after a full, wasteful rewrite).
func TestRunBatch_ReingestNoChange(t *testing.T) {
	h := newHarness()
	notes := []vault.Note{
		testNote(t, "research/curadoria/a.md", 3),
		testNote(t, "research/curadoria/b.md", 1),
		testNote(t, "personal/c.md", 5),
	}

	first, err := RunBatch(t.Context(), h.deps, notes)
	if err != nil {
		t.Fatalf("first RunBatch: %v", err)
	}
	if got, want := first.Count(StatePruned), len(notes); got != want {
		t.Fatalf("first run: %d note(s) pruned, want %d (%s)", got, want, first)
	}

	before := map[string][]int{}
	for _, n := range notes {
		before[n.Path] = h.store.indices(testTenant, n.UID)
	}
	h.reset()

	second, err := RunBatch(t.Context(), h.deps, notes)
	if err != nil {
		t.Fatalf("second RunBatch: %v", err)
	}
	if got, want := second.Count(StateSkipped), len(notes); got != want {
		t.Fatalf("second run: %d note(s) skipped, want %d (%s)", got, want, second)
	}
	if h.embed.calls != 0 {
		t.Errorf("the second run made %d embedder call(s) over an unchanged vault", h.embed.calls)
	}
	if len(h.store.upserts) != 0 || len(h.store.deletes) != 0 {
		t.Errorf("the second run made %d upsert(s) and %d delete(s) over an unchanged vault",
			len(h.store.upserts), len(h.store.deletes))
	}
	for _, n := range notes {
		if got, want := h.store.indices(testTenant, n.UID), before[n.Path]; len(got) != len(want) {
			t.Errorf("%s: point set changed from %v to %v", n.Path, want, got)
		}
	}
}

// TestRunBatch_FailureIsolation: one bad note does not take the other 729 with it, every note is
// accounted for in the report, and the run signals non-zero.
func TestRunBatch_FailureIsolation(t *testing.T) {
	h := newHarness()
	notes := []vault.Note{
		testNote(t, "research/curadoria/a.md", 2),
		testNote(t, "research/curadoria/b.md", 2),
		testNote(t, "research/curadoria/c.md", 2),
	}

	// The second note's upsert fails; the others must be untouched by it.
	h.store.upsertHook = func(call int, points []Point) (int, UpsertOutcome, error) {
		if call == 2 {
			return 0, UpsertFailed, errors.New("qdrant refused this batch")
		}
		return len(points), UpsertConfirmed, nil
	}

	report, err := RunBatch(t.Context(), h.deps, notes)
	if err != nil {
		t.Fatalf("RunBatch returned a batch-level error for a per-note failure: %v", err)
	}
	if got, want := len(report.Results), len(notes); got != want {
		t.Fatalf("report covers %d note(s), want %d", got, want)
	}
	if got, want := report.Count(StatePruned), 2; got != want {
		t.Errorf("%d note(s) completed, want %d (%s)", got, want, report)
	}
	if got, want := report.Count(StateFailed), 1; got != want {
		t.Errorf("%d note(s) failed, want %d (%s)", got, want, report)
	}
	if !report.Failed() || report.ExitCode() == 0 {
		t.Error("the run exits zero with a failed note in it")
	}

	failures := report.Failures()
	if len(failures) != 1 {
		t.Fatalf("%d failure(s) recorded, want 1", len(failures))
	}
	if failures[0].Path != notes[1].Path {
		t.Errorf("the failure is attributed to %q, want %q", failures[0].Path, notes[1].Path)
	}
	if failures[0].Err == nil || !strings.Contains(failures[0].Err.Error(), "qdrant refused") {
		t.Errorf("the underlying reason was lost: %v", failures[0].Err)
	}
}

// TestRunBatch_EmbedderFailure_IsolatedToNote is T15: the failing note writes nothing at all, and
// its sibling in the same batch completes normally.
func TestRunBatch_EmbedderFailure_IsolatedToNote(t *testing.T) {
	h := newHarness()
	a := noteWithBodies(t, "research/curadoria/a.md", "healthy body")
	b := noteWithBodies(t, "research/curadoria/b.md", "poisoned body")
	h.embed.failOn = "poisoned"

	report, err := RunBatch(t.Context(), h.deps, []vault.Note{a, b})
	if err != nil {
		t.Fatalf("RunBatch: %v", err)
	}
	if got := report.Count(StatePruned); got != 1 {
		t.Errorf("%d note(s) completed, want 1 (%s)", got, report)
	}
	if got := len(h.store.indices(testTenant, a.UID)); got != 1 {
		t.Errorf("the healthy note has %d point(s), want 1", got)
	}
	if got := len(h.store.indices(testTenant, b.UID)); got != 0 {
		t.Errorf("the failing note wrote %d point(s); an embedder failure is upstream of every write", got)
	}
	if !report.Failed() {
		t.Error("the run does not signal failure")
	}
}

// TestRunBatch_StoreUnavailable is T16: with Qdrant unreachable every note fails, the run exits
// non-zero, and — the load-bearing assertion — no prune is attempted anywhere.
func TestRunBatch_StoreUnavailable(t *testing.T) {
	h := newHarness()
	h.deps.UpsertAttempts = 2
	h.store.scrollErr = errors.New("connection refused")

	notes := []vault.Note{
		testNote(t, "research/curadoria/a.md", 2),
		testNote(t, "research/curadoria/b.md", 2),
	}
	report, err := RunBatch(t.Context(), h.deps, notes)
	if err != nil {
		t.Fatalf("RunBatch: %v", err)
	}
	if got, want := report.Count(StateFailed), len(notes); got != want {
		t.Errorf("%d note(s) failed, want all %d (%s)", got, want, report)
	}
	if report.ExitCode() == 0 {
		t.Error("the run exits zero with an unreachable store")
	}
	if got := h.journal.count("delete"); got != 0 {
		t.Errorf("%d prune(s) were attempted against an unreachable store", got)
	}
}

// TestRunBatch_ValidatesDeps: an empty tenant_id would write points invisible to every
// tenant-scoped search, so it fails once, up front, rather than 730 times in silence.
func TestRunBatch_ValidatesDeps(t *testing.T) {
	h := newHarness()
	h.deps.TenantID = ""

	if _, err := RunBatch(t.Context(), h.deps, []vault.Note{testNote(t, "a.md", 1)}); err == nil {
		t.Fatal("RunBatch accepted an empty TenantID")
	}
	if len(h.journal.calls) != 0 {
		t.Errorf("calls %v happened despite the invalid configuration", h.journal.calls)
	}
}

// TestRunBatch_RejectsUnconfirmedHandshake: the embedder config in point_hash must come from S04's
// handshake. A run started without one would write points attesting to a model revision nobody
// confirmed, and they would look integral forever.
func TestRunBatch_RejectsUnconfirmedHandshake(t *testing.T) {
	h := newHarness()
	h.deps.Handshake.ModelRevision = ""

	if _, err := RunBatch(t.Context(), h.deps, []vault.Note{testNote(t, "a.md", 1)}); err == nil {
		t.Fatal("RunBatch accepted a Deps with no confirmed model revision")
	}
}

// TestReport_CountsAndRendering pins the report's shape: one entry per observed state, no
// payload_only entry anywhere, and every failure printed with its path and reason.
func TestReport_CountsAndRendering(t *testing.T) {
	r := Report{Results: []NoteResult{
		{Path: "a.md", State: StateSkipped},
		{Path: "b.md", State: StatePruned},
		{Path: "c.md", State: StateFailed, Err: errors.New("boom")},
		{Path: "d.md", State: StateFailed, Err: errors.New("bang")},
	}}

	counts := r.Counts()
	if got, want := counts[StateFailed], 2; got != want {
		t.Errorf("failed count = %d, want %d", got, want)
	}
	if _, ok := counts[StateEmbedded]; ok {
		t.Error("Counts has an entry for a state that never occurred")
	}
	if len(counts) != 3 {
		t.Errorf("Counts has %d entries, want one per observed state", len(counts))
	}

	rendered := r.String()
	for _, want := range []string{"skipped=1", "pruned=1", "failed=2", "c.md", "boom", "d.md", "bang"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("report %q does not contain %q", rendered, want)
		}
	}
	if strings.Contains(rendered, "payload_only") {
		t.Error("the report mentions payload_only, a state removed with ADR-004")
	}
}
