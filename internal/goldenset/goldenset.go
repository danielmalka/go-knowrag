// Package goldenset is the golden-set file: its schema, the reader that validates it, the coverage
// table that governs it, and the append that adds one entry to it.
//
// It is a package apart from internal/eval, and the reason is an import edge rather than tidiness.
// internal/goldenauthor is the interactive session in which the owner writes the questions, and it
// must have no way to obtain a search result — a question written after seeing what retrieval returns
// is a question tuned until it passes, and a golden set of those measures the tool that produced it.
// While the schema and the gate shared one package, importing the schema brought the gate: authoring
// code could call eval.LoadCorpus, eval.NewCorpusSearcher and eval.RunGolden and get real hits over a
// corpus file. Nothing here imports a searcher, so that no longer compiles, and
// TestArch_GoldenAuthoringCannotReachTheIndex (internal/archtest/boundary_test.go) is what keeps it
// that way — it forbids internal/eval to internal/goldenauthor, which is only possible because the
// schema left.
//
// So the rule for anything added here: this package may not reach internal/retrieval or
// internal/store, directly or through anything it imports. Measuring belongs on the other side of
// that line, in internal/eval — which is itself unreachable from here for the same reason, since it
// imports internal/retrieval. The test enforces reachability rather than a list of forbidden names,
// so a new dependency that searches is caught the day it lands.
package goldenset

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/danielmalka/go-knowrag/internal/config"
)

// ErrGoldenSetMissing is a golden set that is not there. It is a sentinel because the CLI has to
// tell it apart from a file that exists and is malformed: the first is "you have not authored one
// yet", the second is "the one you have is broken", and both used to surface as the same wall of
// YAML error text. errors.Is(err, fs.ErrNotExist) also holds — the underlying os error is wrapped —
// so a caller may match on either.
var ErrGoldenSetMissing = errors.New("goldenset: golden set not found")

// GoldenQuestion is one question and the note that answers it.
//
// UID is a string rather than a uuid.UUID because it is what the YAML holds and because a hand-
// built question in a test should not have to import uuid to say "this is not a UUID". LoadGoldenSet
// proves it parses; the runner re-checks it before searching, because RunGolden also serves
// hand-built questions that never went through the loader (internal/eval/runner.go).
type GoldenQuestion struct {
	Question string `yaml:"question" json:"question"`
	UID      string `yaml:"uid" json:"uid"`

	// ChunkIndex is optional. Absent means any chunk of that UID counts as a hit; present means
	// that exact chunk and no other.
	ChunkIndex *int `yaml:"chunk_index,omitempty" json:"chunk_index,omitempty"`

	Area string `yaml:"area" json:"area"`

	// Author and Date are required and are documentation, not evidence. Authoring order is proven
	// by git history (internal/eval/provenance.go), never by these two fields — a date typed into a
	// file the author also controls proves nothing about when the entry landed.
	Author string `yaml:"author" json:"author"`
	Date   string `yaml:"date" json:"date"`
}

// EntryIdentity is what makes a golden-set entry "the same question" across edits to the file.
//
// It hashes content, not position, because the obvious alternative does not work: `git blame` by
// line number reattributes every entry below any insertion, so rewrapping the file or moving one
// question rewrites the provenance of all the rest without a single question having changed. The
// NUL separator is there so that ("ab", "c") and ("a", "bc") cannot hash the same — a UUID contains
// no NUL, so the split point is unambiguous.
//
// It lives here rather than next to its only caller today (internal/eval/provenance.go, which keys
// the git attribution by it) because it is a property of the entry and needs no git and nothing else
// of the gate. The reason to care is the next obvious authoring feature — "warn me if I have already
// asked this" — whose natural implementation reuses exactly this hash. With it in internal/eval, that
// feature would make internal/goldenauthor import the gate for a one-line hash and reopen the route
// this package exists to close (see the package doc above).
func EntryIdentity(q GoldenQuestion) string {
	sum := sha256.Sum256([]byte(q.Question + "\x00" + q.UID))
	return hex.EncodeToString(sum[:])
}

// GoldenSet is the whole file: the questions and the coverage table they were authored against.
//
// The table lives in the file rather than as a literal in coverage.go, and that is not a style
// choice. This repository is public (CLAUDE.md, "Este repositório é público") and the area names in
// the real table are the owner's vault folders — the same class of value commit "chore: neutral
// tenant names in test fixtures" removed from the tracked fixtures. docs/eval/ is gitignored, so
// the table travels with the questions it governs and no vault folder name is ever written into a
// tracked Go file. The consequence to know about: coverage.go enforces the shape of a table, and
// the numbers it enforces are whatever this file declares — see ValidateCoverage, which refuses an
// empty table rather than passing everything.
type GoldenSet struct {
	Coverage  CoverageTable    `yaml:"coverage" json:"coverage"`
	Questions []GoldenQuestion `yaml:"questions" json:"questions"`
}

// LoadGoldenSet reads and validates one golden set.
//
// Strict decoding, so an unknown key is an error naming the key rather than a setting that silently
// never took effect — the same KnownFields(true) decoder internal/embed/config.go uses for the same
// reason. A misspelled `chunk_indx` must not read as "this entry accepts any chunk".
//
// The signature differs from S10 T1's `LoadGoldenSet(path) ([]GoldenQuestion, error)`: it returns
// the whole GoldenSet because the coverage table is part of the file. See the GoldenSet doc comment
// for why the table is not a literal in coverage.go.
func LoadGoldenSet(path string) (GoldenSet, error) {
	set, err := ReadGoldenSet(path)
	if err != nil {
		return GoldenSet{}, err
	}
	if len(set.Questions) == 0 {
		return GoldenSet{}, fmt.Errorf("goldenset: the golden set at %s declares no questions — an empty "+
			"golden set measures nothing and must not be run as if it did", path)
	}
	return set, nil
}

// ReadGoldenSet is LoadGoldenSet without the non-empty requirement: same file, same strict decoding,
// same per-entry validation, but a set with no questions comes back as a valid empty set instead of
// an error.
//
// It exists for the authoring side (authoring.go). A file that declares its coverage table and no
// questions yet is exactly what an author starts from, and it is the one caller for which "nothing
// was measured" is the normal state rather than the failure LoadGoldenSet refuses. Every other
// caller wants LoadGoldenSet: a *measuring* run over an empty set is a pass nobody earned.
func ReadGoldenSet(path string) (GoldenSet, error) {
	// #nosec G304 -- the path is the golden set the operator named; reading a file the operator
	// asked to read is this function's whole job.
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return GoldenSet{}, fmt.Errorf("%w at %s: nothing was measured, and an eval with no "+
				"questions is not an eval that passed: %w", ErrGoldenSetMissing, path, err)
		}
		return GoldenSet{}, fmt.Errorf("goldenset: reading the golden set at %s: %w", path, err)
	}
	return decodeGoldenSet(data, path)
}

// decodeGoldenSet parses and validates the bytes of one golden set. It is separate from the read so
// authoring.go can decode the file it is *about* to write without putting it on disk first.
func decodeGoldenSet(data []byte, path string) (GoldenSet, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var set GoldenSet
	if derr := dec.Decode(&set); derr != nil && !errors.Is(derr, io.EOF) {
		return GoldenSet{}, fmt.Errorf("goldenset: parsing the golden set at %s: %w", path, derr)
	}
	if err := validateQuestions(set.Questions); err != nil {
		return GoldenSet{}, fmt.Errorf("goldenset: the golden set at %s is invalid: %w", path, err)
	}
	return set, nil
}

// validateQuestions rejects every entry this package can prove is unusable, and reports all of them
// at once: an author fixing a 60-question file one error per run is an author who stops running it.
func validateQuestions(questions []GoldenQuestion) error {
	var errs []error
	for i, q := range questions {
		// Index is 1-based and the question text is echoed, because a YAML list has no line number
		// a reader can see from the error and "entry 34" alone sends them counting.
		where := fmt.Sprintf("entry %d (%q)", i+1, q.Question)
		if q.Question == "" {
			where = fmt.Sprintf("entry %d", i+1)
		}

		for _, required := range []struct{ field, value string }{
			{"question", q.Question},
			{"uid", q.UID},
			{"area", q.Area},
			{"author", q.Author},
			{"date", q.Date},
		} {
			if required.value == "" {
				errs = append(errs, fmt.Errorf("%s: field %q is missing or empty", where, required.field))
			}
		}

		if q.UID != "" {
			if _, err := uuid.Parse(q.UID); err != nil {
				errs = append(errs, fmt.Errorf("%s: field \"uid\" is %q, which is not a UUID: %w",
					where, q.UID, err))
			}
		}
		if q.Area != "" {
			// The area enum S10 T1 says to reuse no longer exists: D-26 moved `vault` and `area` out
			// of internal/schema into installation configuration (internal/config/config.go), so
			// which areas exist is a fact about a deployment and cannot be a constant here. What is
			// still a contract is the shape — the value is written verbatim into the payload and into
			// point_hash — and config.ValidateSlug is the one place that shape is spelled. Membership
			// in the real roster is checked against the coverage table instead (coverage.go).
			if err := config.ValidateSlug("area", q.Area); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", where, err))
			}
		}
		if q.ChunkIndex != nil && *q.ChunkIndex < 0 {
			errs = append(errs, fmt.Errorf("%s: field \"chunk_index\" is %d, which indexes no chunk",
				where, *q.ChunkIndex))
		}
	}
	return errors.Join(errs...)
}
