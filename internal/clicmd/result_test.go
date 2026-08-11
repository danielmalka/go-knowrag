package clicmd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// envelopeGolden is the wire contract of every --json answer this package writes, pinned as the
// exact bytes Emit produces rather than as a set of field assertions.
//
// A field-by-field test passes through a rename, which is the change that actually breaks a script
// in the runbook: the values stay correct and the keys move. The fixture is one line per envelope
// because that is how Emit writes them — a consumer reading the stream line by line depends on that
// as much as on the key names, and a test on the marshaled bytes alone would not notice it going
// away.
const envelopeGolden = "testdata/envelope_golden.jsonl"

// TestResult_JSON_MatchesGolden covers both shapes in the order the fixture lists them: a success
// carrying a payload, and a failure carrying a category and a message.
func TestResult_JSON_MatchesGolden(t *testing.T) {
	var got bytes.Buffer

	// A payload with two keys of different types, so the fixture would catch a `data` that started
	// being wrapped, flattened or re-encoded as a string.
	if err := Emit(&got, Succeeded(map[string]any{"collection": "interno", "hits": 2})); err != nil {
		t.Fatalf("Emit(success): %v", err)
	}
	if err := Emit(&got, Failed(Usage("--tenant is required"))); err != nil {
		t.Fatalf("Emit(failure): %v", err)
	}

	want, err := os.ReadFile(envelopeGolden) // #nosec G304 -- a literal path inside the package
	if err != nil {
		t.Fatalf("reading the golden fixture: %v", err)
	}
	if got.String() != string(want) {
		t.Errorf("the --json envelope changed. Every consumer reading these keys breaks with it; if "+
			"the change is intended, update %s deliberately.\n\ngot:\n%s\nwant:\n%s",
			envelopeGolden, got.String(), want)
	}
}

// TestResult_JSON_OmitsTheKeyItHasNothingFor is the half a golden of two populated envelopes cannot
// state: a success must carry no `error` key at all, and a failure no `data`. A consumer that
// branches on key presence — which is the ordinary way to read this envelope — is broken by an
// `error` object full of empty strings just as surely as by a renamed field.
func TestResult_JSON_OmitsTheKeyItHasNothingFor(t *testing.T) {
	var success, failure bytes.Buffer
	if err := Emit(&success, Succeeded([]string{"a"})); err != nil {
		t.Fatalf("Emit(success): %v", err)
	}
	if err := Emit(&failure, Failed(errors.New("qdrant is unreachable"))); err != nil {
		t.Fatalf("Emit(failure): %v", err)
	}

	if strings.Contains(success.String(), `"error"`) {
		t.Errorf("a successful envelope carries an error key: %s", success.String())
	}
	if strings.Contains(failure.String(), `"data"`) {
		t.Errorf("a failed envelope carries a data key: %s", failure.String())
	}
}

// TestCategoryOf_RecoversThroughWrapping is the property the whole mechanism rests on: cobra's
// Execute returns one error for the whole tree, and whatever wrapped it on the way up must not cost
// the category. If this stops holding, every categorized failure silently exits on the backend code.
func TestCategoryOf_RecoversThroughWrapping(t *testing.T) {
	tests := map[string]struct {
		err  error
		want Category
	}{
		"usage, wrapped twice": {
			err:  fmt.Errorf("running the command: %w", fmt.Errorf("search: %w", Usage("--tenant is required"))),
			want: CategoryUsage,
		},
		"backend, wrapped": {
			err:  fmt.Errorf("search: %w", Backend(errors.New("qdrant is unreachable"))),
			want: CategoryBackend,
		},
		"assertion": {
			err:  &Error{Category: CategoryAssertion, Err: errors.New("the golden set scored 0.60")},
			want: CategoryAssertion,
		},
		// The default, and the reason it is the default: nobody classified this, and reporting an
		// unclassified failure as a usage error would tell a scheduler to stop retrying an outage.
		"uncategorized falls back to backend": {
			err:  errors.New("something nobody categorized"),
			want: CategoryBackend,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := CategoryOf(tc.err); got != tc.want {
				t.Errorf("CategoryOf(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestError_KeepsTheUnderlyingMessage covers the other half of the seam: the category must not cost
// the operator the sentence that says what happened.
func TestError_KeepsTheUnderlyingMessage(t *testing.T) {
	underlying := errors.New("qdrant is unreachable at qdrant.internal:6334")
	err := Backend(underlying)

	if err.Error() != underlying.Error() {
		t.Errorf("Backend(err).Error() = %q, want the underlying message %q", err, underlying)
	}
	if !errors.Is(err, underlying) {
		t.Errorf("errors.Is could not reach the underlying error through %T", err)
	}
}

// TestEmit_ReportsAWriteFailure is why Emit returns an error at all. A consumer that received half
// an envelope and a zero exit status was told the command succeeded by a write that did not finish.
func TestEmit_ReportsAWriteFailure(t *testing.T) {
	if err := Emit(failingWriter{}, Succeeded("anything")); err == nil {
		t.Fatal("Emit onto a broken writer returned nil, so a truncated envelope would exit 0")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("the pipe is closed") }
