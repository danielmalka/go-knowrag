package eval

import (
	"bytes"
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
var ErrGoldenSetMissing = errors.New("eval: golden set not found")

// GoldenQuestion is one question and the note that answers it.
//
// UID is a string rather than a uuid.UUID because it is what the YAML holds and because a hand-
// built question in a test should not have to import uuid to say "this is not a UUID". LoadGoldenSet
// proves it parses; the runner re-checks it before searching, because RunGolden also serves
// hand-built questions that never went through the loader (runner.go).
type GoldenQuestion struct {
	Question string `yaml:"question" json:"question"`
	UID      string `yaml:"uid" json:"uid"`

	// ChunkIndex is optional. Absent means any chunk of that UID counts as a hit; present means
	// that exact chunk and no other.
	ChunkIndex *int `yaml:"chunk_index,omitempty" json:"chunk_index,omitempty"`

	Area string `yaml:"area" json:"area"`

	// Author and Date are required and are documentation, not evidence. Authoring order is proven
	// by git history (provenance.go), never by these two fields — a date typed into a file the
	// author also controls proves nothing about when the entry landed.
	Author string `yaml:"author" json:"author"`
	Date   string `yaml:"date" json:"date"`
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
	// #nosec G304 -- the path is the golden set the operator named; reading a file the operator
	// asked to read is this function's whole job.
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return GoldenSet{}, fmt.Errorf("%w at %s: nothing was measured, and an eval with no "+
				"questions is not an eval that passed: %w", ErrGoldenSetMissing, path, err)
		}
		return GoldenSet{}, fmt.Errorf("eval: reading the golden set at %s: %w", path, err)
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var set GoldenSet
	if derr := dec.Decode(&set); derr != nil && !errors.Is(derr, io.EOF) {
		return GoldenSet{}, fmt.Errorf("eval: parsing the golden set at %s: %w", path, derr)
	}

	if len(set.Questions) == 0 {
		return GoldenSet{}, fmt.Errorf("eval: the golden set at %s declares no questions — an empty "+
			"golden set measures nothing and must not be run as if it did", path)
	}
	if err := validateQuestions(set.Questions); err != nil {
		return GoldenSet{}, fmt.Errorf("eval: the golden set at %s is invalid: %w", path, err)
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
