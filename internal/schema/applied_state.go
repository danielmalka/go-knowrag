package schema

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// Applied-schema state: which embedding-model revision each managed collection was last
// successfully provisioned with.
//
// This does NOT become a fourth Qdrant collection, and the reason is not squeamishness about
// storage: Qdrant holds real collection properties (dimension, distance, named vectors, payload
// indexes) and nothing else. There is no "which model wrote these vectors" attribute to read back,
// and an empty collection does not even have a point to infer it from. So the record lives here,
// versioned in the repo next to the manifest it is compared against, which also means a model
// revision bump shows up as a reviewable diff beside the manifest change that caused it.
//
// Known and accepted gap: a checkout that never applied — a second operator machine pointed at an
// already-provisioned Qdrant, say — has no record, and model-revision drift is undetectable for
// that run. There is no oracle in Qdrant to fall back to. Dimension/distance drift still fires
// (apply.go), so a changed schema is still caught; a revision-only bump on an unclaimed checkout is
// not. This is why the file is meant to be committed.

// DefaultAppliedStatePath is where the committed record lives, relative to the repository root.
// `cli schema apply` therefore expects to run from the repo root, or to be pointed elsewhere with
// its --state-file flag.
const DefaultAppliedStatePath = "internal/schema/applied_state.json"

// AppliedState is the whole record: one entry per managed collection, kept sorted by name so the
// committed file's diff reflects what changed rather than map iteration order.
type AppliedState struct {
	Collections []AppliedCollection `json:"collections"`
}

// AppliedCollection is one collection's last successful apply.
type AppliedCollection struct {
	ManagedCollection string    `json:"managed_collection"`
	EmbeddingModel    string    `json:"embedding_model"`
	AppliedAt         time.Time `json:"applied_at"`
}

// find returns the record for the named collection. ok is false when there is none, which is the
// first-run case and explicitly not drift.
func (s AppliedState) find(name string) (AppliedCollection, bool) {
	i := slices.IndexFunc(s.Collections, func(c AppliedCollection) bool {
		return c.ManagedCollection == name
	})
	if i < 0 {
		return AppliedCollection{}, false
	}
	return s.Collections[i], true
}

// with returns a copy of s carrying model for the named collection, stamped now.
//
// It copies rather than mutates because Apply builds the next state while still holding the loaded
// one: it compares the two to decide whether writing is necessary at all, and an in-place update
// would make that comparison compare a value against itself.
func (s AppliedState) with(name, model string) AppliedState {
	next := AppliedState{Collections: slices.Clone(s.Collections)}
	rec := AppliedCollection{
		ManagedCollection: name,
		EmbeddingModel:    model,
		AppliedAt:         time.Now().UTC(),
	}
	if i := slices.IndexFunc(next.Collections, func(c AppliedCollection) bool {
		return c.ManagedCollection == name
	}); i >= 0 {
		next.Collections[i] = rec
	} else {
		next.Collections = append(next.Collections, rec)
	}
	slices.SortFunc(next.Collections, func(a, b AppliedCollection) int {
		return cmp.Compare(a.ManagedCollection, b.ManagedCollection)
	})
	return next
}

// sameRevisions reports whether s and other record the same revision for the same collections.
//
// AppliedAt is deliberately excluded: it moves on every apply, and comparing it would turn every
// no-op run into a file write — churning a committed file, and breaking the "second run performs
// zero writes" guarantee that proves idempotency.
func (s AppliedState) sameRevisions(other AppliedState) bool {
	if len(s.Collections) != len(other.Collections) {
		return false
	}
	for _, c := range s.Collections {
		rec, ok := other.find(c.ManagedCollection)
		if !ok || rec.EmbeddingModel != c.EmbeddingModel {
			return false
		}
	}
	return true
}

// AppliedStateStore is where the record is read from and written to. Apply takes one rather than a
// path so a test can supply an in-memory store and count its writes.
type AppliedStateStore interface {
	Load() (AppliedState, error)
	Save(AppliedState) error
}

// FileAppliedStateStore is the production store: the JSON file committed in this repo.
type FileAppliedStateStore struct {
	Path string
}

// Load reads the record. A missing file is the first run — an empty state and no error, never an
// error the operator has to work around before their first apply.
func (s *FileAppliedStateStore) Load() (AppliedState, error) {
	// #nosec G304 -- Path is operator configuration (a flag with a repo-relative default), not
	// input crossing a trust boundary.
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return AppliedState{}, nil
	}
	if err != nil {
		return AppliedState{}, fmt.Errorf("reading applied state %s: %w", s.Path, err)
	}

	var state AppliedState
	if err := json.Unmarshal(data, &state); err != nil {
		// The recovery is named because the operator has one and it is not obvious: this file is
		// committed by design, so the last good copy is in git, not in a backup nobody took.
		return AppliedState{}, fmt.Errorf(
			"parsing applied state %s: %w — this file is versioned in the repository, so restore the "+
				"last committed copy with `git checkout -- %s` and run apply again; if it was never "+
				"committed, delete it and the next apply will rewrite it from the manifest (losing "+
				"model-revision drift detection for that one run)",
			s.Path, err, s.Path)
	}
	return state, nil
}

// Save writes the record back, indented and newline-terminated because it is a file a human reads
// in a diff.
//
// The write goes to a temporary file next to the target and is renamed into place, because
// os.WriteFile truncates first: a process killed between the truncate and the write leaves a
// half-written file that the next Load refuses to parse, and an apply that cannot load its state
// never reaches Qdrant at all — so a crash here would block every later run against a Qdrant that
// is perfectly correct. The temporary lives in the same directory on purpose; a rename across
// filesystems is a copy, and a copy is not atomic.
func (s *FileAppliedStateStore) Save(state AppliedState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding applied state: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(s.Path), filepath.Base(s.Path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("creating a temporary file beside applied state %s: %w", s.Path, err)
	}
	// Removed on every path that does not end in a successful rename; after the rename the name no
	// longer exists and the failed Remove is ignored deliberately.
	defer func() { _ = os.Remove(tmp.Name()) }()

	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing applied state %s: %w", s.Path, err)
	}
	// Closed before the rename rather than deferred: a rename of a file with buffered writes still
	// pending would publish a file whose content has not reached the kernel yet.
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing applied state %s: %w", s.Path, err)
	}
	if err := os.Rename(tmp.Name(), s.Path); err != nil {
		return fmt.Errorf("writing applied state %s: %w", s.Path, err)
	}
	return nil
}
