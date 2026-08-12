package clicmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Category is what kind of failure a command is reporting, and it exists so the process can exit on
// a number that means something.
//
// The three are the three different things an operator or a scheduler does next. A usage failure is
// fixed by changing the command line, and retrying it verbatim fails identically. A backend failure
// is worth retrying, because the thing that broke is not the request. An assertion failure is a run
// that worked perfectly and answered no — an evaluation below its threshold — which is neither of
// the other two and must not be reported as a broken system.
//
// The exit codes themselves are deliberately not here. They are the process's contract, they live
// beside the ones the ingestion already owns, and cmd/cli/main.go is where all of them are read at
// once — a second list of numbers in a second file is how two of them end up meaning the same thing.
type Category string

const (
	CategoryUsage     Category = "usage"
	CategoryBackend   Category = "backend"
	CategoryAssertion Category = "assertion"
)

// Error is a failure that carries its category through cobra.
//
// It exists because cobra's Execute() returns one plain error for the whole tree, so by the time a
// subcommand's refusal reaches main it has lost everything except its message — and guessing the
// category back out of message text is the sort of thing that works until somebody rewords a
// sentence. A command returns this instead, and main recovers the category with errors.As.
type Error struct {
	Category Category
	Err      error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// Usage builds a refusal about how the command was invoked.
func Usage(format string, args ...any) error {
	return &Error{Category: CategoryUsage, Err: fmt.Errorf(format, args...)}
}

// Backend wraps a failure of something the command depends on.
func Backend(err error) error { return &Error{Category: CategoryBackend, Err: err} }

// Assertion marks a command that ran perfectly and answered no — a gate below its threshold. It is
// a separate category from Backend because the two need opposite things from whoever reads them:
// a backend failure is worth retrying, and a gate that failed will fail again until somebody fixes
// what it measured.
func Assertion(format string, args ...any) error {
	return &Error{Category: CategoryAssertion, Err: fmt.Errorf(format, args...)}
}

// CategoryOf reports the category err carries, and CategoryBackend when it carries none.
//
// The default is the whole reason this is a function rather than a type switch at each call site:
// an uncategorized error is one nobody classified, and the safe reading of "nobody classified it"
// is that something broke rather than that the operator typed the command wrong. Reported as usage,
// a genuine outage would tell a scheduler to stop retrying.
func CategoryOf(err error) Category {
	var e *Error
	if errors.As(err, &e) {
		return e.Category
	}
	return CategoryBackend
}

// Result is the envelope every --json answer in this package is wrapped in.
//
// The shape is the contract, not the Go type: the same arrangement internal/ingest/reportjson.go
// documents at length, for the same reason and with the same honest limit on what it buys. Producer
// and golden fixture live in one repo, so a patch that renames a key here and regenerates the
// fixture passes green — nothing stops it. What the fixture guarantees is that such a patch cannot
// be an accident and cannot be invisible: it lands in the diff as a deliberate edit to a file whose
// entire job is the wire format.
//
// Data is `any` because each command declares its own payload type. That type is always a type
// written for the wire — never a struct the rest of the program also uses — so that renaming a Go
// field cannot silently rewrite what a script reads.
type Result struct {
	OK   bool `json:"ok"`
	Data any  `json:"data,omitempty"`
	// Error is a pointer so a successful answer has no `error` key at all, rather than an `error`
	// object full of empty strings that a consumer has to test field by field.
	Error *ErrorInfo `json:"error,omitempty"`
}

// ErrorInfo is a failure as a consumer reads it: the category it can branch on, and the message a
// human eventually has to read.
type ErrorInfo struct {
	Category string `json:"category"`
	Message  string `json:"message"`
}

// Succeeded wraps a payload.
func Succeeded(data any) Result { return Result{OK: true, Data: data} }

// Failed wraps a failure, taking its category from the error itself so the JSON envelope and the
// process exit code can never disagree about what kind of failure it was — both read CategoryOf.
func Failed(err error) Result {
	return Result{Error: &ErrorInfo{Category: string(CategoryOf(err)), Message: err.Error()}}
}

// Emit writes one envelope as a single line of JSON.
//
// The write error is returned rather than dropped, unlike most printing in this repo: a closed pipe
// or a full disk truncates the document mid-object, and a consumer that got half an envelope and a
// zero exit status has been told the command succeeded by a write that did not finish.
func Emit(w io.Writer, r Result) error {
	encoded, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("rendering the result envelope: %w", err)
	}
	if _, err := fmt.Fprintln(w, string(encoded)); err != nil {
		return fmt.Errorf("writing the result envelope: %w", err)
	}
	return nil
}
