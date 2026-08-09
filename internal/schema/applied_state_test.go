package schema

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// These exercise the real file-backed store. Every other test in this package uses the in-memory
// fake, which is right for asserting what Apply calls — but it means the only implementation an
// operator ever runs was, until now, the one nothing tested.

func TestFileAppliedStateStore_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "applied_state.json")
	store := &FileAppliedStateStore{Path: path}
	want := AppliedState{}.With("interno", "bge-m3@rev-a").With("clientes", "bge-m3@rev-b")

	if err := store.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(got.Collections) != len(want.Collections) {
		t.Fatalf("loaded %d record(s), want %d", len(got.Collections), len(want.Collections))
	}
	for _, w := range want.Collections {
		rec, ok := got.Find(w.ManagedCollection)
		if !ok {
			t.Errorf("no record for %s survived the round trip", w.ManagedCollection)
			continue
		}
		if rec.EmbeddingModel != w.EmbeddingModel {
			t.Errorf("%s: revision = %q, want %q", w.ManagedCollection, rec.EmbeddingModel, w.EmbeddingModel)
		}
		// The timestamp is what tells an operator when this Qdrant was last provisioned, so it has
		// to survive JSON as more than a zero value.
		if !rec.AppliedAt.Equal(w.AppliedAt) {
			t.Errorf("%s: applied_at = %v, want %v", w.ManagedCollection, rec.AppliedAt, w.AppliedAt)
		}
	}
}

// TestFileAppliedStateStore_Load_MissingFile_IsFirstRun pins the rule the whole first-apply path
// depends on: no file is a checkout that has never applied, not a failure to work around.
func TestFileAppliedStateStore_Load_MissingFile_IsFirstRun(t *testing.T) {
	store := &FileAppliedStateStore{Path: filepath.Join(t.TempDir(), "never-written.json")}

	state, err := store.Load()
	if err != nil {
		t.Fatalf("Load of a missing file = %v, want no error", err)
	}
	if len(state.Collections) != 0 {
		t.Errorf("Load of a missing file returned %d record(s), want an empty state", len(state.Collections))
	}
}

// TestFileAppliedStateStore_Load_CorruptJSON_TellsTheOperatorWhatToDo covers the file this store
// used to be able to produce itself, and the message an operator meets when they find one. Naming
// the file is not enough: the recovery is `git checkout`, and it is only obvious to someone who
// already knows the file is committed on purpose.
func TestFileAppliedStateStore_Load_CorruptJSON_TellsTheOperatorWhatToDo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "applied_state.json")
	// Exactly what a kill -9 between truncate and write leaves behind: valid JSON's opening, cut off.
	if err := os.WriteFile(path, []byte(`{"collections": [{"managed_col`), 0o600); err != nil {
		t.Fatalf("seeding a truncated file: %v", err)
	}

	_, err := (&FileAppliedStateStore{Path: path}).Load()
	if err == nil {
		t.Fatal("Load of a truncated file = nil error, want a parse error")
	}
	for _, want := range []string{path, "git checkout"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestFileAppliedStateStore_Save_ReplacesRatherThanTruncates is the atomicity proof, and it is
// mechanical rather than timing-based: a rename publishes a different file, an os.WriteFile
// truncates the same one in place. If the second Save leaves the same file behind, the write is
// the truncate-then-write kind, and a process killed mid-write leaves state no later apply can
// load — which blocks every run against a Qdrant that is perfectly correct.
func TestFileAppliedStateStore_Save_ReplacesRatherThanTruncates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "applied_state.json")
	store := &FileAppliedStateStore{Path: path}

	if err := store.Save(AppliedState{}.With("interno", "bge-m3@rev-a")); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after first Save: %v", err)
	}

	if err := store.Save(AppliedState{}.With("interno", "bge-m3@rev-b")); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after second Save: %v", err)
	}
	if os.SameFile(before, after) {
		t.Error("Save overwrote the file in place — a killed process leaves it truncated; " +
			"write to a temporary in the same directory and rename")
	}

	// The temporary must not survive its own success: a directory filling with .tmp-* files is a
	// committed directory nobody wants to review.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(path) {
			t.Errorf("Save left %q behind in %s", e.Name(), dir)
		}
	}

	// And the content that landed is the second one, not a merge of the two.
	state, err := store.Load()
	if err != nil {
		t.Fatalf("Load after second Save: %v", err)
	}
	if rec, _ := state.Find("interno"); rec.EmbeddingModel != "bge-m3@rev-b" {
		t.Errorf("loaded revision %q, want %q", rec.EmbeddingModel, "bge-m3@rev-b")
	}
}

// TestFileAppliedStateStore_Save_UnwritableDirectory_ReportsTheFile checks the failure an operator
// actually hits — a repo checked out read-only, or a --state-file pointed somewhere they do not own
// — and that the error names the path instead of some temporary file they never chose.
func TestFileAppliedStateStore_Save_UnwritableDirectory_ReportsTheFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not deny writes, so there is no failure to observe")
	}
	dir := t.TempDir()
	// Restored so t.TempDir's own cleanup can remove the directory afterwards.
	// #nosec G302 -- these are directory modes, not file modes: a directory with no execute bit
	// cannot be traversed at all, so 0600 would make the test unable to reach its own path.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	// #nosec G302 -- same: 0500 is exactly the "readable, not writable" directory under test.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skipf("cannot make %s read-only on this filesystem: %v", dir, err)
	}

	path := filepath.Join(dir, "applied_state.json")
	err := (&FileAppliedStateStore{Path: path}).Save(AppliedState{}.With("interno", "bge-m3@rev-a"))
	if err == nil {
		t.Fatal("Save into a read-only directory = nil error, want a failure")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the file it failed to write (%s)", err, path)
	}
}

// TestAppliedState_With_DoesNotMutateTheReceiver guards the property Apply relies on to leave the
// loaded state untouched until every collection has succeeded.
func TestAppliedState_With_DoesNotMutateTheReceiver(t *testing.T) {
	loaded := AppliedState{}.With("interno", "rev-a")
	updated := loaded.With("interno", "rev-b")

	rec, _ := loaded.Find("interno")
	if rec.EmbeddingModel != "rev-a" {
		t.Errorf("with() changed the receiver: %q, want %q", rec.EmbeddingModel, "rev-a")
	}
	rec, _ = updated.Find("interno")
	if rec.EmbeddingModel != "rev-b" {
		t.Errorf("with() returned %q, want %q", rec.EmbeddingModel, "rev-b")
	}
}

// TestAppliedState_With_KeepsRecordsSorted keeps the committed applied_state.json diff readable:
// map iteration order must never reach the file.
func TestAppliedState_With_KeepsRecordsSorted(t *testing.T) {
	s := AppliedState{}.With("interno", "r").With("base_paga", "r").With("clientes", "r")

	names := make([]string, len(s.Collections))
	for i, c := range s.Collections {
		names[i] = c.ManagedCollection
	}
	if !slices.IsSorted(names) {
		t.Errorf("applied-state records are %v, want them sorted by collection name", names)
	}
}
